package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// digestMetadataKey is the gocloud.dev object-metadata key that holds a
// part's sha256 digest. gocloud.dev lowercases every metadata key on
// write and on read, so this name stays lowercase.
const digestMetadataKey = "digest"

// writerMetadataKey is the gocloud.dev object-metadata key that holds a
// part's ObjectOptions.Writer.
const writerMetadataKey = "writer"

// bucketStore is the Store driver over a gocloud.dev bucket. A chunked
// object's parts live at "<key>/<seq padded to 12 digits>". A native
// object lives at the bare key, so the two forms never share a key
// space.
type bucketStore struct {
	bucket *blob.Bucket
	// afterCheck, when not nil, runs between Append's existence check
	// and its write. Only a test sets it, to put another writer's part
	// into that window.
	afterCheck func(key string)
	// beforeOverwrite, when not nil, runs between Overwrite's stamp
	// check and its conditional write. Only a test sets it, to change
	// the part inside that window.
	beforeOverwrite func(key string)
	// isS3 is true for a bucket of the S3 driver, whose object metadata
	// holds the true digest of a part.
	isS3 bool
}

// ErrOverwriteUnsupported is the error Overwrite returns on a driver
// that can send no If-Match condition. A retry can never succeed.
var ErrOverwriteUnsupported = errors.New("blob: conditional overwrite needs the s3 driver; this bucket cannot send If-Match")

// NewBucketStore returns a Store backed by bucket.
func NewBucketStore(bucket *blob.Bucket) Store {
	var client *s3.Client
	return &bucketStore{bucket: bucket, isS3: bucket.As(&client)}
}

// Put writes key as a single part at sequence 0. It returns
// ErrObjectExists when any chunked part already exists under key.
func (s *bucketStore) Put(ctx context.Context, key string, r io.Reader, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	exists, err := s.anyPartExists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		return ErrObjectExists
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read object body: %w", err)
	}
	return s.writeObject(ctx, partKey(key, 0), data, opts, false)
}

// Replace deletes every part under key and writes the new object as
// sequence 0. The object tier carries no transaction, so a reader can
// observe the key absent between the two calls. A run-state reader
// retries, because the next push writes the object again.
func (s *bucketStore) Replace(ctx context.Context, key string, r io.Reader, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read object body: %w", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		return err
	}
	return s.writeObject(ctx, partKey(key, 0), data, opts, false)
}

// Append writes one part object at sequence seq, with the digest in
// object metadata. An identical retry succeeds. A conflicting retry
// returns ErrChunkConflict.
func (s *bucketStore) Append(ctx context.Context, key string, seq int64, p []byte, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	return s.writePart(ctx, partKey(key, seq), p, opts)
}

// Overwrite writes one part object at sequence seq, with the digest in
// object metadata, over a part that still carries expect.
//
// It reads the part's attributes, compares the stamp with expect, and
// then writes with If-Match on the ETag it read, so the bucket refuses
// the write when the part changed after the read. An ETag covers the
// bytes alone: a part that another writer stored with the same bytes
// passes If-Match. The stamp check before the write still compares the
// writer. Only the S3 driver carries If-Match. fileblob carries no
// condition, so on that driver the check before the write is the only
// guard. Every other driver refuses the write.
func (s *bucketStore) Overwrite(ctx context.Context, key string, seq int64, p []byte, expect Stamp, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	pk := partKey(key, seq)
	attrs, err := s.bucket.Attributes(ctx, pk)
	switch {
	case gcerrors.Code(err) == gcerrors.NotFound:
		return ErrChunkConflict
	case err != nil:
		return fmt.Errorf("read part attributes %q: %w", pk, err)
	}
	stored, err := s.stampOf(ctx, pk, attrs)
	if err != nil {
		return err
	}
	if stored != expect {
		return ErrChunkConflict
	}
	if s.beforeOverwrite != nil {
		s.beforeOverwrite(pk)
	}
	err = s.writeIfMatch(ctx, pk, p, opts, attrs.ETag)
	switch gcerrors.Code(err) {
	case gcerrors.OK:
		return nil
	case gcerrors.FailedPrecondition, gcerrors.NotFound:
		return ErrChunkConflict
	default:
		return err
	}
}

// PartStamp reads the writer from the part's object metadata, and the
// digest as stampOf answers it.
func (s *bucketStore) PartStamp(ctx context.Context, key string, seq int64) (Stamp, error) {
	pk := partKey(key, seq)
	attrs, err := s.bucket.Attributes(ctx, pk)
	switch {
	case gcerrors.Code(err) == gcerrors.NotFound:
		return Stamp{}, ErrObjectNotFound
	case err != nil:
		return Stamp{}, fmt.Errorf("read part attributes %q: %w", pk, err)
	}
	return s.stampOf(ctx, pk, attrs)
}

