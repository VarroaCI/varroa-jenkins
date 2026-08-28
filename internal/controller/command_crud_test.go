package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// fakeCRUDClient implements CommandCRUDClient for tests.
type fakeCRUDClient struct {
	controllers          map[string]*v1alpha1.Controller
	composedBundles      map[string]*v1alpha1.ComposedBundle
	provisioningDefaults map[string]*v1alpha1.ProvisioningDefaults
	applyErr             error
	ssaApplyErr          error // error returned by ApplyControllerSpecSSA (separate from applyErr)
	deletePodErr         error
	listErr              error
	getErr               error

	// store mirrors applied/created Controllers into the crdstore read surface
	// (the same object in production, where Client and Store are both the
	// ClientsetClient). The rollback re-reads the live object through Store, so
	// the fake must keep the two views coherent.
	store *crdstore.Fake

	// hibernatedClears records SetHibernated(false) calls ("ns/name").
	hibernatedClears []string
	// setHibernatedErr, when set, makes SetHibernated return it.
	setHibernatedErr error
}

func newFakeCRUDClient() *fakeCRUDClient {
	return &fakeCRUDClient{
		controllers:          make(map[string]*v1alpha1.Controller),
		composedBundles:      make(map[string]*v1alpha1.ComposedBundle),
		provisioningDefaults: make(map[string]*v1alpha1.ProvisioningDefaults),
	}
}

func (f *fakeCRUDClient) key(name, ns string) string { return ns + "/" + name }

func (f *fakeCRUDClient) ListControllerCRDs(_ context.Context, namespace string) ([]*v1alpha1.Controller, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*v1alpha1.Controller
	for _, cr := range f.controllers {
		if namespace == "" || cr.Namespace == namespace {
			out = append(out, cr.DeepCopy())
		}
	}
	return out, nil
}

func (f *fakeCRUDClient) GetControllerCRD(_ context.Context, name, namespace string) (*v1alpha1.Controller, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	cr, ok := f.controllers[f.key(name, namespace)]
	if !ok {
		return nil, k8serrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "controllers"}, name)
	}
	return cr.DeepCopy(), nil
}

func (f *fakeCRUDClient) ApplyControllerSpecSSA(_ context.Context, ns, name string, specPatch map[string]any, _ string, _ bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	if f.ssaApplyErr != nil {
		return nil, nil, f.ssaApplyErr
	}
	if f.applyErr != nil {
		return nil, nil, f.applyErr
	}
	removals, _ := ApplyRemovalPaths(specPatch)
	key := f.key(name, ns)
	existing, ok := f.controllers[key]
	if !ok {
		// Create a new controller from the spec patch.
		cr := &v1alpha1.Controller{
			TypeMeta:   metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
		}
		// Unmarshal specPatch onto the spec.
		specJSON, _ := json.Marshal(specPatch)
		json.Unmarshal(specJSON, &cr.Spec)
		f.controllers[key] = cr
		f.mirrorController(cr)
		return cr.DeepCopy(), UnappliedRemovals(cr, removals), nil
	}
	// Merge sparse spec onto existing.
	existing = existing.DeepCopy()
	specJSON, _ := json.Marshal(specPatch)
	json.Unmarshal(specJSON, &existing.Spec)
	f.controllers[key] = existing
	f.mirrorController(existing)
	return existing.DeepCopy(), UnappliedRemovals(existing, removals), nil
}

// mirrorController writes cr into the crdstore read surface so Store reads see
// the same object the SSA/delete surfaces operate on (in production they are
// the same ClientsetClient).
func (f *fakeCRUDClient) mirrorController(cr *v1alpha1.Controller) {
	if f.store == nil {
		return
	}
	crdstore.MustSeed(f.store, cr)
}

func (f *fakeCRUDClient) DeleteControllerPod(_ context.Context, namespace, name string) error {
	if f.deletePodErr != nil {
		return f.deletePodErr
	}
	return nil
}

func (f *fakeCRUDClient) SetHibernated(_ context.Context, name, namespace string, want bool) (bool, error) {
	if f.setHibernatedErr != nil {
		return false, f.setHibernatedErr
	}
	if !want {
		f.hibernatedClears = append(f.hibernatedClears, namespace+"/"+name)
	}
	return true, nil
}

func (f *fakeCRUDClient) GetComposedBundleCRD(_ context.Context, name, namespace string) (*v1alpha1.ComposedBundle, error) {
	cb, ok := f.composedBundles[f.key(name, namespace)]
	if !ok {
		return nil, k8serrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "composedbundles"}, name)
	}
	return cb, nil
}

func (f *fakeCRUDClient) ApplyComposedBundleCRD(_ context.Context, cr *v1alpha1.ComposedBundle) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.composedBundles[f.key(cr.Name, cr.Namespace)] = cr
	return nil
}

func (f *fakeCRUDClient) DeleteComposedBundleCRD(_ context.Context, name, namespace string) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	delete(f.composedBundles, f.key(name, namespace))
	return nil
}

func (f *fakeCRUDClient) GetProvisioningDefaultsCRD(_ context.Context, name string) (*v1alpha1.ProvisioningDefaults, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	pd, ok := f.provisioningDefaults[name]
	if !ok {
		return nil, k8serrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "provisioningdefaults"}, name)
	}
	return pd, nil
}

func (f *fakeCRUDClient) GetControllerClassCRD(_ context.Context, name string) (*v1alpha1.ControllerClass, error) {
	return nil, nil
}

func (f *fakeCRUDClient) ListControllerClassCRDs(_ context.Context) ([]*v1alpha1.ControllerClass, error) {
	return nil, nil
}

// commandCRUDTest harness.
type commandCRUDTest struct {
	t      *testing.T
	crud   *CommandCRUD
	client *fakeCRUDClient
	store  *crdstore.Fake
}

func newCommandCRUDTest(t *testing.T) *commandCRUDTest {
	t.Helper()
	client := newFakeCRUDClient()
	store := crdstore.NewFake()
	client.store = store
	crud := &CommandCRUD{
		Client:            client,
		Store:             store,
		OperatorNamespace: "varroa-system",
		ManagedNamespaces: "",
		Logger:            slog.Default(),
		PreflightCheck: func(_ context.Context, _ PreflightDepsInterface, _ *v1alpha1.Controller, _ *v1alpha1.ComposedBundleSpec, _ PreflightOptions) []bus.Check {
			return nil
		},
	}
	return &commandCRUDTest{t: t, crud: crud, client: client, store: store}
}

// seedController stores a Controller in both the client fake map (the SSA
// write/assert surface) and the crdstore fake (the read surface), keeping the
// two views coherent at seed time.
func (ct *commandCRUDTest) seedController(cr *v1alpha1.Controller) {
	ct.client.controllers[cr.Namespace+"/"+cr.Name] = cr
	crdstore.MustSeed(ct.store, cr)
}

