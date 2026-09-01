package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// versionCandidateSpyClient wraps fakeResourceClient, recording ConfigMap
// writes (and their call order) so promotion's ordering guarantees (label
// removed before content changes; ResolveVersion advances before the
// content overwrite) can be asserted directly.
type versionCandidateSpyClient struct {
	*fakeResourceClient

	store crdstore.Backend // set by the test after construction

	callOrder []string
	// resolveVersionAtOverwrite captures the profile's Spec.ResolveVersion,
	// read from store at the moment UpdateConfigMapData is invoked.
	resolveVersionAtOverwrite string
	profileNameForCapture     string

	updateErr error
}

func (s *versionCandidateSpyClient) RemoveConfigMapLabel(ctx context.Context, name, namespace, labelKey string) error {
	s.callOrder = append(s.callOrder, "RemoveConfigMapLabel:"+name)
	return s.fakeResourceClient.RemoveConfigMapLabel(ctx, name, namespace, labelKey)
}

func (s *versionCandidateSpyClient) UpdateConfigMapData(ctx context.Context, name, namespace string, data map[string]string) error {
	s.callOrder = append(s.callOrder, "UpdateConfigMapData:"+name)
	if s.store != nil && s.profileNameForCapture != "" {
		if p, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, s.store, s.profileNameForCapture, ""); err == nil {
			s.resolveVersionAtOverwrite = p.Spec.ResolveVersion
		}
	}
	if s.updateErr != nil {
		return s.updateErr
	}
	if s.configMaps == nil {
		s.configMaps = map[string]map[string]string{}
	}
	s.configMaps[name] = data
	return nil
}

func newVersionCandidateTestDeps(t *testing.T) (*Dependencies, *versionCandidateSpyClient) {
	t.Helper()
	client := newFakeResourceClient()
	client.configMaps = map[string]map[string]string{}
	spy := &versionCandidateSpyClient{fakeResourceClient: client}
	store := storeFromFake(client)
	spy.store = store

	deps := &Dependencies{
		Client:                   spy,
		Store:                    store,
		Authorizer:               adminTestAuthorizer(),
		Logger:                   slog.New(slog.DiscardHandler),
		OperatorNamespace:        "operator-ns",
		VersionProfileReconciler: controller.NewJenkinsVersionProfileReconciler(spy, store, "operator-ns", slog.New(slog.DiscardHandler)),
	}
	return deps, spy
}

func seedPromotionProfile(t *testing.T, deps *Dependencies, spy *versionCandidateSpyClient) {
	t.Helper()
	const name, pluginSetName, resolveVersion = "profile-a", "profile-a-pluginset", "2.500.1"
	profile := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{versionCandidateSeedLabel: "true"},
		},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			ResolveVersion: resolveVersion,
			PluginSetRef:   &v1alpha1.ConfigMapRef{Name: pluginSetName},
		},
	}
	crdstore.MustSeed(deps.Store.(*crdstore.Fake), profile)
	spy.configMaps[pluginSetName] = map[string]string{
		"plugins.yaml": "core:\n  - \"" + resolveVersion + "\"\nplugins:\n  - artifactId: git\n    version: \"5.0.0\"\n",
	}
}

func seedPromotionCandidate(t *testing.T, deps *Dependencies, name, profileRef, resolveVersion string, phase v1alpha1.ProfileCandidatePhase, closureRef string) *v1alpha1.ProfileCandidate {
	t.Helper()
	candidate := &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ProfileCandidateSpec{
			ProfileRef:        profileRef,
			ObservedVersion:   resolveVersion,
			ResolveVersion:    resolveVersion,
			ClosureContentRef: closureRef,
		},
		Status: v1alpha1.ProfileCandidateStatus{Phase: phase},
	}
	crdstore.MustSeed(deps.Store.(*crdstore.Fake), candidate)
	return candidate
}

func doPromoteRequest(t *testing.T, deps *Dependencies, candidateName string) *http.Response {
	t.Helper()
	srv := &Server{deps: deps}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/version-candidates/"+candidateName+"/promote", nil)
	req = req.WithContext(contextWithClaims(req.Context(), adminClaims))
	req.URL.Path = "/version-candidates/" + candidateName + "/promote"
	w := httptest.NewRecorder()
	srv.HandleVersionCandidateDispatch(w, req)
	return w.Result()
}