// stampOf answers the stamp of the part at pk. On S3 the digest comes
// from object metadata. On every other driver it is computed from the
// stored bytes: fileblob writes the metadata of a refused conditional
// write, so its digest metadata can name bytes it never stored.
func (s *bucketStore) stampOf(ctx context.Context, pk string, attrs *blob.Attributes) (Stamp, error) {
	stamp := Stamp{Digest: attrs.Metadata[digestMetadataKey], Writer: attrs.Metadata[writerMetadataKey]}
	if s.isS3 {
		return stamp, nil
	}
	data, err := s.bucket.ReadAll(ctx, pk)
	switch {
	case gcerrors.Code(err) == gcerrors.NotFound:
		return Stamp{}, ErrObjectNotFound
	case err != nil:
		return Stamp{}, fmt.Errorf("read part %q: %w", pk, err)
	}
	stamp.Digest = sha256Digest(data)
	return stamp, nil
}

// writeIfMatch writes data at pk with If-Match on etag. The condition
// reaches the S3 upload through BeforeWrite, because gocloud.dev has no
// If-Match option of its own. fileblob takes the write with no
// condition. Every other driver gets ErrOverwriteUnsupported.
func (s *bucketStore) writeIfMatch(ctx context.Context, pk string, data []byte, opts ObjectOptions, etag string) error {
	unsupported := false
	err := s.writeObjectWith(ctx, pk, data, opts, false, ifMatchBeforeWrite(etag, &unsupported))
	if unsupported {
		return ErrOverwriteUnsupported
	}
	return err
}

// ifMatchBeforeWrite answers the BeforeWrite hook that sets If-Match to
// etag on an S3 upload. It accepts a fileblob write with no condition.
// For every other driver it sets *unsupported and fails the write.
func ifMatchBeforeWrite(etag string, unsupported *bool) func(func(any) bool) error {
	return func(asFunc func(any) bool) error {
		var upload *transfermanager.UploadObjectInput
		if asFunc(&upload) {
			upload.IfMatch = &etag
			return nil
		}
		var file *os.File
		if asFunc(&file) {
			return nil
		}
		*unsupported = true
		return ErrOverwriteUnsupported
	}
}

// Parts lists every part under key at or above from, with the size the
// bucket listing reports. It reads no part.
func (s *bucketStore) Parts(ctx context.Context, key string, from int64) ([]Part, error) {
	parts, err := s.listParts(ctx, key)
	if err != nil {
		return nil, err
	}
	out := []Part{}
	for _, part := range parts {
		if part.seq >= from {
			out = append(out, Part{Seq: part.seq, Size: part.size})
		}
	}
	return out, nil
}

// NewRangeReader concatenates the parts under key in ascending sequence
// order, starting at offset. It stops at the first missing sequence.
func (s *bucketStore) NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := validateRange(offset, length); err != nil {
		return nil, err
	}
	parts, err := s.listParts(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, ErrObjectNotFound
	}

	var buf bytes.Buffer
	wantSeq := int64(0)
	for _, part := range parts {
		if part.seq != wantSeq {
			break
		}
		data, err := s.bucket.ReadAll(ctx, part.key)
		if err != nil {
			return nil, fmt.Errorf("read part %q: %w", part.key, err)
		}
		buf.Write(data)
		wantSeq++
	}
	return sliceReader(buf.Bytes(), offset, length), nil
}

// NewObjectReader opens the bare-key object and reports its total size.
func (s *bucketStore) NewObjectReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, int64, error) {
	if err := validateRange(offset, length); err != nil {
		return nil, 0, err
	}
	r, err := s.bucket.NewRangeReader(ctx, key, offset, length, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, 0, ErrObjectNotFound
		}
		return nil, 0, fmt.Errorf("open native reader %q: %w", key, err)
	}
	return r, r.Size(), nil
}

// Delete removes every part under key and the bare-key object. It
// reports no error when key holds nothing.
func (s *bucketStore) Delete(ctx context.Context, key string) error {
	if err := s.DeletePrefix(ctx, key+"/"); err != nil {
		return err
	}
	if err := s.bucket.Delete(ctx, key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// SignedURL delegates to the gocloud.dev bucket's own signer. It signs
// the bare key with no part suffix, matching the native-object key
// space Put and NewObjectReader already use. A driver or a bucket
// configured with no signer answers whatever error gocloud.dev itself
// raises; this package adds no ErrUnsupported mapping here, because
// that sentinel is pgblob's own answer for a tier that structurally
// cannot sign a URL, not every driver's answer for a bucket nobody
// configured to sign one.
func (s *bucketStore) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	url, err := s.bucket.SignedURL(ctx, key, &blob.SignedURLOptions{Expiry: ttl, Method: http.MethodGet})
	if err != nil {
		return "", fmt.Errorf("sign url %q: %w", key, err)
	}
	return url, nil
}

// DeletePrefix removes every object whose key starts with prefix.
func (s *bucketStore) DeletePrefix(ctx context.Context, prefix string) error {
	iter := s.bucket.List(&blob.ListOptions{Prefix: prefix})
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("list prefix %q: %w", prefix, err)
		}
		if err := s.bucket.Delete(ctx, obj.Key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			return fmt.Errorf("delete %q: %w", obj.Key, err)
		}
	}
}

