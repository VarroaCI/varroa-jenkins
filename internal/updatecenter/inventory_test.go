package updatecenter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// seedRichPack builds a plugin pack whose layers carry the full T1.1 annotation
// set, so the inventory route has metadata to surface.
func seedRichPack(t *testing.T, store oci.BlobStore, ref string, plugins []oci.ResolvedPlugin) {
	t.Helper()
	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: "2.479.1",
		Profile:        "test-profile",
		LockHash:       oci.LockHash(plugins),
		PluginCount:    len(plugins),
		CreatedAt:      "2025-01-01T00:00:00Z",
	}
	if err := oci.BuildPluginPack(context.Background(), store, ref, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack(%s): %v", ref, err)
	}
}

func richPlugin(name, version string, mutate func(*oci.ResolvedPlugin)) oci.ResolvedPlugin {
	body := newPluginBytes(name, version)
	p := oci.ResolvedPlugin{
		Name:    name,
		Version: version,
		SHA256:  "sha256:" + sha256Hex(body),
		Content: bytes.NewReader(body),
	}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

// getInventory GETs /api/v1/inventory and decodes the envelope shape
// ({"plugins": [...], "skippedPacks": [...]}). On a non-200, skippedPacks is
// still decoded best-effort from the error body so a caller can assert on the
// disclosed refs without duplicating the decode logic.
func getInventory(t *testing.T, ts *testServer) (int, []inventoryEntry, []skippedPackEntry, string) {
	t.Helper()
	resp := ts.get("/api/v1/inventory")
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read inventory body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			SkippedPacks []skippedPackEntry `json:"skippedPacks"`
		}
		_ = json.Unmarshal(raw, &errBody)
		return resp.StatusCode, nil, errBody.SkippedPacks, string(raw)
	}
	var payload inventoryResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode inventory: %v (body %s)", err, raw)
	}
	return resp.StatusCode, payload.Plugins, payload.SkippedPacks, string(raw)
}

// ---------------------------------------------------------------------------
// 2.5 — payload and canonicalization
// ---------------------------------------------------------------------------

func TestInventory_SurfacesPluginMetadata(t *testing.T) {
	store := newTestStore(t)
	seedRichPack(t, store, "pack:v1", []oci.ResolvedPlugin{
		richPlugin("acme-widget", "1.2.0", func(p *oci.ResolvedPlugin) {
			p.DisplayName = "Acme Widget"
			p.Description = "does widget things"
			p.Tags = []string{"ui", "acme"}
			p.RequiredCore = "2.479.1"
			p.Dependencies = []hpi.Dependency{
				{Name: "mailer", Min: "1.0"},
				{Name: "chatty", Min: "2.0", Optional: true},
			}
		}),
	})

	ts := newTestServer(t, store)
	code, entries, _, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %s", len(entries), body)
	}
	e := entries[0]
	if e.DisplayName != "Acme Widget" || e.Description != "does widget things" || e.RequiredCore != "2.479.1" {
		t.Errorf("metadata not surfaced: %+v", e)
	}
	// Canonicalization sorts tags.
	if !reflect.DeepEqual(e.Tags, []string{"acme", "ui"}) {
		t.Errorf("tags = %v, want [acme ui]", e.Tags)
	}
	want := []pluginDep{
		{Name: "chatty", Min: "2.0", Optional: true},
		{Name: "mailer", Min: "1.0"},
	}
	if !reflect.DeepEqual(e.Dependencies, want) {
		t.Errorf("dependencies = %+v, want %+v", e.Dependencies, want)
	}
}

func TestInventory_PreAnnotationPluginStillListed(t *testing.T) {
	store := newTestStore(t)
	// seedPluginPack writes only name/version/sha256 — the pre-T1.1 shape.
	seedPluginPack(t, store, "pack:v1", []testPlugin{
		{name: "legacy", version: "0.9", hpiBytes: newPluginBytes("legacy", "0.9")},
	})

	ts := newTestServer(t, store)
	code, entries, _, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	if len(entries) != 1 || entries[0].Name != "legacy" {
		t.Fatalf("legacy plugin not listed: %s", body)
	}
	e := entries[0]
	if e.DisplayName != "" || e.Description != "" || e.RequiredCore != "" || e.Tags != nil || e.Dependencies != nil {
		t.Errorf("absent annotations should be omitted, got %+v", e)
	}
	// The manifest is readable and lists no dependencies, so the list is known.
	if !e.DependenciesKnown {
		t.Errorf("dependenciesKnown = false, want true for a readable manifest with no dependencies")
	}
	// omitempty must actually drop the keys, not emit nulls.
	for _, k := range []string{"displayName", "description", "tags", "requiredCore", "dependencies"} {
		if bytes.Contains([]byte(body), []byte(`"`+k+`"`)) {
			t.Errorf("body should omit %q: %s", k, body)
		}
	}
}

