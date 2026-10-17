package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportRelease_DestinationCollisionFails covers two images that share a
// repository path and tag under different hosts, with different digests.
func TestImportRelease_DestinationCollisionFails(t *testing.T) {
	dir, _ := buildReleaseFixture(t)
	_, m, err := readReleaseManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := m.Images[0]
	second := first
	second.Source = "mirror.example.com/example/varroa-jenkins:3.0.0"
	second.LayoutTag = m.Chart.LayoutTag
	second.Digest = m.Chart.Digest // a different digest, same identity
	m.Images = append(m.Images, second)
	digest, err := writeReleaseManifest(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = verifyReleaseBundle(dir, digest, "registry.example.com/varroa")
	if err == nil || !strings.Contains(err.Error(), "map to destination") {
		t.Fatalf("collision with different digests must fail, got %v", err)
	}
}

// TestBundleDirAgentImages_NonYAMLPodTemplateFails covers a raw pod template
// that the export cannot read. It must fail rather than drop the image.
func TestBundleDirAgentImages_NonYAMLPodTemplateFails(t *testing.T) {
	dir := t.TempDir()
	body := "jenkins:\n  clouds:\n    - kubernetes:\n        templates:\n          - yaml: \"[unclosed\"\n"
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := bundleDirImages([]string{dir}, "jenkins/inbound-agent:1"); err == nil || !strings.Contains(err.Error(), "not a YAML document") {
		t.Fatalf("expected a pod template parse error, got %v", err)
	}
}

// TestExportRelease_DirDestinationUntouchedOnFailure checks that a refused
// export leaves an empty dir:// destination empty.
func TestExportRelease_DirDestinationUntouchedOnFailure(t *testing.T) {
	testSetup(t)
	tmp := t.TempDir()
	helmPath := writeFakeHelm(t, tmp, "apiVersion: v2\nname: varroa\nversion: 0.4.0\n", "operator:\n  image:\n    repository: x.example/varroa-jenkins\n    tag: latest\n", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: op\nspec:\n  template:\n    spec:\n      containers:\n        - name: c\n          image: x.example/varroa-jenkins:latest\n")
	chartTgz := filepath.Join(tmp, "varroa-0.4.0.tgz")
	if err := os.WriteFile(chartTgz, []byte("chart"), 0644); err != nil {
		t.Fatal(err)
	}
	bff := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"name":"lts-2.479","version":"2.479.1","plugins":["workflow-aggregator@608.v6c5a"]}]}`))
	})
	defer bff.Close()
	dlSrv := ucAndPluginServer(t, map[string]pluginFixture{})
	defer dlSrv.Close()
	prev := releaseDownloadURLBase
	releaseDownloadURLBase = dlSrv.URL
	t.Cleanup(func() { releaseDownloadURLBase = prev })

	out := filepath.Join(tmp, "bundle")
	root := newRootCmd()
	root.SetArgs([]string{"export", "release", "--version", "3.0.0", "--profile", "lts-2.479",
		"--chart", chartTgz, "--helm", helmPath, "--to", "dir://" + out})
	if err := root.Execute(); err == nil {
		t.Fatal("export with a latest tag must fail")
	}
	entries, err := os.ReadDir(out)
	if err == nil && len(entries) > 0 {
		t.Fatalf("failed export left %d entries in the destination", len(entries))
	}
}
