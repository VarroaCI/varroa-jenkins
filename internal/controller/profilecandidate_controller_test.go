package controller

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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// candidateFixtureHPI builds a genuine .hpi holding just a manifest, mirroring
// internal/pluginresolve/bootstrap_test.go's fixtureHPI (unexported there, so
// restated here rather than imported across package boundaries).
func candidateFixtureHPI(t *testing.T, shortName, version, requiredCore, deps string) []byte {
	t.Helper()
	mf := "Manifest-Version: 1.0\r\nShort-Name: " + shortName + "\r\nPlugin-Version: " + version + "\r\n"
	if requiredCore != "" {
		mf += "Jenkins-Version: " + requiredCore + "\r\n"
	}
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

// candidateUpstreamServer serves a minimal online update-center + plugin
// download surface: /update-center.actual.json and
// /dynamic-stable-<target>/update-center.actual.json both list gitVersion for
// "git", and /download/plugins/git/<version>/git.hpi serves gitHPI.
func candidateUpstreamServer(t *testing.T, gitHPI []byte) *httptest.Server {
	t.Helper()
	const gitVersion = "5.6.0"
	mux := http.NewServeMux()
	body, err := json.Marshal(map[string]any{
		"plugins": map[string]any{
			"git": map[string]any{
				"version":      gitVersion,
				"sha256":       "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				"requiredCore": "2.400",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal update-center fixture: %v", err)
	}
	mux.HandleFunc("/update-center.actual.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		want := fmt.Sprintf("/download/plugins/git/%s/git.hpi", gitVersion)
		if r.URL.Path == want {
			_, _ = w.Write(gitHPI)
			return
		}
		if len(r.URL.Path) > 1 {
			// Any /dynamic-stable-*/update-center.actual.json also answers, so
			// the resolver's second source is healthy too.
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

// newCandidateTestFixtures seeds a JenkinsVersionProfile with a materialized
// "<profile>-pluginset-content" ConfigMap (seed: ["git"]) and a matching
// ProfileCandidate targeting resolveVersion, both via newTestClient's fake.
func newCandidateTestFixtures(t *testing.T) (*testClient, *v1alpha1.ProfileCandidate) {
	t.Helper()
	const resolveVersion = "2.479.4"
	client := newTestClient()

	profile := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-lts", UID: "00000000-0000-0000-0000-000000000010"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.479", Channel: "lts", ResolveVersion: "2.479.3"},
		Status:     v1alpha1.JenkinsVersionProfileStatus{ContentRef: "jenkins-lts-pluginset-content"},
	}
	crdstore.MustSeed(client.store, profile)
	client.configMapData["jenkins-lts-pluginset-content"] = map[string]string{
		"plugins.yaml": "core:\n  - git\nplugins:\n  - artifactId: git\n    version: \"5.6.0\"\n",
	}

	candidate := &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-lts-candidate", UID: "00000000-0000-0000-0000-000000000020"},
		Spec: v1alpha1.ProfileCandidateSpec{
			ProfileRef:      "jenkins-lts",
			ObservedVersion: "2.479.3",
			ResolveVersion:  resolveVersion,
		},
	}
	crdstore.MustSeed(client.store, candidate)

	return client, candidate
}

func newCandidateReconciler(client *testClient, upstreamBaseURL string) *ProfileCandidateReconciler {
	rec := NewProfileCandidateReconciler(client, client.store, profileReconcilerNamespace, "http://unused-uc-base", nil, testLogger())
	rec.upstreamBaseURL = upstreamBaseURL
	rec.readRootHPI = func() ([]byte, error) {
		return candidateFixtureHPIForRoot, nil
	}
	return rec
}

// candidateFixtureHPIForRoot is set per-test via package-level indirection to
// avoid threading a root-HPI byte slice through every helper; see
// withRootHPI.
var candidateFixtureHPIForRoot []byte

func reconcileCandidate(t *testing.T, rec *ProfileCandidateReconciler, name string) (*v1alpha1.ProfileCandidate, reconcile.Result) {
	t.Helper()
	result, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	got, getErr := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), rec.store, name, "")
	if getErr != nil {
		t.Fatalf("failed to re-read candidate: %v", getErr)
	}
	return got, result
}

func candidateCondition(c *v1alpha1.ProfileCandidate, typ string) *v1alpha1.ProfileCandidateCondition {
	for i := range c.Status.Conditions {
		if c.Status.Conditions[i].Type == typ {
			return &c.Status.Conditions[i]
		}
	}
	return nil
}

// TestProfileCandidateReconciler_FullyPassingReachesReady covers a candidate
// whose closure resolves cleanly, verifies clean against the root HPI,
// satisfies the core floor, and is fully servable from the (absent, so
// online-fallback) in-cluster update center — it reaches Phase Ready.
func TestProfileCandidateReconciler_FullyPassingReachesReady(t *testing.T) {
	gitHPI := candidateFixtureHPI(t, "git", "5.6.0", "", "")
	rootHPI := candidateFixtureHPI(t, "varroa-mite-auth", "1.0", "2.400", "git:5.0")
	candidateFixtureHPIForRoot = rootHPI

	srv := candidateUpstreamServer(t, gitHPI)
	defer srv.Close()

	client, candidate := newCandidateTestFixtures(t)
	rec := newCandidateReconciler(client, srv.URL)

	got, result := reconcileCandidate(t, rec, candidate.Name)

	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected Phase Ready, got %q (conditions: %+v)", got.Status.Phase, got.Status.Conditions)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue once Ready, got %v", result.RequeueAfter)
	}
	for _, typ := range []string{
		v1alpha1.ConditionCandidateResolved,
		v1alpha1.ConditionCandidateClosureClean,
		v1alpha1.ConditionCandidateCoreCompatOK,
		v1alpha1.ConditionCandidatePluginsServable,
		v1alpha1.ConditionCandidatePreflightChecked,
	} {
		cond := candidateCondition(got, typ)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("expected condition %s=True, got %+v", typ, cond)
		}
	}
	if got.Spec.ClosureContentRef == "" {
		t.Error("expected ClosureContentRef to be set")
	}
}

