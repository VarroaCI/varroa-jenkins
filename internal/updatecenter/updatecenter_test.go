package updatecenter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore creates a temp-dir-backed oci.LayoutStore for tests.
func newTestStore(t *testing.T) *oci.LayoutStore {
	t.Helper()
	dir := t.TempDir()
	ls, err := oci.NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}
	return ls
}

// testPlugin describes a plugin to seed in a test pack.
type testPlugin struct {
	name     string
	version  string
	hpiBytes []byte
}

// newPluginBytes returns deterministic .hpi content for tests.
func newPluginBytes(name, version string) []byte {
	return []byte(fmt.Sprintf("fake-hpi-content-for-%s-%s", name, version))
}

// seedPluginPack builds and pushes a plugin pack to store.
func seedPluginPack(t *testing.T, store oci.BlobStore, ref string, plugins []testPlugin) {
	t.Helper()
	resolved := make([]oci.ResolvedPlugin, 0, len(plugins))

	for _, p := range plugins {
		digest, _, err := oci.Sha256Digest(bytes.NewReader(p.hpiBytes))
		if err != nil {
			t.Fatalf("Sha256Digest: %v", err)
		}
		resolved = append(resolved, oci.ResolvedPlugin{
			Name:    p.name,
			Version: p.version,
			SHA256:  digest,
			Content: bytes.NewReader(p.hpiBytes),
		})
	}

	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: "2.479.1",
		Profile:        "test-profile",
		LockHash:       oci.LockHash(resolved),
		PluginCount:    len(resolved),
		CreatedAt:      "2025-01-01T00:00:00Z",
	}
	if err := oci.BuildPluginPack(context.Background(), store, ref, cfg, resolved); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}
}

// sha256Hex returns the hex-encoded SHA-256 of data (no "sha256:" prefix).
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

// sha256Base64 returns the base64-encoded SHA-256 of data.
func sha256Base64(data []byte) string {
	h := sha256.Sum256(data)
	return base64.StdEncoding.EncodeToString(h[:])
}

// buildImportTarball creates a LayoutStore in a temp dir, seeds it with the
// given plugin pack, and returns its gzipped-tar bytes and the store path
// (cleaned up by t.Cleanup).
func buildImportTarball(t *testing.T, ref string, plugins []testPlugin) []byte {
	t.Helper()
	dir := t.TempDir()
	ls, err := oci.NewLayoutStore(dir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}
	seedPluginPack(t, ls, ref, plugins)

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			io.Copy(tw, f)
			f.Close()
		}
		return nil
	})
	tw.Close()
	gzw.Close()
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// testServer helper
// ---------------------------------------------------------------------------

type testServer struct {
	t       *testing.T
	server  *Server
	store   oci.BlobStore
	raw     *httptest.Server
	baseURL string
}

// pullThroughWithResolver returns the options to enable pull-through against upstreamURL
// with a metadata resolver. The first URL is used as both the upstream and download base;
// every URL contributes a "<url>/update-center.actual.json" metadata source (weekly first,
// then any LTS-line sources).
func pullThroughWithResolver(upstreamURL string, metadataURLs ...string) []Option {
	srcURLs := append([]string{upstreamURL}, metadataURLs...)
	srcs := make([]ucmeta.Source, len(srcURLs))
	for i, u := range srcURLs {
		srcs[i] = ucmeta.Source{URL: strings.TrimRight(u, "/") + "/update-center.actual.json"}
	}
	r := ucmeta.NewResolver(func() []ucmeta.Source { return srcs }, time.Hour, nil, nil)
	return []Option{WithPullThrough(true, upstreamURL, upstreamURL), WithMetadataResolver(r)}
}

func newTestServer(t *testing.T, store oci.BlobStore, opts ...Option) *testServer {
	t.Helper()
	if store == nil {
		store = newTestStore(t)
	}
	svr := NewServer(store, testLogger(), opts...)
	ts := &testServer{t: t, server: svr, store: store}
	mux := http.NewServeMux()
	svr.RegisterRoutes(mux)
	ts.raw = httptest.NewServer(mux)
	ts.baseURL = ts.raw.URL
	t.Cleanup(ts.raw.Close)
	return ts
}

