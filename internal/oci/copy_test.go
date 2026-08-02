package oci

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopy_Basic(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	src, err := NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore src: %v", err)
	}
	dst, err := NewLayoutStore(dstDir)
	if err != nil {
		t.Fatalf("NewLayoutStore dst: %v", err)
	}

	// Push a layer blob
	layerContent := []byte("layer-data-for-copy-test")
	layerDigest, layerSize, err := src.PushBlob(ctx, "application/octet-stream", bytes.NewReader(layerContent))
	if err != nil {
		t.Fatalf("PushBlob layer: %v", err)
	}

	// Push a config blob
	configContent := []byte(`{"key":"value"}`)
	configDigest, configSize, err := src.PushBlob(ctx, "application/vnd.test.config.v1+json", bytes.NewReader(configContent))
	if err != nil {
		t.Fatalf("PushBlob config: %v", err)
	}

	// Push a manifest
	srcManifest := Manifest{
		ArtifactType: "application/vnd.test.artifact.v1",
		Config: Descriptor{
			MediaType: "application/vnd.test.config.v1+json",
			Digest:    configDigest,
			Size:      configSize,
		},
		Layers: []Descriptor{
			{
				MediaType:   "application/octet-stream",
				Digest:      layerDigest,
				Size:        layerSize,
				Annotations: map[string]string{"key": "layer-annotation"},
			},
		},
		Annotations: map[string]string{"key": "manifest-annotation"},
	}

	srcRef := "test:v1"
	if err := src.Push(ctx, srcRef, srcManifest); err != nil {
		t.Fatalf("Push src manifest: %v", err)
	}

	// Copy from src to dst
	dstRef := "test-copy:v1"
	if err := Copy(ctx, src, srcRef, dst, dstRef); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	// Verify destination
	dstManifest, err := dst.Pull(ctx, dstRef)
	if err != nil {
		t.Fatalf("Pull from dst: %v", err)
	}

	if dstManifest.ArtifactType != srcManifest.ArtifactType {
		t.Errorf("ArtifactType = %q, want %q", dstManifest.ArtifactType, srcManifest.ArtifactType)
	}
	if dstManifest.Config.Digest != configDigest {
		t.Errorf("config digest = %q, want %q", dstManifest.Config.Digest, configDigest)
	}
	if dstManifest.Config.Size != configSize {
		t.Errorf("config size = %d, want %d", dstManifest.Config.Size, configSize)
	}
	if len(dstManifest.Layers) != 1 {
		t.Fatalf("got %d layers, want 1", len(dstManifest.Layers))
	}
	if dstManifest.Layers[0].Digest != layerDigest {
		t.Errorf("layer digest = %q, want %q", dstManifest.Layers[0].Digest, layerDigest)
	}
	if dstManifest.Layers[0].Size != layerSize {
		t.Errorf("layer size = %d, want %d", dstManifest.Layers[0].Size, layerSize)
	}
	if dstManifest.Layers[0].Annotations["key"] != "layer-annotation" {
		t.Errorf("layer annotation = %q", dstManifest.Layers[0].Annotations["key"])
	}
	if dstManifest.Annotations["key"] != "manifest-annotation" {
		t.Errorf("manifest annotation = %q", dstManifest.Annotations["key"])
	}
}