// TestProfileCandidateReconciler_CoreFloorExceededReachesFailed covers a root
// HPI whose own RequiredCore exceeds the resolution target: it fails
// AssertCoreFloor and reaches Phase Failed.
func TestProfileCandidateReconciler_CoreFloorExceededReachesFailed(t *testing.T) {
	gitHPI := candidateFixtureHPI(t, "git", "5.6.0", "", "")
	rootHPI := candidateFixtureHPI(t, "varroa-mite-auth", "1.0", "9.999", "git:5.0")
	candidateFixtureHPIForRoot = rootHPI

	srv := candidateUpstreamServer(t, gitHPI)
	defer srv.Close()

	client, candidate := newCandidateTestFixtures(t)
	rec := newCandidateReconciler(client, srv.URL)

	got, result := reconcileCandidate(t, rec, candidate.Name)

	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseFailed {
		t.Fatalf("expected Phase Failed, got %q (conditions: %+v)", got.Status.Phase, got.Status.Conditions)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for a terminal Failed phase, got %v", result.RequeueAfter)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidateCoreCompatOK)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected CoreCompatOK=False, got %+v", cond)
	}
}

func TestProfileCandidateReconciler_RootHPIUnavailableReachesFailed(t *testing.T) {
	gitHPI := candidateFixtureHPI(t, "git", "5.6.0", "", "")
	srv := candidateUpstreamServer(t, gitHPI)
	defer srv.Close()

	client, candidate := newCandidateTestFixtures(t)
	rec := newCandidateReconciler(client, srv.URL)
	rec.readRootHPI = func() ([]byte, error) { return nil, fmt.Errorf("no such file or directory") }

	got, _ := reconcileCandidate(t, rec, candidate.Name)

	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseFailed {
		t.Fatalf("expected Phase Failed, got %q", got.Status.Phase)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidateClosureClean)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "RootHPIUnavailable" {
		t.Fatalf("expected ClosureClean=False/RootHPIUnavailable, got %+v", cond)
	}
	coreCond := candidateCondition(got, v1alpha1.ConditionCandidateCoreCompatOK)
	if coreCond == nil || coreCond.Status != metav1.ConditionFalse || coreCond.Reason != "RootHPIUnavailable" {
		t.Fatalf("expected CoreCompatOK=False/RootHPIUnavailable, got %+v", coreCond)
	}
}

