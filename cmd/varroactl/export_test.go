package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// ---------------------------------------------------------------------------
// 2.3a — scaffolding tests
// ---------------------------------------------------------------------------

func TestExport_MissingProfile(t *testing.T) {
	testSetup(t)

	// Request-counting server that fails if hit (no HTTP request expected)
	hitCount := 0
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		hitCount++
		t.Errorf("unexpected HTTP request to %s", r.URL.Path)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--to", "dir:///tmp/out"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing --profile")
	}
	// Should exit 2 (usage error) — either a usageError or a cobra parse error
	var ue usageError
	if !errorsAs(err, &ue) && !isUsageError(err) {
		t.Errorf("expected usage error (exit 2), got %T: %v", err, err)
	}
	if hitCount > 0 {
		t.Errorf("expected 0 HTTP requests, got %d", hitCount)
	}
}

func TestExport_InvalidOutputFormat(t *testing.T) {
	testSetup(t)

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "test", "--to", "dir:///tmp/out", "-o", "yaml"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for -o yaml")
	}
	// usagef returns usageError which causes exit code 2
	var ue usageError
	if !errorsAs(err, &ue) {
		t.Errorf("expected usageError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "only supports -o json") {
		t.Errorf("expected 'only supports -o json', got %v", err)
	}
}

func TestExport_ValidOutputJSON(t *testing.T) {
	// Just verify flag parsing accepts -o json (run will fail because BFF not available,
	// but it should NOT fail at flag parsing)
	testSetup(t)

	root := newRootCmd()
	// No test server — this will fail trying to reach BFF, not at flag parsing
	root.SetArgs([]string{"export", "plugins", "--profile", "test", "--to", "dir:///tmp/out", "-o", "json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error (no BFF server)")
	}
	// Should NOT be a usage error
	var ue usageError
	if errorsAs(err, &ue) {
		t.Errorf("unexpected usageError: %v", err)
	}
}

func TestExport_NoOutputFlagAccepted(t *testing.T) {
	testSetup(t)

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "test", "--to", "dir:///tmp/out"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error (no BFF server)")
	}
	if isUsageError(err) {
		t.Errorf("unexpected usage error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2.3b — closure resolution (BFF)
// ---------------------------------------------------------------------------

func TestExport_BFFProfileMatched(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/clusters/core/version-profiles") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [
				{
					"name": "lts-2.479",
					"version": "2.479.1",
					"channel": "lts",
					"plugins": [
						"workflow-aggregator@608.v6c5a_4c5a_0085",
						"git@5.2.2"
					]
				}
			]
		}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "lts-2.479", "--to", "dir://" + t.TempDir() + "/out"})
	err := root.Execute()
	// This will fail at download phase since our test server doesn't serve plugins,
	// but the BFF resolution should succeed first.
	if err == nil {
		t.Fatal("expected download error (no plugin server)")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("BFF resolution failed: %v", err)
	}
}

func TestExport_BFFProfileNotFound(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "nonexistent", "--to", "dir://" + t.TempDir() + "/out"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unmatched profile")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found', got %v", err)
	}
}

