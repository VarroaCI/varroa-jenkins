package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// --- test doubles ----------------------------------------------------------

// recordingSink is a minimal activity.EventSink test double that records
// every published event.
type recordingSink struct {
	mu     sync.Mutex
	events []activity.Event
}

func (s *recordingSink) Publish(e activity.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recordingSink) ofType(typ string) []activity.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []activity.Event
	for _, e := range s.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// doerFunc adapts a function to UpgradeTrackerReconciler's httpDoer interface.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// rewriteToServer returns a doerFunc that redirects every request's
// scheme/host to ts (an httptest.Server), preserving the path — so
// UpgradeTrackerReconciler's hardcoded https://updates.jenkins.io URLs land
// on the local test server instead of the real internet.
func rewriteToServer(t *testing.T, ts *httptest.Server) doerFunc {
	t.Helper()
	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return func(req *http.Request) (*http.Response, error) {
		u := *req.URL
		u.Scheme = base.Scheme
		u.Host = base.Host
		req.URL = &u
		req.Host = ""
		return ts.Client().Do(req)
	}
}

// upstreamFixture is a local stand-in for the upstream stable/ documents and
// tiers.json. It records every request path, so a test can assert that no
// dynamic-stable path was read.
type upstreamFixture struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

// newUpstreamFixture serves stable/update-center.actual.json with stableCore
// (500 when empty), tiers.json with stableCores (500 when nil), and
// stable/latestCore.txt with latestCore (404 when empty). Every other path,
// including every dynamic-stable path, answers 404.
func newUpstreamFixture(stableCore string, stableCores []string, latestCore string) *upstreamFixture {
	f := &upstreamFixture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/stable/update-center.actual.json", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if stableCore == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"core":{"version":%q}}`, stableCore)
	})
	mux.HandleFunc("/tiers.json", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if stableCores == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, _ := json.Marshal(map[string][]string{"stableCores": stableCores})
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/stable/latestCore.txt", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if latestCore == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(latestCore))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.WriteHeader(http.StatusNotFound)
	})
	f.Server = httptest.NewServer(mux)
	return f
}

func (f *upstreamFixture) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
}

// requested returns every path the fixture served.
func (f *upstreamFixture) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// paddedStableServer serves /stable/update-center.actual.json as a body of
// more than 1 MiB, with core.version placed after a large padding field, the
// shape of the real full update-center listing.
func paddedStableServer(stableCore string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/stable/update-center.actual.json", func(w http.ResponseWriter, _ *http.Request) {
		pad := strings.Repeat("x", 3<<19) // 1.5 MiB
		fmt.Fprintf(w, `{"plugins":{"pad":%q},"core":{"version":%q}}`, pad, stableCore)
	})
	return httptest.NewServer(mux)
}

func testProfile(name, version, resolveVersion string) *v1alpha1.JenkinsVersionProfile {
	return &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version:        version,
			Channel:        "lts",
			ResolveVersion: resolveVersion,
		},
	}
}

// --- tests -------------------------------------------------------------

// TestUpgradeTrackerNewLineDedup verifies that a newer LTS line than every
// tracked profile's line fires exactly one upgrade.upstream.observed event,
// never one per profile, and never a second time on a later tick once the
// fact has been observed.
func TestUpgradeTrackerNewLineDedup(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		testProfile("profile-2-479", "2.479", "2.479.10"),
		testProfile("profile-2-555", "2.555", "2.555.3"),
	)
	fx := newUpstreamFixture("2.999.1", []string{"2.999.1"}, "")
	defer fx.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())
	tracker.Reconcile(context.Background())

	events := sink.ofType("upgrade.upstream.observed")
	if len(events) != 1 {
		t.Fatalf("upgrade.upstream.observed events = %d, want exactly 1 (deduplicated across profiles and ticks)", len(events))
	}
}