// TestProfileCandidateReconciler_PluginsServableFalseHoldsPending covers an
// air-gapped in-cluster UpdateCenter whose inventory is missing the resolved
// plugin: it holds the candidate at Phase Pending (never Failed) with
// WaitingForUpdateCenter, and a second reconcile pass re-evaluates rather
// than getting stuck (re-using the persisted ClosureContentRef instead of
// re-resolving).
func TestProfileCandidateReconciler_PluginsServableFalseHoldsPending(t *testing.T) {
	gitHPI := candidateFixtureHPI(t, "git", "5.6.0", "", "")
	rootHPI := candidateFixtureHPI(t, "varroa-mite-auth", "1.0", "2.400", "git:5.0")
	candidateFixtureHPIForRoot = rootHPI

	srv := candidateUpstreamServer(t, gitHPI)
	defer srv.Close()

	client, candidate := newCandidateTestFixtures(t)

	// An in-cluster UpdateCenter (default/zero storage type still routes
	// selectSource to the online source, per its own default branch) with an
	// empty inventory and pull-through disabled: "git" is never servable.
	uc := &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: updateCenterSingletonName},
		Spec: v1alpha1.UpdateCenterSpec{
			PullThrough: v1alpha1.UpdateCenterPullThrough{Enabled: false},
		},
	}
	crdstore.MustSeed(client.store, uc)

	ucSrv := candidateUCServer(t, nil, map[string]ucServabilityResult{"git": {Reason: "pull-through-disabled"}}, 0, nil)

	rec := NewProfileCandidateReconciler(client, client.store, profileReconcilerNamespace, ucSrv.URL, nil, testLogger())
	rec.upstreamBaseURL = srv.URL
	rec.readRootHPI = func() ([]byte, error) { return rootHPI, nil }

	got1, result1 := reconcileCandidate(t, rec, candidate.Name)
	if got1.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Fatalf("expected Phase Pending, got %q (conditions: %+v)", got1.Status.Phase, got1.Status.Conditions)
	}
	if result1.RequeueAfter == 0 {
		t.Error("expected a requeue while blocked on PluginsServable")
	}
	cond := candidateCondition(got1, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WaitingForUpdateCenter" {
		t.Fatalf("expected PluginsServable=False/WaitingForUpdateCenter, got %+v", cond)
	}
	if got1.Spec.ClosureContentRef == "" {
		t.Fatal("expected ClosureContentRef to be persisted even while blocked")
	}

	// Second pass: still missing, but must re-run (not get stuck) — reads the
	// closure back from ClosureContentRef instead of re-resolving.
	got2, result2 := reconcileCandidate(t, rec, candidate.Name)
	if got2.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Fatalf("expected Phase Pending on second pass, got %q", got2.Status.Phase)
	}
	if result2.RequeueAfter == 0 {
		t.Error("expected a requeue on the second pass too")
	}
}

// TestProfileCandidateReconciler_PreflightMissingOnlyDoesNotFail covers a
// controller whose bundle is merely missing a plugin the candidate's closure
// adds (bundle.PinPreflightReport.Missing): that must not count toward
// ControllersFailing — only a version Conflict does.
func TestProfileCandidateReconciler_PreflightMissingOnlyDoesNotFail(t *testing.T) {
	gitHPI := candidateFixtureHPI(t, "git", "5.6.0", "", "")
	rootHPI := candidateFixtureHPI(t, "varroa-mite-auth", "1.0", "2.400", "git:5.0")
	candidateFixtureHPIForRoot = rootHPI

	srv := candidateUpstreamServer(t, gitHPI)
	defer srv.Close()

	client, candidate := newCandidateTestFixtures(t)
	rec := newCandidateReconciler(client, srv.URL)

	// A Controller resolving to the same profile line, with no
	// composedBundleRef set (so EffectiveBundleRef falls back to the
	// operator-seeded starter bundle) whose bundle pins no plugins at all —
	// the closure's "git" pin is Missing from the bundle, never Conflicting.
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns1", UID: "00000000-0000-0000-0000-000000000030"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.479.3"},
	}
	crdstore.MustSeed(client.store, cr)
	cb := &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.StarterBundleName, Namespace: profileReconcilerNamespace, UID: "00000000-0000-0000-0000-000000000031"},
		Status:     v1alpha1.ComposedBundleStatus{ContentRef: "varroa-starter-content", Phase: v1alpha1.ComposedBundleReady},
	}
	crdstore.MustSeed(client.store, cb)
	client.configMapData["varroa-starter-content"] = map[string]string{
		"plugins.yaml": "plugins: []\n",
	}

	got, _ := reconcileCandidate(t, rec, candidate.Name)

	preflightCond := candidateCondition(got, v1alpha1.ConditionCandidatePreflightChecked)
	if preflightCond == nil || preflightCond.Status != metav1.ConditionTrue {
		t.Fatalf("expected PreflightChecked=True, got %+v", preflightCond)
	}
	if got.Status.Preflight == nil {
		t.Fatal("expected Status.Preflight to be set")
	}
	if got.Status.Preflight.ControllersFailing != 0 {
		t.Errorf("expected ControllersFailing=0 for a Missing-only report, got %d (%+v)", got.Status.Preflight.ControllersFailing, got.Status.Preflight.FailingControllers)
	}
	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected Phase Ready (pre-flight is advisory-only), got %q", got.Status.Phase)
	}
}