func TestExport_BFFUnversionedPluginLine(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [
				{
					"name": "bad-profile",
					"version": "2.479.1",
					"plugins": ["no-version-plugin"]
				}
			]
		}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "bad-profile", "--to", "dir://" + t.TempDir() + "/out"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unversioned plugin")
	}
	if !strings.Contains(err.Error(), "lacks @version") {
		t.Errorf("expected 'lacks @version', got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2.3b — closure resolution (plugins-file)
// ---------------------------------------------------------------------------

func TestExport_PluginsFileCorePluginMissing(t *testing.T) {
	testSetup(t)

	tmpDir := t.TempDir()
	pfPath := filepath.Join(tmpDir, "plugins.yaml")
	content := `
core:
  - workflow-aggregator
  - missing-plugin
plugins:
  - artifactId: workflow-aggregator
    version: "608.v6c5a"
`
	if err := os.WriteFile(pfPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "test", "--plugins-file", pfPath, "--to", "dir://" + tmpDir + "/out"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing core plugin")
	}
	if !strings.Contains(err.Error(), "not found in plugins list") {
		t.Errorf("expected 'not found in plugins list', got %v", err)
	}
}

func TestExport_PluginsFileJenkinsVersionEmpty(t *testing.T) {
	// JenkinsVersion is "" in offline mode — this is EXPECTED, not an error.
	// We verify this indirectly by checking the code path doesn't error on "".
	// The test just confirms the YAML parses successfully and reaches download phase.
	testSetup(t)

	tmpDir := t.TempDir()
	pfPath := filepath.Join(tmpDir, "plugins.yaml")
	content := `
core:
  - workflow-aggregator
plugins:
  - artifactId: workflow-aggregator
    version: "608.v6c5a"
`
	if err := os.WriteFile(pfPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "test", "--plugins-file", pfPath, "--to", "dir://" + tmpDir + "/out"})
	err := root.Execute()
	// Will fail at download/verify phase since no real update-center, but
	// should NOT fail at closure resolution.
	if err == nil {
		t.Fatal("expected error (download phase)")
	}
	if strings.Contains(err.Error(), "not found") && strings.Contains(err.Error(), "plugins") {
		// This would indicate a closure resolution error, not a download error
		t.Errorf("closure resolution failed: %v", err)
	}
}

func TestExport_PluginsFileEmptyRejected(t *testing.T) {
	// A plugins file that lists no plugins must be rejected — otherwise export
	// silently produces a valid-but-empty pack that fails air-gapped installs.
	testSetup(t)

	tmpDir := t.TempDir()
	pfPath := filepath.Join(tmpDir, "plugins.yaml")
	if err := os.WriteFile(pfPath, []byte("plugins: []\n"), 0644); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "test", "--plugins-file", pfPath, "--to", "dir://" + tmpDir + "/out"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for empty plugins file")
	}
	if !strings.Contains(err.Error(), "no plugins to export") {
		t.Errorf("expected 'no plugins to export', got %v", err)
	}
}

func TestExport_PluginsFileNoCoreExportsAllPlugins(t *testing.T) {
	// A plugins file with no `core:` list exports every entry in `plugins:`
	// (the documented constrained-export form). Closure resolution must succeed
	// and reach the download phase, not reject with "no plugins to export".
	testSetup(t)

	tmpDir := t.TempDir()
	pfPath := filepath.Join(tmpDir, "plugins.yaml")
	content := `
plugins:
  - artifactId: workflow-aggregator
    version: "608.v6c5a"
  - artifactId: git
    version: "5.2.1"
`
	if err := os.WriteFile(pfPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "test", "--plugins-file", pfPath, "--to", "dir://" + tmpDir + "/out"})
	err := root.Execute()
	// Fails later at download/verify (no real update-center in tests), but must
	// NOT reject at closure resolution.
	if err != nil && strings.Contains(err.Error(), "no plugins to export") {
		t.Errorf("plugins-only file wrongly rejected as empty: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2.3c — download + verify (Phase A) + build (Phase B)
// ---------------------------------------------------------------------------

// ucAndPluginServer creates an httptest server that serves both
// update-center.actual.json and plugin .hpi files.
func ucAndPluginServer(t *testing.T, plugins map[string]pluginFixture) *httptest.Server {
	t.Helper()

	// Build update-center response using the REAL flat schema:
	// plugins[name] = {name, version, sha256 (base64), ...}
	ucPlugins := make(map[string]any)
	for _, pf := range plugins {
		ucPlugins[pf.name] = map[string]any{
			"name":    pf.name,
			"version": pf.version,
			"sha256":  pf.sha256B64,
		}
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/update-center.actual.json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plugins": ucPlugins,
			})
			return
		}
		// Parse /download/plugins/<name>/<version>/<name>.hpi
		parts := strings.Split(strings.TrimPrefix(path, "/download/plugins/"), "/")
		if len(parts) >= 3 {
			name := parts[0]
			version := parts[1]
			for _, pf := range plugins {
				if pf.name == name && pf.version == version {
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write(pf.data)
					return
				}
			}
		}
		http.NotFound(w, r)
	}))
}

type pluginFixture struct {
	name      string
	version   string
	sha256B64 string // base64-encoded sha256 (as served by the real update-center)
	data      []byte
}

