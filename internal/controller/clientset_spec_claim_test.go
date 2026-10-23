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

// claimLog records the patches that reach the dynamic fake. The apply reactor
// returns the stored object, because the fake does not implement server-side
// apply. The JSON patch reactor applies the managedFields operation to the
// stored object, so a later read sees the claimed entries.
type claimLog struct {
	// order lists "json" or "apply" for each accepted patch, in call order.
	order []string
	// applies records every apply that reached the fake.
	applies []clienttesting.PatchActionImpl
	// jsonPatches records the body of every JSON patch that reached the fake.
	jsonPatches [][]map[string]any
	// jsonConflicts counts the JSON patches to fail with a Conflict first.
	jsonConflicts int
	// jsonErr, when set, makes every JSON patch fail with this error.
	jsonErr error
	// applyErr, when set, makes every apply fail with this error.
	applyErr error
	// beforeApply, when set, runs before each apply reaches the fake.
	beforeApply func()
}

// installClaimReactors wires claimLog into dyn. A successful JSON patch
// stores resourceVersion "6", so the tests can check the claimed version.
func installClaimReactors(dyn *dynamicfake.FakeDynamicClient, log *claimLog) {
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchActionImpl)
		switch pa.GetPatchType() {
		case types.ApplyPatchType:
			if log.beforeApply != nil {
				log.beforeApply()
			}
			log.order = append(log.order, "apply")
			log.applies = append(log.applies, pa)
			if log.applyErr != nil {
				return true, nil, log.applyErr
			}
			obj, err := dyn.Tracker().Get(controllerGVR, pa.GetNamespace(), pa.GetName())
			return true, obj, err
		case types.JSONPatchType:
			if log.jsonConflicts > 0 {
				log.jsonConflicts--
				return true, nil, apierrors.NewConflict(controllerGVR.GroupResource(), pa.GetName(), errors.New("stale write"))
			}
			var ops []map[string]any
			if err := json.Unmarshal(pa.GetPatch(), &ops); err != nil {
				return true, nil, err
			}
			log.order = append(log.order, "json")
			log.jsonPatches = append(log.jsonPatches, ops)
			if log.jsonErr != nil {
				return true, nil, log.jsonErr
			}
			obj, err := dyn.Tracker().Get(controllerGVR, pa.GetNamespace(), pa.GetName())
			if err != nil {
				return true, nil, err
			}
			u := obj.(*unstructured.Unstructured)
			for _, op := range ops {
				if op["path"] != "/metadata/managedFields" {
					continue
				}
				raw, err := json.Marshal(op["value"])
				if err != nil {
					return true, nil, err
				}
				var entries []metav1.ManagedFieldsEntry
				if err := json.Unmarshal(raw, &entries); err != nil {
					return true, nil, err
				}
				u.SetManagedFields(entries)
			}
			u.SetResourceVersion("6")
			if err := dyn.Tracker().Update(controllerGVR, u, pa.GetNamespace()); err != nil {
				return true, nil, err
			}
			return true, u, nil
		}
		return false, nil, nil
	})
}

// specClaimEntry builds a managedFields entry on the main resource. fields is
// the raw FieldsV1 JSON.
func specClaimEntry(manager string, op metav1.ManagedFieldsOperationType, fields string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager:    manager,
		Operation:  op,
		APIVersion: "varroa.dev/v1alpha1",
		FieldsType: "FieldsV1",
		FieldsV1:   &metav1.FieldsV1{Raw: []byte(fields)},
	}
}

// specClaimSeed returns the ifExistsController object with the given spec and
// managedFields entries.
func specClaimSeed(spec map[string]any, entries ...metav1.ManagedFieldsEntry) *unstructured.Unstructured {
	obj := ifExistsController()
	obj.Object["spec"] = spec
	if len(entries) > 0 {
		obj.SetManagedFields(entries)
	}
	return obj
}

// applyBodyMap decodes the body of an apply that reached the fake.
func applyBodyMap(t *testing.T, pa clienttesting.PatchActionImpl) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
		t.Fatalf("decode apply body: %v", err)
	}
	return body
}

// managedEntries returns the stored managedFields of ctrl1/ns1.
func managedEntries(t *testing.T, dyn *dynamicfake.FakeDynamicClient) []metav1.ManagedFieldsEntry {
	t.Helper()
	obj, err := dyn.Tracker().Get(controllerGVR, "ns1", "ctrl1")
	if err != nil {
		t.Fatalf("get stored controller: %v", err)
	}
	return obj.(*unstructured.Unstructured).GetManagedFields()
}

