package updatecenter

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// newCorePluginBytes returns .hpi bytes whose manifest declares requiredCore.
func newCorePluginBytes(t *testing.T, version, requiredCore string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatalf("create manifest entry: %v", err)
	}
	fmt.Fprintf(w, "Manifest-Version: 1.0\r\nShort-Name: %s\r\nPlugin-Version: %s\r\nJenkins-Version: %s\r\n\r\n",
		"foo", version, requiredCore)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// seedCorePack stores one addon plugin "foo" whose manifest declares requiredCore.
func seedCorePack(t *testing.T, store oci.BlobStore, ref, version, requiredCore string) {
	t.Helper()
	body := newCorePluginBytes(t, version, requiredCore)
	p := oci.ResolvedPlugin{Name: "foo", Version: version, SHA256: digestOf(body), Content: bytes.NewReader(body)}
	if err := oci.ApplyHPIMetadata(&p, body); err != nil {
		t.Fatalf("apply metadata: %v", err)
	}
	cfg := oci.PackConfig{
		Kind:        oci.PackKindAddon,
		PluginCount: 1,
		LockHash:    oci.LockHash([]oci.ResolvedPlugin{p}),
		CreatedAt:   "2026-01-01T00:00:00Z",
	}
	if err := oci.BuildPluginPack(context.Background(), store, ref, cfg, []oci.ResolvedPlugin{p}); err != nil {
		t.Fatalf("seed core pack %s: %v", ref, err)
	}
}

// upstreamEntry is one plugin that an upstream metadata source lists.
type upstreamEntry struct {
	name, version, requiredCore string
	hpi                         []byte
}