func (ct *commandCRUDTest) setPreflightFailing(checks ...bus.Check) {
	ct.crud.PreflightCheck = func(_ context.Context, _ PreflightDepsInterface, _ *v1alpha1.Controller, _ *v1alpha1.ComposedBundleSpec, _ PreflightOptions) []bus.Check {
		return checks
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// flakyMetaStore wraps a crdstore.Backend and fails the first failN
// PatchObjectMeta calls with err, then delegates to the wrapped backend. It
// counts every attempt so tests can assert the retry loop was exercised.
// Test-local only: HandleCreate runs synchronously in one goroutine.
type flakyMetaStore struct {
	crdstore.Backend
	attempts int
	failN    int
	err      error
}

func (f *flakyMetaStore) PatchObjectMeta(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, meta map[string]any) error {
	f.attempts++
	if f.attempts <= f.failN {
		return f.err
	}
	return f.Backend.PatchObjectMeta(ctx, gvr, namespace, name, meta)
}

// --- Tests ---

func TestCommandCRUD_CreateHappyPath(t *testing.T) {
	ct := newCommandCRUDTest(t)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" {
		t.Fatalf("expected success, got code=%q error=%q", createResp.Code, createResp.Error)
	}
	if createResp.Item == nil {
		t.Fatal("expected non-nil item")
	}
}

func TestCommandCRUD_CreateDryRunPersistsNothing(t *testing.T) {
	ct := newCommandCRUDTest(t)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dryrun-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
		DryRun:     true,
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" {
		t.Fatalf("expected success, got code=%q error=%q", createResp.Code, createResp.Error)
	}
	// Nothing persisted.
	if _, err := ct.client.GetControllerCRD(context.Background(), "dryrun-ctrl", "team-a"); err == nil {
		t.Fatal("expected controller not found after dry run")
	}
}

func TestCommandCRUD_CreatePreflightFailInvalid(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.setPreflightFailing(bus.Check{ID: "version", Status: "fail", Message: "version too low"})

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "fail-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != bus.CodeInvalid {
		t.Fatalf("expected code=%q, got %q", bus.CodeInvalid, createResp.Code)
	}
	// Pin that this is specifically a below-baseline VERSION failure surfacing,
	// not just any generic preflight fail: the version check must come back.
	if len(createResp.Checks) != 1 || createResp.Checks[0].ID != "version" || createResp.Checks[0].Status != "fail" {
		t.Fatalf("expected the below-baseline version check surfaced, got %+v", createResp.Checks)
	}
	if createResp.Checks[0].Message != "version too low" {
		t.Errorf("check message = %q, want %q", createResp.Checks[0].Message, "version too low")
	}
}

func TestCommandCRUD_CreateNameCollisionConflict(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.setPreflightFailing(bus.Check{ID: "name", Status: "fail", Message: "name collision"})

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != bus.CodeConflict {
		t.Fatalf("expected code=%q (name check), got %q", bus.CodeConflict, createResp.Code)
	}
}

func TestCommandCRUD_CreateInlineBundleDiffSpecConflict(t *testing.T) {
	ct := newCommandCRUDTest(t)
	// Pre-create a bundle with a different input spec.
	ct.client.composedBundles["team-a/new-ctrl-bundle"] = &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "new-ctrl-bundle", Namespace: "team-a"},
		Spec:       v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "old-item", Namespace: "team-a"}}}},
	}
	crdstore.MustSeed(ct.store, ct.client.composedBundles["team-a/new-ctrl-bundle"])

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "new-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
		Bundle:     mustMarshal(t, &v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "new-item", Namespace: "team-a"}}}}),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != bus.CodeConflict {
		t.Fatalf("expected code=%q for inline bundle diff spec, got %q", bus.CodeConflict, createResp.Code)
	}
}

func TestCommandCRUD_CreateApplyFailureCleansUpBundle(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.client.ssaApplyErr = k8serrors.NewInternalError(k8serrors.NewTimeoutError("test timeout", 0))

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "fail-apply", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
		Bundle:     mustMarshal(t, &v1alpha1.ComposedBundleSpec{}),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code == "" {
		t.Fatal("expected error code after apply failure")
	}
	// Bundle should be cleaned up.
	if _, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), ct.store, "fail-apply-bundle", "team-a"); err == nil {
		t.Fatal("expected composed bundle to be cleaned up after apply failure")
	}
}

// TestCommandCRUD_CreateMetadataStampRetriesOnTransientFailure pins the F1
// follow-on: once the SSA apply has persisted the Controller, a transient
// metadata stamp failure must not delete the inline bundle or reply with an
// error — the create has already succeeded and an error reply would wedge a
// retry against the duplicate-name preflight. Instead the stamp is retried a
// bounded number of times, and here the retry self-heals: the first two
// attempts fail, the third succeeds, and the labels actually land.
func TestCommandCRUD_CreateMetadataStampRetriesOnTransientFailure(t *testing.T) {
	ct := newCommandCRUDTest(t)
	store := &flakyMetaStore{Backend: ct.store, failN: 2, err: k8serrors.NewTimeoutError("apiserver timeout", 0)}
	ct.crud.Store = store

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "meta-retry",
			Namespace: "team-a",
			Labels:    map[string]string{"team": "platform"},
		},
		Spec: v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
		Bundle:     mustMarshal(t, &v1alpha1.ComposedBundleSpec{}),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" || createResp.Error != "" {
		t.Fatalf("expected success, got code=%q error=%q", createResp.Code, createResp.Error)
	}
	if createResp.Item == nil {
		t.Fatal("expected non-nil item")
	}
	// A successful create carries no checks: the 201 body has no warning channel.
	if len(createResp.Checks) != 0 {
		t.Fatalf("checks = %+v, want none on a successful create", createResp.Checks)
	}

	// The retry was exercised: two injected failures then one success.
	if store.attempts != 3 {
		t.Fatalf("PatchObjectMeta attempts = %d, want 3 (two failures then success)", store.attempts)
	}
	// The labels actually landed.
	patches := ct.store.MetaPatches(controllerGVR)
	if len(patches) != 1 {
		t.Fatalf("expected exactly 1 recorded metadata patch, got %d", len(patches))
	}
	if labels, ok := patches[0].Meta["labels"].(map[string]string); !ok || labels["team"] != "platform" {
		t.Fatalf("recorded metadata labels = %#v, want {team: platform}", patches[0].Meta["labels"])
	}

	// The inline bundle must NOT be deleted.
	if _, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), ct.store, "meta-retry-bundle", "team-a"); err != nil {
		t.Fatalf("expected composed bundle to survive metadata stamp retries, got %v", err)
	}
	// The Controller must still exist.
	if _, err := ct.client.GetControllerCRD(context.Background(), "meta-retry", "team-a"); err != nil {
		t.Fatalf("expected controller to survive metadata stamp retries, got %v", err)
	}
}