// TestUpgradeTrackerNoNewerPatchNoOp verifies that a profile already on the
// newest observed patch of its line gets no ProfileCandidate and no event.
func TestUpgradeTrackerNoNewerPatchNoOp(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	fx := newUpstreamFixture("2.555.3", []string{"2.555.1", "2.555.3"}, "")
	defer fx.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())

	candidates, err := crdstore.List[v1alpha1.ProfileCandidate](context.Background(), store, "", "")
	if err != nil {
		t.Fatalf("list ProfileCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %d, want 0 (no observed patch is newer than 2.555.3)", len(candidates))
	}
	if events := sink.ofType("upgrade.candidate.created"); len(events) != 0 {
		t.Fatalf("upgrade.candidate.created events = %d, want 0", len(events))
	}
}

// TestUpgradeTrackerDiscoversNonTierPatchFromStableCore verifies that a newer
// patch which is not a tier is still discovered from the stable core, with no
// dynamic-stable document read.
func TestUpgradeTrackerDiscoversNonTierPatchFromStableCore(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-580", "2.580", "2.580.1"))
	fx := newUpstreamFixture("2.580.2", []string{"2.568.3", "2.580.1"}, "")
	defer fx.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())

	name := candidateName("profile-2-580", "2.580.2")
	got, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, name, "")
	if err != nil {
		t.Fatalf("get candidate %s: %v", name, err)
	}
	if got.Spec.ObservedVersion != "2.580.1" {
		t.Errorf("ObservedVersion = %q, want 2.580.1", got.Spec.ObservedVersion)
	}
	if got.Spec.ResolveVersion != "2.580.2" {
		t.Errorf("ResolveVersion = %q, want 2.580.2", got.Spec.ResolveVersion)
	}
	if got.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Errorf("Phase = %q, want Pending", got.Status.Phase)
	}
	for _, p := range fx.requested() {
		if strings.Contains(p, "/dynamic-stable-") {
			t.Errorf("tracker requested %q; it must not probe dynamic-stable paths", p)
		}
	}
}

// TestUpgradeTrackerDiscoversTierPatchOnOlderLine verifies that a tier on an
// older line is discovered for a profile on that line, and that a stable core
// on a newer line does not affect it.
func TestUpgradeTrackerDiscoversTierPatchOnOlderLine(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-568", "2.568", "2.568.2"))
	fx := newUpstreamFixture("2.580.2", []string{"2.568.3", "2.580.1"}, "")
	defer fx.Close()

	tracker := NewUpgradeTrackerReconciler(store, &recordingSink{}, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())

	name := candidateName("profile-2-568", "2.568.3")
	got, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, name, "")
	if err != nil {
		t.Fatalf("get candidate %s: %v", name, err)
	}
	if got.Spec.ResolveVersion != "2.568.3" {
		t.Errorf("ResolveVersion = %q, want 2.568.3", got.Spec.ResolveVersion)
	}
	candidates, _ := crdstore.List[v1alpha1.ProfileCandidate](context.Background(), store, "", "")
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1 (the 2.580 line must not create one for 2.568)", len(candidates))
	}
}

// TestUpgradeTrackerPicksHighestObservedPatch verifies that when the tier list
// and the stable core both name newer patches, the candidate takes the highest.
func TestUpgradeTrackerPicksHighestObservedPatch(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	fx := newUpstreamFixture("2.555.5", []string{"2.555.4"}, "")
	defer fx.Close()

	tracker := NewUpgradeTrackerReconciler(store, &recordingSink{}, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())

	name := candidateName("profile-2-555", "2.555.5")
	got, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, name, "")
	if err != nil {
		t.Fatalf("get candidate %s: %v", name, err)
	}
	if got.Spec.ResolveVersion != "2.555.5" {
		t.Errorf("ResolveVersion = %q, want 2.555.5", got.Spec.ResolveVersion)
	}
}

// TestUpgradeTrackerTiersFailureUsesStableCore verifies that a failed
// tiers.json read leaves the stable core as the only observed input.
func TestUpgradeTrackerTiersFailureUsesStableCore(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	fx := newUpstreamFixture("2.555.5", nil, "")
	defer fx.Close()

	tracker := NewUpgradeTrackerReconciler(store, &recordingSink{}, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())

	if _, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, candidateName("profile-2-555", "2.555.5"), ""); err != nil {
		t.Fatalf("expected a candidate from the stable core when tiers.json fails: %v", err)
	}
}