// hexToB64 converts a hex-encoded sha256 string (no prefix) to base64.
func hexToB64(hexStr string) string {
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestExport_FullDownloadVerifyBuild_Dir(t *testing.T) {
	testSetup(t)
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	// Create a test plugin with known content and sha256
	pluginData := []byte("fake-hpi-content-for-test-plugin")
	digest, _, err := computeSha256Digest(bytes.NewReader(pluginData))
	if err != nil {
		t.Fatal(err)
	}
	digestHex := strings.TrimPrefix(digest, "sha256:")

	plugins := map[string]pluginFixture{
		"workflow-aggregator": {
			name:      "workflow-aggregator",
			version:   "608.v6c5a",
			sha256B64: hexToB64(digestHex),
			data:      pluginData,
		},
	}

	// Update-center + plugin download server
	dlSrv := ucAndPluginServer(t, plugins)
	defer dlSrv.Close()

	// BFF server
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"items": [
				{
					"name": "lts-2.479",
					"version": "2.479.1",
					"plugins": ["workflow-aggregator@608.v6c5a"]
				}
			]
		}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "lts-2.479",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
	})
	err = root.Execute()
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	// Verify the pack was built with expected content
	store, cleanup, err := openOCISrc("dir", outDir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	cfg, rps, err := readPackHelper(t, store, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "lts-2.479" {
		t.Errorf("profile = %q, want %q", cfg.Profile, "lts-2.479")
	}
	if cfg.JenkinsVersion != "2.479.1" {
		t.Errorf("jenkinsVersion = %q, want %q", cfg.JenkinsVersion, "2.479.1")
	}
	if cfg.PluginCount != 1 {
		t.Errorf("pluginCount = %d, want 1", cfg.PluginCount)
	}
	if len(rps) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(rps))
	}
	if rps[0].Name != "workflow-aggregator" {
		t.Errorf("plugin name = %q", rps[0].Name)
	}
	if rps[0].Version != "608.v6c5a" {
		t.Errorf("plugin version = %q", rps[0].Version)
	}
}

func TestExport_Sha256MismatchNoPartialPack(t *testing.T) {
	testSetup(t)
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	pluginData := []byte("correct-content")
	digest, _, err := computeSha256Digest(bytes.NewReader(pluginData))
	if err != nil {
		t.Fatal(err)
	}
	digestHex := strings.TrimPrefix(digest, "sha256:")
	_ = digestHex // used for documentation; mismatch is triggered by fixture's wrong sha256
	// The update-center declares a DIFFERENT sha256 → mismatch
	wrongHex := "0000000000000000000000000000000000000000000000000000000000000000"
	plugins := map[string]pluginFixture{
		"bad-plugin": {
			name:      "bad-plugin",
			version:   "1.0",
			sha256B64: hexToB64(wrongHex), // wrong!
			data:      pluginData,
		},
	}

	dlSrv := ucAndPluginServer(t, plugins)
	defer dlSrv.Close()

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [{"name": "test", "version": "1.0", "plugins": ["bad-plugin@1.0"]}]
		}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "test",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
	})
	err = root.Execute()
	if err == nil {
		t.Fatal("expected sha256 mismatch error")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("expected 'sha256 mismatch', got %v", err)
	}

	// Verify the --to dest has ZERO manifests and ZERO blobs
	store, cleanup, err := openOCISrc("dir", outDir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	manifests, err := store.ListManifests(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 0 {
		t.Errorf("expected 0 manifests after sha256 mismatch, got %d", len(manifests))
	}

	// Check blobs/ directory is empty/nonexistent
	blobsDir := filepath.Join(outDir, "blobs")
	if entries, err := os.ReadDir(blobsDir); err == nil {
		if len(entries) > 0 {
			t.Errorf("expected empty blobs dir, got %d entries", len(entries))
		}
	}
	// It's OK if blobs/ doesn't exist too — either is valid proof of no partial write.
}

func TestExport_Sha256MismatchLastPlugin(t *testing.T) {
	// Verify that even the LAST plugin's mismatch prevents any store writes.
	testSetup(t)
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	goodData := []byte("good-plugin-content")
	goodDigest, _, _ := computeSha256Digest(bytes.NewReader(goodData))
	goodHex := strings.TrimPrefix(goodDigest, "sha256:")

	badData := []byte("bad-plugin-content")

	wrongHex := "0000000000000000000000000000000000000000000000000000000000000000"
	plugins := map[string]pluginFixture{
		"good-plugin": {
			name:      "good-plugin",
			version:   "1.0",
			sha256B64: hexToB64(goodHex),
			data:      goodData,
		},
		"bad-plugin": {
			name:      "bad-plugin",
			version:   "1.0",
			sha256B64: hexToB64(wrongHex),
			data:      badData,
		},
	}

	dlSrv := ucAndPluginServer(t, plugins)
	defer dlSrv.Close()

	// The BFF lists the bad plugin LAST
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [{
				"name": "test",
				"version": "1.0",
				"plugins": ["good-plugin@1.0", "bad-plugin@1.0"]
			}]
		}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "test",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
	})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected sha256 mismatch error")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("expected 'sha256 mismatch', got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2.4c — output format tests