// TestNewProfileCandidateReconciler_HTTPDoerHasTimeout guards the
// constructor itself: TestGetUCInventory_StalledEndpointDoesNotHang below
// exercises getUCInventory against an httpDoer built directly in the test,
// so it would keep passing even if NewProfileCandidateReconciler reverted to
// an httpDoer with no timeout (e.g. http.DefaultClient). This test checks
// the constructor's actual default instead.
func TestNewProfileCandidateReconciler_HTTPDoerHasTimeout(t *testing.T) {
	rec := NewProfileCandidateReconciler(nil, nil, "ns", "http://unused", nil, testLogger())
	client, ok := rec.httpDoer.(*http.Client)
	if !ok {
		t.Fatalf("expected httpDoer to be *http.Client, got %T", rec.httpDoer)
	}
	if client.Timeout <= 0 {
		t.Fatal("expected httpDoer to carry a positive Timeout: a stalled update-center connection must not be able to block the reconciler's single-threaded tick forever")
	}
}

// TestGetUCInventory_StalledEndpointDoesNotHang covers the reconciler's
// leader-gated single-goroutine ticker: an update-center that accepts the
// connection and then never responds must fail getUCInventory within
// httpDoer's timeout rather than blocking the tick forever. The client here
// carries a short injected timeout so the test doesn't wait out the
// production candidateHTTPTimeout.
func TestGetUCInventory_StalledEndpointDoesNotHang(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // held open until the test releases it below; the client timeout must win the race
	}))
	// srv.Close waits for the handler goroutine to return, so block must close
	// first on every exit path, including a failed assertion: t.Cleanup runs
	// LIFO, so registering close(block) after srv.Close makes it run before
	// srv.Close and guarantees the handler goroutine is never left parked.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	start := time.Now()
	_, _, err := getUCInventory(context.Background(), &http.Client{Timeout: 50 * time.Millisecond}, srv.URL)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a stalled endpoint, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("getUCInventory took %s to return; expected it to abort near the client's 50ms timeout, not hang", elapsed)
	}
}

// candidateUCServer serves the update center's inventory and servability
// routes for candidate tests. answers maps a plugin name to its servability
// answer; a plugin absent from answers is servable. A non-zero status answers
// the servability route with that status instead. posted, when non-nil,
// collects every plugin the candidate posts.
func candidateUCServer(t *testing.T, inventory []inventoryEntry, answers map[string]ucServabilityResult, status int, posted *[]ucServabilityPlugin) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/inventory":
			_ = json.NewEncoder(w).Encode(ucInventoryResponse{Plugins: inventory})
		case "/api/v1/servability":
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			var req ucServabilityRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if posted != nil {
				*posted = append(*posted, req.Plugins...)
			}
			resp := ucServabilityResponse{}
			for _, p := range req.Plugins {
				res := ucServabilityResult{Name: p.Name, Version: p.Version, Servable: true, Via: "pull-through"}
				if a, ok := answers[p.Name]; ok {
					res = a
					res.Name, res.Version = p.Name, p.Version
				}
				resp.Plugins = append(resp.Plugins, res)
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// candidateServabilityRig seeds the candidate fixtures, an UpdateCenter with
// the given pull-through setting, and a reconciler whose update-center base
// URL is ucURL. The closure resolves to git@5.6.0.
func candidateServabilityRig(t *testing.T, pullThrough bool, ucURL string) (*ProfileCandidateReconciler, *testClient, *v1alpha1.ProfileCandidate) {
	t.Helper()
	gitHPI := candidateFixtureHPI(t, "git", "5.6.0", "", "")
	candidateFixtureHPIForRoot = candidateFixtureHPI(t, "varroa-mite-auth", "1.0", "2.400", "git:5.0")
	upstream := candidateUpstreamServer(t, gitHPI)
	t.Cleanup(upstream.Close)

	client, candidate := newCandidateTestFixtures(t)
	crdstore.MustSeed(client.store, &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: updateCenterSingletonName},
		Spec: v1alpha1.UpdateCenterSpec{
			PullThrough: v1alpha1.UpdateCenterPullThrough{Enabled: pullThrough},
		},
	})
	rec := newCandidateReconciler(client, upstream.URL)
	rec.ucBaseURL = ucURL
	return rec, client, candidate
}