// TestCommandCRUD_CreateMetadataStampExhaustedStillSucceeds pins the new
// contract: when every bounded metadata stamp attempt fails, the create still
// SUCCEEDS. The SSA apply already persisted the Controller, so the reply is a
// success with a non-nil Item; the Controller and any inline bundle SURVIVE;
// only the request-supplied labels/annotations did not land (log-only — the 201
// body is the Controller CRD with no warning channel). The retry stays bounded
// at retry.DefaultRetry's 5 steps.
func TestCommandCRUD_CreateMetadataStampExhaustedStillSucceeds(t *testing.T) {
	ct := newCommandCRUDTest(t)
	store := &flakyMetaStore{Backend: ct.store, failN: 100, err: k8serrors.NewTimeoutError("apiserver timeout", 0)}
	ct.crud.Store = store

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "meta-exhaust",
			Namespace: "team-a",
			Labels:    map[string]string{"team": "platform"},
		},
		Spec: v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
		Bundle:     mustMarshal(t, &v1alpha1.ComposedBundleSpec{}),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" || createResp.Error != "" {
		t.Fatalf("expected success, got code=%q error=%q", createResp.Code, createResp.Error)
	}
	if createResp.Item == nil {
		t.Fatal("expected non-nil item")
	}

	// The retry stayed bounded at retry.DefaultRetry's 5 steps.
	if store.attempts != 5 {
		t.Fatalf("PatchObjectMeta attempts = %d, want 5 (retry.DefaultRetry steps)", store.attempts)
	}

	// The Controller SURVIVES: the create succeeded and must not be rolled back.
	if _, err := ct.client.GetControllerCRD(context.Background(), "meta-exhaust", "team-a"); err != nil {
		t.Fatalf("expected controller to survive exhausted metadata stamp retries, got %v", err)
	}
	// The inline bundle SURVIVES too.
	if _, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), ct.store, "meta-exhaust-bundle", "team-a"); err != nil {
		t.Fatalf("expected inline bundle to survive exhausted metadata stamp retries, got %v", err)
	}
}

// TestCommandCRUD_CreateReplyCarriesStampedLabels pins the fix for the
// suppressed create review comment: the 201 body must carry the
// request-supplied labels/annotations when the metadata stamp SUCCEEDS. The SSA
// apply runs before the stamp, so `applied` never has the labels; HandleCreate
// must re-read the Controller after a successful stamp and reply with that
// object, otherwise a client that creates a controller with labels gets back a
// body showing none.
func TestCommandCRUD_CreateReplyCarriesStampedLabels(t *testing.T) {
	ct := newCommandCRUDTest(t)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "meta-reply",
			Namespace: "team-a",
			Labels:    map[string]string{"team": "platform"},
		},
		Spec: v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" || createResp.Error != "" {
		t.Fatalf("expected success, got code=%q error=%q", createResp.Code, createResp.Error)
	}
	if createResp.Item == nil {
		t.Fatal("expected non-nil item")
	}

	// The REPLY BODY must carry the labels, not just the stored object.
	var reply v1alpha1.Controller
	if err := json.Unmarshal(createResp.Item, &reply); err != nil {
		t.Fatalf("unmarshal reply item: %v", err)
	}
	if reply.Labels["team"] != "platform" {
		t.Fatalf("reply body labels = %#v, want {team: platform}", reply.Labels)
	}
}

func TestCommandCRUD_UpdateNotFound(t *testing.T) {
	ct := newCommandCRUDTest(t)

	req := bus.ControllersUpdateRequest{
		Namespace: "team-a",
		Name:      "nonexistent",
		Patch:     mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"version": "2.0"}}),
	}
	resp := ct.crud.HandleUpdate(mustMarshal(t, req))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != bus.CodeNotFound {
		t.Fatalf("expected code=%q, got %q", bus.CodeNotFound, updateResp.Code)
	}
}

// TestCommandCRUD_HandleUpdate_BlocksVersionChangeBelowBaseline pins the
// update-path wiring for the version-downgrade safety guard: when preflight
// fails (a below-baseline version CHANGE), HandleUpdate must refuse the update
// with CodeInvalid, surface the failing check, and not reach the apply seam.
func TestCommandCRUD_HandleUpdate_BlocksVersionChangeBelowBaseline(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.479"},
	})

	// The real preflight emits a "version" fail when the version changes below
	// the plugin baseline. Stub that result: the handler must refuse.
	ct.setPreflightFailing(bus.Check{ID: "version", Status: "fail", Message: "core 2.478 is older than the plugin baseline"})

	resp := ct.crud.HandleUpdate(mustMarshal(t, bus.ControllersUpdateRequest{
		Namespace: "team-a",
		Name:      "ctrl1",
		Patch:     mustMarshal(t, map[string]any{"spec": map[string]any{"version": "2.478"}}),
	}))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != bus.CodeInvalid {
		t.Fatalf("code = %q, want %q (error %q)", updateResp.Code, bus.CodeInvalid, updateResp.Error)
	}
	if updateResp.Error != "preflight failed" {
		t.Errorf("error = %q, want %q", updateResp.Error, "preflight failed")
	}
	if len(updateResp.Checks) != 1 || updateResp.Checks[0].ID != "version" || updateResp.Checks[0].Status != "fail" {
		t.Fatalf("checks = %+v, want the surfaced failing version check", updateResp.Checks)
	}
	// The refused update must not reach the apply seam.
	if got := ct.client.controllers["team-a/ctrl1"].Spec.Version; got != "2.479" {
		t.Fatalf("stored version = %q, want unchanged %q (apply must not run)", got, "2.479")
	}
}