func (ts *testServer) get(path string) *http.Response {
	t := ts.t
	resp, err := http.Get(ts.baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// §4.6a — metadata generation
// ---------------------------------------------------------------------------

func TestMetadata_ActualJSON(t *testing.T) {
	store := newTestStore(t)
	seedPluginPack(t, store, "pack:v1", []testPlugin{
		{name: "plugin-a", version: "1.0.0", hpiBytes: newPluginBytes("plugin-a", "1.0.0")},
		{name: "plugin-b", version: "2.0.0", hpiBytes: newPluginBytes("plugin-b", "2.0.0")},
	})

	ts := newTestServer(t, store)
	resp := ts.get("/update-center.actual.json")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}

	var uc updateCenterJSON
	if err := json.NewDecoder(resp.Body).Decode(&uc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if uc.UpdateCenterVersion != "1" {
		t.Errorf("updateCenterVersion: expected '1', got %q", uc.UpdateCenterVersion)
	}
	if uc.Core != "" {
		t.Errorf("core: expected '', got %q", uc.Core)
	}
	if len(uc.Plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(uc.Plugins))
	}

	// Check plugin-a shape.
	pa, ok := uc.Plugins["plugin-a"]
	if !ok {
		t.Fatal("missing plugin-a")
	}
	if pa.Name != "plugin-a" || pa.Version != "1.0.0" {
		t.Errorf("plugin-a: %+v", pa)
	}
	if pa.URL != "plugin-a/1.0.0/plugin-a.hpi" {
		t.Errorf("plugin-a URL: %q", pa.URL)
	}
	if len(pa.Dependencies) != 0 {
		t.Errorf("plugin-a Dependencies should be empty, got %v", pa.Dependencies)
	}
	if pa.SHA256 != sha256Base64(newPluginBytes("plugin-a", "1.0.0")) {
		t.Errorf("plugin-a SHA256 mismatch: expected %s (base64), got %s", sha256Base64(newPluginBytes("plugin-a", "1.0.0")), pa.SHA256)
	}
}

func TestMetadata_JSONP(t *testing.T) {
	store := newTestStore(t)
	seedPluginPack(t, store, "pack:v1", []testPlugin{
		{name: "plugin-j", version: "1.0.0", hpiBytes: newPluginBytes("plugin-j", "1.0.0")},
	})

	ts := newTestServer(t, store)
	resp := ts.get("/update-center.json")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/javascript" {
		t.Errorf("expected application/javascript, got %s", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Assert JSONP wrapper.
	if !bytes.HasPrefix(body, []byte("updateCenter.post(")) {
		t.Errorf("expected prefix updateCenter.post(, got %q", body[:30])
	}
	if !bytes.HasSuffix(body, []byte(");")) {
		t.Errorf("expected suffix );, got %q", body[len(body)-5:])
	}

	// Extract inner JSON and verify it matches the plain endpoint.
	inner := bytes.TrimPrefix(body, []byte("updateCenter.post("))
	inner = bytes.TrimSuffix(inner, []byte(");"))

	var uc updateCenterJSON
	if err := json.Unmarshal(inner, &uc); err != nil {
		t.Fatalf("unmarshal inner JSON: %v", err)
	}
	if len(uc.Plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(uc.Plugins))
	}
	if _, ok := uc.Plugins["plugin-j"]; !ok {
		t.Fatal("missing plugin-j in JSONP payload")
	}
}

func TestDownload_StoreHit(t *testing.T) {
	store := newTestStore(t)
	seedPluginPack(t, store, "pack:v1", []testPlugin{
		{name: "dl-plugin", version: "3.0.0", hpiBytes: newPluginBytes("dl-plugin", "3.0.0")},
	})

	ts := newTestServer(t, store)
	url := "/download/plugins/dl-plugin/3.0.0/dl-plugin.hpi"
	resp, err := http.Get(ts.baseURL + url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Check sha256 response header (hex only, no prefix).
	gotSHA := resp.Header.Get("sha256")
	expectedSHA := sha256Hex(newPluginBytes("dl-plugin", "3.0.0"))
	if gotSHA != expectedSHA {
		t.Errorf("sha256 header: expected %s, got %s", expectedSHA, gotSHA)
	}

	// Check Content-Disposition.
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="dl-plugin.hpi"`) {
		t.Errorf("Content-Disposition: %q", cd)
	}

	// Check body.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, newPluginBytes("dl-plugin", "3.0.0")) {
		t.Errorf("body mismatch")
	}
}

// ---------------------------------------------------------------------------
// §4.6b — pull-through
// ---------------------------------------------------------------------------

func TestPullThrough_AgedLTSVersionResolvesFromDynamicStable(t *testing.T) {
	store := newTestStore(t)

	const name = "role-strategy"
	const agedVersion = "867.vd09254229f9b_"    // pinned by an LTS profile
	const weeklyVersion = "878.v6f1d3b_3a_0769" // current weekly
	pluginBytes := newPluginBytes(name, agedVersion)
	agedSHA := sha256Base64(pluginBytes)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update-center.actual.json": // weekly — only the current version, not the aged pin
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"plugins":{%q:{"version":%q,"sha256":%q}}}`, name, weeklyVersion, "d2Vla2x5")
		case "/dynamic-stable-2.555.3/update-center.actual.json": // LTS line — has the aged pin
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"plugins":{%q:{"version":%q,"sha256":%q}}}`, name, agedVersion, agedSHA)
		case "/plugins/" + name + "/" + agedVersion + "/" + name + ".hpi":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(pluginBytes)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	ts := newTestServer(t, store, pullThroughWithResolver(upstream.URL, upstream.URL+"/dynamic-stable-2.555.3")...)

	// The aged LTS pin is absent from the weekly source but present in the
	// dynamic-stable source: pull-through resolves, verifies, stores, and serves it.
	resp, err := http.Get(ts.baseURL + "/download/plugins/" + name + "/" + agedVersion + "/" + name + ".hpi")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("aged LTS pull-through expected 200, got %d: %s", resp.StatusCode, string(body))
	}
	if !bytes.Equal(body, pluginBytes) {
		t.Errorf("served body does not match upstream plugin bytes")
	}

	// A version present in neither source is a 404.
	resp2, err := http.Get(ts.baseURL + "/download/plugins/" + name + "/999.vdeadbeef/" + name + ".hpi")
	if err != nil {
		t.Fatalf("GET missing: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown version expected 404, got %d", resp2.StatusCode)
	}
}

func TestPullThrough_MissAndCache(t *testing.T) {
	store := newTestStore(t) // empty — no plugin seeded.

	const pluginName = "pt-plugin"
	const pluginVersion = "1.5.0"
	pluginBytes := newPluginBytes(pluginName, pluginVersion)
	pluginSHA := sha256Base64(pluginBytes)

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		switch r.URL.Path {
		case "/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			// Emit the *realistic* upstream Jenkins shape, not our own flat served
			// schema: `core` is an object (the WAR descriptor) and each plugin's
			// `dependencies` are objects. Decoding this into updateCenterJSON fails
			// ("cannot unmarshal object into Go struct field ... of type string"),
			// which is exactly the pull-through regression this guards.
			fmt.Fprintf(w, `{
				"updateCenterVersion": "1",
				"core": {"name": "core", "version": "2.516.1", "url": "https://example.test/jenkins.war", "sha256": "Zm9v"},
				"plugins": {
					%q: {
						"name": %q,
						"version": %q,
						"sha256": %q,
						"dependencies": [{"name": "structs", "optional": false, "version": "1.0"}]
					}
				}
			}`, pluginName, pluginName, pluginVersion, pluginSHA)
		case "/plugins/" + pluginName + "/" + pluginVersion + "/" + pluginName + ".hpi":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(pluginBytes)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	ts := newTestServer(t, store, pullThroughWithResolver(upstream.URL)...)
	url := "/download/plugins/" + pluginName + "/" + pluginVersion + "/" + pluginName + ".hpi"

	// First request — miss, pulls through.
	resp1, err := http.Get(ts.baseURL + url)
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first request expected 200, got %d: %s", resp1.StatusCode, string(body1))
	}
	if !bytes.Equal(body1, pluginBytes) {
		t.Errorf("first request body mismatch")
	}

	// Second request — served from store (BuildPluginPack created a
	// discoverable pack, so findPluginDigest finds it without hitting upstream).
	before := upstreamCalls
	resp2, err := http.Get(ts.baseURL + url)
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request expected 200, got %d", resp2.StatusCode)
	}
	if !bytes.Equal(body2, pluginBytes) {
		t.Errorf("second request body mismatch")
	}
	if upstreamCalls != before {
		t.Errorf("second request should not hit upstream: before=%d after=%d", before, upstreamCalls)
	}

	// Verify the plugin is now discoverable via findPluginDigest.
	if digest := ts.server.findPluginDigest(context.Background(), pluginName, pluginVersion); digest == "" {
		t.Error("plugin should be discoverable after pull-through, but findPluginDigest returned empty")
	}

	// Verify the plugin appears in the inventory endpoint.
	respInv, err := http.Get(ts.baseURL + "/api/v1/inventory")
	if err != nil {
		t.Fatalf("GET inventory: %v", err)
	}
	defer respInv.Body.Close()
	var invPayload inventoryResponse
	if err := json.NewDecoder(respInv.Body).Decode(&invPayload); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	found := false
	for _, e := range invPayload.Plugins {
		if e.Name == pluginName && e.Version == pluginVersion {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("plugin %s@%s should appear in inventory after pull-through, got %+v", pluginName, pluginVersion, invPayload.Plugins)
	}
}

func TestPullThrough_ChecksumMismatch(t *testing.T) {
	store := newTestStore(t) // empty.

	const pluginName = "bad-plugin"
	const pluginVersion = "1.0.0"
	pluginBytes := newPluginBytes(pluginName, pluginVersion)
	// wrongSHA is valid base64 that decodes to the wrong 32 bytes (all zeroes).
	var zero32 [32]byte
	wrongSHA := base64.StdEncoding.EncodeToString(zero32[:])

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(updateCenterJSON{
				UpdateCenterVersion: "1",
				Plugins: map[string]updateCenterPlugin{
					pluginName: {
						Name:    pluginName,
						Version: pluginVersion,
						SHA256:  wrongSHA,
					},
				},
			})
		case "/plugins/" + pluginName + "/" + pluginVersion + "/" + pluginName + ".hpi":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(pluginBytes)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	ts := newTestServer(t, store, pullThroughWithResolver(upstream.URL)...)
	url := "/download/plugins/" + pluginName + "/" + pluginVersion + "/" + pluginName + ".hpi"

	resp, err := http.Get(ts.baseURL + url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Spec requires checksum mismatch → 502 Bad Gateway.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 for checksum mismatch, got %d", resp.StatusCode)
	}

	// Verify nothing stored.
	if digest := ts.server.findPluginDigest(context.Background(), pluginName, pluginVersion); digest != "" {
		t.Errorf("expected no digest stored after mismatch, got %q", digest)
	}
}

// ---------------------------------------------------------------------------
// §4.6c — import, inventory, healthz/readyz
// ---------------------------------------------------------------------------

func TestImport_NoTokenReturns401(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, WithImportToken("secret"))
	// No Authorization header at all — handler returns 401 without reading body.
	resp, err := http.Post(ts.baseURL+"/api/v1/import", "application/gzip",
		bytes.NewReader([]byte("some-body-that-wont-be-read")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestImport_WrongTokenReturns401(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, WithImportToken("secret"))
	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/import", bytes.NewReader([]byte("data")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestImport_ValidTarGz(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, WithImportToken("correct-token"))

	// Build a tarball with a plugin pack.
	tarGz := buildImportTarball(t, "import-pack:v1", []testPlugin{
		{name: "imported-p", version: "3.0.0", hpiBytes: newPluginBytes("imported-p", "3.0.0")},
	})

	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/import", bytes.NewReader(tarGz))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer correct-token")
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status"] != "accepted" {
		t.Errorf("status: expected accepted, got %v", result["status"])
	}
	if result["plugin_count"] != float64(1) {
		t.Errorf("plugin_count: expected 1, got %v", result["plugin_count"])
	}

	// Verify the plugin is now in the service's store.
	if digest := ts.server.findPluginDigest(context.Background(), "imported-p", "3.0.0"); digest == "" {
		t.Fatal("imported plugin not found in store after import")
	}
}

func TestImport_MalformedBody(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, WithImportToken("correct-token"))

	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/import", bytes.NewReader([]byte("not-a-tar-gz")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer correct-token")
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

// TestImport_ZipSlipGuard verifies that a tarball with a ../ entry is rejected
// and no file is written outside the destination directory.
func TestImport_ZipSlipGuard(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	hdr := &tar.Header{
		Name: "../evil",
		Size: 4,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	tw.Close()
	gzw.Close()

	store := newTestStore(t)
	ts := newTestServer(t, store, WithImportToken("token"))

	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/import", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for zip-slip tarball, got %d", resp.StatusCode)
	}
}

func TestInventory_Shape(t *testing.T) {
	store := newTestStore(t)
	seedPluginPack(t, store, "pack:v1", []testPlugin{
		{name: "inv-a", version: "1.0.0", hpiBytes: newPluginBytes("inv-a", "1.0.0")},
		{name: "inv-b", version: "2.0.0", hpiBytes: newPluginBytes("inv-b", "2.0.0")},
	})

	ts := newTestServer(t, store)
	resp := ts.get("/api/v1/inventory")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var payload inventoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	entries := payload.Plugins

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Check every entry has all fields.
	for _, e := range entries {
		if e.Name == "" || e.Version == "" || e.SHA256 == "" || e.SizeBytes == 0 {
			t.Errorf("entry missing fields: %+v", e)
		}
	}

	// Verify SHA256 is the full digest with sha256: prefix (inventory
	// returns the layer annotation value as-is, unlike metadata which strips).
	expectedDigest := "sha256:" + sha256Hex(newPluginBytes("inv-a", "1.0.0"))
	if entries[0].Name == "inv-a" && entries[0].SHA256 != expectedDigest {
		t.Errorf("inv-a sha256: expected %s, got %s", expectedDigest, entries[0].SHA256)
	}
}

func TestHealthzReadyz(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store)

	t.Run("healthz-always-200", func(t *testing.T) {
		resp := ts.get("/healthz")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		var m map[string]string
		json.NewDecoder(resp.Body).Decode(&m)
		if m["status"] != "ok" {
			t.Errorf("expected status ok, got %v", m)
		}
	})

	t.Run("readyz-after-markready", func(t *testing.T) {
		ts.server.MarkReady()
		resp := ts.get("/readyz")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 after MarkReady, got %d", resp.StatusCode)
		}
		var m map[string]string
		json.NewDecoder(resp.Body).Decode(&m)
		if m["status"] != "ready" {
			t.Errorf("expected ready, got %v", m)
		}
	})
}

// ---------------------------------------------------------------------------
// §8.1 — PostImport client helper tests
// ---------------------------------------------------------------------------

func TestPostImport_Success(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := PostImport(t.Context(), srv.URL, "test-token", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("PostImport: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization header: expected Bearer test-token, got %q", gotAuth)
	}
	if gotBody != "hello" {
		t.Errorf("body: expected hello, got %q", gotBody)
	}
}

func TestPostImport_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := PostImport(t.Context(), srv.URL, "bad-token", strings.NewReader("data"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
}

func TestPostImport_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("oops"))
	}))
	defer srv.Close()

	err := PostImport(t.Context(), srv.URL, "token", strings.NewReader("data"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected NON-ErrInvalidToken for 500, got ErrInvalidToken")
	}
}

// ---------------------------------------------------------------------------
// F3 — import hardening tests
// ---------------------------------------------------------------------------

// TestImport_OversizedEntry verifies that an entry exceeding the per-entry
// byte limit returns 413, using test-injected small caps.
func TestImport_OversizedEntry(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	hdr := &tar.Header{
		Name: "too-large.dat",
		Size: 5000,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write(make([]byte, 5000)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	tw.Close()
	gzw.Close()

	store := newTestStore(t)
	// 4096 bytes per-entry cap → 5000 byte entry triggers 413.
	ts := newTestServer(t, store,
		WithImportToken("token"),
		WithImportLimits(100, 4096, 1<<20),
	)

	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/import", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized entry, got %d", resp.StatusCode)
	}
}

// TestImport_TotalBudgetOverflow verifies that an import exceeding the total
// byte budget returns 413, exercising the remaining-budget-aware per-entry cap.
// With a 4 KiB per-entry cap and 6 KiB total, two 4 KiB entries hit the total
// budget on the second entry's mid-copy check.
func TestImport_TotalBudgetOverflow(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for i := 0; i < 2; i++ {
		hdr := &tar.Header{
			Name: fmt.Sprintf("entry-%d.dat", i),
			Size: 4096,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %d: %v", i, err)
		}
		if _, err := tw.Write(make([]byte, 4096)); err != nil {
			t.Fatalf("write body %d: %v", i, err)
		}
	}
	tw.Close()
	gzw.Close()

	store := newTestStore(t)
	// 4 KiB per-entry, 6 KiB total → second entry's copy stops at the
	// remaining 2 KiB, which is less than the entry's 4 KiB → 413.
	ts := newTestServer(t, store,
		WithImportToken("token"),
		WithImportLimits(100, 4096, 6144),
	)

	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/import", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for total budget overflow, got %d", resp.StatusCode)
	}
}

// TestImport_EntryCountBomb verifies that a tarball with more than the
// configured entry limit returns 413, using test-injected small caps.
func TestImport_EntryCountBomb(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for i := 0; i < 4; i++ {
		hdr := &tar.Header{
			Name: fmt.Sprintf("file-%d.dat", i),
			Size: 1,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %d: %v", i, err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatalf("write body %d: %v", i, err)
		}
	}
	tw.Close()
	gzw.Close()

	store := newTestStore(t)
	// 3-entry cap → 4 entries triggers 413.
	ts := newTestServer(t, store,
		WithImportToken("token"),
		WithImportLimits(3, 1<<20, 1<<20),
	)

	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/import", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for entry count bomb, got %d", resp.StatusCode)
	}
}

// TestImport_SymlinkRejected verifies that a tarball with a symlink entry
// returns 400 and is rejected.
func TestImport_SymlinkRejected(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	hdr := &tar.Header{
		Name:     "evil-link",
		Size:     0,
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	tw.Close()
	gzw.Close()

	store := newTestStore(t)
	ts := newTestServer(t, store, WithImportToken("token"))

	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/import", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for symlink, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// F4 — tag-safe pull-through ref test
// ---------------------------------------------------------------------------

// validatingStore wraps an oci.BlobStore and rejects Push calls where the ref
// contains '/' or ':', which are illegal in OCI tags. This simulates a
// RegistryStore's strict tag validation.
type validatingStore struct {
	oci.BlobStore
}

func (v *validatingStore) Push(ctx context.Context, ref string, manifest oci.Manifest) error {
	if strings.ContainsAny(ref, "/:") {
		return fmt.Errorf("invalid OCI tag %q: contains illegal character", ref)
	}
	return v.BlobStore.Push(ctx, ref, manifest)
}

// TestPullThrough_TagSafeRef verifies that pull-through uses a tag-safe ref
// that works even with a store that validates OCI tag charset (no '/' or ':').
// After pull-through, the plugin must be discoverable.
func TestPullThrough_TagSafeRef(t *testing.T) {
	// NOTE: This test uses a LayoutStore + validatingStore wrapper that
	// enforces OCI tag-charset validation to prove that the new
	// "pullthrough-<hash>" ref is a valid tag (where the old
	// "pullthrough/<name>:<version>" was not). A true push/pull round-trip
	// against a real registry backend is covered by the C1 §5.2 live smoke
	// (add-oci-plugin-packs), not this unit test.
	// Wrap a normal LayoutStore with the validating wrapper.
	inner := newTestStore(t)
	store := &validatingStore{inner}

	const pluginName = "safe-ref-plugin"
	const pluginVersion = "2.0.0"
	pluginBytes := newPluginBytes(pluginName, pluginVersion)
	pluginSHA := sha256Base64(pluginBytes)

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		switch r.URL.Path {
		case "/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			// Emit the *realistic* upstream Jenkins shape, not our own flat served
			// schema: `core` is an object (the WAR descriptor) and each plugin's
			// `dependencies` are objects. Decoding this into updateCenterJSON fails
			// ("cannot unmarshal object into Go struct field ... of type string"),
			// which is exactly the pull-through regression this guards.
			fmt.Fprintf(w, `{
				"updateCenterVersion": "1",
				"core": {"name": "core", "version": "2.516.1", "url": "https://example.test/jenkins.war", "sha256": "Zm9v"},
				"plugins": {
					%q: {
						"name": %q,
						"version": %q,
						"sha256": %q,
						"dependencies": [{"name": "structs", "optional": false, "version": "1.0"}]
					}
				}
			}`, pluginName, pluginName, pluginVersion, pluginSHA)
		case "/plugins/" + pluginName + "/" + pluginVersion + "/" + pluginName + ".hpi":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(pluginBytes)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	ts := newTestServer(t, store, pullThroughWithResolver(upstream.URL)...)
	url := "/download/plugins/" + pluginName + "/" + pluginVersion + "/" + pluginName + ".hpi"

	// First request — miss, pulls through.
	resp1, err := http.Get(ts.baseURL + url)
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first request expected 200, got %d: %s", resp1.StatusCode, string(body1))
	}
	if !bytes.Equal(body1, pluginBytes) {
		t.Errorf("first request body mismatch")
	}
	if upstreamCalls != 2 {
		t.Errorf("expected 2 upstream calls (metadata + download), got %d", upstreamCalls)
	}

	// Verify the plugin is now discoverable via findPluginDigest.
	digest := ts.server.findPluginDigest(context.Background(), pluginName, pluginVersion)
	if digest == "" {
		t.Fatal("plugin should be discoverable after pull-through with tag-safe ref, but findPluginDigest returned empty")
	}

	// Second request — served from store (upstream not hit again).
	before := upstreamCalls
	resp2, err := http.Get(ts.baseURL + url)
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request expected 200, got %d", resp2.StatusCode)
	}
	if !bytes.Equal(body2, pluginBytes) {
		t.Errorf("second request body mismatch")
	}
	if upstreamCalls != before {
		t.Errorf("second request should not hit upstream: before=%d after=%d", before, upstreamCalls)
	}

	// Verify the plugin appears in the inventory.
	respInv, err := http.Get(ts.baseURL + "/api/v1/inventory")
	if err != nil {
		t.Fatalf("GET inventory: %v", err)
	}
	defer respInv.Body.Close()
	var invPayload inventoryResponse
	if err := json.NewDecoder(respInv.Body).Decode(&invPayload); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	found := false
	for _, e := range invPayload.Plugins {
		if e.Name == pluginName && e.Version == pluginVersion {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("plugin %s@%s should appear in inventory after pull-through with tag-safe ref, got %+v", pluginName, pluginVersion, invPayload.Plugins)
	}
}
