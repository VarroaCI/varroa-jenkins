package updatecenter

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
)

// pluginVersionsDoc fetches GET /plugin-versions.json and decodes it.
func pluginVersionsDoc(t *testing.T, ts *testServer) (pluginVersionsJSON, []byte) {
	t.Helper()
	resp := ts.get("/plugin-versions.json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plugin-versions status = %d, want 200", resp.StatusCode)
	}
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	var doc pluginVersionsJSON
	if err := json.Unmarshal(raw.Bytes(), &doc); err != nil {
		t.Fatalf("decode plugin-versions: %v", err)
	}
	return doc, raw.Bytes()
}

func TestPluginVersions_ListsEveryStoredVersion(t *testing.T) {
	store := newTestStore(t)
	bodyV1 := newPluginBytes("multi", "1.0")
	bodyV2 := newPluginBytes("multi", "2.0")
	seedAddonPack(t, store, "pack:multi-1", "multi", "1.0", bodyV1, []hpi.Dependency{
		{Name: "structs", Min: "1.0", Optional: false},
	})
	seedAddonPack(t, store, "pack:multi-2", "multi", "2.0", bodyV2, []hpi.Dependency{
		{Name: "structs", Min: "1.1", Optional: false},
		{Name: "cred", Min: "2.0", Optional: true},
	})

	ts := newTestServer(t, store)
	doc, _ := pluginVersionsDoc(t, ts)

	versions := doc.Plugins["multi"]
	if len(versions) != 2 {
		t.Fatalf("multi has %d versions, want 2: %+v", len(versions), versions)
	}
	v2, ok := versions["2.0"]
	if !ok {
		t.Fatal("version 2.0 missing")
	}
	if _, ok := versions["1.0"]; !ok {
		t.Fatal("version 1.0 missing")
	}
	rawV2, err := hex.DecodeString(stripDigestPrefix(digestOf(bodyV2)))
	if err != nil {
		t.Fatal(err)
	}
	if v2.SHA256 != base64.StdEncoding.EncodeToString(rawV2) {
		t.Errorf("sha256 = %q, want base64 of the stored digest", v2.SHA256)
	}
	if v2.URL != "multi/2.0/multi.hpi" {
		t.Errorf("url = %q, want multi/2.0/multi.hpi", v2.URL)
	}
	if v2.Name != "multi" || v2.Version != "2.0" {
		t.Errorf("name/version = %q/%q, want multi/2.0", v2.Name, v2.Version)
	}
	wantDeps := []updateCenterDependency{
		{Name: "structs", Optional: false, Version: "1.1"},
		{Name: "cred", Optional: true, Version: "2.0"},
	}
	if !reflect.DeepEqual(v2.Dependencies, wantDeps) {
		t.Errorf("dependencies = %+v, want %+v", v2.Dependencies, wantDeps)
	}
}

func TestPluginVersions_RequiredCoreKeyAlwaysPresent(t *testing.T) {
	store := newTestStore(t)
	seedAddonPack(t, store, "pack:nocore", "nocore", "1.0", newPluginBytes("nocore", "1.0"), nil)

	ts := newTestServer(t, store)
	_, raw := pluginVersionsDoc(t, ts)

	var generic struct {
		Plugins map[string]map[string]map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	entry, ok := generic.Plugins["nocore"]["1.0"]
	if !ok {
		t.Fatal("nocore 1.0 missing")
	}
	if _, ok := entry["requiredCore"]; !ok {
		t.Errorf("requiredCore key absent; entry = %s", entry)
	}
}

func TestPluginVersions_UnknownDependenciesOmitsVersion(t *testing.T) {
	store := newTestStore(t)
	seedPluginPack(t, store, "pack:opaque-pv", []testPlugin{
		{name: "opaque-pv", version: "1.0", hpiBytes: []byte("not-a-zip")},
	})

	ts := newTestServer(t, store)
	doc, _ := pluginVersionsDoc(t, ts)
	if _, ok := doc.Plugins["opaque-pv"]; ok {
		t.Error("version with unknown dependencies must be left out of plugin-versions")
	}
}

func TestPluginVersions_EmptyStoreAnswersEmptyPlugins(t *testing.T) {
	ts := newTestServer(t, nil)
	_, raw := pluginVersionsDoc(t, ts)
	if got := strings.TrimSpace(string(raw)); got != `{"plugins":{}}` {
		t.Errorf("body = %s, want {\"plugins\":{}}", got)
	}
}

func TestPluginVersions_MethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, nil)
	resp, err := http.Post(ts.baseURL+"/plugin-versions.json", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp.StatusCode)
	}
}

