package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// nativeChunkSeq is the chunk_seq value that marks a native, whole-object
// row. A chunked sequence starts at 0, so this value never collides with
// a chunked part.
const nativeChunkSeq = int64(-1)

// pgStore is the Store driver over table varroa.log_chunks.
type pgStore struct {
	pool *pgxpool.Pool
}

// NewPGBlobStore returns a Store backed by pool. The caller supplies a
// pool already pointed at the database that holds schema varroa.
func NewPGBlobStore(pool *pgxpool.Pool) Store {
	return &pgStore{pool: pool}
}

// Put writes key as a single row at sequence 0. It returns
// ErrObjectExists when any row already exists for key, chunked or
// native, so a whole-object write never half-overwrites an appended log.
func (s *pgStore) Put(ctx context.Context, key string, r io.Reader, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read object body: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin put transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM varroa.log_chunks WHERE object_key = $1)
	`, key).Scan(&exists); err != nil {
		return fmt.Errorf("check existing object: %w", err)
	}
	if exists {
		return ErrObjectExists
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.log_chunks (object_key, chunk_seq, created_at, object_class, content_type, digest, bytes, writer)
		VALUES ($1, 0, $2, $3, $4, $5, $6, $7)
	`, key, opts.CreatedAt, opts.ObjectClass, nullableString(opts.ContentType), sha256Digest(data), data, opts.Writer); err != nil {
		if isUniqueViolation(err) {
			return ErrObjectExists
		}
		return fmt.Errorf("insert object: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit put transaction: %w", err)
	}
	return nil
}

// Replace deletes every row of key and inserts the new object as
// sequence 0, inside one transaction, so a reader never sees the key
// empty.
func (s *pgStore) Replace(ctx context.Context, key string, r io.Reader, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read object body: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, `DELETE FROM varroa.log_chunks WHERE object_key = $1`, key); err != nil {
		return fmt.Errorf("delete replaced object: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.log_chunks (object_key, chunk_seq, created_at, object_class, content_type, digest, bytes, writer)
		VALUES ($1, 0, $2, $3, $4, $5, $6, $7)
	`, key, opts.CreatedAt, opts.ObjectClass, nullableString(opts.ContentType), sha256Digest(data), data, opts.Writer); err != nil {
		return fmt.Errorf("insert replaced object: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit replace transaction: %w", err)
	}
	return nil
}

// Append inserts one row per call, holding the chunk's sha256 in digest,
// with ON CONFLICT DO NOTHING on the primary key. It then reads the
// stored digest: an identical retry succeeds, and a conflicting retry
// returns ErrChunkConflict.
func (s *pgStore) Append(ctx context.Context, key string, seq int64, p []byte, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	digest := sha256Digest(p)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO varroa.log_chunks (object_key, chunk_seq, created_at, object_class, content_type, digest, bytes, writer)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (created_at, object_key, chunk_seq) DO NOTHING
	`, key, seq, opts.CreatedAt, opts.ObjectClass, nullableString(opts.ContentType), digest, p, opts.Writer); err != nil {
		return fmt.Errorf("append chunk: %w", err)
	}

	var stored string
	if err := s.pool.QueryRow(ctx, `
		SELECT digest FROM varroa.log_chunks
		WHERE created_at = $1 AND object_key = $2 AND chunk_seq = $3
	`, opts.CreatedAt, key, seq).Scan(&stored); err != nil {
		return fmt.Errorf("read stored chunk digest: %w", err)
	}
	if stored != digest {
		return ErrChunkConflict
	}
	return nil
}

