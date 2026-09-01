package pluginresolve

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func (f *fakeFetcher) fetch(_ context.Context, name, version string) ([]byte, error) {
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

	entries, err := ResolveClosure(context.Background(), root, resolved, ff.fetch)
	if err != nil {
		t.Fatalf("ResolveClosure: %v", err)
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
	want := []BootstrapEntry{
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

	_, err := ResolveClosure(context.Background(), root, resolved, ff.fetch)
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

	entries, err := ResolveClosure(context.Background(), root, resolved, ff.fetch)
	if err != nil {
		t.Fatalf("ResolveClosure: %v", err)
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

	var shared *BootstrapEntry
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
	entries, err := ResolveClosure(context.Background(), root, map[string]string{"a": "2.0", "b": "2.0", "shared": "4.0"}, ff.fetch)
	if err != nil {
		t.Fatalf("ResolveClosure: %v", err)
	}
	for _, e := range entries {
		if e.ArtifactID == "shared" && len(e.Mins) != 1 {
			t.Errorf("identical minimums must de-duplicate, got %v", e.Mins)
		}
	}
}

func TestAssertBootstrapClosure_Satisfied(t *testing.T) {
	root := fixtureHPI(t, "varroa-mite-auth", "1.0", "mailer:534.v1")
	ff := &fakeFetcher{bodies: map[string][]byte{
		"mailer@534.v9": fixtureHPI(t, "mailer", "534.v9", ""),
	}}
	closure := Closure{Plugins: []PluginPin{{ArtifactID: "mailer", Version: "534.v9"}}}

	if err := AssertBootstrapClosure(context.Background(), root, closure, ff.fetch); err != nil {
		t.Fatalf("AssertBootstrapClosure: %v", err)
	}
}

func TestAssertBootstrapClosure_MissingMemberFails(t *testing.T) {
	root := fixtureHPI(t, "varroa-mite-auth", "1.0", "mailer:534.v1")
	ff := &fakeFetcher{}
	closure := Closure{} // mailer absent

	err := AssertBootstrapClosure(context.Background(), root, closure, ff.fetch)
	if err == nil {
		t.Fatal("expected a failure for a bootstrap member absent from closure")
	}
	if !strings.Contains(err.Error(), "mailer") {
		t.Errorf("error must name the missing member, got: %v", err)
	}
}

// blockingHPIServer starts an httptest server whose single handler blocks
// until the test releases it. srv.Close waits for that handler goroutine to
// return, so the release must happen first on every exit path: t.Cleanup runs
// LIFO, so registering the release after srv.Close makes it run before
// srv.Close and guarantees the handler goroutine is never left parked.
func blockingHPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block // held open until the test releases it below
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })
	return srv
}

// TestHTTPFetcherWithClient_ClientTimeoutBoundsStalledDownload proves the
// client-level backstop alone — with no ctx deadline, exactly like the
// reconciler's long-lived, never-cancelled ctx — still aborts a stalled
// download. It injects a short client timeout rather than waiting out the
// real hpiDownloadTimeout.
func TestHTTPFetcherWithClient_ClientTimeoutBoundsStalledDownload(t *testing.T) {
	srv := blockingHPIServer(t)
	fetch := httpFetcherWithClient(srv.URL, &http.Client{Timeout: 50 * time.Millisecond})

	start := time.Now()
	_, err := fetch(context.Background(), "some-plugin", "1.0")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a stalled endpoint, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("fetch took %s to return; expected it to abort near the client's 50ms timeout, not hang", elapsed)
	}
}

// TestHTTPFetcher_ContextCancellationAbortsStalledDownload exercises the real,
// exported HTTPFetcher (production's default client, hpiDownloadTimeout) and
// proves a cancelled/deadlined ctx aborts a stalled download well before that
// timeout, confirming the request is genuinely ctx-aware end to end.
func TestHTTPFetcher_ContextCancellationAbortsStalledDownload(t *testing.T) {
	srv := blockingHPIServer(t)
	fetch := HTTPFetcher(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := fetch(ctx, "some-plugin", "1.0")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a stalled endpoint, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("fetch took %s to return; expected ctx cancellation to abort it near 50ms, not hang", elapsed)
	}
}