func TestPluginVersions_DuplicateVersionMatchesDownload(t *testing.T) {
	store := newTestStore(t)
	bodyA := newPluginBytes("dup", "1.0")
	bodyB := append(newPluginBytes("dup", "1.0"), []byte("trailing")...)
	seedAddonPack(t, store, "pack:dup-a", "dup", "1.0", bodyA, nil)
	seedAddonPack(t, store, "pack:dup-b", "dup", "1.0", bodyB, nil)

	ts := newTestServer(t, store)
	doc, _ := pluginVersionsDoc(t, ts)
	entry, ok := doc.Plugins["dup"]["1.0"]
	if !ok {
		t.Fatal("dup 1.0 missing")
	}

	digest := ts.server.findPluginDigest(t.Context(), "dup", "1.0")
	if digest == "" {
		t.Fatal("findPluginDigest returned no digest")
	}
	raw, err := hex.DecodeString(stripDigestPrefix(digest))
	if err != nil {
		t.Fatal(err)
	}
	if entry.SHA256 != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("advertised sha256 = %q, does not match download digest %s", entry.SHA256, digest)
	}
}

// A skipped first pack must keep a later pack's digest out of the document,
// because the download serves the first pack's bytes. Packs list in
// reference-name order, so the opaque pack's name sorts first.
func TestPluginVersions_SkippedFirstPackBlocksLaterDigest(t *testing.T) {
	store := newTestStore(t)
	opaque := []byte("not-a-zip")
	seedPluginPack(t, store, "pack:dup2-a-opaque", []testPlugin{
		{name: "dup2", version: "1.0", hpiBytes: opaque},
	})
	seedAddonPack(t, store, "pack:dup2-b-known", "dup2", "1.0", newPluginBytes("dup2", "1.0"), []hpi.Dependency{
		{Name: "structs", Min: "1.0", Optional: false},
	})

	ts := newTestServer(t, store)
	if got := ts.server.findPluginDigest(t.Context(), "dup2", "1.0"); got != digestOf(opaque) {
		t.Fatalf("premise: download digest = %s, want the first pack %s", got, digestOf(opaque))
	}
	doc, _ := pluginVersionsDoc(t, ts)
	if _, ok := doc.Plugins["dup2"]["1.0"]; ok {
		t.Error("dup2 1.0 advertised from a later pack, but the download serves the skipped first pack")
	}
}

// depUpstream serves one plugin at the weekly metadata root, with the given
// dependency JSON array, and its HPI bytes under /plugins/.
func depUpstream(t *testing.T, name, version string, hpiBytes []byte, deps string) *httptest.Server {
	t.Helper()
	doc := fmt.Sprintf(`{"updateCenterVersion":"1","plugins":{%q:{"name":%q,"version":%q,"sha256":%q,"requiredCore":"2.555.3","dependencies":%s}}}`,
		name, name, version, sha256Base64(hpiBytes), deps)
	hpiPath := "/plugins/" + name + "/" + version + "/" + name + ".hpi"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, doc)
		case hpiPath:
			_, _ = w.Write(hpiBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPluginVersions_UnstoredDeclaredPinFilledFromUpstream(t *testing.T) {
	hpiBytes := newCorePluginBytes(t, "1.9", "2.555.3")
	up := depUpstream(t, "foo", "1.9", hpiBytes, `[{"name":"bar","version":"1.2","optional":true}]`)
	ts := newTestServer(t, nil,
		append(pullThroughWithResolver(up.URL), WithDeclaredPluginsFile(declaredFile(t, "foo@1.9")))...)

	doc, _ := pluginVersionsDoc(t, ts)
	entry, ok := doc.Plugins["foo"]["1.9"]
	if !ok {
		t.Fatalf("foo 1.9 missing from plugin-versions: %+v", doc)
	}
	if entry.SHA256 != sha256Base64(hpiBytes) {
		t.Errorf("sha256 = %q, want upstream base64 digest", entry.SHA256)
	}
	if entry.URL != "foo/1.9/foo.hpi" {
		t.Errorf("url = %q, want foo/1.9/foo.hpi", entry.URL)
	}
	if entry.RequiredCore != "2.555.3" {
		t.Errorf("requiredCore = %q, want 2.555.3", entry.RequiredCore)
	}
	wantDeps := []updateCenterDependency{{Name: "bar", Optional: true, Version: "1.2"}}
	if !reflect.DeepEqual(entry.Dependencies, wantDeps) {
		t.Errorf("dependencies = %+v, want %+v", entry.Dependencies, wantDeps)
	}
}

func TestPluginVersions_FilledDigestMatchesPulledBytes(t *testing.T) {
	hpiBytes := newCorePluginBytes(t, "1.9", "2.555.3")
	up := depUpstream(t, "foo", "1.9", hpiBytes, "[]")
	ts := newTestServer(t, nil,
		append(pullThroughWithResolver(up.URL), WithDeclaredPluginsFile(declaredFile(t, "foo@1.9")))...)

	doc, _ := pluginVersionsDoc(t, ts)
	advertised := doc.Plugins["foo"]["1.9"].SHA256
	if advertised == "" {
		t.Fatal("foo 1.9 missing from plugin-versions")
	}

	resp := ts.get("/download/plugins/foo/1.9/foo.hpi")
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	if got := sha256Base64(body); got != advertised {
		t.Errorf("pulled bytes digest = %q, advertised = %q", got, advertised)
	}

	again, _ := pluginVersionsDoc(t, ts)
	if got := again.Plugins["foo"]["1.9"].SHA256; got != advertised {
		t.Errorf("after pull-through, sha256 = %q, want %q", got, advertised)
	}
}

func TestPluginVersions_StoredVersionNotReplacedByUpstream(t *testing.T) {
	store := newTestStore(t)
	seedCorePack(t, store, "pack:foo-2.0", "2.0", "2.555.3")
	upBytes := append(newCorePluginBytes(t, "2.0", "2.555.3"), []byte("upstream")...)
	up := depUpstream(t, "foo", "2.0", upBytes, "[]")

	ts := newTestServer(t, store,
		append(pullThroughWithResolver(up.URL), WithDeclaredPluginsFile(declaredFile(t, "foo@2.0")))...)
	digest := ts.server.findPluginDigest(t.Context(), "foo", "2.0")
	if digest == "" {
		t.Fatal("findPluginDigest returned no digest for the stored foo 2.0")
	}
	raw, err := hex.DecodeString(stripDigestPrefix(digest))
	if err != nil {
		t.Fatal(err)
	}

	doc, _ := pluginVersionsDoc(t, ts)
	entry, ok := doc.Plugins["foo"]["2.0"]
	if !ok {
		t.Fatal("foo 2.0 missing from plugin-versions")
	}
	if entry.SHA256 != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("sha256 = %q, want the stored digest", entry.SHA256)
	}
	if entry.SHA256 == sha256Base64(upBytes) {
		t.Error("stored version was replaced by the upstream digest")
	}
}