// mangleAnnotationStore rewrites one layer annotation on the way out of Pull,
// which is how a malformed structured annotation reaches the parser.
type mangleAnnotationStore struct {
	oci.BlobStore
	key   string
	value string
}

func (m *mangleAnnotationStore) Pull(ctx context.Context, ref string) (oci.Manifest, error) {
	man, err := m.BlobStore.Pull(ctx, ref)
	if err != nil {
		return man, err
	}
	for i := range man.Layers {
		if man.Layers[i].MediaType != oci.MediaTypePluginHPI {
			continue
		}
		if man.Layers[i].Annotations == nil {
			man.Layers[i].Annotations = map[string]string{}
		}
		man.Layers[i].Annotations[m.key] = m.value
	}
	return man, nil
}

// TestInventory_MalformedAnnotationFailsClosed records a deliberate departure
// from this change's frozen spec scenario ("a malformed annotation value SHALL
// be treated as absent rather than failing the request").
//
// T1.1 landed oci.ReadPluginPack treating a PRESENT but malformed structured
// annotation as an error, on the grounds that a corrupted dependency list must
// not read as "no dependencies". That is the stronger position: a silently
// empty closure under-pins an item, which is exactly the failure the solver
// exists to prevent, whereas a 503 merely freezes derivation with nothing
// pruned. So a malformed annotation makes the pack unreadable, which makes the
// store scan incomplete, which the inventory route fails closed on.
//
// pluginLayersFromManifest stays tolerant at the layer-descriptor level (it is
// how import counts plugins, where a hard failure would be wrong); the pack
// config read is what decides.
func TestInventory_MalformedAnnotationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"tags", oci.AnnPluginTags},
		{"dependencies", oci.AnnPluginDependencies},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newTestStore(t)
			seedRichPack(t, base, "pack:v1", []oci.ResolvedPlugin{
				richPlugin("acme-widget", "1.2.0", func(p *oci.ResolvedPlugin) {
					p.Tags = []string{"ui"}
					p.Dependencies = []hpi.Dependency{{Name: "mailer", Min: "1.0"}}
				}),
			})
			store := &mangleAnnotationStore{BlobStore: base, key: tc.key, value: "{not json"}

			ts := newTestServer(t, store)
			code, entries, skipped, body := getInventory(t, ts)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503 for a malformed %s annotation, got %d: %s", tc.name, code, body)
			}
			if len(entries) != 0 {
				t.Errorf("must not serve a listing derived from an unreadable pack: %s", body)
			}
			// The only pack in the store is the one that failed to read, so the
			// scan found nothing readable — the response must still name it. Its
			// error is allowed (expected) to mention the plugin name: that is
			// diagnostic detail, not a leaked listing.
			if len(skipped) != 1 || skipped[0].Ref != "pack:v1" {
				t.Fatalf("expected the unreadable pack disclosed by ref, got %+v", skipped)
			}
		})
	}
}

// TestPluginLayersFromManifest_MalformedAnnotationTolerated pins the layer-level
// reader's own contract: it never fails, it just drops what it cannot decode.
func TestPluginLayersFromManifest_MalformedAnnotationTolerated(t *testing.T) {
	m := oci.Manifest{Layers: []oci.Descriptor{{
		MediaType: oci.MediaTypePluginHPI,
		Digest:    "sha256:" + sha256Hex([]byte("x")),
		Annotations: map[string]string{
			oci.AnnPluginName:         "acme-widget",
			oci.AnnPluginVersion:      "1.2.0",
			oci.AnnPluginTags:         "{not json",
			oci.AnnPluginDependencies: "also not json",
			oci.AnnPluginRequiredCore: "2.479.1",
		},
	}}}
	got := pluginLayersFromManifest(m, testLogger())
	if len(got) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(got))
	}
	if got[0].Tags != nil || got[0].Dependencies != nil {
		t.Errorf("malformed structured annotations should be dropped, got %+v", got[0])
	}
	if got[0].RequiredCore != "2.479.1" || got[0].Name != "acme-widget" {
		t.Errorf("scalar annotations should survive, got %+v", got[0])
	}
}

