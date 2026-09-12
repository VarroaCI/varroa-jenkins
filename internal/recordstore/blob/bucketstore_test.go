package blob

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"gocloud.dev/blob"
)

func TestFileBucketConformance(t *testing.T) {
	RunConformance(t, newFileTestStore, writeNativeBucket)
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