func TestPromoteVersionCandidate_Success(t *testing.T) {
	deps, spy := newVersionCandidateTestDeps(t)
	spy.profileNameForCapture = "profile-a"

	seedPromotionProfile(t, deps, spy)
	spy.configMaps["profile-a-2.500.2-closure"] = map[string]string{
		"plugins.yaml": "core:\n  - \"2.500.2\"\nplugins:\n  - artifactId: git\n    version: \"5.1.0\"\n",
	}
	target := seedPromotionCandidate(t, deps, "profile-a-2.500.2", "profile-a", "2.500.2", v1alpha1.ProfileCandidatePhaseReady, "profile-a-2.500.2-closure")

	// Two open siblings for the same profile: one older, one newer — both
	// must be superseded regardless of ordering.
	older := seedPromotionCandidate(t, deps, "profile-a-2.500.0", "profile-a", "2.500.0", v1alpha1.ProfileCandidatePhasePending, "")
	newer := seedPromotionCandidate(t, deps, "profile-a-2.500.3", "profile-a", "2.500.3", v1alpha1.ProfileCandidatePhaseReady, "")
	// A candidate for a DIFFERENT profile must be left untouched.
	other := seedPromotionCandidate(t, deps, "profile-b-1.0.0", "profile-b", "1.0.0", v1alpha1.ProfileCandidatePhaseReady, "")

	resp := doPromoteRequest(t, deps, target.Name)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Write-order assertions.
	labelIdx, dataIdx := -1, -1
	for i, c := range spy.callOrder {
		if c == "RemoveConfigMapLabel:profile-a-pluginset" {
			labelIdx = i
		}
		if c == "UpdateConfigMapData:profile-a-pluginset" {
			dataIdx = i
		}
	}
	if labelIdx == -1 || dataIdx == -1 || labelIdx >= dataIdx {
		t.Fatalf("expected label removal before content overwrite, got order %v", spy.callOrder)
	}
	if spy.resolveVersionAtOverwrite != "2.500.2" {
		t.Fatalf("expected ResolveVersion already advanced to 2.500.2 at content-overwrite time, got %q", spy.resolveVersionAtOverwrite)
	}

	// Profile ended up materialized with the candidate's target version.
	profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](context.Background(), deps.Store, "profile-a", "")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if profile.Spec.ResolveVersion != "2.500.2" {
		t.Fatalf("expected profile ResolveVersion 2.500.2, got %s", profile.Spec.ResolveVersion)
	}
	if _, ok := profile.Labels[versionCandidateSeedLabel]; ok {
		t.Fatalf("expected seed label removed from profile")
	}
	if !controller.ProfilePluginSetReady(profile) || profile.Status.ContentRef == "" {
		t.Fatalf("expected profile PluginSetReady with a ContentRef, got status %+v", profile.Status)
	}

	// The promoted candidate itself.
	promoted, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), deps.Store, target.Name, "")
	if err != nil {
		t.Fatalf("get candidate: %v", err)
	}
	if promoted.Status.Phase != v1alpha1.ProfileCandidatePhasePromoted {
		t.Fatalf("expected candidate Promoted, got %s", promoted.Status.Phase)
	}
	if promoted.Status.PromotedAt == nil {
		t.Fatalf("expected PromotedAt to be set")
	}

	// Sibling supersession: both older and newer open siblings for the same
	// profile are superseded; the other profile's candidate is untouched.
	for _, sib := range []*v1alpha1.ProfileCandidate{older, newer} {
		got, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), deps.Store, sib.Name, "")
		if err != nil {
			t.Fatalf("get sibling %s: %v", sib.Name, err)
		}
		if got.Status.Phase != v1alpha1.ProfileCandidatePhaseSuperseded {
			t.Fatalf("expected sibling %s superseded, got %s", sib.Name, got.Status.Phase)
		}
	}
	unrelated, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), deps.Store, other.Name, "")
	if err != nil {
		t.Fatalf("get other profile's candidate: %v", err)
	}
	if unrelated.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected other profile's candidate to be untouched (still Ready), got %s", unrelated.Status.Phase)
	}
}

