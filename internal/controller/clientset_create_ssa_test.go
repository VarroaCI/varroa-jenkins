package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// createClaimUID is the UID the create reactor stamps on each created object.
const createClaimUID = "uid-ctrl1"

// createClaimHarness drives CreateControllerSpecSSA over the dynamic fake.
// The create reactor stamps a resourceVersion and a UID on each created
// object. The claim reactors in claimLog handle the apply and the JSON patch.
type createClaimHarness struct {
	client *ClientsetClient
	dyn    *dynamicfake.FakeDynamicClient
	log    *claimLog

	// createManagedFields, when set, is written onto each created object. It
	// models the Update entry that a real POST leaves for the creator.
	createManagedFields []metav1.ManagedFieldsEntry
}

func newCreateClaimHarness(t *testing.T, existing *unstructured.Unstructured) *createClaimHarness {
	t.Helper()
	client, dyn := newIfExistsTestClient(t, existing)
	h := &createClaimHarness{client: client, dyn: dyn, log: &claimLog{}}

	dyn.PrependReactor("create", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		obj, ok := action.(clienttesting.CreateActionImpl).GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		obj.SetResourceVersion("1")
		obj.SetUID(createClaimUID)
		if h.createManagedFields != nil {
			obj.SetManagedFields(h.createManagedFields)
		}
		return false, nil, nil
	})
	installClaimReactors(dyn, h.log)
	return h
}

func (h *createClaimHarness) count(verb string) int {
	n := 0
	for _, a := range h.dyn.Actions() {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}

func (h *createClaimHarness) stored(t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := h.dyn.Tracker().Get(controllerGVR, ns, name)
	if err != nil {
		t.Fatalf("get stored controller: %v", err)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("stored object is %T, want *unstructured.Unstructured", obj)
	}
	return u
}

// createClaimSeed builds a stored Controller ctrl1/ns1 at resourceVersion "5"
// with the given spec version and managedFields (nil leaves the field unset).
func createClaimSeed(version string, managedFields []any) *unstructured.Unstructured {
	meta := map[string]any{"name": "ctrl1", "namespace": "ns1", "resourceVersion": "5"}
	if managedFields != nil {
		meta["managedFields"] = managedFields
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata":   meta,
		"spec":       map[string]any{"version": version},
	}}
}

func TestCreateControllerSpecSSA_ExistingNameFailsAndKeepsObject(t *testing.T) {
	h := newCreateClaimHarness(t, createClaimSeed("1.0", nil))

	_, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui")
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("err = %v, want AlreadyExists", err)
	}
	if len(h.log.applies) != 0 {
		t.Fatalf("apply calls = %d, want 0 after a refused create", len(h.log.applies))
	}
	if len(h.log.jsonPatches) != 0 {
		t.Fatalf("JSON patches = %d, want 0 after a refused create", len(h.log.jsonPatches))
	}
	version, _, _ := unstructured.NestedString(h.stored(t, "ns1", "ctrl1").Object, "spec", "version")
	if version != "1.0" {
		t.Fatalf("stored spec.version = %q, want 1.0", version)
	}
}

func TestCreateControllerSpecSSA_ClaimsSpecAndMergesCreateEntry(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.createManagedFields = []metav1.ManagedFieldsEntry{
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
		{Manager: "varroa-operator", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "varroa.dev/v1alpha1", Subresource: "status"},
	}

	got, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui")
	if err != nil {
		t.Fatalf("CreateControllerSpecSSA: %v", err)
	}
	if got == nil || got.Spec.Version != "2.0" {
		t.Fatalf("returned object = %#v, want spec.version 2.0", got)
	}

	// The create carries name and namespace only, under the field manager.
	var created *unstructured.Unstructured
	for _, a := range h.dyn.Actions() {
		if ca, ok := a.(clienttesting.CreateActionImpl); ok {
			if ca.CreateOptions.FieldManager != "varroa-ui" {
				t.Fatalf("create field manager = %q, want varroa-ui", ca.CreateOptions.FieldManager)
			}
			created = ca.GetObject().(*unstructured.Unstructured)
		}
	}
	if created == nil {
		t.Fatal("no create action recorded")
	}
	for _, key := range []string{"labels", "annotations", "finalizers"} {
		if _, ok := created.Object["metadata"].(map[string]any)[key]; ok {
			t.Fatalf("create metadata carries %q", key)
		}
	}

	// Exactly one apply, with force off.
	if len(h.log.applies) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(h.log.applies))
	}
	if h.log.applies[0].PatchOptions.Force == nil || *h.log.applies[0].PatchOptions.Force {
		t.Fatal("apply must set Force=false")
	}
	if h.log.applies[0].PatchOptions.FieldManager != "varroa-ui" {
		t.Fatalf("apply field manager = %q, want varroa-ui", h.log.applies[0].PatchOptions.FieldManager)
	}

	// The create's Update entry is merged into the Apply entry, and the status
	// entry stays.
	var sawStatus, sawApply bool
	for _, e := range h.stored(t, "ns1", "ctrl1").GetManagedFields() {
		if e.Manager == "varroa-ui" && e.Operation == metav1.ManagedFieldsOperationUpdate && e.Subresource == "" {
			t.Fatal("varroa-ui Update entry survived the claim")
		}
		if e.Manager == "varroa-ui" && e.Operation == metav1.ManagedFieldsOperationApply {
			sawApply = true
			var fields map[string]map[string]map[string]any
			if err := json.Unmarshal(e.FieldsV1.GetRawBytes(), &fields); err != nil {
				t.Fatalf("decode varroa-ui Apply fields: %v", err)
			}
			if _, ok := fields["f:spec"]["f:version"]; !ok {
				t.Fatalf("varroa-ui Apply entry does not own f:spec.f:version: %s", e.FieldsV1.GetRawBytes())
			}
		}
		if e.Manager == "varroa-operator" && e.Subresource == "status" {
			sawStatus = true
		}
	}
	if !sawApply {
		t.Fatal("no varroa-ui Apply entry after the claim")
	}
	if !sawStatus {
		t.Fatal("varroa-operator status entry was dropped")
	}

	// The JSON patch pins the resourceVersion of the read it used.
	if len(h.log.jsonPatches) != 1 {
		t.Fatalf("JSON patches = %d, want 1", len(h.log.jsonPatches))
	}
	var pinned bool
	for _, op := range h.log.jsonPatches[0] {
		if op["path"] == "/metadata/resourceVersion" {
			pinned = op["value"] == "1"
		}
	}
	if !pinned {
		t.Fatalf("JSON patch does not replace /metadata/resourceVersion with \"1\": %#v", h.log.jsonPatches[0])
	}
}

