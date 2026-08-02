package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
	deleteErr            error
	deletePodErr         error
	listErr              error
	getErr               error
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

func (f *fakeCRUDClient) ApplyControllerCRD(_ context.Context, cr *v1alpha1.Controller) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.controllers[f.key(cr.Name, cr.Namespace)] = cr.DeepCopy()
	return nil
}

func (f *fakeCRUDClient) ApplyControllerSpecSSA(_ context.Context, ns, name string, specPatch map[string]any, _ string, _ bool) (*v1alpha1.Controller, error) {
	if f.ssaApplyErr != nil {
		return nil, f.ssaApplyErr
	}
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	key := f.key(name, ns)
	existing, ok := f.controllers[key]
	if !ok {
		// Create a new controller from the spec patch.
		cr := &v1alpha1.Controller{
			TypeMeta:   metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		}
		// Unmarshal specPatch onto the spec.
		specJSON, _ := json.Marshal(specPatch)
		json.Unmarshal(specJSON, &cr.Spec)
		f.controllers[key] = cr
		return cr.DeepCopy(), nil
	}
	// Merge sparse spec onto existing.
	existing = existing.DeepCopy()
	specJSON, _ := json.Marshal(specPatch)
	json.Unmarshal(specJSON, &existing.Spec)
	f.controllers[key] = existing
	return existing.DeepCopy(), nil
}

func (f *fakeCRUDClient) DeleteControllerCRD(_ context.Context, name, namespace string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.controllers[f.key(name, namespace)]; !ok {
		return k8serrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "controllers"}, name)
	}
	delete(f.controllers, f.key(name, namespace))
	return nil
}

func (f *fakeCRUDClient) DeleteControllerPod(_ context.Context, namespace, name string) error {
	if f.deletePodErr != nil {
		return f.deletePodErr
	}
	return nil
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
	if len(createResp.Checks) == 0 {
		t.Fatal("expected non-empty checks")
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
	ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatal(err)
	}
	ct.store.FailAlways("create", ctrlGVR, k8serrors.NewInternalError(k8serrors.NewTimeoutError("test timeout", 0)))

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
