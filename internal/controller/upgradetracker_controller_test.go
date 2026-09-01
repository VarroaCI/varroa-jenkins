package controller

import (
	"context"
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

// dynamicStableServer serves /stable/update-center.actual.json with
// stableCore as {"core":{"version":...}} (or a 500 if stableCore is empty,
// simulating an unreachable/degraded response), and 200s for exactly the
// paths listed in okPaths, 404 for every other /dynamic-stable-*/ request.
func dynamicStableServer(stableCore string, okPaths map[string]bool) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/stable/update-center.actual.json", func(w http.ResponseWriter, _ *http.Request) {
		if stableCore == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"core":{"version":%q}}`, stableCore)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if okPaths[r.URL.Path] {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
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
	// No patch ever succeeds — isolates the new-line signal from candidate
	// creation.
	ts := dynamicStableServer("2.999.1", nil)
	defer ts.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, testLogger())
	tracker.httpDoer = rewriteToServer(t, ts)

	tracker.Reconcile(context.Background())
	tracker.Reconcile(context.Background())

	events := sink.ofType("upgrade.upstream.observed")
	if len(events) != 1 {
		t.Fatalf("upgrade.upstream.observed events = %d, want exactly 1 (deduplicated across profiles and ticks)", len(events))
	}
}

// TestUpgradeTrackerFirstProbe404NoOp verifies the "404-at-start" case: the
// profile's current patch is already the newest on its line, so no
// ProfileCandidate is created and no event fires.
func TestUpgradeTrackerFirstProbe404NoOp(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	// stable/ names the same line as the tracked profile, so no new-line
	// event either — isolates this test to the no-op assertion.
	ts := dynamicStableServer("2.555.1", nil)
	defer ts.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, testLogger())
	tracker.httpDoer = rewriteToServer(t, ts)

	tracker.Reconcile(context.Background())

	candidates, err := crdstore.List[v1alpha1.ProfileCandidate](context.Background(), store, "", "")
	if err != nil {
		t.Fatalf("list ProfileCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %d, want 0 (first probe 404 means current patch already newest)", len(candidates))
	}
	if events := sink.ofType("upgrade.candidate.created"); len(events) != 0 {
		t.Fatalf("upgrade.candidate.created events = %d, want 0", len(events))
	}
}

// TestUpgradeTrackerCreatesCandidateOnPatchDiscovery verifies that one or
// more 200s before a 404 creates a ProfileCandidate off the last confirmed
// 200, with ObservedVersion/ResolveVersion set correctly.
func TestUpgradeTrackerCreatesCandidateOnPatchDiscovery(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	ts := dynamicStableServer("2.555.1", map[string]bool{
		"/dynamic-stable-2.555.4/update-center.actual.json": true,
		"/dynamic-stable-2.555.5/update-center.actual.json": true,
		// 2.555.6 404s (absent from the map).
	})
	defer ts.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, testLogger())
	tracker.httpDoer = rewriteToServer(t, ts)

	tracker.Reconcile(context.Background())

	name := candidateName("profile-2-555", "2.555.5")
	got, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, name, "")
	if err != nil {
		t.Fatalf("get candidate %s: %v", name, err)
	}
	if got.Spec.ProfileRef != "profile-2-555" {
		t.Errorf("ProfileRef = %q, want profile-2-555", got.Spec.ProfileRef)
	}
	if got.Spec.ObservedVersion != "2.555.3" {
		t.Errorf("ObservedVersion = %q, want 2.555.3", got.Spec.ObservedVersion)
	}
	if got.Spec.ResolveVersion != "2.555.5" {
		t.Errorf("ResolveVersion = %q, want 2.555.5", got.Spec.ResolveVersion)
	}
	if got.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Errorf("Phase = %q, want Pending", got.Status.Phase)
	}
	if events := sink.ofType("upgrade.candidate.created"); len(events) != 1 {
		t.Fatalf("upgrade.candidate.created events = %d, want 1", len(events))
	}
}

// TestUpgradeTrackerExhaustsProbeStepsCreatesCandidate verifies the
// bound-reached case: exhausting maxPatchProbeSteps with no 404 still
// creates a candidate off the last confirmed 200 (a lower-bound discovery,
// not a proven newest patch).
func TestUpgradeTrackerExhaustsProbeStepsCreatesCandidate(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	okPaths := make(map[string]bool, maxPatchProbeSteps)
	for k := 4; k <= 3+maxPatchProbeSteps; k++ {
		okPaths[fmt.Sprintf("/dynamic-stable-2.555.%d/update-center.actual.json", k)] = true
	}
	ts := dynamicStableServer("2.555.1", okPaths)
	defer ts.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, testLogger())
	tracker.httpDoer = rewriteToServer(t, ts)

	tracker.Reconcile(context.Background())

	wantVersion := fmt.Sprintf("2.555.%d", 3+maxPatchProbeSteps)
	name := candidateName("profile-2-555", wantVersion)
	got, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, name, "")
	if err != nil {
		t.Fatalf("get candidate %s: %v", name, err)
	}
	if got.Spec.ResolveVersion != wantVersion {
		t.Errorf("ResolveVersion = %q, want %q (last confirmed 200 within the %d-probe bound)", got.Spec.ResolveVersion, wantVersion, maxPatchProbeSteps)
	}
}

// TestUpgradeTrackerTransportErrorSkipsProfile verifies the never-hard-fail
// semantics: a transport error probing one profile's line is logged and
// skipped, and does not abort the tick's remaining profiles.
func TestUpgradeTrackerTransportErrorSkipsProfile(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		testProfile("profile-broken", "2.400", "2.400.1"),
		testProfile("profile-ok", "2.555", "2.555.3"),
	)
	ts := dynamicStableServer("2.555.1", map[string]bool{
		"/dynamic-stable-2.555.4/update-center.actual.json": true,
	})
	defer ts.Close()
	good := rewriteToServer(t, ts)

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, testLogger())
	tracker.httpDoer = doerFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "2.400") {
			return nil, errors.New("simulated transport failure")
		}
		return good(req)
	})

	tracker.Reconcile(context.Background())

	if _, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, candidateName("profile-broken", "2.400.2"), ""); err == nil {
		t.Fatalf("expected no candidate for profile-broken (transport error should skip it silently)")
	}
	if _, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), store, candidateName("profile-ok", "2.555.4"), ""); err != nil {
		t.Fatalf("expected profile-ok's candidate to still be created despite profile-broken's error: %v", err)
	}
}

// TestUpgradeTrackerApplyOwnedNoOpOnRepeat verifies that a second discovery
// of an already-existing candidate name hits ErrNotOwned and is a verified
// no-op — no duplicate object, no duplicate event.
func TestUpgradeTrackerApplyOwnedNoOpOnRepeat(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testProfile("profile-2-555", "2.555", "2.555.3"))
	ts := dynamicStableServer("2.555.1", map[string]bool{
		"/dynamic-stable-2.555.4/update-center.actual.json": true,
	})
	defer ts.Close()

	sink := &recordingSink{}
	tracker := NewUpgradeTrackerReconciler(store, sink, testLogger())
	tracker.httpDoer = rewriteToServer(t, ts)

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