func TestCreateControllerSpecSSA_ClaimApplyCarriesCreatedUID(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.createManagedFields = []metav1.ManagedFieldsEntry{
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	}

	if _, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui"); err != nil {
		t.Fatalf("CreateControllerSpecSSA: %v", err)
	}
	if len(h.log.applies) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(h.log.applies))
	}
	meta, _ := applyBodyMap(t, h.log.applies[0])["metadata"].(map[string]any)
	if meta["uid"] != createClaimUID {
		t.Fatalf("apply uid = %#v, want %q", meta["uid"], createClaimUID)
	}
	if _, ok := meta["resourceVersion"]; ok {
		t.Fatal("apply pins resourceVersion; the claim must not")
	}
}

func TestCreateControllerSpecSSA_ClaimRetriesOnConflict(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.createManagedFields = []metav1.ManagedFieldsEntry{
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	}
	h.log.jsonConflicts = 1

	if _, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui"); err != nil {
		t.Fatalf("CreateControllerSpecSSA: %v", err)
	}
	// One GET from the apply's read, then one GET after the Conflict.
	if gets := h.count("get"); gets != 2 {
		t.Fatalf("get calls = %d, want 2 (apply read + one claim re-read)", gets)
	}
	if len(h.log.jsonPatches) != 1 {
		t.Fatalf("accepted JSON patches = %d, want 1", len(h.log.jsonPatches))
	}
}

func TestCreateControllerSpecSSA_ReplacedObjectReturnsPlainError(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.log.applyErr = apierrors.NewConflict(controllerGVR.GroupResource(), "ctrl1", errors.New("uid mismatch"))

	got, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui")
	if !errors.Is(err, errControllerReplaced) {
		t.Fatalf("err = %v, want errControllerReplaced", err)
	}
	if errors.Is(err, ErrControllerSpecClaim) {
		t.Fatal("a replaced object must not answer ErrControllerSpecClaim")
	}
	if got != nil {
		t.Fatalf("returned object = %#v, want nil for a replaced object", got)
	}
}

func TestCreateControllerSpecSSA_ReplacedDuringConflictIsReplaced(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.log.applyErr = errors.New("apply conflict")
	h.log.beforeApply = func() {
		obj, err := h.dyn.Tracker().Get(controllerGVR, "ns1", "ctrl1")
		if err != nil {
			t.Fatalf("get created controller: %v", err)
		}
		u := obj.(*unstructured.Unstructured)
		u.SetUID("uid-replacement")
		if err := h.dyn.Tracker().Update(controllerGVR, u, "ns1"); err != nil {
			t.Fatalf("replace controller: %v", err)
		}
	}

	got, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui")
	if !errors.Is(err, errControllerReplaced) {
		t.Fatalf("err = %v, want errControllerReplaced when the name now holds another UID", err)
	}
	if got != nil {
		t.Fatalf("returned object = %#v, want nil", got)
	}
}

func TestCreateControllerSpecSSA_ClaimFailureReturnsObjectAndSentinel(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.log.applyErr = errors.New("apply timed out")

	got, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui")
	if !errors.Is(err, ErrControllerSpecClaim) {
		t.Fatalf("err = %v, want ErrControllerSpecClaim", err)
	}
	if got == nil || got.Name != "ctrl1" || got.Spec.Version != "2.0" {
		t.Fatalf("returned object = %#v, want the created ctrl1 with spec.version 2.0", got)
	}
}