func TestInventory_ConflictingEntriesBothReturned(t *testing.T) {
	store := newTestStore(t)
	// Same (name, version) in two packs, differing metadata — under the whole-
	// entry dedupe key both must surface so the operator can treat the plugin
	// as a store integrity failure rather than the service picking a winner.
	seedRichPack(t, store, "pack-a:v1", []oci.ResolvedPlugin{
		richPlugin("acme-widget", "1.2.0", func(p *oci.ResolvedPlugin) { p.RequiredCore = "2.479.1" }),
	})
	seedRichPack(t, store, "pack-b:v1", []oci.ResolvedPlugin{
		richPlugin("acme-widget", "1.2.0", func(p *oci.ResolvedPlugin) { p.RequiredCore = "2.555.1" }),
	})

	ts := newTestServer(t, store)
	code, entries, _, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	if len(entries) != 2 {
		t.Fatalf("expected both conflicting entries, got %d: %s", len(entries), body)
	}
	if entries[0].RequiredCore == entries[1].RequiredCore {
		t.Errorf("expected the entries to differ, got %+v", entries)
	}
}

func TestInventory_IdenticalEntriesCollapse(t *testing.T) {
	store := newTestStore(t)
	for _, ref := range []string{"pack-a:v1", "pack-b:v1"} {
		seedRichPack(t, store, ref, []oci.ResolvedPlugin{
			richPlugin("acme-widget", "1.2.0", func(p *oci.ResolvedPlugin) {
				p.RequiredCore = "2.479.1"
				p.Tags = []string{"ui"}
			}),
		})
	}
	ts := newTestServer(t, store)
	code, entries, _, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	if len(entries) != 1 {
		t.Fatalf("identical entries should collapse, got %d: %s", len(entries), body)
	}
}

// reverseListStore flips ListManifests order, standing in for the unordered
// enumeration both real backends actually perform.
type reverseListStore struct{ oci.BlobStore }

func (r *reverseListStore) ListManifests(ctx context.Context) ([]oci.Descriptor, error) {
	descs, err := r.BlobStore.ListManifests(ctx)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(descs)-1; i < j; i, j = i+1, j-1 {
		descs[i], descs[j] = descs[j], descs[i]
	}
	return descs, nil
}

func TestInventory_OrderingIsStableAcrossScans(t *testing.T) {
	base := newTestStore(t)
	seedRichPack(t, base, "pack-a:v1", []oci.ResolvedPlugin{
		richPlugin("zeta", "1.0", func(p *oci.ResolvedPlugin) { p.Tags = []string{"b", "a"} }),
		richPlugin("alpha", "2.0", nil),
	})
	seedRichPack(t, base, "pack-b:v1", []oci.ResolvedPlugin{
		// Same plugin, tags in the other order: canonicalization must make this
		// identical to the pack-a entry rather than a second, conflicting one.
		richPlugin("zeta", "1.0", func(p *oci.ResolvedPlugin) { p.Tags = []string{"a", "b"} }),
		richPlugin("mid", "1.5", nil),
	})

	forward := newTestServer(t, base)
	_, want, _, _ := getInventory(t, forward)

	reversed := newTestServer(t, &reverseListStore{BlobStore: base})
	_, got, _, body := getInventory(t, reversed)

	if !reflect.DeepEqual(want, got) {
		t.Fatalf("ordering not stable across scans:\n forward %+v\n reversed %+v", want, got)
	}
	if len(got) != 3 {
		t.Fatalf("list-order-only difference should collapse, got %d entries: %s", len(got), body)
	}
}

// ---------------------------------------------------------------------------
// 2.6 — partial scan and route isolation
// ---------------------------------------------------------------------------

// failPullStore fails Pull for one ref, standing in for a registry hiccup.
type failPullStore struct {
	oci.BlobStore
	failRef string
}

func (f *failPullStore) Pull(ctx context.Context, ref string) (oci.Manifest, error) {
	if ref == f.failRef {
		return oci.Manifest{}, errors.New("simulated registry hiccup")
	}
	return f.BlobStore.Pull(ctx, ref)
}