// TestProfileCandidateReconciler_PullThroughServableReachesReady covers a
// candidate whose unstored pin the update center serves by pull-through: the
// candidate posts only that pin and reaches Ready.
func TestProfileCandidateReconciler_PullThroughServableReachesReady(t *testing.T) {
	var posted []ucServabilityPlugin
	ucSrv := candidateUCServer(t, nil, nil, 0, &posted)
	rec, _, candidate := candidateServabilityRig(t, true, ucSrv.URL)

	got, _ := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected Phase Ready, got %q (conditions: %+v)", got.Status.Phase, got.Status.Conditions)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "AllPluginsServable" {
		t.Fatalf("expected PluginsServable=True/AllPluginsServable, got %+v", cond)
	}
	if len(posted) != 1 || posted[0] != (ucServabilityPlugin{Name: "git", Version: "5.6.0"}) {
		t.Fatalf("expected the servability endpoint to receive only git@5.6.0, got %+v", posted)
	}
}

// TestProfileCandidateReconciler_StoredPinsSkipServability covers a closure
// whose pins are all stored: the candidate reaches Ready without calling the
// servability endpoint.
func TestProfileCandidateReconciler_StoredPinsSkipServability(t *testing.T) {
	var posted []ucServabilityPlugin
	ucSrv := candidateUCServer(t, []inventoryEntry{{Name: "git", Version: "5.6.0"}}, nil, 0, &posted)
	rec, _, candidate := candidateServabilityRig(t, false, ucSrv.URL)

	got, _ := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected Phase Ready, got %q", got.Status.Phase)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "AllPluginsServable" {
		t.Fatalf("expected PluginsServable=True/AllPluginsServable, got %+v", cond)
	}
	if len(posted) != 0 {
		t.Fatalf("expected no servability call for a stored pin, got %+v", posted)
	}
}

// TestProfileCandidateReconciler_NotListedHoldsPending covers a pin the update
// center cannot serve at all: PluginsUnservable names it and the candidate
// holds Pending.
func TestProfileCandidateReconciler_NotListedHoldsPending(t *testing.T) {
	ucSrv := candidateUCServer(t, nil, map[string]ucServabilityResult{"git": {Reason: "not-listed"}}, 0, nil)
	rec, _, candidate := candidateServabilityRig(t, true, ucSrv.URL)

	got, result := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Fatalf("expected Phase Pending, got %q", got.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected a requeue while blocked on PluginsServable")
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "PluginsUnservable" {
		t.Fatalf("expected PluginsServable=False/PluginsUnservable, got %+v", cond)
	}
	if !strings.Contains(cond.Message, "git@5.6.0 (NotListed)") {
		t.Fatalf("expected the message to name git@5.6.0 (NotListed), got %q", cond.Message)
	}
}

// TestProfileCandidateReconciler_SourcesDegradedHoldsPendingUnknown covers an
// undecided answer: the candidate reports Unknown/ResolutionDegraded and holds
// Pending.
func TestProfileCandidateReconciler_SourcesDegradedHoldsPendingUnknown(t *testing.T) {
	ucSrv := candidateUCServer(t, nil, map[string]ucServabilityResult{"git": {Reason: "sources-degraded"}}, 0, nil)
	rec, _, candidate := candidateServabilityRig(t, true, ucSrv.URL)

	got, _ := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Fatalf("expected Phase Pending, got %q", got.Status.Phase)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != "ResolutionDegraded" {
		t.Fatalf("expected PluginsServable=Unknown/ResolutionDegraded, got %+v", cond)
	}
}

// TestProfileCandidateReconciler_ServabilityUnsupportedBlocks covers an update
// center that does not route the servability endpoint: the candidate holds
// Pending with InventoryUnavailable.
func TestProfileCandidateReconciler_ServabilityUnsupportedBlocks(t *testing.T) {
	ucSrv := candidateUCServer(t, nil, nil, http.StatusNotFound, nil)
	rec, _, candidate := candidateServabilityRig(t, true, ucSrv.URL)

	got, _ := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Fatalf("expected Phase Pending, got %q", got.Status.Phase)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "InventoryUnavailable" {
		t.Fatalf("expected PluginsServable=False/InventoryUnavailable, got %+v", cond)
	}
}

// TestProfileCandidateReconciler_ServabilityTransientErrorKeepsPrior covers a
// transient servability failure: the prior PluginsServable value stays and the
// candidate requeues instead of changing its verdict.
func TestProfileCandidateReconciler_ServabilityTransientErrorKeepsPrior(t *testing.T) {
	ucSrv := candidateUCServer(t, nil, nil, http.StatusServiceUnavailable, nil)
	rec, client, candidate := candidateServabilityRig(t, true, ucSrv.URL)

	prior := v1alpha1.ProfileCandidateCondition{
		Type:               v1alpha1.ConditionCandidatePluginsServable,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "AllPluginsServable",
		Message:            "prior verdict",
	}
	if err := crdstore.PatchStatus[v1alpha1.ProfileCandidate](context.Background(), client.store, candidate.Name, "", &v1alpha1.ProfileCandidateStatus{
		Phase:      v1alpha1.ProfileCandidatePhasePending,
		Conditions: []v1alpha1.ProfileCandidateCondition{prior},
	}); err != nil {
		t.Fatalf("seed prior status: %v", err)
	}

	got, result := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Fatalf("expected Phase Pending, got %q", got.Status.Phase)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected a requeue after a transient servability failure")
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Message != "prior verdict" {
		t.Fatalf("expected the prior PluginsServable condition kept, got %+v", cond)
	}
}

// TestProfileCandidateReconciler_EmptyUCBaseURLIsNoUpdateCenter covers a
// reconciler with no update-center base URL: the check is skipped even when an
// UpdateCenter CR exists.
func TestProfileCandidateReconciler_EmptyUCBaseURLIsNoUpdateCenter(t *testing.T) {
	rec, _, candidate := candidateServabilityRig(t, true, "")

	got, _ := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected Phase Ready, got %q", got.Status.Phase)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "NoUpdateCenter" {
		t.Fatalf("expected PluginsServable=True/NoUpdateCenter, got %+v", cond)
	}
}