// TestCommandCRUD_HandleUpdate_AllowsUnchangedGrandfatheredVersion pins the
// PriorVersion wiring: an update that keeps a grandfathered below-baseline
// version must not be blocked. The real preflight downgrades a below-baseline
// fail to warn only when ForUpdate && PriorVersion == new version; the stub
// reproduces that leniency, so a handler that drops PriorVersion (or
// ForUpdate) fails here.
func TestCommandCRUD_HandleUpdate_AllowsUnchangedGrandfatheredVersion(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.478"}, // grandfathered below-baseline
	})

	var gotForUpdate bool
	var gotPriorVersion string
	ct.crud.PreflightCheck = func(_ context.Context, _ PreflightDepsInterface, draft *v1alpha1.Controller, _ *v1alpha1.ComposedBundleSpec, opts PreflightOptions) []bus.Check {
		gotForUpdate = opts.ForUpdate
		gotPriorVersion = opts.PriorVersion
		if opts.ForUpdate && draft.Spec.Version == opts.PriorVersion {
			return []bus.Check{{ID: "version", Status: "warn", Message: "pre-existing version, unchanged by this update"}}
		}
		return []bus.Check{{ID: "version", Status: "fail", Message: "core is older than the plugin baseline"}}
	}

	// Patch an unrelated field; version stays at the already-unsafe prior value.
	resp := ct.crud.HandleUpdate(mustMarshal(t, bus.ControllersUpdateRequest{
		Namespace: "team-a",
		Name:      "ctrl1",
		Patch:     mustMarshal(t, map[string]any{"spec": map[string]any{"className": "small"}}),
	}))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != "" || updateResp.Error != "" {
		t.Fatalf("expected success for unchanged grandfathered version, got code=%q error=%q", updateResp.Code, updateResp.Error)
	}
	if !gotForUpdate || gotPriorVersion != "2.478" {
		t.Fatalf("preflight got ForUpdate=%v PriorVersion=%q, want true / %q", gotForUpdate, gotPriorVersion, "2.478")
	}
}

// TestCommandCRUD_HandleUpdate_SelfNameCollisionPasses pins the ForUpdate
// wiring for the name check: updating an existing controller must not trip the
// name-collision check that a create of the same name would. The stub
// reproduces the real checkName ForUpdate leniency.
func TestCommandCRUD_HandleUpdate_SelfNameCollisionPasses(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.479"},
	})

	ct.crud.PreflightCheck = func(_ context.Context, _ PreflightDepsInterface, draft *v1alpha1.Controller, _ *v1alpha1.ComposedBundleSpec, opts PreflightOptions) []bus.Check {
		if opts.ForUpdate && draft.Name == "ctrl1" {
			return []bus.Check{{ID: "name", Status: "pass", Message: "name unchanged (update)"}}
		}
		return []bus.Check{{ID: "name", Status: "fail", Message: "a controller named ctrl1 already exists in namespace team-a"}}
	}

	resp := ct.crud.HandleUpdate(mustMarshal(t, bus.ControllersUpdateRequest{
		Namespace: "team-a",
		Name:      "ctrl1",
		Patch:     mustMarshal(t, map[string]any{"spec": map[string]any{"className": "small"}}),
	}))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != "" || updateResp.Error != "" {
		t.Fatalf("expected success for self-name update, got code=%q error=%q", updateResp.Code, updateResp.Error)
	}
}

// lifecycleStub returns a LifecycleStateReader that always returns a given state.
type lifecycleStub struct {
	state LifecycleState
}

func (s *lifecycleStub) State() LifecycleState { return s.state }

func TestCommandCRUD_CreateBlockedWhenDraining(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.crud.Lifecycle = &lifecycleStub{state: LifecycleState{State: bus.ClusterStateDraining}}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != bus.CodeDraining {
		t.Errorf("code = %q, want %q", createResp.Code, bus.CodeDraining)
	}
	if createResp.Error == "" {
		t.Error("expected non-empty error")
	}
}

func TestCommandCRUD_CreateBlockedWhenDrained(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.crud.Lifecycle = &lifecycleStub{state: LifecycleState{State: bus.ClusterStateDrained}}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != bus.CodeDraining {
		t.Errorf("code = %q, want %q", createResp.Code, bus.CodeDraining)
	}
}

func TestCommandCRUD_DryRunBlockedWhenDraining(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.crud.Lifecycle = &lifecycleStub{state: LifecycleState{State: bus.ClusterStateDraining}}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dryrun-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
		DryRun:     true,
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != bus.CodeDraining {
		t.Errorf("code = %q, want %q", createResp.Code, bus.CodeDraining)
	}
}

func TestCommandCRUD_CreateProceedsWhenActive(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.crud.Lifecycle = &lifecycleStub{state: LifecycleState{State: bus.ClusterStateActive}}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" {
		t.Errorf("expected success code, got %q", createResp.Code)
	}
}

func TestCommandCRUD_CreateProceedsWhenLifecycleNil(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.crud.Lifecycle = nil

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" {
		t.Errorf("expected success code, got %q", createResp.Code)
	}
}

func TestCommandCRUD_HandleDeleteWorksWithDrainingLifecycle(t *testing.T) {
	ct := newCommandCRUDTest(t)

	// Create a controller first (without lifecycle blocking).
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "delete-me", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
	}
	_ = ct.crud.HandleCreate(mustMarshal(t, req))

	// Now set the lifecycle to draining and verify delete still works.
	ct.crud.Lifecycle = &lifecycleStub{state: LifecycleState{State: bus.ClusterStateDraining}}

	delReq := bus.ControllersDeleteRequest{Namespace: "team-a", Name: "delete-me"}
	resp := ct.crud.HandleDelete(mustMarshal(t, delReq))
	var delResp bus.ControllersDeleteResponse
	if err := json.Unmarshal(resp, &delResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if delResp.Code != "" {
		t.Errorf("expected success, got code=%q error=%q", delResp.Code, delResp.Error)
	}
}

func TestCommandCRUD_DeleteNotFound(t *testing.T) {
	ct := newCommandCRUDTest(t)

	req := bus.ControllersDeleteRequest{Namespace: "team-a", Name: "nope"}
	resp := ct.crud.HandleDelete(mustMarshal(t, req))
	var delResp bus.ControllersDeleteResponse
	if err := json.Unmarshal(resp, &delResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if delResp.Code != "" {
		t.Fatalf("expected empty code (idempotent delete), got %q", delResp.Code)
	}
}

func TestCommandCRUD_ListStripsManagedFields(t *testing.T) {
	ct := newCommandCRUDTest(t)

	// Seed a controller with managedFields.
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "mc",
			Namespace:     "team-a",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "test", Operation: "Update"}},
		},
		Spec: v1alpha1.ControllerSpec{Version: "2.0"},
	})

	req := bus.ControllersListRequest{Namespace: "team-a"}
	resp := ct.crud.HandleList(mustMarshal(t, req))
	var listResp bus.ControllersListResponse
	if err := json.Unmarshal(resp, &listResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(listResp.Items))
	}

	var ctrl v1alpha1.Controller
	if err := json.Unmarshal(listResp.Items[0], &ctrl); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if len(ctrl.ManagedFields) > 0 {
		t.Fatal("expected managedFields to be stripped")
	}
}