func TestApplyControllerSpecSSA_ClaimsUpdateEntryBeforeApply(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{},"f:powerState":{}}}`),
	))
	log := &claimLog{}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSA: %v", err)
	}
	if len(log.order) != 2 || log.order[0] != "json" || log.order[1] != "apply" {
		t.Fatalf("call order = %v, want [json apply]", log.order)
	}
	if len(log.applies) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(log.applies))
	}
	spec, _ := applyBodyMap(t, log.applies[0])["spec"].(map[string]any)
	if spec["version"] != "2.0" || spec["powerState"] != "Running" {
		t.Fatalf("apply spec = %#v, want version 2.0 and backfilled powerState Running", spec)
	}
}

func TestApplyControllerSpecSSA_MergesUpdateEntryIntoExistingApplyEntry(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationApply, `{"f:spec":{"f:powerState":{}}}`),
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	))
	log := &claimLog{}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSA: %v", err)
	}
	if len(log.jsonPatches) != 1 {
		t.Fatalf("claim patches = %d, want 1", len(log.jsonPatches))
	}

	var applyEntries []metav1.ManagedFieldsEntry
	for _, e := range managedEntries(t, dyn) {
		if e.Manager == "varroa-ui" && e.Operation == metav1.ManagedFieldsOperationUpdate {
			t.Fatal("varroa-ui Update entry survived the claim")
		}
		if e.Manager == "varroa-ui" && e.Operation == metav1.ManagedFieldsOperationApply {
			applyEntries = append(applyEntries, e)
		}
	}
	if len(applyEntries) != 1 {
		t.Fatalf("varroa-ui Apply entries = %d, want 1", len(applyEntries))
	}
	var fields map[string]map[string]map[string]any
	if err := json.Unmarshal(applyEntries[0].FieldsV1.GetRawBytes(), &fields); err != nil {
		t.Fatalf("decode merged fields: %v", err)
	}
	if _, ok := fields["f:spec"]["f:powerState"]; !ok {
		t.Fatalf("merged entry lost f:powerState: %s", applyEntries[0].FieldsV1.GetRawBytes())
	}
	if _, ok := fields["f:spec"]["f:version"]; !ok {
		t.Fatalf("merged entry lacks f:version: %s", applyEntries[0].FieldsV1.GetRawBytes())
	}
}

func TestApplyControllerSpecSSA_NoUpdateEntryNoClaimPatch(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationApply, `{"f:spec":{"f:version":{}}}`),
	))
	log := &claimLog{}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSA: %v", err)
	}
	if len(log.jsonPatches) != 0 {
		t.Fatalf("claim patches = %d, want 0", len(log.jsonPatches))
	}
}

func TestApplyControllerSpecSSA_SkipsUpdateEntryWithNonSpecFields(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate,
			`{"f:metadata":{"f:labels":{"f:tenant":{}}},"f:spec":{"f:version":{}}}`),
	))
	log := &claimLog{}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSA: %v", err)
	}
	if len(log.jsonPatches) != 0 {
		t.Fatalf("claim patches = %d, want 0 for an entry that owns labels", len(log.jsonPatches))
	}
}

func TestApplyControllerSpecSSA_SkipsUpdateEntryOfOtherAPIVersion(t *testing.T) {
	other := specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`)
	other.APIVersion = "varroa.dev/v1alpha2"
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"}, other,
	))
	log := &claimLog{}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSA: %v", err)
	}
	if len(log.jsonPatches) != 0 {
		t.Fatalf("claim patches = %d, want 0 for an entry of another apiVersion", len(log.jsonPatches))
	}
}

func TestApplyControllerSpecSSA_SkipsApplyEntryOfOtherAPIVersion(t *testing.T) {
	oldApply := specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationApply, `{"f:spec":{"f:powerState":{}}}`)
	oldApply.APIVersion = "varroa.dev/v1alpha2"
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		oldApply,
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	))
	log := &claimLog{}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSA: %v", err)
	}
	if len(log.jsonPatches) != 0 {
		t.Fatalf("claim patches = %d, want 0 while an Apply entry of another apiVersion exists", len(log.jsonPatches))
	}
}

func TestApplyControllerSpecSSA_UIDChangedSkipsClaim(t *testing.T) {
	seed := specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	)
	seed.SetUID("uid-replacement")
	c, dyn := newIfExistsTestClient(t, seed)
	log := &claimLog{}
	installClaimReactors(dyn, log)

	_, _, err := c.applyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false, types.UID("uid-created"))
	if !errors.Is(err, errControllerReplaced) {
		t.Fatalf("err = %v, want errControllerReplaced for a replaced object", err)
	}
	if len(log.jsonPatches) != 0 || len(log.applies) != 0 {
		t.Fatalf("patches = %d, applies = %d, want 0 and 0 on the replacement", len(log.jsonPatches), len(log.applies))
	}
}