// TestInventory_PartialScanServesPartialAndDisclosesSkip is the graceful-
// degradation case: one unreadable plugin-pack manifest (e.g. a legacy pack
// predating the pack-kind field) must not take the whole inventory offline
// when at least one OTHER pack is readable. The readable
// subset is served with 200, and the unreadable pack is named — ref and
// error — in "skippedPacks" instead of being silently dropped.
func TestInventory_PartialScanServesPartialAndDisclosesSkip(t *testing.T) {
	base := newTestStore(t)
	seedRichPack(t, base, "pack-a:v1", []oci.ResolvedPlugin{richPlugin("readable", "1.0", nil)})
	seedRichPack(t, base, "pack-b:v1", []oci.ResolvedPlugin{richPlugin("unreadable", "1.0", nil)})
	store := &failPullStore{BlobStore: base, failRef: "pack-b:v1"}

	ts := newTestServer(t, store)
	code, entries, skipped, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("expected 200 with a partial inventory, got %d: %s", code, body)
	}
	if len(entries) != 1 || entries[0].Name != "readable" {
		t.Fatalf("expected the readable pack's plugin to be served, got %s", body)
	}
	if len(skipped) != 1 || skipped[0].Ref != "pack-b:v1" {
		t.Fatalf("expected pack-b:v1 disclosed as skipped, got %+v", skipped)
	}
	if !strings.Contains(skipped[0].Error, "simulated registry hiccup") {
		t.Errorf("skippedPacks should carry the read error, got %+v", skipped[0])
	}
}

// TestInventory_AllPacksUnreadableFailsClosed is the other half of the
// graceful-degradation contract: when the scan reads NOTHING, a 200 with an
// empty "plugins" array would be indistinguishable from a genuinely empty
// store and would license a pruning caller to delete everything. That case
// must still be a clear, closed error, with every offending ref disclosed
// rather than only a count.
func TestInventory_AllPacksUnreadableFailsClosed(t *testing.T) {
	base := newTestStore(t)
	seedRichPack(t, base, "pack-a:v1", []oci.ResolvedPlugin{richPlugin("unreadable-a", "1.0", nil)})
	seedRichPack(t, base, "pack-b:v1", []oci.ResolvedPlugin{richPlugin("unreadable-b", "1.0", nil)})
	store := &failAllPullStore{BlobStore: base}

	ts := newTestServer(t, store)
	code, entries, skipped, body := getInventory(t, ts)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when nothing is readable, got %d: %s", code, body)
	}
	if len(entries) != 0 {
		t.Errorf("expected no plugins in an all-broken response, got %+v", entries)
	}
	if len(skipped) != 2 {
		t.Fatalf("expected both unreadable packs disclosed, got %+v", skipped)
	}
	refs := map[string]bool{}
	for _, sp := range skipped {
		refs[sp.Ref] = true
	}
	if !refs["pack-a:v1"] || !refs["pack-b:v1"] {
		t.Errorf("expected both pack-a:v1 and pack-b:v1 named, got %+v", skipped)
	}
}

// failAllPullStore fails Pull for every ref, standing in for a store where
// nothing at all is readable.
type failAllPullStore struct{ oci.BlobStore }

func (f *failAllPullStore) Pull(ctx context.Context, ref string) (oci.Manifest, error) {
	return oci.Manifest{}, fmt.Errorf("simulated registry outage: %s", ref)
}

// extraArtifactStore appends a descriptor for an artifact whose type is not a
// plugin pack. Skipping it is not a scan failure — it is simply not a plugin
// pack, and it must not push the inventory into failing closed.
type extraArtifactStore struct {
	oci.BlobStore
	ref string
}

func (e *extraArtifactStore) ListManifests(ctx context.Context) ([]oci.Descriptor, error) {
	descs, err := e.BlobStore.ListManifests(ctx)
	if err != nil {
		return nil, err
	}
	return append(descs, oci.Descriptor{
		Digest:      "sha256:" + sha256Hex([]byte(e.ref)),
		Annotations: map[string]string{"org.opencontainers.image.ref.name": e.ref},
	}), nil
}

func (e *extraArtifactStore) Pull(ctx context.Context, ref string) (oci.Manifest, error) {
	if ref == e.ref {
		return oci.Manifest{ArtifactType: "application/vnd.example.other"}, nil
	}
	return e.BlobStore.Pull(ctx, ref)
}