// coreUpstream serves a weekly metadata document, one LTS-line document at
// /dynamic-stable-2.555.3/, and the HPI bytes of every entry under /plugins/.
func coreUpstream(t *testing.T, weekly, lts []upstreamEntry) *httptest.Server {
	t.Helper()
	doc := func(entries []upstreamEntry) string {
		parts := make([]string, 0, len(entries))
		for _, e := range entries {
			parts = append(parts, fmt.Sprintf(`%q:{"name":%q,"version":%q,"sha256":%q,"requiredCore":%q,"dependencies":[]}`,
				e.name, e.name, e.version, sha256Base64(e.hpi), e.requiredCore))
		}
		return `{"updateCenterVersion":"1","plugins":{` + strings.Join(parts, ",") + `}}`
	}
	files := map[string][]byte{}
	for _, e := range append(append([]upstreamEntry{}, weekly...), lts...) {
		files["/plugins/"+e.name+"/"+e.version+"/"+e.name+".hpi"] = e.hpi
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, doc(weekly))
		case "/dynamic-stable-2.555.3/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, doc(lts))
		default:
			if b, ok := files[r.URL.Path]; ok {
				_, _ = w.Write(b)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fooDeclaredLatest declares foo at latest, so the metadata may resolve it upstream.
func fooDeclaredLatest(t *testing.T) Option {
	return WithDeclaredPluginsFile(declaredFile(t, "foo@latest"))
}

// getMetadataPlugins fetches path and returns the plugins it lists.
func getMetadataPlugins(t *testing.T, ts *testServer, path string) map[string]updateCenterPlugin {
	t.Helper()
	resp := ts.get(path)
	defer func() { _ = resp.Body.Close() }()
	var doc updateCenterJSON
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return doc.Plugins
}

func TestPreferCompatible(t *testing.T) {
	newer := pluginLayerInfo{Name: "foo", Version: "2.0", RequiredCore: "2.570"}
	older := pluginLayerInfo{Name: "foo", Version: "1.9", RequiredCore: "2.555.3"}
	unpinned := pluginLayerInfo{Name: "foo", Version: "1.8"}

	got := preferCompatible([]pluginLayerInfo{newer, older, unpinned}, "2.555.3")
	if len(got) != 2 || got[0].Version != "1.9" || got[1].Version != "1.8" {
		t.Errorf("compatible subset = %+v, want 1.9 and 1.8", got)
	}

	got = preferCompatible([]pluginLayerInfo{newer}, "2.400")
	if len(got) != 1 || got[0].Version != "2.0" {
		t.Errorf("no compatible candidate = %+v, want the input unchanged", got)
	}

	got = preferCompatible([]pluginLayerInfo{newer, older}, "")
	if len(got) != 2 {
		t.Errorf("empty core = %+v, want every candidate", got)
	}
}

func TestMetadata_VersionQuerySelectsCompatibleStoredVersion(t *testing.T) {
	store := newTestStore(t)
	seedCorePack(t, store, "pack:foo-2", "2.0", "2.570")
	seedCorePack(t, store, "pack:foo-1", "1.9", "2.555.3")
	ts := newTestServer(t, store)

	if got := getMetadataPlugins(t, ts, "/update-center.actual.json?version=2.555.3")["foo"].Version; got != "1.9" {
		t.Errorf("?version=2.555.3 served %q, want 1.9", got)
	}
	if got := getMetadataPlugins(t, ts, "/update-center.actual.json")["foo"].Version; got != "2.0" {
		t.Errorf("no query served %q, want 2.0", got)
	}
	// No stored version runs on 2.400, so the highest stored version is listed and the
	// tool reports the requiredCore error itself.
	if got := getMetadataPlugins(t, ts, "/update-center.actual.json?version=2.400")["foo"].Version; got != "2.0" {
		t.Errorf("?version=2.400 served %q, want 2.0 by fallback", got)
	}
}

func TestMetadata_VersionQueryAppliesToJSONP(t *testing.T) {
	store := newTestStore(t)
	seedCorePack(t, store, "pack:foo-2", "2.0", "2.570")
	seedCorePack(t, store, "pack:foo-1", "1.9", "2.555.3")
	ts := newTestServer(t, store)

	resp := ts.get("/update-center.json?version=2.555.3")
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	body := strings.TrimSuffix(strings.TrimPrefix(buf.String(), "updateCenter.post("), ");")
	var doc updateCenterJSON
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode JSONP body: %v", err)
	}
	if got := doc.Plugins["foo"].Version; got != "1.9" {
		t.Errorf("JSONP ?version=2.555.3 served %q, want 1.9", got)
	}
}

func TestMetadata_RequiredCoreEmittedForStoredEntry(t *testing.T) {
	store := newTestStore(t)
	seedCorePack(t, store, "pack:foo-1", "1.9", "2.555.3")
	ts := newTestServer(t, store)

	if got := getMetadataPlugins(t, ts, "/update-center.actual.json")["foo"].RequiredCore; got != "2.555.3" {
		t.Errorf("requiredCore = %q, want 2.555.3", got)
	}
}

func TestMetadata_DeclaredLatestUnstoredListsCompatibleUpstream(t *testing.T) {
	weekly := []upstreamEntry{{name: "foo", version: "2.0", requiredCore: "2.570", hpi: newCorePluginBytes(t, "2.0", "2.570")}}
	ltsBytes := newCorePluginBytes(t, "1.9", "2.555.3")
	lts := []upstreamEntry{{name: "foo", version: "1.9", requiredCore: "2.555.3", hpi: ltsBytes}}
	upstream := coreUpstream(t, weekly, lts)
	ts := newTestServer(t, newTestStore(t),
		append(pullThroughWithResolver(upstream.URL, upstream.URL+"/dynamic-stable-2.555.3"), fooDeclaredLatest(t))...)

	entry, ok := getMetadataPlugins(t, ts, "/update-center.actual.json?version=2.555.3")["foo"]
	if !ok {
		t.Fatal("foo not listed for core 2.555.3")
	}
	if entry.Version != "1.9" || entry.RequiredCore != "2.555.3" {
		t.Errorf("entry = %+v, want 1.9 with requiredCore 2.555.3", entry)
	}
	if entry.SHA256 != sha256Base64(ltsBytes) {
		t.Errorf("sha256 = %q, want the upstream digest", entry.SHA256)
	}

	resp, body := getBody(t, ts.baseURL+"/download/plugins/foo/1.9/foo.hpi")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, ltsBytes) {
		t.Errorf("download body is not the 1.9 bytes")
	}

	_, invBody := getBody(t, ts.baseURL+"/api/v1/inventory")
	var inv inventoryResponse
	if err := json.Unmarshal(invBody, &inv); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	stored := false
	for _, e := range inv.Plugins {
		if e.Name == "foo" && e.Version == "1.9" {
			stored = true
		}
	}
	if !stored {
		t.Errorf("inventory does not hold foo@1.9 after pull-through: %+v", inv.Plugins)
	}
}