// raceInjectingBackend wraps a real crdstore.Backend to reproduce, on demand,
// exactly one interleaving: an upgrade-tracker supersedeOlder tick landing on
// a named object between a caller's read of it and a later status write. The
// first UpdateObjectStatus call matching gvr/name genuinely applies a
// Superseded status patch to the underlying store first (supersedeOlder's
// real write: a merge patch, so it always succeeds regardless of what
// resourceVersion the racing caller is holding), then reports the conflict a
// real apiserver would return for that caller's now-stale write. Every other
// call, and every other method, passes straight through to Backend.
type raceInjectingBackend struct {
	crdstore.Backend
	fake  *crdstore.Fake
	gvr   schema.GroupVersionResource
	name  string
	fired bool
}

func (b *raceInjectingBackend) UpdateObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if !b.fired && gvr == b.gvr && obj.GetName() == b.name {
		b.fired = true
		if err := b.fake.PatchObjectStatus(ctx, gvr, namespace, b.name, map[string]any{"phase": "Superseded"}); err != nil {
			return nil, err
		}
		return nil, apierrors.NewConflict(schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, b.name, errors.New("stale resourceVersion"))
	}
	return b.Backend.UpdateObjectStatus(ctx, gvr, namespace, obj)
}

// TestPromoteVersionCandidate_SurvivesConcurrentSupersedeRace reproduces the
// upgrade-tracker interleaving: a concurrent supersedeOlder tick lands on the
// very candidate being promoted (its phase is still Pending/Ready, since
// promotion has not reached step 7 yet) after promotion's initial Get has
// already captured a resourceVersion. That patch carries no resourceVersion
// precondition and always succeeds, so the stored candidate is genuinely
// Superseded by the time promotion's own status write lands, aborting it with
// a real conflict. Promotion must still land Promoted rather than stranding
// the candidate un-retriable in Superseded after the profile has already been
// advanced and re-materialized (steps 3-5, unaffected by this race).
func TestPromoteVersionCandidate_SurvivesConcurrentSupersedeRace(t *testing.T) {
	deps, spy := newVersionCandidateTestDeps(t)
	spy.profileNameForCapture = "profile-a"
	seedPromotionProfile(t, deps, spy)
	spy.configMaps["profile-a-2.500.2-closure"] = map[string]string{
		"plugins.yaml": "core:\n  - \"2.500.2\"\nplugins:\n  - artifactId: git\n    version: \"5.1.0\"\n",
	}
	target := seedPromotionCandidate(t, deps, "profile-a-2.500.2", "profile-a", "2.500.2", v1alpha1.ProfileCandidatePhaseReady, "profile-a-2.500.2-closure")

	realStore := deps.Store.(*crdstore.Fake)
	gvr, err := crdstore.GVRFor[v1alpha1.ProfileCandidate]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	deps.Store = &raceInjectingBackend{Backend: realStore, fake: realStore, gvr: gvr, name: target.Name}

	resp := doPromoteRequest(t, deps, target.Name)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 despite the concurrent supersede race, got %d: %s", resp.StatusCode, body)
	}

	promoted, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), deps.Store, target.Name, "")
	if err != nil {
		t.Fatalf("get candidate: %v", err)
	}
	if promoted.Status.Phase != v1alpha1.ProfileCandidatePhasePromoted {
		t.Fatalf("expected candidate Promoted despite the race, got %s (stranded un-retriable if Superseded)", promoted.Status.Phase)
	}
	if promoted.Status.PromotedAt == nil {
		t.Fatalf("expected PromotedAt to be set")
	}

	profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](context.Background(), deps.Store, "profile-a", "")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if profile.Spec.ResolveVersion != "2.500.2" {
		t.Fatalf("expected profile ResolveVersion 2.500.2, got %s", profile.Spec.ResolveVersion)
	}
}

