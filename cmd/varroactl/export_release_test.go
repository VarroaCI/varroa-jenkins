package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

func unmarshalYAMLForTest(data []byte, out any) error {
	return yaml.Unmarshal(data, out)
}

// fakeSourceRegistry serves the tags of a layout over the OCI distribution API,
// read-only. The export copies from it as a source. Only tags are served.
func fakeSourceRegistry(t *testing.T, layoutDir string) *httptest.Server {
	t.Helper()
	layout, err := oci.NewLayoutStore(layoutDir)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if i := strings.LastIndex(r.URL.Path, "/manifests/"); i >= 0 {
			ref := r.URL.Path[i+len("/manifests/"):]
			desc, err := layout.Resolve(context.Background(), ref)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			blob, err := os.ReadFile(filepath.Join(layoutDir, "blobs", "sha256", strings.TrimPrefix(desc.Digest, "sha256:")))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", desc.MediaType)
			w.Header().Set("Docker-Content-Digest", desc.Digest)
			w.Header().Set("Content-Length", fmt.Sprint(len(blob)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(blob)
			}
			return
		}
		if i := strings.LastIndex(r.URL.Path, "/blobs/"); i >= 0 {
			digest := r.URL.Path[i+len("/blobs/"):]
			blob, err := os.ReadFile(filepath.Join(layoutDir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Length", fmt.Sprint(len(blob)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(blob)
			}
			return
		}
		http.NotFound(w, r)
	}))
}

// seedSourceLayout writes tagged artifacts into a layout: one per image tag.
func seedSourceLayout(t *testing.T, dir string, tags ...string) {
	t.Helper()
	layout, err := oci.NewLayoutStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tgz := filepath.Join(dir, "..", filepath.Base(dir)+"-payload.bin")
	if err := os.WriteFile(tgz, []byte("payload for "+strings.Join(tags, ",")), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		if _, err := pushChartArtifact(context.Background(), layout, tag, tgz, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
}

// writeFakeHelm writes a helm stand-in that answers show, template, and
// package from fixture files in dir. It returns the script path.
func writeFakeHelm(t *testing.T, dir, chartYAML, valuesYAML, renderedYAML string) string {
	t.Helper()
	for name, body := range map[string]string{
		"chart.yaml":    chartYAML,
		"values.yaml":   valuesYAML,
		"rendered.yaml": renderedYAML,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  show)
    if [ "$2" = chart ]; then cat %[1]q; else cat %[2]q; fi ;;
  template)
    cat %[3]q ;;
  *)
    echo "unexpected helm call: $*" >&2; exit 1 ;;
esac
`, filepath.Join(dir, "chart.yaml"), filepath.Join(dir, "values.yaml"), filepath.Join(dir, "rendered.yaml"))
	path := filepath.Join(dir, "helm")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExportRelease_DirLayoutAndManifest(t *testing.T) {
	testSetup(t)
	tmp := t.TempDir()

	// Source registry: the Varroa image and the Jenkins core image.
	srcLayout := filepath.Join(tmp, "src")
	seedSourceLayout(t, srcLayout, "3.0.0", "2.479.1")
	reg := fakeSourceRegistry(t, srcLayout)
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")

	// Plugin download server with one plugin.
	pluginData := []byte("fake-hpi-content-for-release-test")
	digest, _, err := oci.Sha256Digest(bytes.NewReader(pluginData))
	if err != nil {
		t.Fatal(err)
	}
	dlSrv := ucAndPluginServer(t, map[string]pluginFixture{
		"workflow-aggregator": {
			name:      "workflow-aggregator",
			version:   "608.v6c5a",
			sha256B64: hexToB64(strings.TrimPrefix(digest, "sha256:")),
			data:      pluginData,
		},
	})
	defer dlSrv.Close()
	prev := releaseDownloadURLBase
	releaseDownloadURLBase = dlSrv.URL
	t.Cleanup(func() { releaseDownloadURLBase = prev })

	// BFF with the profile.
	bff := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"name":"lts-2.479","version":"2.479.1","plugins":["workflow-aggregator@608.v6c5a"]}]}`)
	})
	defer bff.Close()

	valuesYAML := fmt.Sprintf(`operator:
  image:
    repository: %[1]s/varroa-jenkins
    tag: "3.0.0"
jenkins:
  image:
    repository: %[1]s/jenkins/jenkins
  agentImage: ""
`, host)
	renderedYAML := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: varroa-operator
spec:
  template:
    spec:
      containers:
        - name: operator
          image: %[1]s/varroa-jenkins:3.0.0
          env:
            - name: VARROA_MITE_IMAGE
              value: %[1]s/varroa-jenkins:3.0.0
