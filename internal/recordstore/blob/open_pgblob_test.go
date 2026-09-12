package blob_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// TestOpenPGBlobWithPool asserts Open("pgblob:") with a live pool returns
// a store that persists through table varroa.log_chunks. It checks this
// behaviorally, by writing through the store and reading the row back
// with a plain query, because package blob_test cannot reach the
// unexported pgStore type a dynamic type assertion would otherwise name.
func TestOpenPGBlobWithPool(t *testing.T) {
	pool := setupTestPool(t)
	store, err := blob.Open(context.Background(), "pgblob:", pool)
	if err != nil {
		t.Fatalf("Open(\"pgblob:\"): %v", err)
	}

	key := "open-pgblob-with-pool/put"
	if err := store.Put(context.Background(), key, strings.NewReader("hello"), blob.ObjectOptions{
		ObjectClass: "log", ContentType: "text/plain", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put through the opened store: %v", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM varroa.log_chunks WHERE object_key = $1
	`, key).Scan(&count); err != nil {
		t.Fatalf("count rows written through the opened store: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows for %q = %d, want 1; Open(\"pgblob:\") did not return a Postgres-backed store", key, count)
	}
}