func TestInventory_NonPluginArtifactIsNotAScanFailure(t *testing.T) {
	base := newTestStore(t)
	seedRichPack(t, base, "pack-a:v1", []oci.ResolvedPlugin{richPlugin("readable", "1.0", nil)})
	store := &extraArtifactStore{BlobStore: base, ref: "other:v1"}

	ts := newTestServer(t, store)
	code, entries, _, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("a non-plugin artifact must not fail the scan, got %d: %s", code, body)
	}
	if len(entries) != 1 || entries[0].Name != "readable" {
		t.Fatalf("expected only the plugin pack, got %s", body)
	}
}

func TestPartialStore_MetadataAndDownloadStillServe(t *testing.T) {
	base := newTestStore(t)
	body := newPluginBytes("readable", "1.0")
	seedRichPack(t, base, "pack-a:v1", []oci.ResolvedPlugin{richPlugin("readable", "1.0", nil)})
	seedRichPack(t, base, "pack-b:v1", []oci.ResolvedPlugin{richPlugin("unreadable", "1.0", nil)})
	store := &failPullStore{BlobStore: base, failRef: "pack-b:v1"}

	ts := newTestServer(t, store)

	// Inventory now serves the readable subset too, disclosing the gap …
	code, entries, skipped, b := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("inventory should be 200 with a partial listing, got %d: %s", code, b)
	}
	if len(entries) != 1 || entries[0].Name != "readable" {
		t.Fatalf("expected the readable plugin listed, got %s", b)
	}
	if len(skipped) != 1 || skipped[0].Ref != "pack-b:v1" {
		t.Fatalf("expected pack-b:v1 disclosed as skipped, got %+v", skipped)
	}

	// … while metadata keeps serving what it can read.
	resp := ts.get("/update-center.actual.json")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata should still serve, got %d", resp.StatusCode)
	}
	var uc updateCenterJSON
	if err := json.NewDecoder(resp.Body).Decode(&uc); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if _, ok := uc.Plugins["readable"]; !ok {
		t.Errorf("metadata should list the readable plugin, got %v", uc.Plugins)
	}

	// … and so does the download route.
	dl := ts.get("/download/plugins/readable/1.0/readable.hpi")
	defer func() { _ = dl.Body.Close() }()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download should still serve, got %d", dl.StatusCode)
	}
	got, _ := io.ReadAll(dl.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("download body mismatch")
	}
}

// seedInventoryLayer stores name@version as a profile pack with one plugin
// layer. known sets the dependency annotation to the given list, including an
// empty list. Unknown writes no annotation at all.
func seedInventoryLayer(t *testing.T, store oci.BlobStore, ref, name string, body []byte, known bool, deps []hpi.Dependency) {
	t.Helper()
	p := oci.ResolvedPlugin{
		Name:              name,
		Version:           "1.0",
		SHA256:            digestOf(body),
		Dependencies:      deps,
		DependenciesKnown: known,
		Content:           bytes.NewReader(body),
	}
	cfg := oci.PackConfig{
		Kind:        oci.PackKindProfile,
		Profile:     "test-profile",
		PluginCount: 1,
		LockHash:    oci.LockHash([]oci.ResolvedPlugin{p}),
		CreatedAt:   "2026-01-01T00:00:00Z",
	}
	if err := oci.BuildPluginPack(context.Background(), store, ref, cfg, []oci.ResolvedPlugin{p}); err != nil {
		t.Fatalf("seed inventory layer %s: %v", ref, err)
	}
}

// findInventory returns the entry for name@version, or fails the test.
func findInventory(t *testing.T, entries []inventoryEntry, name string) inventoryEntry {
	t.Helper()
	for _, e := range entries {
		if e.Name == name && e.Version == "1.0" {
			return e
		}
	}
	t.Fatalf("inventory has no %s@1.0: %+v", name, entries)
	return inventoryEntry{}
}

// TestInventory_KnownEmptyAndUnknownDiffer: a known-empty list and an unknown
// list must not look the same. The key is present in both, with its own value.
func TestInventory_KnownEmptyAndUnknownDiffer(t *testing.T) {
	store := newTestStore(t)
	seedInventoryLayer(t, store, "empty-pack", "empty",
		buildHPI(t, hpiSpec{shortName: "empty", version: "1.0"}), true, nil)
	seedInventoryLayer(t, store, "unknown-pack", "unknown", []byte("not-a-zip"), false, nil)

	ts := newTestServer(t, store)
	code, entries, _, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	if !findInventory(t, entries, "empty").DependenciesKnown {
		t.Errorf("known-empty entry: dependenciesKnown = false, want true")
	}
	if findInventory(t, entries, "unknown").DependenciesKnown {
		t.Errorf("unknown entry: dependenciesKnown = true, want false")
	}
	if strings.Count(body, `"dependenciesKnown":true`) != 1 || strings.Count(body, `"dependenciesKnown":false`) != 1 {
		t.Errorf("body must carry the key on both entries: %s", body)
	}
}

