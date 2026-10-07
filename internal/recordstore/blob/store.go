package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

// Store abstracts the storage of build logs and build artifacts. Two
// kinds of read exist. A chunked read serves an object Varroa wrote
// itself, one part at a time, which is a log. A native read serves an
// object a foreign writer wrote whole, which is an artifact.
type Store interface {
	// Put writes a complete object as sequence 0. It returns
	// ErrObjectExists when any part of key already exists.
	Put(ctx context.Context, key string, r io.Reader, opts ObjectOptions) error

	// Replace writes a complete object as sequence 0, over whatever key
	// already holds. Put answers ErrObjectExists for a key that already
	// holds any part, so a writer that rewrites one whole object needs
	// this pair instead.
	Replace(ctx context.Context, key string, r io.Reader, opts ObjectOptions) error

	// Append writes one chunk at sequence seq. The writer owns sequence
	// assignment and starts at 0. A retry that carries identical bytes
	// succeeds. A retry that carries different bytes returns
	// ErrChunkConflict. The store itself enforces the write-once rule
	// when the write commits: a call that started before another writer
	// created the part never replaces that part, however late it lands.
	Append(ctx context.Context, key string, seq int64, p []byte, opts ObjectOptions) error

	// Overwrite writes one chunk at sequence seq over the part that
	// sequence holds, but only while that part still carries expect. The
	// store checks expect when the write commits. It returns
	// ErrChunkConflict when the stored stamp differs from expect, and
	// when key holds no part at seq. Append stays write-once. Only a
	// writer that read the stored part and knows it is stale may call
	// Overwrite: the log writer calls it for a part that a lost writer
	// left above a hole, and for its own index reservation.
	Overwrite(ctx context.Context, key string, seq int64, p []byte, expect Stamp, opts ObjectOptions) error

	// Parts lists every chunked part of key at or above from, in
	// ascending sequence order, holes included, with the byte size of
	// each part. Unlike NewRangeReader, it does not stop at a missing
	// sequence. It returns an empty list when key holds no such part.
	// A driver answers the sizes from its listing and reads no part.
	Parts(ctx context.Context, key string, from int64) ([]Part, error)
	// PartStamp answers the stamp of the part at seq: the digest of its
	// bytes, and the ObjectOptions.Writer it was last written with ("" when
	// that write named none). It returns ErrObjectNotFound when key holds
	// no part at seq.
	PartStamp(ctx context.Context, key string, seq int64) (Stamp, error)

	// NewRangeReader concatenates the parts of a chunked object in
	// ascending sequence order, starting offset bytes from the start.
	// length -1 reads to the end. It stops at the first missing sequence
	// and returns io.EOF, because a gap in a live log is a short read,
	// not an error. It returns ErrObjectNotFound when key holds no part,
	// and ErrInvalidRange for a negative offset or a length below -1.
	NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// Delete removes every row or object at key, in either chunked or
	// native form. It returns no error when key holds nothing.
	Delete(ctx context.Context, key string) error

	// DeletePrefix removes every object whose key starts with prefix.
	DeletePrefix(ctx context.Context, prefix string) error

	// NewObjectReader opens the one native object at key and returns its
	// total size alongside a reader. offset and length name bytes inside
	// that object, with the same -1 rule as NewRangeReader. It returns
	// ErrObjectNotFound when key holds no native object, including when
	// key holds only chunked parts.
	NewObjectReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, int64, error)

	// SignedURL mints a read URL for key, valid for ttl. It signs the
	// exact key it receives, derives nothing, and appends no part
	// suffix: the caller owns the key layout. It returns ErrUnsupported
	// on a tier that cannot sign a URL.
	SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Part names one chunked part of an object: its sequence and its byte
// size.
type Part struct {
	Seq  int64
	Size int64
}

// Stamp identifies the content of one stored part: the "sha256:<hex>"
// digest of its bytes and the writer that stored it. Overwrite compares
// it to decide whether a part changed since the caller read it.
type Stamp struct {
	Digest string
	Writer string
}

// ObjectOptions carries the metadata a write needs.
type ObjectOptions struct {
	// ObjectClass is "log", "artifact", or "state". pgblob writes it to
	// log_chunks.object_class.
	ObjectClass string
	// ContentType is the MIME type of the object.
	ContentType string
	// CreatedAt is supplied by the caller and stays the same for every
	// part of one object. pgblob uses it as part of the log_chunks
	// primary key, so a default value would give a retry a different
	// key and insert a second row.
	CreatedAt time.Time
	// Writer names the writer of a log part. The driver stores it with
	// the part, and PartStamp reads it back. Restart recovery compares
	// it to tell a moved build's parts from the parts of the cell that
	// lost the build. It is optional.
	Writer string
}

// Validate reports ErrInvalidOptions when ObjectClass is empty or
// CreatedAt is zero.
func (o ObjectOptions) Validate() error {
	if o.ObjectClass == "" || o.CreatedAt.IsZero() {
		return ErrInvalidOptions
	}
	return nil
}

// Sentinel errors every Store driver returns.
var (
	// ErrChunkConflict means a retried Append carried bytes that differ
	// from the chunk already stored at that key and sequence, or that an
	// Overwrite found a stored part that no longer carries its expected
	// stamp.
	ErrChunkConflict = errors.New("blob: chunk conflict")
	// ErrObjectExists means Put found a part already at key.
	ErrObjectExists = errors.New("blob: object already exists")
	// ErrObjectNotFound means a read found nothing at key, in the form
	// the caller asked for.
	ErrObjectNotFound = errors.New("blob: object not found")
	// ErrInvalidRange means offset is negative or length is below -1.
	ErrInvalidRange = errors.New("blob: invalid range")
	// ErrInvalidOptions means an ObjectOptions value is missing a
	// required field.
	ErrInvalidOptions = errors.New("blob: invalid object options")
	// ErrUnsupported means the tier cannot sign a read URL. pgblob
	// always returns it: the Postgres tier stores bytes in log_chunks
	// and can sign no URL.
	ErrUnsupported = errors.New("blob: signed URLs are not supported on this tier")
)

// Digest returns the "sha256:<hex>" digest of p, the value a Stamp
// carries.
func Digest(p []byte) string {
	return sha256Digest(p)
}

// sha256Digest returns the "sha256:<hex>" digest of p. Every driver
// stores this value so an Append retry can tell an identical write from
// a conflicting one.
func sha256Digest(p []byte) string {
	sum := sha256.Sum256(p)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// validateRange reports ErrInvalidRange for a negative offset or a
// length below -1.
func validateRange(offset, length int64) error {
	if offset < 0 || length < -1 {
		return ErrInvalidRange
	}
	return nil
}

// sliceReader returns a closer over the bytes of all, starting at
// offset, for at most length bytes. length -1 reads to the end. An
// offset past the end of all yields an empty reader.
//
// The upper bound compares length against end-offset, never offset+length:
// a caller-supplied length as large as math.MaxInt64 (an unbounded Range
// header end) would overflow offset+length into a negative number, and
// the resulting slice expression would panic with a negative bound.
// end and offset are both already clamped to len(all), so end-offset
// never overflows.
func sliceReader(all []byte, offset, length int64) io.ReadCloser {
	if offset > int64(len(all)) {
		offset = int64(len(all))
	}
	end := int64(len(all))
	if length >= 0 && length < end-offset {
		end = offset + length
	}
	return io.NopCloser(bytes.NewReader(all[offset:end]))
}