func TestCopy_CorruptedBlob(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	src, err := NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore src: %v", err)
	}
	dst, err := NewLayoutStore(dstDir)
	if err != nil {
		t.Fatalf("NewLayoutStore dst: %v", err)
	}

	// Push a layer blob
	layerContent := []byte("original-content")
	layerDigest, layerSize, err := src.PushBlob(ctx, "application/octet-stream", bytes.NewReader(layerContent))
	if err != nil {
		t.Fatalf("PushBlob layer: %v", err)
	}

	// Push a config blob
	configContent := []byte(`{"key":"value"}`)
	configDigest, configSize, err := src.PushBlob(ctx, "application/vnd.test.config.v1+json", bytes.NewReader(configContent))
	if err != nil {
		t.Fatalf("PushBlob config: %v", err)
	}

	// Push a manifest
	manifest := Manifest{
		ArtifactType: "application/vnd.test.artifact.v1",
		Config: Descriptor{
			MediaType: "application/vnd.test.config.v1+json",
			Digest:    configDigest,
			Size:      configSize,
		},
		Layers: []Descriptor{
			{
				MediaType: "application/octet-stream",
				Digest:    layerDigest,
				Size:      layerSize,
			},
		},
	}

	srcRef := "test:v1"
	if err := src.Push(ctx, srcRef, manifest); err != nil {
		t.Fatalf("Push src manifest: %v", err)
	}

	// Corrupt the blob on disk
	// The blob is stored at blobs/sha256/<hex>
	blobDigest := strings.TrimPrefix(layerDigest, "sha256:")
	blobPath := filepath.Join(srcDir, "blobs", "sha256", blobDigest)
	// Make writable in case it was created read-only
	os.Chmod(blobPath, 0644)
	if err := os.WriteFile(blobPath, []byte("corrupted-data"), 0644); err != nil {
		t.Fatalf("write corrupted blob: %v", err)
	}

	// Copy should fail
	dstRef := "test-copy:v1"
	err = Copy(ctx, src, srcRef, dst, dstRef)
	if err == nil {
		t.Fatal("Copy should have failed due to corrupted blob")
	}

	// Verify destination has no manifest (copy was atomic)
	_, err = dst.Pull(ctx, dstRef)
	if err == nil {
		t.Fatal("Expected error pulling from dst after failed copy")
	}
}

func TestCopy_AlreadyPresent(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	src, err := NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore src: %v", err)
	}
	dst, err := NewLayoutStore(dstDir)
	if err != nil {
		t.Fatalf("NewLayoutStore dst: %v", err)
	}

	// Push a layer blob
	layerContent := []byte("same-content")
	layerDigest, layerSize, err := src.PushBlob(ctx, "application/octet-stream", bytes.NewReader(layerContent))
	if err != nil {
		t.Fatalf("PushBlob layer: %v", err)
	}

	// Push a config blob
	configContent := []byte(`{"key":"value"}`)
	configDigest, configSize, err := src.PushBlob(ctx, "application/vnd.test.config.v1+json", bytes.NewReader(configContent))
	if err != nil {
		t.Fatalf("PushBlob config: %v", err)
	}

	// Push a manifest
	manifest := Manifest{
		ArtifactType: "application/vnd.test.artifact.v1",
		Config: Descriptor{
			MediaType: "application/vnd.test.config.v1+json",
			Digest:    configDigest,
			Size:      configSize,
		},
		Layers: []Descriptor{
			{
				MediaType: "application/octet-stream",
				Digest:    layerDigest,
				Size:      layerSize,
			},
		},
	}

	srcRef := "test:v1"
	if err := src.Push(ctx, srcRef, manifest); err != nil {
		t.Fatalf("Push src manifest: %v", err)
	}

	// Pre-push the same content into dest
	dstRef := "test-copy:v1"
	if err := dst.Push(ctx, dstRef, manifest); err != nil {
		t.Fatalf("Push dst manifest: %v", err)
	}

	// Copy should succeed (no-op)
	if err := Copy(ctx, src, srcRef, dst, dstRef); err != nil {
		t.Fatalf("Copy already-present content: %v", err)
	}

	// Verify dest still has the manifest
	_, err = dst.Pull(ctx, dstRef)
	if err != nil {
		t.Fatalf("Pull from dst after no-op copy: %v", err)
	}
}

func TestCopy_UnsupportedStoreType(t *testing.T) {
	ctx := context.Background()

	// Create a mock BlobStore that doesn't implement hasTarget
	mockSrc := &mockBlobStore{}
	mockDst := &mockBlobStore{}

	err := Copy(ctx, mockSrc, "ref:v1", mockDst, "ref:v1")
	if err == nil {
		t.Fatal("Copy should fail with unsupported store types")
	}
}

// mockBlobStore implements BlobStore but not hasTarget.
type mockBlobStore struct{}

func (m *mockBlobStore) Push(ctx context.Context, ref string, manifest Manifest) error {
	return nil
}
func (m *mockBlobStore) PushBlob(ctx context.Context, mediaType string, content io.Reader) (string, int64, error) {
	return "", 0, nil
}
func (m *mockBlobStore) Pull(ctx context.Context, ref string) (Manifest, error) {
	return Manifest{}, nil
}
func (m *mockBlobStore) Resolve(ctx context.Context, ref string) (Descriptor, error) {
	return Descriptor{}, nil
}
func (m *mockBlobStore) FetchBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockBlobStore) ListManifests(ctx context.Context) ([]Descriptor, error) {
	return nil, nil
}
