package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// newIfExistsTestClient builds a ClientsetClient backed by a dynamic fake,
// seeded with the given controller. existing may be nil (no object stored).
func newIfExistsTestClient(t *testing.T, existing *unstructured.Unstructured) (*ClientsetClient, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "Controller"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControllerList"), &unstructured.UnstructuredList{})

	var seeds []runtime.Object
	if existing != nil {
		seeds = append(seeds, existing.DeepCopy())
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{controllerGVR: "ControllerList"},
		seeds...,
	)
	return &ClientsetClient{dynamic: dyn}, dyn
}

// ifExistsController builds a minimal Controller unstructured at
// resourceVersion "5", named ctrl1/ns1, with spec.powerState=Running.
func ifExistsController() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata": map[string]any{
			"name":            "ctrl1",
			"namespace":       "ns1",
			"resourceVersion": "5",
		},
		"spec": map[string]any{"powerState": "Running"},
	}}
}

// TestApplyControllerSpecSSAIfExists_NotFound pins the existence guard: a
// target absent when the method's own GET runs must fail with NotFound and
// must never reach the apply, so no phantom Controller is created. NotFound
// is terminal, not a conflict, so retry.RetryOnConflict must not retry it —
// the GET reactor below fails the test if it is called more than once.
func TestApplyControllerSpecSSAIfExists_NotFound(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, nil)

	getAttempts := 0
	dyn.PrependReactor("get", "controllers", func(clienttesting.Action) (bool, runtime.Object, error) {
		getAttempts++
		return false, nil, nil
	})
	patched := false
	dyn.PrependReactor("patch", "controllers", func(clienttesting.Action) (bool, runtime.Object, error) {
		patched = true
		return false, nil, nil
	})

	_, _, err := c.ApplyControllerSpecSSAIfExists(context.Background(), "ns1", "ctrl1",
		map[string]any{"powerState": "Stopped"}, "varroa-ui", false)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
	if patched {
		t.Fatal("expected no patch call for a missing controller (must not resurrect)")
	}
	if getAttempts != 1 {
		t.Fatalf("get attempts = %d, want exactly 1 (NotFound must not be retried)", getAttempts)
	}
	if _, err := dyn.Resource(controllerGVR).Namespace("ns1").Get(context.Background(), "ctrl1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected controller to remain absent, get err = %v", err)
	}
}

// TestApplyControllerSpecSSAIfExists_StaleResourceVersionConflicts simulates
// a delete-or-change landing between this method's internal GET and its
// apply: the GET reactor serves a stale resourceVersion while the tracked
// object has already moved on, mirroring a real apiserver's optimistic
// concurrency rejection (the fake dynamic client does not enforce
// resourceVersion preconditions itself, so a patch reactor stands in for it).
func TestApplyControllerSpecSSAIfExists_StaleResourceVersionConflicts(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, ifExistsController())

	dyn.PrependReactor("get", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		obj, err := dyn.Tracker().Get(controllerGVR, ga.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		u.SetResourceVersion("5")
		return true, u, nil
	})

	// Simulate the concurrent change: the tracked object moves to
	// resourceVersion "6" between the GET above and the apply below.
	live, err := dyn.Tracker().Get(controllerGVR, "ns1", "ctrl1")
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	liveU := live.(*unstructured.Unstructured).DeepCopy()
	liveU.SetResourceVersion("6")
	if err := dyn.Tracker().Update(controllerGVR, liveU, "ns1"); err != nil {
		t.Fatalf("bump live resourceVersion: %v", err)
	}

	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		var body map[string]any
		if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		meta, _ := body["metadata"].(map[string]any)
		submittedRV, _ := meta["resourceVersion"].(string)

		stored, err := dyn.Tracker().Get(controllerGVR, pa.GetNamespace(), pa.GetName())
		if err != nil {
			return true, nil, err
		}
		storedRV := stored.(*unstructured.Unstructured).GetResourceVersion()
		if submittedRV != storedRV {
			return true, nil, apierrors.NewConflict(controllerGVR.GroupResource(), pa.GetName(),
				fmt.Errorf("resourceVersion mismatch: submitted %q, current %q", submittedRV, storedRV))
		}
		return false, nil, nil
	})

	_, _, err = c.ApplyControllerSpecSSAIfExists(context.Background(), "ns1", "ctrl1",
		map[string]any{"powerState": "Stopped"}, "varroa-ui", false)
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}

	got, err := dyn.Resource(controllerGVR).Namespace("ns1").Get(context.Background(), "ctrl1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ps, _, _ := unstructured.NestedString(got.Object, "spec", "powerState"); ps != "Running" {
		t.Fatalf("spec.powerState = %q, want Running (a rejected write must not apply)", ps)
	}
}

