package blob_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// TestPGBlobConformance runs the shared behavior suite against a pgStore
// over a fresh copy of schema varroa. It writes the native row itself,
// through the pool it opened, because package blob_test cannot reach the
// unexported pgStore type Open("pgblob:") returns.
func TestPGBlobConformance(t *testing.T) {
	pool := setupTestPool(t)

	newStore := func(t *testing.T) blob.Store {
		t.Helper()
		return blob.NewPGBlobStore(pool)
	}

	// writeNativePGBlob writes a native row directly, with chunk_seq -1,
	// the way a foreign writer stores an artifact in postgres blob mode.
	writeNativePGBlob := func(t *testing.T, _ blob.Store, key string, data []byte, opts blob.ObjectOptions) {
		t.Helper()
		var contentType *string
		if opts.ContentType != "" {
			contentType = &opts.ContentType
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO varroa.log_chunks (object_key, chunk_seq, created_at, object_class, content_type, digest, bytes)
			VALUES ($1, -1, $2, $3, $4, $5, $6)
		`, key, opts.CreatedAt, opts.ObjectClass, contentType, sha256Digest(data), data); err != nil {
			t.Fatalf("insert native row: %v", err)
		}
	}

	blob.RunConformance(t, newStore, writeNativePGBlob)
}

// sha256Digest returns the "sha256:<hex>" digest blob's own drivers
// store, computed here because that format is a package-internal
// convention, not an exported one.
func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
