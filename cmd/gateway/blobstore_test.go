package main

import (
	"context"
	"testing"
)

// TestOpenGatewayBlobStoreDisabledWhenBothEmpty asserts a gateway with
// neither VARROA_RECORDSTORE_DSN nor VARROA_BLOBSTORE_URL set opens no
// store and fails startup with no error: the record store is disabled.
func TestOpenGatewayBlobStoreDisabledWhenBothEmpty(t *testing.T) {
	store, err := openGatewayBlobStore(context.Background(), "", "", nil)
	if err != nil {
		t.Fatalf("want no error when the record store is disabled, got %v", err)
	}
	if store != nil {
		t.Fatalf("want a nil store when the record store is disabled, got %v", store)
	}
}

// TestOpenGatewayBlobStoreFailsWhenDSNSetWithNoURL asserts a DSN with no
// blob URL fails startup with a clear message: the record store is
// enabled but misconfigured.
func TestOpenGatewayBlobStoreFailsWhenDSNSetWithNoURL(t *testing.T) {
	_, err := openGatewayBlobStore(context.Background(), "postgres://x", "", nil)
	if err == nil {
		t.Fatal("want an error when VARROA_RECORDSTORE_DSN is set but VARROA_BLOBSTORE_URL is empty")
	}
}

// TestOpenGatewayBlobStoreOpensFileScheme asserts a non-empty blob URL
// opens a real store.
func TestOpenGatewayBlobStoreOpensFileScheme(t *testing.T) {
	store, err := openGatewayBlobStore(context.Background(), "", "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if store == nil {
		t.Fatal("want a non-nil store for a configured blob URL")
	}
}
