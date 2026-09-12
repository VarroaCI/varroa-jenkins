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

// TestOpenGatewayBlobStoreSignalsTheRecordStoreInEveryMode asserts the blob
// store answers true exactly when the record store is enabled, which is what
// the failed-trigger record reads. The chart gives the gateway
// VARROA_RECORDSTORE_DSN only in postgres blob mode, and VARROA_BLOBSTORE_URL
// in every mode, so an s3-mode gateway carries a blob URL and no DSN.
func TestOpenGatewayBlobStoreSignalsTheRecordStoreInEveryMode(t *testing.T) {
	cases := []struct {
		name      string
		dsn       string
		blobURL   string
		wantStore bool
	}{
		{"s3 mode carries a blob url and no dsn", "", "file://" + t.TempDir(), true},
		{"a disabled record store carries neither", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := openGatewayBlobStore(context.Background(), tc.dsn, tc.blobURL, nil)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if (store != nil) != tc.wantStore {
				t.Fatalf("store != nil = %v, want %v", store != nil, tc.wantStore)
			}
		})
	}
}
