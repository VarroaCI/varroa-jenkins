package updatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// fetchCountingStore wraps a BlobStore. It counts FetchBlob calls per digest
// and can fail the next fetches of a digest.
type fetchCountingStore struct {
	oci.BlobStore
	mu      sync.Mutex
	counts  map[string]int
	failFor map[string]int
	// blockFor makes the next fetches of a digest block until the call's context
	// ends, then return its error.
	blockFor map[string]int
}

func newFetchCountingStore(inner oci.BlobStore) *fetchCountingStore {
	return &fetchCountingStore{
		BlobStore: inner,
		counts:    map[string]int{},
		failFor:   map[string]int{},
		blockFor:  map[string]int{},
	}
}

func (c *fetchCountingStore) FetchBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	c.mu.Lock()
	c.counts[digest]++
	if c.blockFor[digest] > 0 {
		c.blockFor[digest]--
		c.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c.failFor[digest] > 0 {
		c.failFor[digest]--
		c.mu.Unlock()
		return nil, errors.New("injected blob fetch failure")
	}
	c.mu.Unlock()
	return c.BlobStore.FetchBlob(ctx, digest)
}

func (c *fetchCountingStore) fetchesOf(digest string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[digest]
}

// servedDeps fetches update-center.actual.json and returns the served entry
// for name, and whether the entry is present.
func servedDeps(t *testing.T, ts *testServer, name string) (updateCenterPlugin, bool) {
	t.Helper()
	resp := ts.get("/update-center.actual.json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d, want 200", resp.StatusCode)
	}
	var uc updateCenterJSON
	if err := json.NewDecoder(resp.Body).Decode(&uc); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	p, ok := uc.Plugins[name]
	return p, ok
}

func TestMetadata_DependenciesFromAnnotation(t *testing.T) {
	store := newTestStore(t)
	body := newPluginBytes("app", "1.0")
	seedAddonPack(t, store, "pack:app", "app", "1.0", body, []hpi.Dependency{
		{Name: "structs", Min: "1.0", Optional: false},
		{Name: "cred", Min: "2.0", Optional: true},
	})

	ts := newTestServer(t, store)
	got, ok := servedDeps(t, ts, "app")
	if !ok {
		t.Fatal("app missing from metadata")
	}
	want := []updateCenterDependency{
		{Name: "structs", Optional: false, Version: "1.0"},
		{Name: "cred", Optional: true, Version: "2.0"},
	}
	if !reflect.DeepEqual(got.Dependencies, want) {
		t.Errorf("dependencies = %+v, want %+v", got.Dependencies, want)
	}
}

func TestMetadata_DependenciesFromManifestWhenAnnotationAbsent(t *testing.T) {
	store := newTestStore(t)
	body := buildHPI(t, hpiSpec{
		shortName: "manifest-only",
		version:   "1.0",
		deps:      "structs:1.0,cred:2.0;resolution:=optional",
	})
	// seedPluginPack writes no dependencies annotation.
	seedPluginPack(t, store, "pack:manifest-only", []testPlugin{
		{name: "manifest-only", version: "1.0", hpiBytes: body},
	})

	ts := newTestServer(t, store)
	got, ok := servedDeps(t, ts, "manifest-only")
	if !ok {
		t.Fatal("manifest-only missing from metadata")
	}
	want := []updateCenterDependency{
		{Name: "structs", Optional: false, Version: "1.0"},
		{Name: "cred", Optional: true, Version: "2.0"},
	}
	if !reflect.DeepEqual(got.Dependencies, want) {
		t.Errorf("dependencies = %+v, want %+v", got.Dependencies, want)
	}
}

func TestMetadata_UnknownDependenciesOmitsEntry(t *testing.T) {
	store := newTestStore(t)
	seedPluginPack(t, store, "pack:opaque", []testPlugin{
		{name: "opaque", version: "1.0", hpiBytes: []byte("not-a-zip")},
	})

	ts := newTestServer(t, store)
	if _, ok := servedDeps(t, ts, "opaque"); ok {
		t.Error("plugin with unknown dependencies must be left out of the metadata")
	}

	dl := ts.get("/download/plugins/opaque/1.0/opaque.hpi")
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Errorf("explicit-version download status = %d, want 200", dl.StatusCode)
	}
}

