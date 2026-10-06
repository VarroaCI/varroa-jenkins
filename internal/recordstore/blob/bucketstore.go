package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// digestMetadataKey is the gocloud.dev object-metadata key that holds a
// part's sha256 digest. gocloud.dev lowercases every metadata key on
// write and on read, so this name stays lowercase.
const digestMetadataKey = "digest"

// bucketStore is the Store driver over a gocloud.dev bucket. A chunked
// object's parts live at "<key>/<seq padded to 12 digits>". A native
// object lives at the bare key, so the two forms never share a key
// space.
type bucketStore struct {
	bucket *blob.Bucket
}

// NewBucketStore returns a Store backed by bucket.
func NewBucketStore(bucket *blob.Bucket) Store {
	return &bucketStore{bucket: bucket}
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
	return s.writeObject(ctx, partKey(key, 0), data, opts)
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
	return s.writeObject(ctx, partKey(key, 0), data, opts)
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
// object metadata, over any part already there.
func (s *bucketStore) Overwrite(ctx context.Context, key string, seq int64, p []byte, opts ObjectOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	return s.writeObject(ctx, partKey(key, seq), p, opts)
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
func (s *bucketStore) writePart(ctx context.Context, key string, data []byte, opts ObjectOptions) error {
	digest := sha256Digest(data)
	attrs, err := s.bucket.Attributes(ctx, key)
	switch {
	case err == nil:
		if attrs.Metadata[digestMetadataKey] == digest {
			return nil
		}
		return ErrChunkConflict
	case gcerrors.Code(err) == gcerrors.NotFound:
		// No part yet at key. Fall through and write it.
	default:
		return fmt.Errorf("check existing part %q: %w", key, err)
	}
	return s.writeObject(ctx, key, data, opts)
}

// writeObject writes data at key with its digest in object metadata.
func (s *bucketStore) writeObject(ctx context.Context, key string, data []byte, opts ObjectOptions) error {
	w, err := s.bucket.NewWriter(ctx, key, &blob.WriterOptions{
		ContentType: opts.ContentType,
		Metadata:    map[string]string{digestMetadataKey: sha256Digest(data)},
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