func TestMetadata_DeclaredLatestIncompatibleStoredUsesUpstream(t *testing.T) {
	store := newTestStore(t)
	seedCorePack(t, store, "pack:foo-2", "2.0", "2.570")
	ltsBytes := newCorePluginBytes(t, "1.9", "2.555.3")
	lts := []upstreamEntry{{name: "foo", version: "1.9", requiredCore: "2.555.3", hpi: ltsBytes}}
	upstream := coreUpstream(t, nil, lts)
	ts := newTestServer(t, store,
		append(pullThroughWithResolver(upstream.URL, upstream.URL+"/dynamic-stable-2.555.3"), fooDeclaredLatest(t))...)

	if got := getMetadataPlugins(t, ts, "/update-center.actual.json?version=2.555.3")["foo"].Version; got != "1.9" {
		t.Errorf("?version=2.555.3 served %q, want 1.9 from upstream", got)
	}
}

func TestMetadata_DeclaredLatestNoVersionQueryUnchanged(t *testing.T) {
	store := newTestStore(t)
	seedCorePack(t, store, "pack:foo-2", "2.0", "2.570")
	lts := []upstreamEntry{{name: "foo", version: "1.9", requiredCore: "2.555.3", hpi: newCorePluginBytes(t, "1.9", "2.555.3")}}
	upstream := coreUpstream(t, nil, lts)
	ts := newTestServer(t, store,
		append(pullThroughWithResolver(upstream.URL, upstream.URL+"/dynamic-stable-2.555.3"), fooDeclaredLatest(t))...)

	if got := getMetadataPlugins(t, ts, "/update-center.actual.json")["foo"].Version; got != "2.0" {
		t.Errorf("no query served %q, want the stored 2.0", got)
	}
}

func TestMetadata_DeclaredLatestPullThroughOffNoUpstreamEntry(t *testing.T) {
	store := newTestStore(t)
	seedCorePack(t, store, "pack:foo-2", "2.0", "2.570")
	ts := newTestServer(t, store, fooDeclaredLatest(t))

	// No stored version runs on 2.555.3, so the stored fallback is listed. No upstream
	// entry replaces it, because pull-through is off.
	if got := getMetadataPlugins(t, ts, "/update-center.actual.json?version=2.555.3")["foo"].Version; got != "2.0" {
		t.Errorf("served %q with pull-through off, want the stored fallback 2.0", got)
	}
}

func TestMetadata_UndeclaredUnstoredNotListed(t *testing.T) {
	lts := []upstreamEntry{{name: "foo", version: "1.9", requiredCore: "2.555.3", hpi: newCorePluginBytes(t, "1.9", "2.555.3")}}
	upstream := coreUpstream(t, nil, lts)
	ts := newTestServer(t, newTestStore(t), pullThroughWithResolver(upstream.URL, upstream.URL+"/dynamic-stable-2.555.3")...)

	if _, ok := getMetadataPlugins(t, ts, "/update-center.actual.json?version=2.555.3")["foo"]; ok {
		t.Error("undeclared foo listed from upstream")
	}
}
