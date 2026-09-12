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

	// Append writes one chunk at sequence seq. The writer owns sequence
	// assignment and starts at 0. A retry that carries identical bytes
	// succeeds. A retry that carries different bytes returns
	// ErrChunkConflict.
	Append(ctx context.Context, key string, seq int64, p []byte, opts ObjectOptions) error

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

// ObjectOptions carries the metadata a write needs.
type ObjectOptions struct {
	// ObjectClass is "log" or "artifact". pgblob writes it to
	// log_chunks.object_class.
	ObjectClass string
	// ContentType is the MIME type of the object.
	ContentType string
	// CreatedAt is supplied by the caller and stays the same for every
	// part of one object. pgblob uses it as part of the log_chunks
	// primary key, so a default value would give a retry a different
	// key and insert a second row.
	CreatedAt time.Time
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
	// from the chunk already stored at that key and sequence.
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