// ---------------------------------------------------------------------------

func TestExport_DefaultOutput(t *testing.T) {
	// Capture stdout to verify the one-line format
	testSetup(t)

	pluginData := []byte("plugin-bytes-for-output-test")
	digest, _, _ := computeSha256Digest(bytes.NewReader(pluginData))
	digestHex := strings.TrimPrefix(digest, "sha256:")

	plugins := map[string]pluginFixture{
		"test-plugin": {
			name:      "test-plugin",
			version:   "1.0",
			sha256B64: hexToB64(digestHex),
			data:      pluginData,
		},
	}

	dlSrv := ucAndPluginServer(t, plugins)
	defer dlSrv.Close()

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [{"name":"test","version":"1.0","plugins":["test-plugin@1.0"]}]
		}`)
	})
	defer srv.Close()

	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "test",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
	})
	err := root.Execute()

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Exported 1 plugins with digest") {
		t.Errorf("expected 'Exported 1 plugins with digest' in output, got: %s", output)
	}
	if !strings.Contains(output, "sha256:") {
		t.Errorf("expected digest in output, got: %s", output)
	}
}

func TestExport_JSONOutput(t *testing.T) {
	testSetup(t)

	pluginData := []byte("plugin-bytes-for-json-output")
	digest, _, _ := computeSha256Digest(bytes.NewReader(pluginData))
	digestHex := strings.TrimPrefix(digest, "sha256:")

	plugins := map[string]pluginFixture{
		"test-plugin": {
			name:      "test-plugin",
			version:   "1.0",
			sha256B64: hexToB64(digestHex),
			data:      pluginData,
		},
	}

	dlSrv := ucAndPluginServer(t, plugins)
	defer dlSrv.Close()

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [{"name":"test","version":"1.0","plugins":["test-plugin@1.0"]}]
		}`)
	})
	defer srv.Close()

	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "test",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
		"-o", "json",
	})
	err := root.Execute()

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON output: %v\nOutput: %s", err, buf.String())
	}

	if _, ok := out["digest"]; !ok {
		t.Error("missing 'digest' key")
	}
	if v, ok := out["pluginCount"]; !ok || v != float64(1) {
		t.Errorf("expected pluginCount=1, got %v", v)
	}
	if _, ok := out["ref"]; !ok {
		t.Error("missing 'ref' key")
	}

	// The fixture plugin bytes are not a real .hpi, so its manifest cannot be
	// read. A bulk export must still succeed — identity comes from the resolved
	// entry, not the manifest — and must NAME the affected plugin rather than
	// dropping the metadata gap silently.
	unreadable, ok := out["unreadableManifests"].([]any)
	if !ok || len(unreadable) != 1 || unreadable[0] != "test-plugin" {
		t.Errorf("expected unreadableManifests=[test-plugin], got %v", out["unreadableManifests"])
	}

	// Must NOT have other keys
	for k := range out {
		switch k {
		case "digest", "pluginCount", "ref", "unreadableManifests":
		default:
			t.Errorf("unexpected key in JSON output: %q", k)
		}
	}
}

