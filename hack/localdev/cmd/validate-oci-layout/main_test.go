package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

const testProfile = "jenkins-version-2-588"

// buildPack writes one profile pack that holds one plugin under ref in dir.
func buildPack(t *testing.T, dir, ref, profile string) {
	t.Helper()
	store, err := oci.NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("open layout: %v", err)
	}
	content := []byte("hpi-" + profile)
	sum, _, err := oci.Sha256Digest(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	plugins := []oci.ResolvedPlugin{{Name: "structs", Version: "1.0", SHA256: sum, Content: bytes.NewReader(content)}}
	cfg := oci.PackConfig{
		Kind:        oci.PackKindProfile,
		Profile:     profile,
		LockHash:    oci.LockHash(plugins),
		PluginCount: len(plugins),
	}
	if err := oci.BuildPluginPack(context.Background(), store, ref, cfg, plugins); err != nil {
		t.Fatalf("build pack: %v", err)
	}
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("validateLayout() = nil, want an error that contains %q", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("validateLayout() = %q, want an error that contains %q", err, substr)
	}
}

func TestValidateLayoutAcceptsBaselinePack(t *testing.T) {
	dir := t.TempDir()
	buildPack(t, dir, "fixture", testProfile)
	if err := validateLayout(context.Background(), dir, "fixture", testProfile); err != nil {
		t.Fatalf("validateLayout() = %v, want nil", err)
	}
}

func TestValidateLayoutRejectsOtherProfile(t *testing.T) {
	dir := t.TempDir()
	buildPack(t, dir, "fixture", "jenkins-version-2-555")
	wantErr(t, validateLayout(context.Background(), dir, "fixture", testProfile), `"jenkins-version-2-555"`)
}

func TestValidateLayoutRejectsRefMismatch(t *testing.T) {
	dir := t.TempDir()
	buildPack(t, dir, "other", testProfile)
	wantErr(t, validateLayout(context.Background(), dir, "fixture", testProfile), "ref name")
}

func TestValidateLayoutRejectsSecondManifest(t *testing.T) {
	dir := t.TempDir()
	buildPack(t, dir, "fixture", "jenkins-version-2-555")
	buildPack(t, dir, "fixture", testProfile)
	wantErr(t, validateLayout(context.Background(), dir, "fixture", testProfile), "2 manifests")
}

func TestValidateLayoutRejectsEmptyLayout(t *testing.T) {
	dir := t.TempDir()
	if _, err := oci.NewLayoutStore(dir); err != nil {
		t.Fatalf("open layout: %v", err)
	}
	wantErr(t, validateLayout(context.Background(), dir, "fixture", testProfile), "0 manifests")
}

func TestValidateLayoutRejectsKindlessConfig(t *testing.T) {
	dir := t.TempDir()
	store, err := oci.NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("open layout: %v", err)
	}
	ctx := context.Background()
	cfgJSON := `{"profile":"` + testProfile + `","pluginCount":0}`
	digest, size, err := store.PushBlob(ctx, oci.MediaTypePackConfig, strings.NewReader(cfgJSON))
	if err != nil {
		t.Fatalf("push config: %v", err)
	}
	manifest := oci.Manifest{
		ArtifactType: oci.ArtifactTypePluginPack,
		Config:       oci.Descriptor{MediaType: oci.MediaTypePackConfig, Digest: digest, Size: size},
		Annotations:  map[string]string{annPackProfile: testProfile},
	}
	if err := store.Push(ctx, "fixture", manifest); err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	wantErr(t, validateLayout(ctx, dir, "fixture", testProfile), "has no kind")
}