// Overwrite inserts one row, or replaces the bytes, the digest, and the
// writer of the row already at the same primary key. It sets no other
// column, because role varroa_gateway holds UPDATE on those three alone.
func (s *pgStore) Overwrite(ctx context.Context, key string, seq int64, p []byte, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO varroa.log_chunks (object_key, chunk_seq, created_at, object_class, content_type, digest, bytes, writer)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (created_at, object_key, chunk_seq) DO UPDATE
		SET digest = EXCLUDED.digest, bytes = EXCLUDED.bytes, writer = EXCLUDED.writer
	`, key, seq, opts.CreatedAt, opts.ObjectClass, nullableString(opts.ContentType), sha256Digest(p), p, opts.Writer); err != nil {
		return fmt.Errorf("overwrite chunk: %w", err)
	}
	return nil
}

// PartWriter reads the writer column of the row at seq.
func (s *pgStore) PartWriter(ctx context.Context, key string, seq int64) (string, error) {
	var writer string
	err := s.pool.QueryRow(ctx, `
		SELECT writer FROM varroa.log_chunks
		WHERE object_key = $1 AND chunk_seq = $2
		LIMIT 1
	`, key, seq).Scan(&writer)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrObjectNotFound
	case err != nil:
		return "", fmt.Errorf("read part writer: %w", err)
	}
	return writer, nil
}

// Parts lists every chunked row of key at or above from, with its byte
// size. The native row, at chunk_seq -1, is never a part.
func (s *pgStore) Parts(ctx context.Context, key string, from int64) ([]Part, error) {
	if from < 0 {
		from = 0
	}
	rows, err := s.pool.Query(ctx, `
		SELECT chunk_seq, octet_length(bytes) FROM varroa.log_chunks
		WHERE object_key = $1 AND chunk_seq >= $2
		ORDER BY chunk_seq ASC
	`, key, from)
	if err != nil {
		return nil, fmt.Errorf("query parts: %w", err)
	}
	defer rows.Close()
	parts := []Part{}
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.Seq, &p.Size); err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		parts = append(parts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate parts: %w", err)
	}
	return parts, nil
}

// NewRangeReader concatenates the chunked parts of key in ascending
// sequence order, starting at offset. It stops at the first missing
// sequence, so a live log gap is a short read, not an error.
func (s *pgStore) NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := validateRange(offset, length); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT chunk_seq, bytes FROM varroa.log_chunks
		WHERE object_key = $1 AND chunk_seq >= 0
		ORDER BY chunk_seq ASC
	`, key)
	if err != nil {
		return nil, fmt.Errorf("query chunks: %w", err)
	}
	defer rows.Close()

	var buf bytes.Buffer
	wantSeq, found := int64(0), false
	for rows.Next() {
		var seq int64
		var data []byte
		if err := rows.Scan(&seq, &data); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		if seq != wantSeq {
			break
		}
		found = true
		buf.Write(data)
		wantSeq++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read chunks: %w", err)
	}
	if !found {
		return nil, ErrObjectNotFound
	}
	return sliceReader(buf.Bytes(), offset, length), nil
}

// NewObjectReader reads the single native row for key, the one whose
// chunk_seq is -1. It returns ErrObjectNotFound when key holds no native
// row, including when key holds only chunked parts.
func (s *pgStore) NewObjectReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, int64, error) {
	if err := validateRange(offset, length); err != nil {
		return nil, 0, err
	}

	var data []byte
	err := s.pool.QueryRow(ctx, `
		SELECT bytes FROM varroa.log_chunks WHERE object_key = $1 AND chunk_seq = $2
	`, key, nativeChunkSeq).Scan(&data)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, 0, ErrObjectNotFound
	case err != nil:
		return nil, 0, fmt.Errorf("read native object: %w", err)
	}

	total := int64(len(data))
	return sliceReader(data, offset, length), total, nil
}

// Delete removes every row of key, chunked and native alike. It reports
// no error for a key that holds nothing, so a retried retention round
// succeeds.
func (s *pgStore) Delete(ctx context.Context, key string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM varroa.log_chunks WHERE object_key = $1`, key); err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

// DeletePrefix removes every row whose object_key starts with prefix.
func (s *pgStore) DeletePrefix(ctx context.Context, prefix string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM varroa.log_chunks WHERE starts_with(object_key, $1)`, prefix); err != nil {
		return fmt.Errorf("delete prefix: %w", err)
	}
	return nil
}

// SignedURL always returns ErrUnsupported. The Postgres tier stores
// bytes in log_chunks and can sign no URL.
func (s *pgStore) SignedURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", ErrUnsupported
}

// nullableString returns a pointer for a non-empty string, and nil for
// an empty one, so an unset content type stores SQL NULL.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