func TestMetadata_DependencyManifestReadIsCached(t *testing.T) {
	inner := newTestStore(t)
	body := newPluginBytes("cached", "1.0")
	seedPluginPack(t, inner, "pack:cached", []testPlugin{
		{name: "cached", version: "1.0", hpiBytes: body},
	})
	store := newFetchCountingStore(inner)
	digest := digestOf(body)

	ts := newTestServer(t, store)
	for i := 0; i < 2; i++ {
		if _, ok := servedDeps(t, ts, "cached"); !ok {
			t.Fatalf("pass %d: cached missing from metadata", i)
		}
	}
	if n := store.fetchesOf(digest); n != 1 {
		t.Errorf("manifest blob fetched %d times over two metadata reads, want 1", n)
	}
}

func TestMetadata_FetchErrorIsNotCached(t *testing.T) {
	inner := newTestStore(t)
	body := newPluginBytes("flaky", "1.0")
	seedPluginPack(t, inner, "pack:flaky", []testPlugin{
		{name: "flaky", version: "1.0", hpiBytes: body},
	})
	store := newFetchCountingStore(inner)
	store.failFor[digestOf(body)] = 1

	ts := newTestServer(t, store)
	if _, ok := servedDeps(t, ts, "flaky"); ok {
		t.Error("first metadata read must omit a plugin whose manifest fetch failed")
	}
	if _, ok := servedDeps(t, ts, "flaky"); !ok {
		t.Error("second metadata read must list the plugin after the fetch recovers")
	}
}

func TestMetadata_RequiredCoreKeyAlwaysPresent(t *testing.T) {
	store := newTestStore(t)
	// The pack carries no requiredCore, so the served value is empty.
	seedAddonPack(t, store, "pack:nocore", "nocore", "1.0", newPluginBytes("nocore", "1.0"), nil)

	ts := newTestServer(t, store)
	resp := ts.get("/update-center.actual.json")
	defer resp.Body.Close()
	var doc struct {
		Plugins map[string]map[string]any `json:"plugins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	entry, ok := doc.Plugins["nocore"]
	if !ok {
		t.Fatal("nocore missing from metadata")
	}
	val, present := entry["requiredCore"]
	if !present {
		t.Fatal("requiredCore key must be present even when the pack has no value")
	}
	if val != "" {
		t.Errorf("requiredCore = %v, want empty string", val)
	}
}

func TestMetadata_StalledManifestReadTimesOutAndIsNotCached(t *testing.T) {
	inner := newTestStore(t)
	body := newPluginBytes("stalled", "1.0")
	seedPluginPack(t, inner, "pack:stalled", []testPlugin{
		{name: "stalled", version: "1.0", hpiBytes: body},
	})
	store := newFetchCountingStore(inner)
	store.blockFor[digestOf(body)] = 1

	ts := newTestServer(t, store)
	ts.server.manifestReadTimeout = 50 * time.Millisecond
	if _, ok := servedDeps(t, ts, "stalled"); ok {
		t.Error("a timed-out manifest read must omit the plugin")
	}
	if _, ok := servedDeps(t, ts, "stalled"); !ok {
		t.Error("the timeout must not be cached: the next read must list the plugin")
	}
}

func TestLatest_UnknownDependenciesIsNotAdvertised(t *testing.T) {
	store := newTestStore(t)
	seedPluginPack(t, store, "pack:opaque-latest", []testPlugin{
		{name: "opaque-latest", version: "1.0", hpiBytes: []byte("not-a-zip")},
	})

	ts := newTestServer(t, store)
	if _, ok := servedDeps(t, ts, "opaque-latest"); ok {
		t.Fatal("precondition: plugin with unknown dependencies must be omitted from metadata")
	}
	latest := ts.get("/download/plugins/opaque-latest/latest/opaque-latest.hpi")
	defer latest.Body.Close()
	if latest.StatusCode != http.StatusNotFound {
		t.Errorf("latest status = %d, want 404 for a plugin the metadata does not advertise", latest.StatusCode)
	}
	pinned := ts.get("/download/plugins/opaque-latest/1.0/opaque-latest.hpi")
	defer pinned.Body.Close()
	if pinned.StatusCode != http.StatusOK {
		t.Errorf("explicit-version status = %d, want 200", pinned.StatusCode)
	}
}
