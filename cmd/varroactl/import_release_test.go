package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// buildReleaseFixture writes a small release bundle into a new directory. It
// holds a chart, one image, and one plugin pack, and its manifest is written
// the way export release writes it. It returns the directory and the manifest
// digest.
func buildReleaseFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	layout, err := oci.NewLayoutStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := filepath.Join(t.TempDir(), "chart.tgz")
	if err := os.WriteFile(payload, []byte("chart archive"), 0644); err != nil {
		t.Fatal(err)
	}
	chart, err := pushChartArtifact(ctx, layout, "chart-varroa-0.4.0", payload, []byte(`{"name":"varroa","version":"0.4.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	img, err := pushChartArtifact(ctx, layout, "img-000000000001", payload, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := pushChartArtifact(ctx, layout, "lts-2.479", payload, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}

	blobs, err := oci.VerifyLayoutBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The export removes the layout store scratch directory; the fixture does too.
	_ = os.Remove(filepath.Join(dir, "ingest"))
	m := releaseManifest{
		SchemaVersion: releaseManifestSchema,
		Kind:          releaseManifestKind,
		VarroaVersion: "3.0.0",
		Chart:         releaseChart{Name: "varroa", Version: "0.4.0", LayoutTag: "chart-varroa-0.4.0", Digest: chart.Digest},
		Images: []releaseImage{{
			Source:     "ghcr.io/example/varroa-jenkins:3.0.0",
			LayoutTag:  "img-000000000001",
			Digest:     img.Digest,
			MediaType:  img.MediaType,
			ValuesKeys: []string{"operator.image", "bff.image", "gateway.image", "updateCenter.image", "recorder.image"},
			Origin:     "chart",
		}},
		PluginPacks: []releasePack{{Profile: "lts-2.479", LayoutTag: "lts-2.479", Digest: pack.Digest, PluginCount: 1}},
		Unseen:      releaseUnseen,
	}
	for _, b := range blobs {
		m.Blobs = append(m.Blobs, releaseBlob{Digest: b.Digest, Size: b.Size})
	}
	digest, err := writeReleaseManifest(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	return dir, digest
}

func TestImportRelease_FixtureVerifies(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	if _, err := verifyReleaseBundle(dir, digest, "registry.example.com/varroa"); err != nil {
		t.Fatalf("clean fixture must verify: %v", err)
	}
}

func TestImportRelease_TamperedBlobFailsBeforePush(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	blobs, err := oci.VerifyLayoutBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	victim := strings.TrimPrefix(blobs[0].Digest, "sha256:")
	victimPath := filepath.Join(dir, "blobs", "sha256", victim)
	if err := os.Remove(victimPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victimPath, []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyReleaseBundle(dir, digest, "registry.example.com/varroa"); err == nil {
		t.Fatal("tampered blob must fail verification before any push")
	}
}

func TestImportRelease_ExtraBlobFails(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	extra := []byte("not in the manifest")
	extraDigest, _, err := oci.Sha256Digest(bytes.NewReader(extra))
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(extraDigest, "sha256:")
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", name), extra, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = verifyReleaseBundle(dir, digest, "registry.example.com/varroa")
	if err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Fatalf("extra blob must fail, got %v", err)
	}
}

func TestImportRelease_MissingBlobFails(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	blobs, err := oci.VerifyLayoutBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(blobs[0].Digest, "sha256:"))); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyReleaseBundle(dir, digest, "registry.example.com/varroa"); err == nil || !strings.Contains(err.Error(), "missing blob") {
		t.Fatalf("missing blob must fail, got %v", err)
	}
}

func TestImportRelease_ManifestDigestMismatchFails(t *testing.T) {
	dir, _ := buildReleaseFixture(t)
	wrong := "sha256:" + strings.Repeat("0", 64)
	_, err := verifyReleaseBundle(dir, wrong, "registry.example.com/varroa")
	if err == nil || !strings.Contains(err.Error(), "not the one the release published") {
		t.Fatalf("manifest digest mismatch must fail, got %v", err)
	}
}

func TestImportRelease_DestinationPaths(t *testing.T) {
	cases := []struct {
		source, wantPath, wantTag string
	}{
		{"nats:2.14.7-alpine", "library/nats", "2.14.7-alpine"},
		{"jenkins/jenkins:2.479.1", "jenkins/jenkins", "2.479.1"},
		{"ghcr.io/example/varroa-jenkins:3.0.0", "example/varroa-jenkins", "3.0.0"},
		{"ghcr.io/example/app@sha256:" + strings.Repeat("ab", 32), "example/app", "digest-abababababab"},
	}
	for _, c := range cases {
		ref, err := parseImageRef(c.source)
		if err != nil {
			t.Fatalf("parse %s: %v", c.source, err)
		}
		if ref.Path != c.wantPath || ref.destTag() != c.wantTag {
			t.Errorf("%s: path=%q tag=%q, want path=%q tag=%q", c.source, ref.Path, ref.destTag(), c.wantPath, c.wantTag)
		}
	}
}

func TestImportRelease_PrintsOverrides(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	plan, err := verifyReleaseBundle(dir, digest, "registry.example.com/varroa")
	if err != nil {
		t.Fatal(err)
	}
	overrides, unattributed := releaseOverrides(plan)
	if len(unattributed) != 0 {
		t.Errorf("unexpected unattributed images: %v", unattributed)
	}
	var out bytes.Buffer
	if err := printReleaseOverrides(&out, plan, overrides, unattributed); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	// One override per key for a shared image, and the mite image follows the operator.
	for _, key := range []string{"operator", "bff", "gateway", "updateCenter", "recorder"} {
		v, ok := overrides[key].(map[string]any)
		if !ok {
			t.Fatalf("no override block for %s", key)
		}
		img, _ := v["image"].(map[string]any)
		if img["repository"] != "registry.example.com/varroa/example/varroa-jenkins" || img["tag"] != "3.0.0" {
			t.Errorf("%s.image override = %v", key, img)
		}
	}
	operatorBlock := overrides["operator"].(map[string]any)
	if got := operatorBlock["miteImage"]; got != "registry.example.com/varroa/example/varroa-jenkins:3.0.0" {
		t.Errorf("operator.miteImage = %v", got)
	}
	for _, want := range []string{
		"repository: registry.example.com/varroa/example/varroa-jenkins",
		"tag: 3.0.0",
		"miteImage: registry.example.com/varroa/example/varroa-jenkins:3.0.0",
		"refs:",
		"registry.example.com/varroa/plugin-pack:lts-2.479",
		`jenkins.upstreamURL to ""`,
		"helm install varroa oci://registry.example.com/varroa/charts/varroa --version 0.4.0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("overrides output lacks %q:\n%s", want, text)
		}
	}
}