// TestUpgradeTrackerStableFailureUsesLatestCoreTxt verifies that when the
// stable/ document cannot be read, the tracker falls back to latestCore.txt.
func TestUpgradeTrackerStableFailureUsesLatestCoreTxt(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	fx := newUpstreamFixture("", []string{"2.555.4"}, "2.555.5\n")
	defer fx.Close()

	tracker := NewUpgradeTrackerReconciler(store, &recordingSink{}, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())

	if _, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, candidateName("profile-2-555", "2.555.5"), ""); err != nil {
		t.Fatalf("expected a candidate from stable/latestCore.txt: %v", err)
	}
}

// TestUpgradeTrackerAllReadsFailNoCandidate verifies that when every upstream
// read fails, the tracker creates no candidate and does not fail.
func TestUpgradeTrackerAllReadsFailNoCandidate(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	fx := newUpstreamFixture("", nil, "")
	defer fx.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("simulated transport failure")
	})

	tracker.Reconcile(context.Background())

	candidates, err := crdstore.List[v1alpha1.ProfileCandidate](context.Background(), store, "", "")
	if err != nil {
		t.Fatalf("list ProfileCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %d, want 0 when every read fails", len(candidates))
	}
	if events := sink.ofType("upgrade.candidate.created"); len(events) != 0 {
		t.Fatalf("upgrade.candidate.created events = %d, want 0", len(events))
	}
}

// TestUpgradeTrackerApplyOwnedNoOpOnRepeat verifies that a second discovery
// of an already-existing candidate name hits ErrNotOwned and is a verified
// no-op — no duplicate object, no duplicate event.
func TestUpgradeTrackerApplyOwnedNoOpOnRepeat(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	fx := newUpstreamFixture("2.555.4", []string{"2.555.4"}, "")
	defer fx.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())
	tracker.Reconcile(context.Background())

	candidates, err := crdstore.List[v1alpha1.ProfileCandidate](context.Background(), store, "", "")
	if err != nil {
		t.Fatalf("list ProfileCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want exactly 1 (repeated discovery must be a no-op)", len(candidates))
	}
	if events := sink.ofType("upgrade.candidate.created"); len(events) != 1 {
		t.Fatalf("upgrade.candidate.created events = %d, want exactly 1", len(events))
	}
}

// TestUpgradeTracker_StableDocumentLargerThanOneMiB verifies that the stable/
// document parses when it exceeds 1 MiB and core.version sits after the
// padding. The tracker must still learn the stable core.
func TestUpgradeTracker_StableDocumentLargerThanOneMiB(t *testing.T) {
	ts := paddedStableServer("2.570.2")
	defer ts.Close()

	tracker := NewUpgradeTrackerReconciler(crdstore.NewFake(), &recordingSink{}, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, ts)

	core, ok := tracker.fetchStableCore(context.Background())
	if !ok {
		t.Fatal("fetchStableCore ok = false, want true for a stable/ document larger than 1 MiB")
	}
	if core != "2.570.2" {
		t.Fatalf("fetchStableCore core = %q, want %q", core, "2.570.2")
	}
}

// TestUpgradeTrackerNewLineFromTiersWhenStableFails verifies that a newer LTS
// line named only by tiers.json is still reported when both stable reads fail.
func TestUpgradeTrackerNewLineFromTiersWhenStableFails(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	fx := newUpstreamFixture("", []string{"2.588.1"}, "")
	defer fx.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, "https://updates.jenkins.io", testLogger())
	tracker.httpDoer = rewriteToServer(t, fx.Server)

	tracker.Reconcile(context.Background())

	if events := sink.ofType("upgrade.upstream.observed"); len(events) != 1 {
		t.Fatalf("upgrade.upstream.observed events = %d, want 1 for the tiers.json line 2.588", len(events))
	}
}
