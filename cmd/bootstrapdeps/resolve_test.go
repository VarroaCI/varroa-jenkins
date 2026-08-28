package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureHPI builds a genuine .hpi holding just a manifest.
func fixtureHPI(t *testing.T, shortName, version, deps string) []byte {
	t.Helper()
	mf := "Manifest-Version: 1.0\r\nShort-Name: " + shortName + "\r\nPlugin-Version: " + version + "\r\n"
	if deps != "" {
		mf += "Plugin-Dependencies: " + deps + "\r\n"
	}
	mf += "\r\n"

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatalf("create manifest entry: %v", err)
	}
	if _, err := w.Write([]byte(mf)); err != nil {
		t.Fatalf("write manifest entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// fakeFetcher serves fixture HPIs keyed by "name@version" and records what it
// was asked for, so a test can assert each HPI is fetched at most once.
type fakeFetcher struct {
	bodies map[string][]byte
	calls  []string
}

func (f *fakeFetcher) fetch(name, version string) ([]byte, error) {
	key := name + "@" + version
	f.calls = append(f.calls, key)
	b, ok := f.bodies[key]
	if !ok {
		return nil, fmt.Errorf("no fixture for %s", key)
	}
	return b, nil
}

func TestNormalizeRootVersion(t *testing.T) {
	// A snapshot build stamps the builder identity into Plugin-Version; it must
	// never reach a committed lock.
	got := normalizeRootVersion("1.0-SNAPSHOT (private-07/18/2026 00:33-root)")
	if got != "1.0-SNAPSHOT" {
		t.Errorf("normalizeRootVersion = %q, want %q", got, "1.0-SNAPSHOT")
	}
	if got := normalizeRootVersion("1.2.3"); got != "1.2.3" {
		t.Errorf("normalizeRootVersion = %q", got)
	}
}

func TestResolveClosure_RootExemptButRecorded(t *testing.T) {
	root := fixtureHPI(t, "varroa-mite-auth", "1.0-SNAPSHOT (private-07/18/2026 00:33-root)",
		"mailer:534.v1,configuration-as-code:2082.v1;resolution:=optional")
	ff := &fakeFetcher{bodies: map[string][]byte{
		"mailer@534.v9":            fixtureHPI(t, "mailer", "534.v9", "jakarta-mail-api:2.1.3-2"),
		"jakarta-mail-api@2.1.5-1": fixtureHPI(t, "jakarta-mail-api", "2.1.5-1", ""),
	}}
	resolved := map[string]string{
		"mailer":           "534.v9",
		"jakarta-mail-api": "2.1.5-1",
	}

	entries, err := resolveClosure(root, resolved, ff.fetch)
	if err != nil {
		t.Fatalf("resolveClosure: %v", err)
	}

	// The root is absent from the resolved set — that is the exemption — and is
	// still recorded first, with its version normalized and no mins.
	if entries[0].ArtifactID != "varroa-mite-auth" {
		t.Fatalf("first entry must be the root, got %+v", entries[0])
	}
	if entries[0].Version != "1.0-SNAPSHOT" {
		t.Errorf("root version = %q, want the normalized form", entries[0].Version)
	}
	if len(entries[0].Mins) != 0 {
		t.Errorf("root must carry no mins, got %v", entries[0].Mins)
	}

	// configuration-as-code is optional: neither required nor traversed.
	for _, e := range entries {
		if e.ArtifactID == "configuration-as-code" {
			t.Error("an optional dependency must not enter the closure")
		}
	}
	for _, c := range ff.calls {
		if strings.HasPrefix(c, "configuration-as-code@") {
			t.Error("an optional dependency must not be fetched")
		}
	}

	// Members follow in BFS order, pinned from the resolved set, with the
	// declared minimum recorded verbatim beside a pin that has moved ahead.
	want := []bootstrapEntry{
		{ArtifactID: "mailer", Version: "534.v9", Mins: []string{"534.v1"}},
		{ArtifactID: "jakarta-mail-api", Version: "2.1.5-1", Mins: []string{"2.1.3-2"}},
	}
	if len(entries) != len(want)+1 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	for i, w := range want {
		g := entries[i+1]
		if g.ArtifactID != w.ArtifactID || g.Version != w.Version {
			t.Errorf("entry %d = %+v, want %+v", i+1, g, w)
		}
		if len(g.Mins) != 1 || g.Mins[0] != w.Mins[0] {
			t.Errorf("entry %d mins = %v, want %v", i+1, g.Mins, w.Mins)
		}
	}
}

func TestResolveClosure_MissingMandatoryDepFailsWithChain(t *testing.T) {
	root := fixtureHPI(t, "varroa-mite-auth", "1.0-SNAPSHOT", "mailer:534.v1")
	ff := &fakeFetcher{bodies: map[string][]byte{
		"mailer@534.v9": fixtureHPI(t, "mailer", "534.v9", "jakarta-mail-api:2.1.3-2"),
	}}
	// jakarta-mail-api is absent from the resolved set.
	resolved := map[string]string{"mailer": "534.v9"}

	_, err := resolveClosure(root, resolved, ff.fetch)
	if err == nil {
		t.Fatal("expected a failure for a missing mandatory dependency")
	}
	// The operator must see WHY a seemingly unrelated plugin is required.
	if !strings.Contains(err.Error(), "varroa-mite-auth → mailer → jakarta-mail-api") {
		t.Errorf("error must print the full chain from the root, got: %v", err)
	}
}

func TestResolveClosure_DiamondFetchesEachHPIOnce(t *testing.T) {
	// root → a, b ; a → shared ; b → shared, with differing declared minimums.
	root := fixtureHPI(t, "varroa-mite-auth", "1.0", "a:1.0,b:1.0")
	ff := &fakeFetcher{bodies: map[string][]byte{
		"a@1.5":      fixtureHPI(t, "a", "1.5", "shared:3.0"),
		"b@1.5":      fixtureHPI(t, "b", "1.5", "shared:2.0"),
		"shared@4.0": fixtureHPI(t, "shared", "4.0", ""),
	}}
	resolved := map[string]string{"a": "1.5", "b": "1.5", "shared": "4.0"}

	entries, err := resolveClosure(root, resolved, ff.fetch)
	if err != nil {
		t.Fatalf("resolveClosure: %v", err)
	}
	fetched := 0
	for _, c := range ff.calls {
		if strings.HasPrefix(c, "shared@") {
			fetched++
		}
	}
	if fetched != 1 {
		t.Errorf("shared fetched %d times, want 1", fetched)
	}

	var shared *bootstrapEntry
	for i := range entries {
		if entries[i].ArtifactID == "shared" {
			shared = &entries[i]
		}
	}
	if shared == nil {
		t.Fatalf("shared missing from closure: %+v", entries)
	}
	// Both declared minimums are recorded, de-duplicated and sorted — NOT
	// reduced to a single greatest value, which would require comparing them.
	if len(shared.Mins) != 2 || shared.Mins[0] != "2.0" || shared.Mins[1] != "3.0" {
		t.Errorf("shared.Mins = %v, want [2.0 3.0]", shared.Mins)
	}
}

func TestResolveClosure_DuplicateMinimumsDeduplicate(t *testing.T) {
	root := fixtureHPI(t, "varroa-mite-auth", "1.0", "a:1.0,b:1.0")
	ff := &fakeFetcher{bodies: map[string][]byte{
		"a@2.0":      fixtureHPI(t, "a", "2.0", "shared:3.0"),
		"b@2.0":      fixtureHPI(t, "b", "2.0", "shared:3.0"),
		"shared@4.0": fixtureHPI(t, "shared", "4.0", ""),
	}}
	entries, err := resolveClosure(root, map[string]string{"a": "2.0", "b": "2.0", "shared": "4.0"}, ff.fetch)
	if err != nil {
		t.Fatalf("resolveClosure: %v", err)
	}
	for _, e := range entries {
		if e.ArtifactID == "shared" && len(e.Mins) != 1 {
			t.Errorf("identical minimums must de-duplicate, got %v", e.Mins)
		}
	}
}

func TestWriteBootstrapYAML(t *testing.T) {
	var buf bytes.Buffer
	err := writeBootstrapYAML(&buf, []bootstrapEntry{
		{ArtifactID: "varroa-mite-auth", Version: "1.0-SNAPSHOT"},
		{ArtifactID: "mailer", Version: "534.v9", Mins: []string{"534.v1"}},
	}, 4)
	if err != nil {
		t.Fatalf("writeBootstrapYAML: %v", err)
	}
	want := "    bootstrap:\n" +
		"      - artifactId: varroa-mite-auth\n" +
		"        version: \"1.0-SNAPSHOT\"\n" +
		"      - artifactId: mailer\n" +
		"        version: \"534.v9\"\n" +
		"        mins:\n" +
		"          - \"534.v1\"\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestRunResolve_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	hpiPath := filepath.Join(dir, "root.hpi")
	if err := os.WriteFile(hpiPath, fixtureHPI(t, "varroa-mite-auth", "1.0-SNAPSHOT (private-x)", "mailer:534.v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	pluginsPath := filepath.Join(dir, "plugins.txt")
	if err := os.WriteFile(pluginsPath, []byte("# comment\nmailer:534.v9\nother:1.0\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ff := &fakeFetcher{bodies: map[string][]byte{
		"mailer@534.v9": fixtureHPI(t, "mailer", "534.v9", ""),
	}}

	var buf bytes.Buffer
	err := runResolve(resolveOptions{
		HPIPath: hpiPath, PluginsPath: pluginsPath, Indent: 4, Fetch: ff.fetch,
	}, &buf)
	if err != nil {
		t.Fatalf("runResolve: %v", err)
	}
	if !strings.Contains(buf.String(), "artifactId: varroa-mite-auth") ||
		!strings.Contains(buf.String(), "version: \"1.0-SNAPSHOT\"") ||
		!strings.Contains(buf.String(), "artifactId: mailer") {
		t.Errorf("unexpected output:\n%s", buf.String())
	}
}