func TestClaimSpecUpdateEntry_ReplacedObjectNotPatched(t *testing.T) {
	seed := specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	)
	seed.SetUID("uid-replacement")
	c, dyn := newIfExistsTestClient(t, seed)
	log := &claimLog{}
	installClaimReactors(dyn, log)

	_, err := c.claimSpecUpdateEntry(context.Background(), "ns1", "ctrl1", "varroa-ui", nil, types.UID("uid-created"))
	if !errors.Is(err, errControllerReplaced) {
		t.Fatalf("err = %v, want errControllerReplaced on the re-read", err)
	}
	if len(log.jsonPatches) != 0 {
		t.Fatalf("claim patches = %d, want 0 on a replacement", len(log.jsonPatches))
	}
}

func TestApplyControllerSpecSSA_ClaimNotFoundWithUIDIsReplaced(t *testing.T) {
	seed := specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	)
	seed.SetUID("uid-created")
	c, dyn := newIfExistsTestClient(t, seed)
	log := &claimLog{jsonErr: apierrors.NewNotFound(controllerGVR.GroupResource(), "ctrl1")}
	installClaimReactors(dyn, log)

	_, _, err := c.applyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false, types.UID("uid-created"))
	if !errors.Is(err, errControllerReplaced) {
		t.Fatalf("err = %v, want errControllerReplaced when the claim finds the object gone", err)
	}
	if len(log.applies) != 0 {
		t.Fatalf("apply calls = %d, want 0", len(log.applies))
	}
}

func TestApplyControllerSpecSSA_AbsentObjectWithUIDNotApplied(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, nil)
	log := &claimLog{}
	installClaimReactors(dyn, log)

	_, _, err := c.applyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false, types.UID("uid-created"))
	if !errors.Is(err, errControllerReplaced) {
		t.Fatalf("err = %v, want errControllerReplaced for a deleted object", err)
	}
	if len(log.applies) != 0 {
		t.Fatalf("apply calls = %d, want 0: a UID-fenced apply must not create", len(log.applies))
	}
}

func TestApplyControllerSpecSSA_OtherManagerUpdateEntryUntouched(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("kubectl-edit", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	))
	log := &claimLog{}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSA: %v", err)
	}
	if len(log.jsonPatches) != 0 {
		t.Fatalf("claim patches = %d, want 0 for another manager's entry", len(log.jsonPatches))
	}
}

func TestApplyControllerSpecSSA_ClaimErrorStopsApply(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	))
	log := &claimLog{jsonErr: apierrors.NewInternalError(errors.New("boom"))}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err == nil {
		t.Fatal("ApplyControllerSpecSSA: want an error when the claim fails")
	}
	if len(log.applies) != 0 {
		t.Fatalf("apply calls = %d, want 0 after a failed claim", len(log.applies))
	}
}

func TestApplyControllerSpecSSAIfExists_ClaimsUpdateEntryFirst(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	))
	log := &claimLog{}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSAIfExists(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSAIfExists: %v", err)
	}
	if len(log.order) != 2 || log.order[0] != "json" || log.order[1] != "apply" {
		t.Fatalf("call order = %v, want [json apply]", log.order)
	}
	meta, _ := applyBodyMap(t, log.applies[0])["metadata"].(map[string]any)
	if meta["resourceVersion"] != "6" {
		t.Fatalf("apply resourceVersion = %#v, want the claimed \"6\"", meta["resourceVersion"])
	}
}

func TestApplyControllerSpecSSAIfExists_ClaimNotFoundPassesThrough(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, nil)
	log := &claimLog{}
	installClaimReactors(dyn, log)

	_, _, err := c.ApplyControllerSpecSSAIfExists(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if len(log.jsonPatches) != 0 || len(log.applies) != 0 {
		t.Fatalf("patches = %d, applies = %d, want 0 and 0", len(log.jsonPatches), len(log.applies))
	}
}

func TestApplyControllerSpecSSA_ClaimNotFoundAppliesAsCreate(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, specClaimSeed(
		map[string]any{"version": "1.0", "powerState": "Running"},
		specClaimEntry("varroa-ui", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:version":{}}}`),
	))
	log := &claimLog{jsonErr: apierrors.NewNotFound(controllerGVR.GroupResource(), "ctrl1")}
	installClaimReactors(dyn, log)

	if _, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.0"}, "varroa-ui", false); err != nil {
		t.Fatalf("ApplyControllerSpecSSA: %v, want the apply to run as a create", err)
	}
	if len(log.applies) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(log.applies))
	}
}