func TestCommandCRUD_ListOverBudget(t *testing.T) {
	ct := newCommandCRUDTest(t)

	bigAnn := make(map[string]string)
	for i := 0; i < 100; i++ {
		bigAnn[fmt.Sprintf("key-%d", i)] = strings.Repeat("x", 2000)
	}
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("ctrl-%d", i)
		ct.seedController(&v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Annotations: bigAnn},
			Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
		})
	}

	req := bus.ControllersListRequest{}
	resp := ct.crud.HandleList(mustMarshal(t, req))
	var listResp bus.ControllersListResponse
	if err := json.Unmarshal(resp, &listResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if listResp.Code != bus.CodeInternal {
		t.Fatalf("expected code=%q (list too large), got %q", bus.CodeInternal, listResp.Code)
	}
	if listResp.Error != "list too large" {
		t.Fatalf("expected error 'list too large', got %q", listResp.Error)
	}
}

func TestCommandCRUD_HandleGetNotFound(t *testing.T) {
	ct := newCommandCRUDTest(t)

	req := bus.ControllersGetRequest{Namespace: "team-a", Name: "missing"}
	resp := ct.crud.HandleGet(mustMarshal(t, req))
	var getResp bus.ControllersGetResponse
	if err := json.Unmarshal(resp, &getResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if getResp.Code != bus.CodeNotFound {
		t.Fatalf("expected code=%q, got %q", bus.CodeNotFound, getResp.Code)
	}
}

func TestCommandCRUD_HandleNamespacesList_DefaultsPresent(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.client.provisioningDefaults["varroa-defaults"] = &v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		Spec: v1alpha1.ProvisioningDefaultsSpec{
			DefaultNamespace: "team-a",
			Namespaces:       []string{"team-a", "team-b"},
		},
	}
	crdstore.MustSeed(ct.store, ct.client.provisioningDefaults["varroa-defaults"])

	resp := ct.crud.HandleNamespacesList(mustMarshal(t, bus.NamespacesListRequest{}))
	var nsResp bus.NamespacesListResponse
	if err := json.Unmarshal(resp, &nsResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if nsResp.Code != "" {
		t.Fatalf("expected success, got code=%q error=%q", nsResp.Code, nsResp.Error)
	}
	if len(nsResp.CuratedNamespaces) != 2 || nsResp.CuratedNamespaces[0] != "team-a" || nsResp.CuratedNamespaces[1] != "team-b" {
		t.Errorf("curated = %v, want [team-a team-b]", nsResp.CuratedNamespaces)
	}
	if nsResp.CuratedDefault != "team-a" {
		t.Errorf("curatedDefault = %q, want team-a", nsResp.CuratedDefault)
	}
}

func TestCommandCRUD_HandleNamespacesList_CRDAbsent(t *testing.T) {
	ct := newCommandCRUDTest(t)
	// No provisioning defaults seeded → IsNotFound

	resp := ct.crud.HandleNamespacesList(mustMarshal(t, bus.NamespacesListRequest{}))
	var nsResp bus.NamespacesListResponse
	if err := json.Unmarshal(resp, &nsResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if nsResp.Code != "" {
		t.Fatalf("expected success, got code=%q error=%q", nsResp.Code, nsResp.Error)
	}
	if len(nsResp.CuratedNamespaces) != 1 || nsResp.CuratedNamespaces[0] != "varroa" {
		t.Errorf("curated = %v, want [varroa]", nsResp.CuratedNamespaces)
	}
	if nsResp.CuratedDefault != "varroa" {
		t.Errorf("curatedDefault = %q, want varroa", nsResp.CuratedDefault)
	}
}

func TestCommandCRUD_HandleNamespacesList_ManagedParsing(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.crud.ManagedNamespaces = "ns-a, ns-b"

	resp := ct.crud.HandleNamespacesList(mustMarshal(t, bus.NamespacesListRequest{}))
	var nsResp bus.NamespacesListResponse
	if err := json.Unmarshal(resp, &nsResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if nsResp.Code != "" {
		t.Fatalf("expected success, got code=%q error=%q", nsResp.Code, nsResp.Error)
	}
	if len(nsResp.ManagedNamespaces) != 2 || nsResp.ManagedNamespaces[0] != "ns-a" || nsResp.ManagedNamespaces[1] != "ns-b" {
		t.Errorf("managed = %v, want [ns-a ns-b]", nsResp.ManagedNamespaces)
	}

	// Empty string → nil
	ct.crud.ManagedNamespaces = ""
	resp = ct.crud.HandleNamespacesList(mustMarshal(t, bus.NamespacesListRequest{}))
	var nsResp2 bus.NamespacesListResponse
	if err := json.Unmarshal(resp, &nsResp2); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if nsResp2.ManagedNamespaces != nil {
		t.Errorf("managed = %v, want nil", nsResp2.ManagedNamespaces)
	}
}

func TestCommandCRUD_HandleNamespacesList_ReadError(t *testing.T) {
	ct := newCommandCRUDTest(t)
	pdGVR, err := crdstore.GVRFor[v1alpha1.ProvisioningDefaults]()
	if err != nil {
		t.Fatal(err)
	}
	ct.store.FailAlways("get", pdGVR, k8serrors.NewInternalError(k8serrors.NewTimeoutError("test timeout", 0)))

	resp := ct.crud.HandleNamespacesList(mustMarshal(t, bus.NamespacesListRequest{}))
	var nsResp bus.NamespacesListResponse
	if err := json.Unmarshal(resp, &nsResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if nsResp.Code != bus.CodeInternal {
		t.Errorf("code = %q, want %q", nsResp.Code, bus.CodeInternal)
	}
	if nsResp.Error == "" {
		t.Error("expected non-empty error message")
	}
}

// TestCommandCRUD_HandleUpdateConflictThenForce validates that HandleUpdate
// returns a conflict response with non-empty Conflicts when SSA returns a
// conflict, and succeeds when Force is true.
func TestCommandCRUD_HandleUpdateConflictThenForce(t *testing.T) {
	ct := newCommandCRUDTest(t)

	// Seed a controller.
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0", ClassName: "small"},
	})

	// First call: conflict without force.
	ct.client.ssaApplyErr = k8serrors.NewApplyConflict(
		[]metav1.StatusCause{
			{Type: metav1.CauseTypeFieldManagerConflict, Field: ".spec.version", Message: `conflict with "other-manager" using varroa.dev/v1alpha1`},
		},
		"Apply failed with 1 conflict",
	)
	req := bus.ControllersUpdateRequest{
		Namespace: "team-a",
		Name:      "ctrl1",
		Patch:     mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"version": "2.0"}}),
	}
	resp := ct.crud.HandleUpdate(mustMarshal(t, req))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != bus.CodeConflict {
		t.Fatalf("expected code=%q, got %q", bus.CodeConflict, updateResp.Code)
	}
	if len(updateResp.Conflicts) == 0 {
		t.Fatal("expected non-empty Conflicts on conflict response")
	}
	if updateResp.Conflicts[0].Field != ".spec.version" {
		t.Errorf("Conflict.Field = %q, want %q", updateResp.Conflicts[0].Field, ".spec.version")
	}
	if updateResp.Conflicts[0].Manager != "other-manager" {
		t.Errorf("Conflict.Manager = %q, want %q", updateResp.Conflicts[0].Manager, "other-manager")
	}

	// Second call: same patch with Force=true should succeed.
	ct.client.ssaApplyErr = nil
	req.Force = true
	req.FieldManager = "varroa-ui"
	resp2 := ct.crud.HandleUpdate(mustMarshal(t, req))
	var updateResp2 bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp2, &updateResp2); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp2.Code != "" {
		t.Errorf("expected success with Force=true, got code=%q error=%q", updateResp2.Code, updateResp2.Error)
	}
	if updateResp2.Item == nil {
		t.Fatal("expected non-nil Item on success")
	}

	// Verify the stored controller was updated.
	stored := ct.client.controllers["team-a/ctrl1"]
	if stored.Spec.Version != "2.0" {
		t.Errorf("stored version = %q, want %q", stored.Spec.Version, "2.0")
	}
}