// TestApplyControllerSpecSSAIfExists_BenignConflictRetries simulates the
// routine case on a live controller: an unrelated concurrent write (e.g. the
// reconciler's continuous status patch) bumps resourceVersion between this
// method's GET and its apply on the first attempt. The retry must re-GET and
// succeed on the second attempt rather than surfacing the conflict to the
// caller.
func TestApplyControllerSpecSSAIfExists_BenignConflictRetries(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, ifExistsController())

	patchAttempts := 0
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		patchAttempts++
		if patchAttempts == 1 {
			return true, nil, apierrors.NewConflict(controllerGVR.GroupResource(), pa.GetName(),
				fmt.Errorf("injected benign conflict from a concurrent status patch"))
		}
		var body map[string]any
		if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		stored, err := dyn.Tracker().Get(controllerGVR, pa.GetNamespace(), pa.GetName())
		if err != nil {
			return true, nil, err
		}
		merged := stored.(*unstructured.Unstructured).DeepCopy()
		if spec, ok := body["spec"].(map[string]any); ok {
			for k, v := range spec {
				unstructured.SetNestedField(merged.Object, v, "spec", k) //nolint:errcheck // test-only merge
			}
		}
		if err := dyn.Tracker().Update(controllerGVR, merged, pa.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, merged, nil
	})

	out, _, err := c.ApplyControllerSpecSSAIfExists(context.Background(), "ns1", "ctrl1",
		map[string]any{"powerState": "Stopped"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSAIfExists: %v, want success after retry", err)
	}
	if patchAttempts < 2 {
		t.Fatalf("patch attempts = %d, want at least 2 (retry must have happened)", patchAttempts)
	}
	if out == nil || out.Spec.PowerState != "Stopped" {
		t.Fatalf("returned controller spec.powerState = %#v, want Stopped", out)
	}

	got, err := dyn.Resource(controllerGVR).Namespace("ns1").Get(context.Background(), "ctrl1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ps, _, _ := unstructured.NestedString(got.Object, "spec", "powerState"); ps != "Stopped" {
		t.Fatalf("stored spec.powerState = %q, want Stopped", ps)
	}
}

// TestApplyControllerSpecSSAIfExists_RebackfillsFreshValueOnRetry pins the
// per-attempt deep copy: attempt 1 backfills an owned leaf (spec.jenkinsImage,
// not present in the caller's patch) from its own GET, then hits a benign
// conflict; the owned leaf changes externally before attempt 2's GET. Without
// a fresh copy of the caller's patch per attempt, backfill's in-place mutation
// of a shared map would make attempt 2 see the leaf as already present from
// attempt 1 and skip re-backfilling it, submitting the stale value instead of
// re-reading it from its own fresh GET.
func TestApplyControllerSpecSSAIfExists_RebackfillsFreshValueOnRetry(t *testing.T) {
	existing := ifExistsController()
	if err := unstructured.SetNestedField(existing.Object, "jenkins:v1", "spec", "jenkinsImage"); err != nil {
		t.Fatalf("seed jenkinsImage: %v", err)
	}
	managedFields := []any{managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1",
		map[string]any{"f:jenkinsImage": map[string]any{}})}
	if err := unstructured.SetNestedSlice(existing.Object, managedFields, "metadata", "managedFields"); err != nil {
		t.Fatalf("seed managedFields: %v", err)
	}

	c, dyn := newIfExistsTestClient(t, existing)

	getAttempts := 0
	dyn.PrependReactor("get", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		obj, err := dyn.Tracker().Get(controllerGVR, ga.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		getAttempts++
		if getAttempts > 1 {
			// The owned leaf moves between attempt 1's GET and attempt 2's.
			if err := unstructured.SetNestedField(u.Object, "jenkins:v2", "spec", "jenkinsImage"); err != nil {
				t.Fatalf("bump jenkinsImage: %v", err)
			}
		}
		return true, u, nil
	})

	patchAttempts := 0
	var submittedImages []string
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		patchAttempts++
		var body map[string]any
		if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		spec, _ := body["spec"].(map[string]any)
		img, _ := spec["jenkinsImage"].(string)
		submittedImages = append(submittedImages, img)
		if patchAttempts == 1 {
			return true, nil, apierrors.NewConflict(controllerGVR.GroupResource(), pa.GetName(),
				fmt.Errorf("injected benign conflict from a concurrent status patch"))
		}
		stored, err := dyn.Tracker().Get(controllerGVR, pa.GetNamespace(), pa.GetName())
		if err != nil {
			return true, nil, err
		}
		merged := stored.(*unstructured.Unstructured).DeepCopy()
		for k, v := range spec {
			unstructured.SetNestedField(merged.Object, v, "spec", k) //nolint:errcheck // test-only merge
		}
		if err := dyn.Tracker().Update(controllerGVR, merged, pa.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, merged, nil
	})

	_, _, err := c.ApplyControllerSpecSSAIfExists(context.Background(), "ns1", "ctrl1",
		map[string]any{"powerState": "Stopped"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSAIfExists: %v, want success after retry", err)
	}
	if len(submittedImages) < 2 {
		t.Fatalf("patch attempts = %d, want at least 2 (retry must have happened)", len(submittedImages))
	}
	if submittedImages[0] != "jenkins:v1" {
		t.Fatalf("attempt 1 backfilled jenkinsImage = %q, want jenkins:v1 (its own GET's value)", submittedImages[0])
	}
	if last := submittedImages[len(submittedImages)-1]; last != "jenkins:v2" {
		t.Fatalf("final attempt backfilled jenkinsImage = %q, want jenkins:v2 (its own fresh GET's value, not attempt 1's stale backfill carried in a shared map)", last)
	}
}

