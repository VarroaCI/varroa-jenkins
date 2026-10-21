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
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// createClaimHarness drives CreateControllerSpecSSA over the dynamic fake.
// The fake tracker stores creates and merge patches. The apply reactor
// records ApplyPatchType calls and returns the stored object, because the
// fake does not implement server-side apply.
type createClaimHarness struct {
	client *ClientsetClient
	dyn    *dynamicfake.FakeDynamicClient

	// createManagedFields, when set, is written onto each created object. It
	// models the Update entry that a real POST leaves for the creator.
	createManagedFields []metav1.ManagedFieldsEntry
	// applyErr, when set, makes the apply reactor fail.
	applyErr error
	// mergeConflicts counts the merge patches to fail with a Conflict first.
	mergeConflicts int

	applies []clienttesting.PatchActionImpl
	merges  []map[string]any
}

func newCreateClaimHarness(t *testing.T, existing *unstructured.Unstructured) *createClaimHarness {
	t.Helper()
	client, dyn := newIfExistsTestClient(t, existing)
	h := &createClaimHarness{client: client, dyn: dyn}

	dyn.PrependReactor("create", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		obj, ok := action.(clienttesting.CreateActionImpl).GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		obj.SetResourceVersion("1")
		if h.createManagedFields != nil {
			obj.SetManagedFields(h.createManagedFields)
		}
		return false, nil, nil
	})

	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchActionImpl)
		switch pa.GetPatchType() {
		case types.ApplyPatchType:
			h.applies = append(h.applies, pa)
			if h.applyErr != nil {
				return true, nil, h.applyErr
			}
			obj, err := dyn.Tracker().Get(controllerGVR, pa.GetNamespace(), pa.GetName())
			return true, obj, err
		case types.MergePatchType:
			if h.mergeConflicts > 0 {
				h.mergeConflicts--
				return true, nil, apierrors.NewConflict(controllerGVR.GroupResource(), pa.GetName(), errors.New("stale write"))
			}
			var body map[string]any
			if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
				return true, nil, err
			}
			h.merges = append(h.merges, body)
			return false, nil, nil
		}
		return false, nil, nil
	})
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
	if len(h.applies) != 0 {
		t.Fatalf("apply calls = %d, want 0 after a refused create", len(h.applies))
	}
	if len(h.merges) != 0 {
		t.Fatalf("merge patches = %d, want 0 after a refused create", len(h.merges))
	}
	version, _, _ := unstructured.NestedString(h.stored(t, "ns1", "ctrl1").Object, "spec", "version")
	if version != "1.0" {
		t.Fatalf("stored spec.version = %q, want 1.0", version)
	}
}

func TestCreateControllerSpecSSA_ClaimsSpecAndDropsCreateEntry(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.createManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: "varroa-ui", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "varroa.dev/v1alpha1"},
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
	if len(h.applies) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(h.applies))
	}
	if h.applies[0].PatchOptions.Force == nil || *h.applies[0].PatchOptions.Force {
		t.Fatal("apply must set Force=false")
	}
	if h.applies[0].PatchOptions.FieldManager != "varroa-ui" {
		t.Fatalf("apply field manager = %q, want varroa-ui", h.applies[0].PatchOptions.FieldManager)
	}

	// The Update entry is gone, the status entry stays.
	var sawStatus bool
	for _, e := range h.stored(t, "ns1", "ctrl1").GetManagedFields() {
		if e.Manager == "varroa-ui" && e.Operation == metav1.ManagedFieldsOperationUpdate && e.Subresource == "" {
			t.Fatal("varroa-ui Update entry survived the claim")
		}
		if e.Manager == "varroa-operator" && e.Subresource == "status" {
			sawStatus = true
		}
	}
	if !sawStatus {
		t.Fatal("varroa-operator status entry was dropped")
	}

	// The merge patch pins the resourceVersion of the read it filtered.
	if len(h.merges) != 1 {
		t.Fatalf("merge patches = %d, want 1", len(h.merges))
	}
	meta, _ := h.merges[0]["metadata"].(map[string]any)
	if meta["resourceVersion"] != "1" {
		t.Fatalf("merge patch resourceVersion = %#v, want \"1\"", meta["resourceVersion"])
	}
}

func TestCreateControllerSpecSSA_ReleaseRetriesOnConflict(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.createManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: "varroa-ui", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "varroa.dev/v1alpha1"},
	}
	h.mergeConflicts = 1

	if _, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui"); err != nil {
		t.Fatalf("CreateControllerSpecSSA: %v", err)
	}
	// One GET from the apply's completion read, then one GET per release try.
	if gets := h.count("get"); gets != 3 {
		t.Fatalf("get calls = %d, want 3 (apply read + two release reads)", gets)
	}
	if len(h.merges) != 1 {
		t.Fatalf("accepted merge patches = %d, want 1", len(h.merges))
	}
}

func TestCreateControllerSpecSSA_ClaimFailureReturnsObjectAndSentinel(t *testing.T) {
	h := newCreateClaimHarness(t, nil)
	h.applyErr = errors.New("apply timed out")

	got, err := h.client.CreateControllerSpecSSA(context.Background(), "ns1", "ctrl1", map[string]any{"version": "2.0"}, "varroa-ui")
	if !errors.Is(err, ErrControllerSpecClaim) {
		t.Fatalf("err = %v, want ErrControllerSpecClaim", err)
	}
	if got == nil || got.Name != "ctrl1" || got.Spec.Version != "2.0" {
		t.Fatalf("returned object = %#v, want the created ctrl1 with spec.version 2.0", got)
	}
}