// bucketPart names one chunked-object part found under a key's prefix.
type bucketPart struct {
	seq  int64
	key  string
	size int64
}

// listParts lists every part under "<key>/", in ascending sequence
// order.
func (s *bucketStore) listParts(ctx context.Context, key string) ([]bucketPart, error) {
	prefix := key + "/"
	var parts []bucketPart
	iter := s.bucket.List(&blob.ListOptions{Prefix: prefix})
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list parts %q: %w", prefix, err)
		}
		seq, err := strconv.ParseInt(strings.TrimPrefix(obj.Key, prefix), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse part sequence %q: %w", obj.Key, err)
		}
		parts = append(parts, bucketPart{seq: seq, key: obj.Key, size: obj.Size})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].seq < parts[j].seq })
	return parts, nil
}

// anyPartExists reports whether a chunked part already sits under
// "<key>/".
func (s *bucketStore) anyPartExists(ctx context.Context, key string) (bool, error) {
	iter := s.bucket.List(&blob.ListOptions{Prefix: key + "/"})
	_, err := iter.Next(ctx)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, io.EOF):
		return false, nil
	default:
		return false, fmt.Errorf("list parts %q: %w", key, err)
	}
}

// writePart applies the identical-or-conflict rule Append needs: an
// identical retry succeeds, and a conflicting retry returns
// ErrChunkConflict.
//
// The write carries If-None-Match: *, so the bucket checks that no part
// exists when the write commits. The check before the write cannot do
// this alone: a write that stalls after it would replace a part that
// another writer created in the meantime. A bucket that does not support
// a conditional write fails the write.
//
// Both checks compare the stored bytes, never the digest metadata. The
// fileblob driver writes the metadata of a conditional write before it
// finds the existing file, so after a lost race its digest can name the
// losing bytes.
func (s *bucketStore) writePart(ctx context.Context, key string, data []byte, opts ObjectOptions) error {
	if found, err := s.matchStoredPart(ctx, key, data); err != nil || found {
		return err
	}
	if s.afterCheck != nil {
		s.afterCheck(key)
	}
	err := s.writeObject(ctx, key, data, opts, true)
	if err == nil || gcerrors.Code(err) != gcerrors.FailedPrecondition {
		return err
	}
	if found, err := s.matchStoredPart(ctx, key, data); err != nil || found {
		return err
	}
	return fmt.Errorf("check existing part %q: the bucket refused the write but holds no part", key)
}

// matchStoredPart reads the part at key. found is false when key holds
// no part. When it holds one, err is nil for the same bytes as data and
// ErrChunkConflict for other bytes.
func (s *bucketStore) matchStoredPart(ctx context.Context, key string, data []byte) (found bool, err error) {
	stored, err := s.bucket.ReadAll(ctx, key)
	switch {
	case err == nil:
		if bytes.Equal(stored, data) {
			return true, nil
		}
		return true, ErrChunkConflict
	case gcerrors.Code(err) == gcerrors.NotFound:
		return false, nil
	default:
		return false, fmt.Errorf("check existing part %q: %w", key, err)
	}
}

// writeObject writes data at key with its digest and its writer in
// object metadata. ifNotExist makes the bucket refuse the write with
// gcerrors.FailedPrecondition when key already exists.
func (s *bucketStore) writeObject(ctx context.Context, key string, data []byte, opts ObjectOptions, ifNotExist bool) error {
	return s.writeObjectWith(ctx, key, data, opts, ifNotExist, nil)
}

// writeObjectWith is writeObject with a driver hook that runs before the
// write.
func (s *bucketStore) writeObjectWith(ctx context.Context, key string, data []byte, opts ObjectOptions, ifNotExist bool, beforeWrite func(func(any) bool) error) error {
	md := map[string]string{digestMetadataKey: sha256Digest(data)}
	if opts.Writer != "" {
		md[writerMetadataKey] = opts.Writer
	}
	w, err := s.bucket.NewWriter(ctx, key, &blob.WriterOptions{
		ContentType: opts.ContentType,
		Metadata:    md,
		IfNotExist:  ifNotExist,
		BeforeWrite: beforeWrite,
	})
	if err != nil {
		return fmt.Errorf("open writer %q: %w", key, err)
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return fmt.Errorf("write object %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close writer %q: %w", key, err)
	}
	return nil
}

// partKey returns the object key for sequence seq of key, zero-padded to
// 12 digits so lexicographic listing order matches sequence order.
func partKey(key string, seq int64) string {
	return fmt.Sprintf("%s/%012d", key, seq)
}