// A create replay — the same request arriving again after the first reply was
// lost to a client timeout or transport reconnect — must converge on the
// existing object instead of failing the name-exists preflight. The create
// tools advertise idempotentHint=true; "create failed" for a create that
// succeeded is the bug this pins.
func TestCommandCRUD_CreateIdenticalReplayConverges(t *testing.T) {
	ct := newCommandCRUDTest(t)
	crdstore.MustSeed(ct.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	})
	// The real name-exists preflight would reject the replay; convergence must
	// happen before preflight ever runs.
	ct.setPreflightFailing(bus.Check{ID: "name", Status: "fail", Message: "name collision"})

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{Namespace: "team-a", Controller: mustMarshal(t, cr)}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" || createResp.Error != "" {
		t.Fatalf("expected idempotent success, got code=%q error=%q", createResp.Code, createResp.Error)
	}
	var item v1alpha1.Controller
	if err := json.Unmarshal(createResp.Item, &item); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if item.Name != "dup" {
		t.Fatalf("expected existing controller back, got %q", item.Name)
	}
}

// A same-name create with a DIFFERENT spec is a genuine conflict, not a
// replay — it must keep failing preflight.
func TestCommandCRUD_CreateDifferentSpecStillConflicts(t *testing.T) {
	ct := newCommandCRUDTest(t)
	crdstore.MustSeed(ct.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	})
	ct.setPreflightFailing(bus.Check{ID: "name", Status: "fail", Message: "name collision"})

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "3.0"},
	}
	req := bus.ControllersCreateRequest{Namespace: "team-a", Controller: mustMarshal(t, cr)}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != bus.CodeConflict {
		t.Fatalf("expected code=%q for different-spec collision, got %q", bus.CodeConflict, createResp.Code)
	}
}

// An inline-bundle create replay is only idempotent when the bundle content
// matches too — the controller spec alone is identical either way because the
// ComposedBundleRef name is derived from the controller name.
func TestCommandCRUD_CreateInlineBundleReplayConverges(t *testing.T) {
	ct := newCommandCRUDTest(t)
	bundleSpec := v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "item", Namespace: "team-a"}}}}
	crdstore.MustSeed(ct.store,
		&v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
			Spec: v1alpha1.ControllerSpec{
				Version:           "2.0",
				ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "dup-bundle"},
			},
		},
		&v1alpha1.ComposedBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "dup-bundle", Namespace: "team-a"},
			Spec:       bundleSpec,
		},
	)
	ct.setPreflightFailing(bus.Check{ID: "name", Status: "fail", Message: "name collision"})

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
		Bundle:     mustMarshal(t, &bundleSpec),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" || createResp.Error != "" {
		t.Fatalf("expected idempotent success, got code=%q error=%q", createResp.Code, createResp.Error)
	}
}

// A replay whose inline bundle content DIFFERS from the stored bundle must not
// converge — success here would silently leave the controller on stale
// configuration.
func TestCommandCRUD_CreateInlineBundleReplayContentDiffers(t *testing.T) {
	ct := newCommandCRUDTest(t)
	crdstore.MustSeed(ct.store,
		&v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
			Spec: v1alpha1.ControllerSpec{
				Version:           "2.0",
				ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "dup-bundle"},
			},
		},
		&v1alpha1.ComposedBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "dup-bundle", Namespace: "team-a"},
			Spec:       v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "old-item", Namespace: "team-a"}}}},
		},
	)
	ct.setPreflightFailing(bus.Check{ID: "name", Status: "fail", Message: "name collision"})

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	newBundle := v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "new-item", Namespace: "team-a"}}}}
	req := bus.ControllersCreateRequest{
		Namespace:  "team-a",
		Controller: mustMarshal(t, cr),
		Bundle:     mustMarshal(t, &newBundle),
	}

	resp := ct.crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code == "" {
		t.Fatal("expected a conflict for a replay with different inline bundle content")
	}
}

// newDynamicSSACapture builds a ClientsetClient backed by a dynamic fake whose
// patch reactor records every ApplyPatchType body, so a test can assert "no
// apply was attempted".
func newDynamicSSACapture(t *testing.T) (*ClientsetClient, *[]map[string]any) {
	t.Helper()
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "Controller"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControllerList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{controllerGVR: "ControllerList"},
	)
	c := &ClientsetClient{dynamic: dyn}
	var captured []map[string]any
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchActionImpl)
		if pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		var body map[string]any
		if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		captured = append(captured, body)
		return true, &unstructured.Unstructured{Object: body}, nil
	})
	return c, &captured
}

// TestCommandCRUD_UpdateNullInListRejectedInvalid (5.10, brood route): a patch
// with a null inside a list is invalid input. ApplyControllerSpecSSA rejects
// it with ErrNullInList (translated before any Get/Patch), and HandleUpdate
// maps that to CodeInvalid, which the BFF renders as 400. No apply is
// attempted: the patch reactor is never called.
func TestCommandCRUD_UpdateNullInListRejectedInvalid(t *testing.T) {
	ct := newCommandCRUDTest(t)
	crdstore.MustSeed(ct.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	})

	// Drive the REAL seam (a real ClientsetClient) so translateNulls actually
	// runs; the patch reactor must never fire.
	realClient, captured := newDynamicSSACapture(t)
	ct.crud.Client = realClient

	req := bus.ControllersUpdateRequest{
		Namespace: "team-a",
		Name:      "ctrl1",
		Patch: mustMarshal(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"podOverrides": map[string]interface{}{
					"tolerations": []interface{}{nil},
				},
			},
		}),
	}
	resp := ct.crud.HandleUpdate(mustMarshal(t, req))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != bus.CodeInvalid {
		t.Fatalf("code = %q, want %q (error %q)", updateResp.Code, bus.CodeInvalid, updateResp.Error)
	}
	if updateResp.Error == "" {
		t.Error("expected a non-empty error message")
	}
	if len(*captured) != 0 {
		t.Fatalf("apply attempted %d time(s) — a null inside a list must be rejected before any apply", len(*captured))
	}
}

