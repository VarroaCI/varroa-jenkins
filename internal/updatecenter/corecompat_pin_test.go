package updatecenter

import "testing"

// A declared exact pin is served even when its requiredCore is above the requesting
// core. The core filter must not replace it with a lower stored version.
func TestMetadata_DeclaredPinNotReplacedByCoreFilter(t *testing.T) {
	store := newTestStore(t)
	seedCorePack(t, store, "pack:foo-19", "1.9", "2.570")
	seedCorePack(t, store, "pack:foo-15", "1.5", "2.555.3")
	ts := newTestServer(t, store, WithDeclaredPluginsFile(declaredFile(t, "foo@1.9")))

	if got := getMetadataPlugins(t, ts, "/update-center.actual.json?version=2.555.3")["foo"].Version; got != "1.9" {
		t.Errorf("?version=2.555.3 served %q, want the declared pin 1.9", got)
	}
}

// A stored winner withheld for an unknown dependency list is not replaced from
// upstream, because the stored bytes would not match an upstream digest.
func TestMetadata_WithheldStoredWinnerNotFilledFromUpstream(t *testing.T) {
	store := newTestStore(t)
	seedAddonPack(t, store, "pack:foo-garbage", "foo", "2.0", []byte("not a plugin archive"), nil)
	lts := []upstreamEntry{{name: "foo", version: "1.9", requiredCore: "2.555.3", hpi: newCorePluginBytes(t, "1.9", "2.555.3")}}
	upstream := coreUpstream(t, nil, lts)
	ts := newTestServer(t, store,
		append(pullThroughWithResolver(upstream.URL, upstream.URL+"/dynamic-stable-2.555.3"), fooDeclaredLatest(t))...)

	if _, ok := getMetadataPlugins(t, ts, "/update-center.actual.json?version=2.555.3")["foo"]; ok {
		t.Error("withheld stored foo was replaced by an upstream entry")
	}
}