`, host)
	chartYAML := "apiVersion: v2\nname: varroa\nversion: 0.4.0\n"
	helmPath := writeFakeHelm(t, tmp, chartYAML, valuesYAML, renderedYAML)

	userValues := filepath.Join(tmp, "values-user.yaml")
	const secret = "sekrit-value-never-stored-7731"
	if err := os.WriteFile(userValues, []byte("auth:\n  oidc:\n    clientSecret: "+secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The chart archive must be a real gzip tar: the export unpacks it to read
	// the dependency defaults. The fake helm answers the metadata questions.
	chartSrc := filepath.Join(tmp, "chart-src")
	if err := os.MkdirAll(chartSrc, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chartSrc, "Chart.yaml"), []byte(chartYAML), 0644); err != nil {
		t.Fatal(err)
	}
	chartTgz := filepath.Join(tmp, "varroa-0.4.0.tgz")
	if err := createTarGz(chartTgz, chartSrc); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "bundle")

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "release",
		"--version", "3.0.0",
		"--values", userValues,
		"--profile", "lts-2.479",
		"--chart", chartTgz,
		"--helm", helmPath,
		"--to", "dir://" + out,
		"--insecure",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("export release: %v", err)
	}

	raw, m, err := readReleaseManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if m.VarroaVersion != "3.0.0" || m.Chart.Name != "varroa" || m.Chart.Version != "0.4.0" {
		t.Errorf("manifest header wrong: %+v", m)
	}
	if len(m.Images) != 2 {
		t.Fatalf("got %d images, want 2 (operator image and the Jenkins core image): %+v", len(m.Images), m.Images)
	}
	keys := map[string]string{}
	for _, img := range m.Images {
		keys[img.Source] = strings.Join(img.ValuesKeys, ",")
	}
	if !strings.Contains(keys[host+"/varroa-jenkins:3.0.0"], "operator.image") {
		t.Errorf("operator image not attributed to operator.image: %v", keys)
	}
	if !strings.Contains(keys[host+"/jenkins/jenkins:2.479.1"], "jenkins.image.repository") {
		t.Errorf("Jenkins image not attributed to jenkins.image.repository: %v", keys)
	}
	if len(m.PluginPacks) != 1 || m.PluginPacks[0].Profile != "lts-2.479" || m.PluginPacks[0].PluginCount != 1 {
		t.Errorf("plugin packs wrong: %+v", m.PluginPacks)
	}
	if len(m.Unseen) == 0 {
		t.Errorf("manifest lists no unseen sources")
	}
	if len(m.ValuesFiles) != 1 || m.ValuesFiles[0].Name != "values-user.yaml" || !strings.HasPrefix(m.ValuesFiles[0].SHA256, "sha256:") {
		t.Errorf("values records wrong: %+v", m.ValuesFiles)
	}
	if strings.Contains(string(raw), secret) {
		t.Errorf("manifest contains a values file's content")
	}

	if _, err := os.Stat(filepath.Join(out, "ingest")); err == nil {
		t.Errorf("export left the layout scratch directory ingest/ in the bundle")
	}
	// The blob list must match the files in the layout exactly.
	blobs, err := oci.VerifyLayoutBlobs(out)
	if err != nil {
		t.Fatalf("verify written blobs: %v", err)
	}
	if len(blobs) != len(m.Blobs) {
		t.Errorf("manifest lists %d blobs, layout holds %d", len(m.Blobs), len(blobs))
	}

	// The written bundle verifies against its own digest.
	mdigest, _, err := oci.Sha256Digest(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyReleaseBundle(out, mdigest, "registry.example.com/varroa"); err != nil {
		t.Errorf("bundle does not verify: %v", err)
	}
}

func TestExportRelease_RefusesMovingTagInRender(t *testing.T) {
	rendered := []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: x\nspec:\n  template:\n    spec:\n      containers:\n        - name: c\n          image: ghcr.io/example/varroa-jenkins:latest\n")
	sources, err := collectRenderedImages(rendered)
	if err != nil {
		t.Fatal(err)
	}
	_, err = planImages(sources, map[string]string{"operator.image": "ghcr.io/example/varroa-jenkins:latest"})
	if err == nil || !strings.Contains(err.Error(), "latest") {
		t.Fatalf("expected latest-tag refusal, got %v", err)
	}
}

func TestManifest_NoValuesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(path, []byte("secret: hunter2-unique-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, digest, err := loadValuesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := releaseManifest{
		SchemaVersion: releaseManifestSchema,
		Kind:          releaseManifestKind,
		ValuesFiles:   []releaseValues{{Name: "values.yaml", SHA256: digest}},
	}
	b, err := m.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2-unique-token") {
		t.Fatalf("manifest carries values content: %s", b)
	}
	var back releaseManifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ValuesFiles[0].SHA256 != digest {
		t.Errorf("digest did not round-trip")
	}
}

func TestMergeValues_MapsMergeListsReplace(t *testing.T) {
	dst := map[string]any{"a": map[string]any{"x": 1, "y": 2}, "l": []any{1, 2, 3}}
	src := map[string]any{"a": map[string]any{"y": 9}, "l": []any{4}}
	got := mergeValues(dst, src)
	a := got["a"].(map[string]any)
	if a["x"] != 1 || a["y"] != 9 {
		t.Errorf("map merge wrong: %v", a)
	}
	if l := got["l"].([]any); len(l) != 1 || l[0] != 4 {
		t.Errorf("list must replace: %v", l)
	}
	if dst["a"].(map[string]any)["y"] != 2 {
		t.Errorf("mergeValues changed its input")
	}
}

func TestSanitizeTag(t *testing.T) {
	if got := sanitizeTag("0.4.0+build.7"); got != "0.4.0_build.7" {
		t.Errorf("sanitizeTag = %q", got)
	}
}