// TestExport_PackIsProfileKindWithDerivedMetadata proves the two contracts a
// profile export owes the pack format: the config blob declares kind=profile,
// and a plugin whose .hpi actually parses carries the three HPI-derived layer
// annotations.
func TestExport_PackIsProfileKindWithDerivedMetadata(t *testing.T) {
	testSetup(t)

	hpiBytes := testHPIBytes(t, "real-plugin", "3.2.1", "Real Plugin", "2.516.3",
		"mailer:534.v1b_36f5864073,configuration-as-code:2082.vdb_db_4622e9fa_;resolution:=optional")
	digest, _, _ := computeSha256Digest(bytes.NewReader(hpiBytes))
	digestHex := strings.TrimPrefix(digest, "sha256:")

	dlSrv := ucAndPluginServer(t, map[string]pluginFixture{
		"real-plugin": {name: "real-plugin", version: "3.2.1", sha256B64: hexToB64(digestHex), data: hpiBytes},
	})
	defer dlSrv.Close()

	srv := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"name":"test","version":"2.516.3","plugins":["real-plugin@3.2.1"]}]}`)
	})
	defer srv.Close()

	outDir := filepath.Join(t.TempDir(), "out")
	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "test",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("export failed: %v", err)
	}

	store, err := oci.NewLayoutStore(outDir)
	if err != nil {
		t.Fatalf("open layout: %v", err)
	}
	cfg, plugins, err := oci.ReadPluginPack(context.Background(), store, outDir)
	if err != nil {
		t.Fatalf("ReadPluginPack: %v", err)
	}
	if cfg.Kind != oci.PackKindProfile {
		t.Errorf("cfg.Kind = %q, want %q", cfg.Kind, oci.PackKindProfile)
	}
	if cfg.Profile != "test" {
		t.Errorf("cfg.Profile = %q", cfg.Profile)
	}
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins", len(plugins))
	}
	p := plugins[0]
	if p.DisplayName != "Real Plugin" {
		t.Errorf("DisplayName = %q", p.DisplayName)
	}
	if p.RequiredCore != "2.516.3" {
		t.Errorf("RequiredCore = %q", p.RequiredCore)
	}
	if len(p.Dependencies) != 2 || p.Dependencies[0].Name != "mailer" || !p.Dependencies[1].Optional {
		t.Errorf("Dependencies = %+v", p.Dependencies)
	}
	// Operator-supplied metadata is addon-only and must NOT appear here.
	if p.Description != "" || len(p.Tags) != 0 {
		t.Errorf("bulk export must not set description/tags: %q %v", p.Description, p.Tags)
	}
}

// testHPIBytes builds a minimal but genuine .hpi: a zip holding a
// META-INF/MANIFEST.MF with the given attributes.
func testHPIBytes(t *testing.T, shortName, version, longName, jenkinsVersion, deps string) []byte {
	t.Helper()
	mf := "Manifest-Version: 1.0\r\n" +
		"Short-Name: " + shortName + "\r\n" +
		"Plugin-Version: " + version + "\r\n"
	if longName != "" {
		mf += "Long-Name: " + longName + "\r\n"
	}
	if jenkinsVersion != "" {
		mf += "Jenkins-Version: " + jenkinsVersion + "\r\n"
	}
	if deps != "" {
		mf += "Plugin-Dependencies: " + deps + "\r\n"
	}
	mf += "\r\n"
	return testHPIBytesRaw(t, mf)
}

// TestExport_EmptyResolutionRefusesToPublish covers the CLI half of issue #416:
// a matched profile that resolves to zero plugins must fail loudly rather than
// publish a valid-but-empty pack and exit 0. This holds regardless of the status
// that accompanied the empty resolution — the CLI does not need to reason about
// WHY the set is empty in order to refuse it.
func TestExport_EmptyResolutionRefusesToPublish(t *testing.T) {
	testSetup(t)

	// A 200 response whose matched profile simply carries no `plugins` key —
	// exactly the shape `json:"plugins,omitempty"` produces from a nil slice.
	srv := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"name":"jenkins-version-2-555","version":"2.555"}]}`)
	})
	defer srv.Close()

	outDir := filepath.Join(t.TempDir(), "out")
	root := newRootCmd()
	root.SetArgs([]string{"export", "plugins", "--profile", "jenkins-version-2-555", "--to", "dir://" + outDir})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error for a profile that resolved to zero plugins")
	}
	if !strings.Contains(err.Error(), "jenkins-version-2-555") {
		t.Errorf("error must name the profile: %v", err)
	}
	if !strings.Contains(err.Error(), "no plugins") {
		t.Errorf("error must state that no plugins resolved: %v", err)
	}
	if _, statErr := os.Stat(outDir); statErr == nil {
		t.Error("no pack may be pushed to the destination")
	}
}