// TestProfileCandidateReconciler_PartialInventoryHoldsUnknown covers an
// inventory with an unreadable plugin-pack manifest: a pin the store may hold
// is held undecided, never posted for a verdict the route cannot give.
func TestProfileCandidateReconciler_PartialInventoryHoldsUnknown(t *testing.T) {
	ucSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/inventory":
			_, _ = w.Write([]byte(`{"plugins":[],"skippedPacks":[{"ref":"git.hpi","error":"unreadable"}]}`))
		case "/api/v1/servability":
			t.Error("a partial inventory must not reach the servability endpoint")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ucSrv.Close()
	rec, _, candidate := candidateServabilityRig(t, false, ucSrv.URL)

	got, _ := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhasePending {
		t.Fatalf("expected Phase Pending, got %q", got.Status.Phase)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != "ResolutionDegraded" {
		t.Fatalf("expected PluginsServable=Unknown/ResolutionDegraded, got %+v", cond)
	}
}

// TestProfileCandidateReconciler_PartialInventoryPullThroughPostsNormally covers
// an inventory with an unreadable plugin-pack manifest and pull-through on: the
// update center answers from upstream metadata, so the unstored pin is posted
// as normal and the candidate reaches True/AllPluginsServable.
func TestProfileCandidateReconciler_PartialInventoryPullThroughPostsNormally(t *testing.T) {
	var posted []ucServabilityPlugin
	ucSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/inventory":
			_, _ = w.Write([]byte(`{"plugins":[],"skippedPacks":[{"ref":"other.hpi","error":"unreadable"}]}`))
		case "/api/v1/servability":
			var req ucServabilityRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			posted = append(posted, req.Plugins...)
			resp := ucServabilityResponse{}
			for _, p := range req.Plugins {
				resp.Plugins = append(resp.Plugins, ucServabilityResult{Name: p.Name, Version: p.Version, Servable: true, Via: "pull-through"})
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ucSrv.Close()
	rec, _, candidate := candidateServabilityRig(t, true, ucSrv.URL)

	got, _ := reconcileCandidate(t, rec, candidate.Name)
	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected Phase Ready, got %q", got.Status.Phase)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidatePluginsServable)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "AllPluginsServable" {
		t.Fatalf("expected PluginsServable=True/AllPluginsServable, got %+v", cond)
	}
	if len(posted) != 1 || posted[0] != (ucServabilityPlugin{Name: "git", Version: "5.6.0"}) {
		t.Fatalf("expected the unstored pin posted as normal, got %+v", posted)
	}
}