// TestCommandCRUD_HandleUpdate_ReportsUnappliedRemovals (6.4, Brood route): a
// removal expressed as null only takes effect for a leaf the applying manager
// can release. Where the applied object still carries the field (another
// manager owns it), HandleUpdate reports it in unappliedRemovals; where the
// removal actually took effect, it reports nothing. Both cases share the
// marshalled-JSON presence check with the BFF's local route, so the two routes
// cannot diverge.
func TestCommandCRUD_HandleUpdate_ReportsUnappliedRemovals(t *testing.T) {
	t.Run("removal blocked by another owner is reported", func(t *testing.T) {
		ct := newCommandCRUDTest(t)
		// Seed a controller with version set. The fake's ApplyControllerSpecSSA
		// keeps version when the patch nulls it (json.Unmarshal of null into a
		// string field has no effect), standing in for another manager owning it.
		ct.seedController(&v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{Version: "2.479"},
		})

		req := bus.ControllersUpdateRequest{
			Namespace: "team-a",
			Name:      "ctrl1",
			Patch:     mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"version": nil}}),
		}
		resp := ct.crud.HandleUpdate(mustMarshal(t, req))
		var updateResp bus.ControllersUpdateResponse
		if err := json.Unmarshal(resp, &updateResp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if updateResp.Code != "" || updateResp.Error != "" {
			t.Fatalf("expected success, got code=%q error=%q", updateResp.Code, updateResp.Error)
		}
		want := []bus.UnappliedRemoval{{Field: "spec.version"}}
		if !reflect.DeepEqual(updateResp.UnappliedRemovals, want) {
			t.Fatalf("unappliedRemovals = %+v, want %+v", updateResp.UnappliedRemovals, want)
		}
	})

	t.Run("removal that takes effect is not reported", func(t *testing.T) {
		ct := newCommandCRUDTest(t)
		ct.seedController(&v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-x"}},
		})

		req := bus.ControllersUpdateRequest{
			Namespace: "team-a",
			Name:      "ctrl1",
			Patch:     mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"composedBundleRef": nil}}),
		}
		resp := ct.crud.HandleUpdate(mustMarshal(t, req))
		var updateResp bus.ControllersUpdateResponse
		if err := json.Unmarshal(resp, &updateResp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if updateResp.Code != "" || updateResp.Error != "" {
			t.Fatalf("expected success, got code=%q error=%q", updateResp.Code, updateResp.Error)
		}
		if len(updateResp.UnappliedRemovals) != 0 {
			t.Fatalf("unappliedRemovals = %+v, want none (composedBundleRef was removed)", updateResp.UnappliedRemovals)
		}
	})
}

// TestCommandCRUD_HandleUpdate_RealSeam_ReportsRetainedZeroValue runs the
// Brood route (CommandCRUD.HandleUpdate) against the REAL server-side-apply
// seam — a *ClientsetClient backed by a dynamic fake — rather than a test
// double that hardcodes the answer. The apiserver (patch reactor) returns an
// applied object that still carries spec.hibernation.enabled=false after a
// null removal request, i.e. another manager retained the field at its ZERO
// value. The real seam must report it as unapplied, end-to-end through the
// route. The typed presence wrapper (UnappliedRemovals) would MISS it
// (omitempty drops enabled=false), so this guards the exact round-1 regression
// that a fake-based parity test cannot catch.
func TestCommandCRUD_HandleUpdate_RealSeam_ReportsRetainedZeroValue(t *testing.T) {
	// The real seam (clientset_client.go) runs against a dynamic fake whose
	// patch reactor simulates the apiserver retaining hibernation.enabled=false
	// after the removal. No stored object is seeded on the dynamic client: the
	// seam's completion Get returns NotFound and applies the cleaned patch
	// as-is; the reactor supplies the post-SSA applied object.
	h := newSSAHarness(t, nil, map[string]any{
		"hibernation": map[string]any{"enabled": false},
	})

	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "ns1"},
		Spec:       v1alpha1.ControllerSpec{ClassName: "standard"},
	})
	crud := &CommandCRUD{
		Client:            h.client,
		Store:             store,
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	}

	req := bus.ControllersUpdateRequest{
		Namespace: "ns1",
		Name:      "ctrl1",
		Patch: mustMarshal(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"hibernation": map[string]interface{}{"enabled": nil},
			},
		}),
	}
	resp := crud.HandleUpdate(mustMarshal(t, req))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != "" || updateResp.Error != "" {
		t.Fatalf("expected success, got code=%q error=%q", updateResp.Code, updateResp.Error)
	}

	want := []bus.UnappliedRemoval{{Field: "spec.hibernation.enabled"}}
	if !reflect.DeepEqual(updateResp.UnappliedRemovals, want) {
		t.Fatalf("unappliedRemovals = %+v, want %+v — the real seam must report a retained zero value end-to-end", updateResp.UnappliedRemovals, want)
	}
}