// TestInventory_MissingAnnotationResolvedFromManifest: a layer with no
// annotation takes its list from the readable stored manifest.
func TestInventory_MissingAnnotationResolvedFromManifest(t *testing.T) {
	store := newTestStore(t)
	seedInventoryLayer(t, store, "manifest-pack", "acme",
		buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "leaf:1.2"}), false, nil)

	ts := newTestServer(t, store)
	code, entries, _, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	e := findInventory(t, entries, "acme")
	if !e.DependenciesKnown {
		t.Fatalf("dependenciesKnown = false, want true from the stored manifest")
	}
	if len(e.Dependencies) != 1 || e.Dependencies[0].Name != "leaf" {
		t.Fatalf("dependencies = %+v, want one leaf entry", e.Dependencies)
	}
}

// TestInventory_AnnotatedDigestSkipsManifestRead: when one pack annotates a
// blob, another pack that stores the same bytes without an annotation needs no
// manifest read for that digest.
func TestInventory_AnnotatedDigestSkipsManifestRead(t *testing.T) {
	inner := newTestStore(t)
	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "leaf:1.2"})
	seedInventoryLayer(t, inner, "annotated-pack", "acme", body, true,
		[]hpi.Dependency{{Name: "leaf", Min: "1.2"}})
	seedInventoryLayer(t, inner, "bare-pack", "acme", body, false, nil)

	counting := newFetchCountingStore(inner)
	ts := newTestServer(t, counting)
	code, entries, _, raw := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, raw)
	}
	e := findInventory(t, entries, "acme")
	if !e.DependenciesKnown || len(e.Dependencies) != 1 {
		t.Fatalf("entry = %+v, want known with one dependency", e)
	}
	packs, _, err := ts.server.listPackInfos(context.Background())
	if err != nil {
		t.Fatalf("listPackInfos: %v", err)
	}
	for _, pk := range packs {
		for _, p := range pk.Plugins {
			if !p.DependenciesKnown {
				if n := counting.fetchesOf(p.LayerDigest); n != 0 {
					t.Fatalf("annotated digest read %d times, want 0", n)
				}
			}
		}
	}
}

// TestInventory_StalledManifestReadReportsUnknownWithinBudget: one stalled
// manifest read must not hold the whole inventory past the wait budget. The
// stalled plugin is reported as unknown, the other plugin as known, and the
// read that finishes later fills the cache for the next request.
func TestInventory_StalledManifestReadReportsUnknownWithinBudget(t *testing.T) {
	inner := newTestStore(t)
	stalled := buildHPI(t, hpiSpec{shortName: "stalled", version: "1.0", deps: "leaf:1.2"})
	seedInventoryLayer(t, inner, "stalled-pack", "stalled", stalled, false, nil)
	seedInventoryLayer(t, inner, "quick-pack", "quick",
		buildHPI(t, hpiSpec{shortName: "quick", version: "1.0", deps: "leaf:1.2"}), false, nil)

	store := newFetchCountingStore(inner)
	store.blockFor[digestOf(stalled)] = 1
	ts := newTestServer(t, store)
	ts.server.inventoryDepsBudget = 50 * time.Millisecond
	ts.server.manifestReadTimeout = 300 * time.Millisecond

	start := time.Now()
	code, entries, _, body := getInventory(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	if took := time.Since(start); took >= ts.server.manifestReadTimeout {
		t.Fatalf("inventory took %v, want less than the manifest read timeout", took)
	}
	if e := findInventory(t, entries, "stalled"); e.DependenciesKnown {
		t.Fatalf("stalled entry = %+v, want dependenciesKnown false at the budget", e)
	}
	if e := findInventory(t, entries, "quick"); !e.DependenciesKnown || len(e.Dependencies) != 1 {
		t.Fatalf("quick entry = %+v, want known with one dependency", e)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, entries, _, body = getInventory(t, ts)
		if code != http.StatusOK {
			t.Fatalf("status = %d, body %s", code, body)
		}
		if e := findInventory(t, entries, "stalled"); e.DependenciesKnown {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stalled entry never became known after the read timed out and was retried")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
