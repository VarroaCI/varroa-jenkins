package blob

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"
)

func TestFileBucketConformance(t *testing.T) {
	RunConformance(t, newFileTestStore, writeNativeBucket)
}

// TestBucketStoreAppendNeverReplacesAPartCreatedAfterItsCheck puts
// another writer's part between Append's existence check and its write.
// The bucket refuses the conditional write, so the part keeps the other
// writer's bytes, and Append answers ErrChunkConflict. The fileblob
// driver rewrites the metadata of a refused write, so the answer must
// come from the stored bytes.
func TestBucketStoreAppendNeverReplacesAPartCreatedAfterItsCheck(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) *blob.Bucket{
		"mem": func(*testing.T) *blob.Bucket { return memblob.OpenBucket(nil) },
		"file": func(t *testing.T) *blob.Bucket {
			b, err := fileblob.OpenBucket(t.TempDir(), nil)
			if err != nil {
				t.Fatalf("open file bucket: %v", err)
			}
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &bucketStore{bucket: open(t)}
			opts := ObjectOptions{ObjectClass: "log", ContentType: "text/plain", CreatedAt: time.Now().UTC()}
			first := opts
			first.Writer = "ns/cell/inst2"
			store.afterCheck = func(string) {
				store.afterCheck = nil
				if err := store.Overwrite(ctx, "logs/r1", 3, []byte("new"), first); err != nil {
					t.Errorf("overwrite inside the window: %v", err)
				}
			}
			late := opts
			late.Writer = "ns/cell/inst1"
			if err := store.Append(ctx, "logs/r1", 3, []byte("late"), late); !errors.Is(err, ErrChunkConflict) {
				t.Fatalf("late append: got %v, want ErrChunkConflict", err)
			}
			got, err := store.bucket.ReadAll(ctx, partKey("logs/r1", 3))
			if err != nil || string(got) != "new" {
				t.Fatalf("part bytes = %q, %v, want the other writer's bytes", got, err)
			}
			if err := store.Append(ctx, "logs/r1", 3, []byte("late"), late); !errors.Is(err, ErrChunkConflict) {
				t.Fatalf("retry of the losing bytes: got %v, want ErrChunkConflict", err)
			}
			if err := store.Append(ctx, "logs/r1", 3, []byte("new"), first); err != nil {
				t.Fatalf("retry of the stored bytes: %v", err)
			}
		})
	}
}

// newFileTestStore opens a file:// store over a fresh temp directory,
// with an HMAC URL signer so the conformance suite's SignedURL subtest
// exercises a genuine signing driver, not only pgblob's ErrUnsupported
// answer. It always runs: file mode needs no credentials and no live
// service.
func newFileTestStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(keyPath, []byte("conformance-test-signing-key"), 0o600); err != nil {
		t.Fatalf("write signing key: %v", err)
	}
	rawURL := "file://" + dir + "?base_url=" + url.QueryEscape("http://example.test/") +
		"&secret_key_path=" + url.QueryEscape(keyPath)
	store, err := Open(context.Background(), rawURL, nil)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	return store
}

// writeNativeBucket writes data at the bare key through the bucket's own
// writer, the way artifact-manager-s3 stores a whole object.
func writeNativeBucket(t *testing.T, s Store, key string, data []byte, opts ObjectOptions) {
	t.Helper()
	store, ok := s.(*bucketStore)
	if !ok {
		t.Fatalf("writeNativeBucket: store is %T, want *bucketStore", s)
	}
	w, err := store.bucket.NewWriter(context.Background(), key, &blob.WriterOptions{
		ContentType: opts.ContentType,
	})
	if err != nil {
		t.Fatalf("open native writer: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		t.Fatalf("write native object: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close native writer: %v", err)
	}
}