// TestApplyControllerSpecSSAIfExists_FieldManagerConflictNotRetried simulates
// an SSA ownership conflict (force=false, another manager owns a field this
// apply touches): the identical apply would fail identically on every retry,
// so unlike a stale-resourceVersion conflict it must surface after exactly
// one patch attempt instead of consuming the retry budget.
func TestApplyControllerSpecSSAIfExists_FieldManagerConflictNotRetried(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, ifExistsController())

	patchAttempts := 0
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		patchAttempts++
		return true, nil, apierrors.NewApplyConflict([]metav1.StatusCause{{
			Type:    metav1.CauseTypeFieldManagerConflict,
			Field:   "spec.powerState",
			Message: `conflict with "other-manager"`,
		}}, "apply failed with conflicts")
	})

	_, _, err := c.ApplyControllerSpecSSAIfExists(context.Background(), "ns1", "ctrl1",
		map[string]any{"powerState": "Stopped"}, "varroa-ui", false)
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if !isFieldManagerConflict(err) {
		t.Fatalf("expected err to be recognized as a field-manager conflict, got %v", err)
	}
	if patchAttempts != 1 {
		t.Fatalf("patch attempts = %d, want exactly 1 (a field-manager conflict must not be retried)", patchAttempts)
	}
}

// TestApplyControllerSpecSSAIfExists_HappyPath verifies the ordinary case:
// an existing controller gets spec.powerState applied and the resulting
// object reflects it.
func TestApplyControllerSpecSSAIfExists_HappyPath(t *testing.T) {
	c, dyn := newIfExistsTestClient(t, ifExistsController())

	// The fake dynamic client's default Apply path requires a typed
	// conversion this test's unstructured-only scheme does not provide, so a
	// patch reactor stands in for the real apiserver's SSA merge: fold the
	// submitted spec onto the stored object and persist it, exactly what a
	// single-field powerState apply resolves to.
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		var body map[string]any
		if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		stored, err := dyn.Tracker().Get(controllerGVR, pa.GetNamespace(), pa.GetName())
		if err != nil {
			return true, nil, err
		}
		merged := stored.(*unstructured.Unstructured).DeepCopy()
		if spec, ok := body["spec"].(map[string]any); ok {
			for k, v := range spec {
				unstructured.SetNestedField(merged.Object, v, "spec", k) //nolint:errcheck // test-only merge
			}
		}
		if err := dyn.Tracker().Update(controllerGVR, merged, pa.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, merged, nil
	})

	out, unapplied, err := c.ApplyControllerSpecSSAIfExists(context.Background(), "ns1", "ctrl1",
		map[string]any{"powerState": "Stopped"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSAIfExists: %v", err)
	}
	if len(unapplied) != 0 {
		t.Fatalf("unapplied removals = %v, want none", unapplied)
	}
	if out == nil || out.Spec.PowerState != "Stopped" {
		t.Fatalf("returned controller spec.powerState = %#v, want Stopped", out)
	}

	got, err := dyn.Resource(controllerGVR).Namespace("ns1").Get(context.Background(), "ctrl1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ps, _, _ := unstructured.NestedString(got.Object, "spec", "powerState"); ps != "Stopped" {
		t.Fatalf("stored spec.powerState = %q, want Stopped", ps)
	}
}