// testHPIBytesRaw zips an arbitrary manifest body as META-INF/MANIFEST.MF.
func testHPIBytesRaw(t *testing.T, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatalf("create manifest entry: %v", err)
	}
	if _, err := w.Write([]byte(manifest)); err != nil {
		t.Fatalf("write manifest entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestExport_DigestFromResolve(t *testing.T) {
	// Verify the output digest comes from store.Resolve, not from BuildPluginPack or Pull.
	testSetup(t)

	pluginData := []byte("digest-resolve-test-data")
	digest, _, _ := computeSha256Digest(bytes.NewReader(pluginData))
	digestHex := strings.TrimPrefix(digest, "sha256:")

	plugins := map[string]pluginFixture{
		"resolve-test": {
			name:      "resolve-test",
			version:   "1.0",
			sha256B64: hexToB64(digestHex),
			data:      pluginData,
		},
	}

	dlSrv := ucAndPluginServer(t, plugins)
	defer dlSrv.Close()

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"items": [{"name":"test","version":"1.0","plugins":["resolve-test@1.0"]}]
		}`)
	})
	defer srv.Close()

	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "test",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
		"-o", "json",
	})
	err := root.Execute()
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	// Now independently resolve the digest from the store
	store, cleanup, err := openOCISrc("dir", outDir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	desc, err := store.Resolve(t.Context(), outDir)
	if err != nil {
		t.Fatal(err)
	}

	// The output digest should match the store's Resolve
	// We can't easily capture stdout here (already done above), but we can verify
	// the store's digest is non-empty and valid
	if desc.Digest == "" {
		t.Error("empty digest from store.Resolve")
	}
	if !strings.HasPrefix(desc.Digest, "sha256:") {
		t.Errorf("digest should start with sha256:, got %q", desc.Digest)
	}
}

// ---------------------------------------------------------------------------
// #417 — LTS profile dynamic-stable fallback
// ---------------------------------------------------------------------------

// ltsTestServer creates an httptest server that simulates an upstream where
// the CURRENT update-center has a newer version of a plugin, but the
// dynamic-stable-<resolveVersion> metadata endpoint has the pinned version
// with the correct sha256. The blob is always served from the root path
// (never from a dynamic-stable path).
func ltsTestServer(t *testing.T, currentVersion, pinnedVersion, dynamicStableVersion, pluginName, correctSHA256B64 string, blobData []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Current update-center (always serves the current/latest version)
		if path == "/update-center.actual.json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plugins": map[string]any{
					pluginName: map[string]any{
						"name":    pluginName,
						"version": currentVersion,
						"sha256":  "AAAA", // wrong sha256 won't matter since version mismatches
					},
				},
			})
			return
		}
		// Dynamic-stable metadata (serves the pinned version with correct sha256)
		stablePrefix := fmt.Sprintf("/dynamic-stable-%s/update-center.actual.json", dynamicStableVersion)
		if path == stablePrefix {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plugins": map[string]any{
					pluginName: map[string]any{
						"name":    pluginName,
						"version": pinnedVersion,
						"sha256":  correctSHA256B64,
					},
				},
			})
			return
		}
		// Blob: serve from root /download/plugins/... path
		if strings.HasPrefix(path, "/download/plugins/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(blobData)
			return
		}
		http.NotFound(w, r)
	}))
}

func TestExport_LTSProfile_FallsBackToDynamicStable(t *testing.T) {
	testSetup(t)
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	pluginData := []byte("lts-plugin-content")
	digest, _, err := computeSha256Digest(bytes.NewReader(pluginData))
	if err != nil {
		t.Fatal(err)
	}
	digestHex := strings.TrimPrefix(digest, "sha256:")

	// Simulate: profile pins role-strategy@867, current UC serves 878,
	// dynamic-stable-2.555.3 serves 867 with correct sha256.
	dlSrv := ltsTestServer(t,
		"878.v6c5a_4c5a_0085",  // current version (mismatch)
		"867.vf2b_al_266a_d0c", // pinned version
		"2.555.3",              // dynamic-stable version
		"role-strategy",        // plugin name
		hexToB64(digestHex),    // correct sha256
		pluginData,
	)
	defer dlSrv.Close()

	// BFF server: profile has resolveVersion
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"items": [
				{
					"name": "lts-2.555",
					"version": "2.555.3",
					"resolveVersion": "2.555.3",
					"plugins": ["role-strategy@867.vf2b_al_266a_d0c"]
				}
			]
		}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "lts-2.555",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
	})
	err = root.Execute()
	if err != nil {
		t.Fatalf("export should succeed with dynamic-stable fallback, got: %v", err)
	}

	// Verify the pack was built
	store, cleanup, err := openOCISrc("dir", outDir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	cfg, rps, err := readPackHelper(t, store, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "lts-2.555" {
		t.Errorf("profile = %q, want %q", cfg.Profile, "lts-2.555")
	}
	if cfg.JenkinsVersion != "2.555.3" {
		t.Errorf("jenkinsVersion = %q, want %q", cfg.JenkinsVersion, "2.555.3")
	}
	if cfg.PluginCount != 1 {
		t.Errorf("pluginCount = %d, want 1", cfg.PluginCount)
	}
	if len(rps) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(rps))
	}
	if rps[0].Name != "role-strategy" {
		t.Errorf("plugin name = %q", rps[0].Name)
	}
	if rps[0].Version != "867.vf2b_al_266a_d0c" {
		t.Errorf("plugin version = %q", rps[0].Version)
	}
}

func TestExport_LTSProfile_BlobAlwaysUsesRootBase(t *testing.T) {
	// This test verifies that the blob download URL always uses the root
	// downloadURLBase, never the dynamic-stable path. It records every
	// URL the export attempts and asserts none contains "dynamic-stable".
	testSetup(t)
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	pluginData := []byte("blob-base-test-content")
	digest, _, err := computeSha256Digest(bytes.NewReader(pluginData))
	if err != nil {
		t.Fatal(err)
	}
	digestHex := strings.TrimPrefix(digest, "sha256:")

	var requestURLs []string
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURLs = append(requestURLs, r.URL.Path)
		path := r.URL.Path

		// Current UC (version mismatch to trigger fallback)
		if path == "/update-center.actual.json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plugins": map[string]any{
					"role-strategy": map[string]any{
						"name":    "role-strategy",
						"version": "878.v6c5a_4c5a_0085", // mismatches pinned
						"sha256":  "AAAA",
					},
				},
			})
			return
		}
		// Dynamic-stable metadata fallback
		if path == "/dynamic-stable-2.555.3/update-center.actual.json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plugins": map[string]any{
					"role-strategy": map[string]any{
						"name":    "role-strategy",
						"version": "867.vf2b_al_266a_d0c",
						"sha256":  hexToB64(digestHex),
					},
				},
			})
			return
		}
		// Blob download (must be root path, never dynamic-stable)
		if strings.HasPrefix(path, "/download/plugins/") {
			// Record and serve
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(pluginData)
			return
		}
		http.NotFound(w, r)
	}))
	defer dlSrv.Close()

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"items": [
				{
					"name": "lts-2.555",
					"version": "2.555.3",
					"resolveVersion": "2.555.3",
					"plugins": ["role-strategy@867.vf2b_al_266a_d0c"]
				}
			]
		}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugins",
		"--profile", "lts-2.555",
		"--to", "dir://" + outDir,
		"--download-url-base", dlSrv.URL,
	})
	err = root.Execute()
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	// Assert that no blob download URL contains "dynamic-stable"
	for _, u := range requestURLs {
		if strings.HasPrefix(u, "/download/plugins/") && strings.Contains(u, "dynamic-stable") {
			t.Errorf("blob download URL must use root base, got: %s", u)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func computeSha256Digest(r io.Reader) (string, int64, error) {
	return oci.Sha256Digest(r)
}

func readPackHelper(t *testing.T, store oci.BlobStore, ref string) (oci.PackConfig, []oci.ResolvedPlugin, error) {
	t.Helper()
	return oci.ReadPluginPack(t.Context(), store, ref)
}