// TestCommandCRUD_Create_ScopedWrites (5.9): HandleCreate persists the spec via
// server-side apply whose manifest carries ONLY metadata.name/namespace — no
// labels, annotations, or finalizers — and writes the request-supplied labels
// and annotations in a SEPARATE metadata merge patch from the operator's
// client. Because the SSA manifest never carries metadata beyond
// name/namespace, varroa-ui can never own the create-time labels, so a later
// varroa-ui spec apply that omits them cannot delete them (SSA deletes only
// what the applying manager owns). This is also the "no operator-owned spec
// leaves" pin: the only spec write is the varroa-ui apply of the supplied spec,
// and the operator's own write is a metadata-only merge patch.
func TestCommandCRUD_Create_ScopedWrites(t *testing.T) {
	h := newSSAHarness(t, nil)
	store := crdstore.NewFake()
	crud := &CommandCRUD{
		Client:            h.client,
		Store:             store,
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
		PreflightCheck: func(_ context.Context, _ PreflightDepsInterface, _ *v1alpha1.Controller, _ *v1alpha1.ComposedBundleSpec, _ PreflightOptions) []bus.Check {
			return nil
		},
	}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-ctrl",
			Namespace:   "team-a",
			Labels:      map[string]string{"team": "platform"},
			Annotations: map[string]string{"owner": "alice"},
		},
		Spec: v1alpha1.ControllerSpec{Version: "2.0"},
	}
	req := bus.ControllersCreateRequest{Namespace: "team-a", Controller: mustMarshal(t, cr)}
	resp := crud.HandleCreate(mustMarshal(t, req))
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if createResp.Code != "" {
		t.Fatalf("expected success, got code=%q error=%q", createResp.Code, createResp.Error)
	}

	// Exactly one SSA apply, and its manifest metadata is name/namespace only.
	if len(h.captured) != 1 {
		t.Fatalf("expected exactly 1 SSA apply, got %d", len(h.captured))
	}
	body := h.captured[0]
	meta, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("SSA manifest has no metadata: %#v", body)
	}
	if len(meta) != 2 || meta["name"] != "my-ctrl" || meta["namespace"] != "team-a" {
		t.Fatalf("SSA manifest metadata = %#v, want exactly {name, namespace} (no labels/annotations/finalizers)", meta)
	}
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("SSA manifest has no spec: %#v", body)
	}
	if spec["version"] != "2.0" {
		t.Fatalf("SSA manifest spec = %#v, want version 2.0", spec)
	}

	// The labels/annotations ride a separate metadata merge patch.
	patches := store.MetaPatches(controllerGVR)
	if len(patches) != 1 {
		t.Fatalf("expected exactly 1 metadata merge patch, got %d", len(patches))
	}
	p := patches[0]
	if p.Name != "my-ctrl" || p.Namespace != "team-a" {
		t.Fatalf("metadata patch target = %s/%s, want team-a/my-ctrl", p.Namespace, p.Name)
	}
	labels, ok := p.Meta["labels"].(map[string]string)
	if !ok || labels["team"] != "platform" {
		t.Fatalf("metadata patch labels = %#v, want {team: platform}", p.Meta["labels"])
	}
	annotations, ok := p.Meta["annotations"].(map[string]string)
	if !ok || annotations["owner"] != "alice" {
		t.Fatalf("metadata patch annotations = %#v, want {owner: alice}", p.Meta["annotations"])
	}
	if _, hasSpec := p.Meta["spec"]; hasSpec {
		t.Fatalf("metadata patch must not carry spec: %#v", p.Meta)
	}
}

// TestCommandCRUD_HandleUpdate_PowerStateClearsHibernation (5.5): a power
// write through the UI/MCP/CLI path must clear status.hibernated via
// SetHibernated, or a coalesced Stop→Start leaves Running + hibernated=true and
// the controller re-hibernates. The clear is only issued when the patch
// actually writes spec.powerState.
func TestCommandCRUD_HandleUpdate_PowerStateClearsHibernation(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	})

	// A non-power patch does not clear.
	ct.crud.HandleUpdate(mustMarshal(t, bus.ControllersUpdateRequest{
		Namespace: "team-a", Name: "ctrl1",
		Patch: mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"version": "2.0"}}),
	}))
	if len(ct.client.hibernatedClears) != 0 {
		t.Fatalf("version-only patch cleared hibernation %d time(s)", len(ct.client.hibernatedClears))
	}

	// A power patch does.
	ct.crud.HandleUpdate(mustMarshal(t, bus.ControllersUpdateRequest{
		Namespace: "team-a", Name: "ctrl1",
		Patch: mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"powerState": "Running"}}),
	}))
	if len(ct.client.hibernatedClears) != 1 || ct.client.hibernatedClears[0] != "team-a/ctrl1" {
		t.Fatalf("hibernated clears = %v, want exactly [team-a/ctrl1]", ct.client.hibernatedClears)
	}
}

// TestCommandCRUD_HandleUpdate_PowerStateEmptyStringClearsHibernation pins the
// F2 key-presence guard: clearing powerState back to "" (the default Running)
// is still a power write and must clear status.hibernated. The previous
// value-based guard skipped it, leaving a parked controller stuck.
func TestCommandCRUD_HandleUpdate_PowerStateEmptyStringClearsHibernation(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	})

	resp := ct.crud.HandleUpdate(mustMarshal(t, bus.ControllersUpdateRequest{
		Namespace: "team-a", Name: "ctrl1",
		Patch: mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"powerState": ""}}),
	}))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != "" || updateResp.Error != "" {
		t.Fatalf("expected success, got code=%q error=%q", updateResp.Code, updateResp.Error)
	}
	if len(ct.client.hibernatedClears) != 1 || ct.client.hibernatedClears[0] != "team-a/ctrl1" {
		t.Fatalf("hibernated clears = %v, want exactly [team-a/ctrl1]", ct.client.hibernatedClears)
	}
}

// TestCommandCRUD_HandleUpdate_PowerStateNullClearsHibernation pins the same
// key-presence guard for an explicit JSON null powerState write.
func TestCommandCRUD_HandleUpdate_PowerStateNullClearsHibernation(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	})

	resp := ct.crud.HandleUpdate(mustMarshal(t, bus.ControllersUpdateRequest{
		Namespace: "team-a", Name: "ctrl1",
		Patch: mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"powerState": nil}}),
	}))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != "" || updateResp.Error != "" {
		t.Fatalf("expected success, got code=%q error=%q", updateResp.Code, updateResp.Error)
	}
	if len(ct.client.hibernatedClears) != 1 || ct.client.hibernatedClears[0] != "team-a/ctrl1" {
		t.Fatalf("hibernated clears = %v, want exactly [team-a/ctrl1]", ct.client.hibernatedClears)
	}
}

// TestCommandCRUD_HandleUpdate_SetHibernatedFailureReturnsInternal pins the F2
// propagation fix: a failed hibernation clear on a power write must be an
// internal error reply, not a logged-and-swallowed success that leaves the
// controller parked with status.hibernated=true.
func TestCommandCRUD_HandleUpdate_SetHibernatedFailureReturnsInternal(t *testing.T) {
	ct := newCommandCRUDTest(t)
	ct.seedController(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	})
	ct.client.setHibernatedErr = errors.New("status update conflict")

	resp := ct.crud.HandleUpdate(mustMarshal(t, bus.ControllersUpdateRequest{
		Namespace: "team-a", Name: "ctrl1",
		Patch: mustMarshal(t, map[string]interface{}{"spec": map[string]interface{}{"powerState": "Running"}}),
	}))
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if updateResp.Code != bus.CodeInternal {
		t.Fatalf("code = %q, want %q (error %q)", updateResp.Code, bus.CodeInternal, updateResp.Error)
	}
	if updateResp.Error == "" {
		t.Fatal("expected non-empty error")
	}
}
