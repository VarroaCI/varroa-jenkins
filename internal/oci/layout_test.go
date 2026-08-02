package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestToOCIManifest_SchemaVersion guards against a regression where the OCI
// manifest was marshaled without schemaVersion/mediaType. In-memory and layout
// stores tolerate schemaVersion 0, but real registries reject the manifest PUT
// with "unrecognized manifest schema version 0", so the on-wire JSON must carry
// schemaVersion 2 and the image-manifest media type.
func TestToOCIManifest_SchemaVersion(t *testing.T) {
	oci := toOCIManifest(Manifest{
		ArtifactType: "application/vnd.varroa.pluginpack.v1",
		Config: Descriptor{
			MediaType: "application/vnd.oci.empty.v1+json",
			Digest:    "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			Size:      2,
		},
	})
	if oci.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", oci.SchemaVersion)
	}
	if oci.MediaType != ocispec.MediaTypeImageManifest {
		t.Errorf("MediaType = %q, want %q", oci.MediaType, ocispec.MediaTypeImageManifest)
	}
	b, err := json.Marshal(oci)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"schemaVersion":2`)) {
		t.Errorf("marshaled manifest missing schemaVersion 2: %s", b)
	}
}

func TestLayoutStore_PushPullRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}

	// Push a synthetic layer blob
	layerContent := []byte("fake-layer-data-12345")
	layerDigest, layerSize, err := store.PushBlob(ctx, "application/octet-stream", bytes.NewReader(layerContent))
	if err != nil {
		t.Fatalf("PushBlob layer: %v", err)
	}
	if layerSize != int64(len(layerContent)) {
		t.Errorf("layer size = %d, want %d", layerSize, len(layerContent))
	}

	// Push a config blob
	configContent := []byte(`{"version":"1.0"}`)
	configDigest, configSize, err := store.PushBlob(ctx, "application/vnd.varroa.test.config.v1+json", bytes.NewReader(configContent))
	if err != nil {
		t.Fatalf("PushBlob config: %v", err)
	}
	if configSize != int64(len(configContent)) {
		t.Errorf("config size = %d, want %d", configSize, len(configContent))
	}

	// Build a manifest
	manifest := Manifest{
		ArtifactType: "application/vnd.varroa.test.v1",
		Config: Descriptor{
			MediaType: "application/vnd.varroa.test.config.v1+json",
			Digest:    configDigest,
			Size:      configSize,
		},
		Layers: []Descriptor{
			{
				MediaType: "application/octet-stream",
				Digest:    layerDigest,
				Size:      layerSize,
				Annotations: map[string]string{
					"dev.varroa.test": "value1",
				},
			},
		},
		Annotations: map[string]string{
			"dev.varroa.test.annot": "manifest-annotation",
		},
	}

	ref := "test-pack:v1"
	if err := store.Push(ctx, ref, manifest); err != nil {
		t.Fatalf("Push manifest: %v", err)
	}

	// Pull it back
	got, err := store.Pull(ctx, ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// Assert manifest fields
	if got.ArtifactType != manifest.ArtifactType {
		t.Errorf("ArtifactType = %q, want %q", got.ArtifactType, manifest.ArtifactType)
	}
	if got.Config.Digest != configDigest {
		t.Errorf("config digest = %q, want %q", got.Config.Digest, configDigest)
	}
	if got.Config.Size != configSize {
		t.Errorf("config size = %d, want %d", got.Config.Size, configSize)
	}
	if len(got.Layers) != 1 {
		t.Fatalf("got %d layers, want 1", len(got.Layers))
	}
	if got.Layers[0].Digest != layerDigest {
		t.Errorf("layer digest = %q, want %q", got.Layers[0].Digest, layerDigest)
	}
	if got.Layers[0].Size != layerSize {
		t.Errorf("layer size = %d, want %d", got.Layers[0].Size, layerSize)
	}
	if got.Layers[0].Annotations["dev.varroa.test"] != "value1" {
		t.Errorf("layer annotation = %q", got.Layers[0].Annotations["dev.varroa.test"])
	}
	if got.Annotations["dev.varroa.test.annot"] != "manifest-annotation" {
		t.Errorf("manifest annotation = %q", got.Annotations["dev.varroa.test.annot"])
	}
}

func TestLayoutStore_ListManifests_Empty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}

	descs, err := store.ListManifests(ctx)
	if err != nil {
		t.Fatalf("ListManifests on empty store: %v", err)
	}
	if len(descs) != 0 {
		t.Errorf("expected empty list, got %d descriptors", len(descs))
	}
}

func TestLayoutStore_Resolve_NotFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}

	_, err = store.Resolve(ctx, "nonexistent:latest")
	if err == nil {
		t.Fatal("Resolve on nonexistent ref: expected error, got nil")
	}
	// The error should contain a meaningful message, not a panic/nil deref
	if strings.Contains(err.Error(), "nil pointer") || strings.Contains(err.Error(), "index out of range") {
		t.Fatalf("Resolve panicked: %v", err)
	}
}
