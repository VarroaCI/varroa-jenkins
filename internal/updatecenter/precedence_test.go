package updatecenter

import (
	"encoding/json"
	"net/http"
	"testing"
)

// servedVersion reads the served update-center metadata and returns the version
// selected for name.
func servedVersion(t *testing.T, ts *testServer, name string) string {
	t.Helper()
	resp := ts.get("/update-center.actual.json")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d", resp.StatusCode)
	}
	var doc updateCenterJSON
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	return doc.Plugins[name].Version
}

// TestPrecedence_UploadDoesNotShadowDeclaredVersion is the case that matters: a
// pin is present in the store exactly when coverage is complete, and an upload
// must not displace it.
func TestPrecedence_UploadDoesNotShadowDeclaredVersion(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t, "mailer@472")...)

	declaredBody := buildHPI(t, hpiSpec{shortName: "mailer", version: "472"})
	newerBody := buildHPI(t, hpiSpec{shortName: "mailer", version: "999"})
	seedAddonPack(t, store, "declared-pack", "mailer", "472", declaredBody, nil)
	seedAddonPack(t, store, "upload-pack", "mailer", "999", newerBody, nil)

	if got := servedVersion(t, ts, "mailer"); got != "472" {
		t.Fatalf("served version = %q, want the declared 472", got)
	}
	// The superseded version stays downloadable at its explicit URL.
	resp := ts.get("/download/plugins/mailer/999/mailer.hpi")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download of the non-served version = %d, want 200", resp.StatusCode)
	}
}

// TestPrecedence_NewerUndeclaredUploadWins: with nothing declared, the highest
// version by pluginver is served and the older stays downloadable.
func TestPrecedence_NewerUndeclaredUploadWins(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t)...)

	seedAddonPack(t, store, "old", "acme", "1.9", buildHPI(t, hpiSpec{shortName: "acme", version: "1.9"}), nil)
	seedAddonPack(t, store, "new", "acme", "1.10", buildHPI(t, hpiSpec{shortName: "acme", version: "1.10"}), nil)

	// 1.10 > 1.9 numerically; the old first-wins dedupe and a lexical comparator
	// would both pick 1.9.
	if got := servedVersion(t, ts, "acme"); got != "1.10" {
		t.Fatalf("served version = %q, want 1.10", got)
	}
	resp := ts.get("/download/plugins/acme/1.9/acme.hpi")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download of the superseded version = %d, want 200", resp.StatusCode)
	}
}

// TestPrecedence_DeclaredButNotStoredFallsThrough: rule 1 selecting nothing must
// fall through to rule 2 rather than serving nothing.
func TestPrecedence_DeclaredButNotStoredFallsThrough(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t, "acme@5.0")...)

	seedAddonPack(t, store, "old", "acme", "1.9", buildHPI(t, hpiSpec{shortName: "acme", version: "1.9"}), nil)
	seedAddonPack(t, store, "new", "acme", "2.0", buildHPI(t, hpiSpec{shortName: "acme", version: "2.0"}), nil)

	if got := servedVersion(t, ts, "acme"); got != "2.0" {
		t.Fatalf("served version = %q, want 2.0 (rule 1 selects nothing, rule 2 decides)", got)
	}
}

// TestSelectServedVersion_Deterministic covers the properties a live store
// cannot: order-independence across both ListManifests orders (index.json
// insertion order locally, tag order on a registry), and the equal-version
// tie-break.
func TestSelectServedVersion_Deterministic(t *testing.T) {
	a := pluginLayerInfo{Name: "x", Version: "1.0", SHA256: "sha256:aaa"}
	b := pluginLayerInfo{Name: "x", Version: "1.0", SHA256: "sha256:bbb"}
	c := pluginLayerInfo{Name: "x", Version: "2.0", SHA256: "sha256:ccc"}

	t.Run("equal version tie-break goes to the lowest sha256", func(t *testing.T) {
		for _, order := range [][]pluginLayerInfo{{a, b}, {b, a}} {
			got, ok := selectServedVersion("x", order, DeclaredSet{})
			if !ok || got.SHA256 != "sha256:aaa" {
				t.Fatalf("selected %+v for order %+v", got, order)
			}
		}
	})

	t.Run("highest version wins regardless of order", func(t *testing.T) {
		for _, order := range [][]pluginLayerInfo{{a, b, c}, {c, b, a}, {b, c, a}} {
			got, ok := selectServedVersion("x", order, DeclaredSet{})
			if !ok || got.Version != "2.0" {
				t.Fatalf("selected %+v for order %+v", got, order)
			}
		}
	})

	t.Run("declared eligibility wins regardless of order", func(t *testing.T) {
		declared := DeclaredSet{"x": {"1.0"}}
		for _, order := range [][]pluginLayerInfo{{a, c}, {c, a}} {
			got, ok := selectServedVersion("x", order, declared)
			if !ok || got.Version != "1.0" {
				t.Fatalf("selected %+v for order %+v", got, order)
			}
		}
	})

	t.Run("multi-version declaration picks the highest declared", func(t *testing.T) {
		declared := DeclaredSet{"x": {"1.0", "2.0"}}
		got, ok := selectServedVersion("x", []pluginLayerInfo{a, c}, declared)
		if !ok || got.Version != "2.0" {
			t.Fatalf("selected %+v", got)
		}
	})

	t.Run("empty candidate set selects nothing", func(t *testing.T) {
		if _, ok := selectServedVersion("x", nil, DeclaredSet{}); ok {
			t.Fatal("expected no selection")
		}
	})
}

func TestReadDeclaredPlugins(t *testing.T) {
	t.Run("absent file is not an empty set", func(t *testing.T) {
		set, ok := ReadDeclaredPlugins("/nonexistent/declared-plugins")
		if ok {
			t.Fatal("ok = true for a missing file")
		}
		if len(set) != 0 {
			t.Fatalf("set = %+v", set)
		}
	})
	t.Run("unset path is not an empty set", func(t *testing.T) {
		if _, ok := ReadDeclaredPlugins(""); ok {
			t.Fatal("ok = true for an unset path")
		}
	})
	t.Run("readable and empty", func(t *testing.T) {
		set, ok := ReadDeclaredPlugins(declaredFile(t))
		if !ok {
			t.Fatal("ok = false for a readable empty file")
		}
		if len(set) != 0 {
			t.Fatalf("set = %+v", set)
		}
	})
	t.Run("parses and dedupes", func(t *testing.T) {
		set, ok := ReadDeclaredPlugins(declaredFile(t, "mailer@472", "", "mailer@470", "mailer@472", "bad-line", "x@"))
		if !ok {
			t.Fatal("ok = false")
		}
		if len(set["mailer"]) != 2 {
			t.Fatalf("mailer = %+v, want two distinct versions", set["mailer"])
		}
		if v, _ := set.Highest("mailer"); v != "472" {
			t.Fatalf("highest = %q, want 472", v)
		}
		if set.Declared("bad-line") || set.Declared("x") {
			t.Fatalf("malformed lines must be skipped: %+v", set)
		}
	})
}
