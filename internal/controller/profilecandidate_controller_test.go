package controller

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	ucSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/inventory" {
			_, _ = w.Write([]byte(`{"plugins":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ucSrv.Close()

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
// constructor itself: TestFetchInventory_StalledEndpointDoesNotHang below
// exercises fetchInventory against an httpDoer built directly in the test,
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

// TestFetchInventory_StalledEndpointDoesNotHang covers the reconciler's
// leader-gated single-goroutine ticker: an update-center that accepts the
// connection and then never responds must fail fetchInventory within
// httpDoer's timeout rather than blocking the tick forever. The client here
// carries a short injected timeout so the test doesn't wait out the
// production candidateHTTPTimeout.
func TestFetchInventory_StalledEndpointDoesNotHang(t *testing.T) {
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

	rec := &ProfileCandidateReconciler{
		ucBaseURL: srv.URL,
		httpDoer:  &http.Client{Timeout: 50 * time.Millisecond},
	}

	start := time.Now()
	_, err := rec.fetchInventory(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a stalled endpoint, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("fetchInventory took %s to return; expected it to abort near the client's 50ms timeout, not hang", elapsed)
	}
}