func TestPluginVersions_WithheldStoredVersionNotFilled(t *testing.T) {
	store := newTestStore(t)
	seedPluginPack(t, store, "pack:opaque-pv", []testPlugin{
		{name: "opaque-pv", version: "1.0", hpiBytes: []byte("not-a-zip")},
	})
	up := depUpstream(t, "opaque-pv", "1.0", []byte("upstream-bytes"), "[]")

	ts := newTestServer(t, store,
		append(pullThroughWithResolver(up.URL), WithDeclaredPluginsFile(declaredFile(t, "opaque-pv@1.0")))...)
	doc, _ := pluginVersionsDoc(t, ts)
	if _, ok := doc.Plugins["opaque-pv"]["1.0"]; ok {
		t.Error("withheld stored version was filled from upstream")
	}
}

func TestPluginVersions_PullThroughOffNoFill(t *testing.T) {
	ts := newTestServer(t, nil, WithDeclaredPluginsFile(declaredFile(t, "foo@1.9")))
	doc, _ := pluginVersionsDoc(t, ts)
	if _, ok := doc.Plugins["foo"]; ok {
		t.Error("declared pin filled with pull-through off")
	}
}

func TestPluginVersions_UndeclaredUnstoredNotFilled(t *testing.T) {
	up := depUpstream(t, "foo", "1.9", []byte("upstream-bytes"), "[]")
	ts := newTestServer(t, nil,
		append(pullThroughWithResolver(up.URL), WithDeclaredPluginsFile(declaredFile(t)))...)
	doc, _ := pluginVersionsDoc(t, ts)
	if _, ok := doc.Plugins["foo"]; ok {
		t.Error("undeclared plugin filled from upstream")
	}
}

func TestPluginVersions_DeclaredLatestNotFilled(t *testing.T) {
	up := depUpstream(t, "foo", "1.9", []byte("upstream-bytes"), "[]")
	ts := newTestServer(t, nil,
		append(pullThroughWithResolver(up.URL), fooDeclaredLatest(t))...)
	doc, _ := pluginVersionsDoc(t, ts)
	if _, ok := doc.Plugins["foo"]; ok {
		t.Error("declared latest filled as an exact pin")
	}
}

func TestPluginVersions_UnlistedPinOmitted(t *testing.T) {
	up := depUpstream(t, "foo", "2.0", []byte("upstream-bytes"), "[]")
	ts := newTestServer(t, nil,
		append(pullThroughWithResolver(up.URL), WithDeclaredPluginsFile(declaredFile(t, "foo@1.0")))...)
	doc, _ := pluginVersionsDoc(t, ts)
	if _, ok := doc.Plugins["foo"]["1.0"]; ok {
		t.Error("pin that upstream does not list was filled")
	}
}

func TestPluginVersions_UpstreamDownOmitsPin(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	ts := newTestServer(t, nil,
		append(pullThroughWithResolver(closedURL), WithDeclaredPluginsFile(declaredFile(t, "foo@1.9")))...)
	resp := ts.get("/plugin-versions.json")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plugin-versions status = %d, want 200", resp.StatusCode)
	}
	doc, _ := pluginVersionsDoc(t, ts)
	if _, ok := doc.Plugins["foo"]; ok {
		t.Error("pin filled while upstream is down")
	}
}