func TestPromoteVersionCandidate_NotReady(t *testing.T) {
	deps, spy := newVersionCandidateTestDeps(t)
	seedPromotionProfile(t, deps, spy)
	candidate := seedPromotionCandidate(t, deps, "profile-a-2.500.2", "profile-a", "2.500.2", v1alpha1.ProfileCandidatePhasePending, "")

	resp := doPromoteRequest(t, deps, candidate.Name)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	if len(spy.callOrder) != 0 {
		t.Fatalf("expected no writes for a non-Ready candidate, got %v", spy.callOrder)
	}
	profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](context.Background(), deps.Store, "profile-a", "")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if profile.Spec.ResolveVersion != "2.500.1" {
		t.Fatalf("expected profile untouched, ResolveVersion still 2.500.1, got %s", profile.Spec.ResolveVersion)
	}
}

func TestPromoteVersionCandidate_MaterializationFailure(t *testing.T) {
	deps, spy := newVersionCandidateTestDeps(t)
	seedPromotionProfile(t, deps, spy)
	// The closure content is missing the plugins.yaml key, so step 4 succeeds
	// (it always writes) but step 5's re-materialization reads back garbage
	// and PluginSetReady never flips true.
	spy.configMaps["profile-a-2.500.2-closure"] = map[string]string{
		"plugins.yaml": "not: valid: yaml: at: all: [",
	}
	candidate := seedPromotionCandidate(t, deps, "profile-a-2.500.2", "profile-a", "2.500.2", v1alpha1.ProfileCandidatePhaseReady, "profile-a-2.500.2-closure")

	resp := doPromoteRequest(t, deps, candidate.Name)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	// ResolveVersion was already advanced (step 3, not rolled back) — the
	// safe pairing, since the new version is left resolving against the old
	// plugin set until the underlying issue is fixed and promotion retried.
	profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](context.Background(), deps.Store, "profile-a", "")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if profile.Spec.ResolveVersion != "2.500.2" {
		t.Fatalf("expected ResolveVersion already advanced to 2.500.2, got %s", profile.Spec.ResolveVersion)
	}

	// The candidate remains Ready, not Promoted, so promotion can be retried.
	got, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), deps.Store, candidate.Name, "")
	if err != nil {
		t.Fatalf("get candidate: %v", err)
	}
	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected candidate to remain Ready after a step-5 failure, got %s", got.Status.Phase)
	}
}

// TestPromoteVersionCandidate_ContentWriteFailure covers a step-4 failure:
// the pluginset ConfigMap write itself fails after Spec.ResolveVersion has
// already advanced (step 3). ResolveVersion is not rolled back — the profile
// is left advertising the new version against its old plugin set, which is
// safe to serve since a plugin resolved for an older core loads fine on a
// newer one. The candidate stays Ready so promotion can be retried.
func TestPromoteVersionCandidate_ContentWriteFailure(t *testing.T) {
	deps, spy := newVersionCandidateTestDeps(t)
	seedPromotionProfile(t, deps, spy)
	spy.configMaps["profile-a-2.500.2-closure"] = map[string]string{
		"plugins.yaml": "core:\n  - \"2.500.2\"\nplugins:\n  - artifactId: git\n    version: \"5.1.0\"\n",
	}
	candidate := seedPromotionCandidate(t, deps, "profile-a-2.500.2", "profile-a", "2.500.2", v1alpha1.ProfileCandidatePhaseReady, "profile-a-2.500.2-closure")
	spy.updateErr = fmt.Errorf("injected pluginset write failure")

	resp := doPromoteRequest(t, deps, candidate.Name)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](context.Background(), deps.Store, "profile-a", "")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if profile.Spec.ResolveVersion != "2.500.2" {
		t.Fatalf("expected ResolveVersion already advanced to 2.500.2 when the content write fails, got %s", profile.Spec.ResolveVersion)
	}
	if pluginSet := spy.configMaps["profile-a-pluginset"]["plugins.yaml"]; !strings.Contains(pluginSet, "5.0.0") || strings.Contains(pluginSet, "5.1.0") {
		t.Fatalf("expected pluginset content to remain unchanged (still pinning git 5.0.0), got %q", pluginSet)
	}

	got, err := crdstore.Get[v1alpha1.ProfileCandidate](context.Background(), deps.Store, candidate.Name, "")
	if err != nil {
		t.Fatalf("get candidate: %v", err)
	}
	if got.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		t.Fatalf("expected candidate to remain Ready after a step-4 failure, got %s", got.Status.Phase)
	}
}
