package blob

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckSchemeAcceptsEverySupportedScheme(t *testing.T) {
	for _, scheme := range []string{"pgblob", "s3", "gs", "azblob", "file"} {
		if err := checkScheme(scheme); err != nil {
			t.Errorf("checkScheme(%q): got %v, want nil", scheme, err)
		}
	}
}

func TestCheckSchemeRejectsUnsupportedScheme(t *testing.T) {
	err := checkScheme("ftp")
	if err == nil {
		t.Fatal("checkScheme(\"ftp\"): got nil, want an error")
	}
	if !strings.Contains(err.Error(), "pgblob") || !strings.Contains(err.Error(), "s3") ||
		!strings.Contains(err.Error(), "gs") || !strings.Contains(err.Error(), "azblob") ||
		!strings.Contains(err.Error(), "file") {
		t.Fatalf("checkScheme(\"ftp\") error does not name every supported scheme: %v", err)
	}
}

func TestOpenRejectsUnsupportedScheme(t *testing.T) {
	_, err := Open(context.Background(), "ftp://example.com/blobs", nil)
	if err == nil {
		t.Fatal("Open with an unsupported scheme: got nil error")
	}
	if !strings.Contains(err.Error(), "s3") {
		t.Fatalf("Open error does not name the supported schemes: %v", err)
	}
}

func TestOpenPGBlobNeedsAPool(t *testing.T) {
	_, err := Open(context.Background(), "pgblob:", nil)
	if err == nil {
		t.Fatal("Open(\"pgblob:\", nil): got nil error, want a pool-required error")
	}
}

func TestOpenFileOpensARealBucket(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(context.Background(), "file://"+dir, nil)
	if err != nil {
		t.Fatalf("Open(\"file://%s\"): %v", dir, err)
	}
	if _, ok := store.(*bucketStore); !ok {
		t.Fatalf("Open(\"file://...\") returned %T, want *bucketStore", store)
	}

	ctx := context.Background()
	if _, err := store.NewRangeReader(ctx, "missing", 0, -1); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("NewRangeReader on an empty bucket: got %v, want ErrObjectNotFound", err)
	}
}
