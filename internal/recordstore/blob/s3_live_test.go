package blob

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// s3TestURLEnv names the S3 bucket URL the live S3 tests use. The tests
// skip when it is unset, so CI, which has no S3 service, skips them.
//
// To run them against the MinIO of the core cluster:
//
//  1. Forward the MinIO port:
//     kubectl -n varroa-smoke-s3 port-forward svc/minio 19000:9000
//  2. Export AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from the keys
//     access-key-id and secret-access-key of Secret varroa-smoke-s3-creds
//     in namespace varroa-system.
//  3. Run the tests:
//     VARROA_TEST_S3_URL='s3://varroa-blobs?region=us-east-1&endpoint=http://127.0.0.1:19000&s3ForcePathStyle=true&disable_https=true' \
//     go test -race -count=1 -run S3 ./internal/recordstore/blob/
//
// Every key the tests write starts with "conformance/", and the tests
// remove that prefix when they end.
const s3TestURLEnv = "VARROA_TEST_S3_URL"

func openS3TestStore(t *testing.T) Store {
	t.Helper()
	rawURL := os.Getenv(s3TestURLEnv)
	if rawURL == "" {
		t.Skipf("%s is not set", s3TestURLEnv)
	}
	store, err := Open(context.Background(), rawURL, nil)
	if err != nil {
		t.Fatalf("open s3 store: %v", err)
	}
	if !store.(*bucketStore).isS3 {
		t.Fatalf("%s opened a bucket of another driver", s3TestURLEnv)
	}
	return store
}

// TestS3Conformance runs the conformance suite against a live S3 bucket.
func TestS3Conformance(t *testing.T) {
	RunConformance(t, openS3TestStore, writeNativeBucket)
}

// TestS3OverwriteRefusesAPartChangedAfterItsCheck changes the part
// between Overwrite's stamp check and its write. The bucket refuses the
// write through If-Match, so the part keeps the other writer's bytes.
func TestS3OverwriteRefusesAPartChangedAfterItsCheck(t *testing.T) {
	ctx := context.Background()
	store := openS3TestStore(t).(*bucketStore)
	key := "conformance/s3-if-match"
	t.Cleanup(func() { _ = store.DeletePrefix(ctx, key+"/") })

	opts := ObjectOptions{ObjectClass: "log", ContentType: "text/plain", CreatedAt: time.Now().UTC(), Writer: "ns/cell/inst1"}
	if err := store.Append(ctx, key, 0, []byte("old"), opts); err != nil {
		t.Fatalf("append: %v", err)
	}
	other := opts
	other.Writer = "ns/cell/inst3"
	store.beforeOverwrite = func(pk string) {
		store.beforeOverwrite = nil
		if err := store.writeObject(ctx, pk, []byte("other"), other, false); err != nil {
			t.Errorf("write inside the window: %v", err)
		}
	}
	stale := opts
	stale.Writer = "ns/cell/inst2"
	err := store.Overwrite(ctx, key, 0, []byte("stale"), Stamp{Digest: Digest([]byte("old")), Writer: "ns/cell/inst1"}, stale)
	if !errors.Is(err, ErrChunkConflict) {
		t.Fatalf("overwrite across a change: got %v, want ErrChunkConflict", err)
	}
	got, err := store.bucket.ReadAll(ctx, partKey(key, 0))
	if err != nil || string(got) != "other" {
		t.Fatalf("part bytes = %q, %v, want the other writer's bytes", got, err)
	}
}
