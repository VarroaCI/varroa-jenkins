package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

// TestPatchComposedBundleStatus_ClearsStaleErrors guards the regression where a
// compose error persisted in ComposedBundle status after a later successful
// recompose. errors/warnings/message carry `omitempty`, so an empty value is
// dropped from a JSON merge patch and the stale prior value survives in etcd.
func TestPatchComposedBundleStatus_ClearsStaleErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "ComposedBundle"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ComposedBundleList"), &unstructured.UnstructuredList{})

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "ComposedBundle",
		"metadata":   map[string]interface{}{"name": "b", "namespace": "ns"},
		"status": map[string]interface{}{
			"phase":   "Invalid",
			"errors":  []interface{}{"compose error: stat /tmp/varroa-catalogs: no such file or directory"},
			"message": "compose error: stat /tmp/varroa-catalogs: no such file or directory",
		},
	}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{composedBundleGVR: "ComposedBundleList"},
		existing,
	)
	c := &ClientsetClient{dynamic: dyn}

	// A successful recompose: no errors, no message.
	err := c.PatchObjectStatus(context.Background(), composedBundleGVR, "ns", "b", &v1alpha1.ComposedBundleStatus{
		Phase:        v1alpha1.ComposedBundleReady,
		ResolvedHash: "newhash",
	})
	if err != nil {
		t.Fatalf("patch failed: %v", err)
	}

	got, err := dyn.Resource(composedBundleGVR).Namespace("ns").Get(context.Background(), "b", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	status, _, _ := unstructured.NestedMap(got.Object, "status")

	if errs, found := status["errors"]; found && errs != nil {
		if s, ok := errs.([]interface{}); !ok || len(s) > 0 {
			t.Errorf("expected errors cleared, got: %v", errs)
		}
	}
	if msg, found := status["message"]; found && msg != nil && msg != "" {
		t.Errorf("expected message cleared, got: %v", msg)
	}
	if rh, _, _ := unstructured.NestedString(status, "resolvedHash"); rh != "newhash" {
		t.Errorf("expected resolvedHash=newhash, got: %q", rh)
	}
}

// TestPatchCatalogSourceStatus_ClearsStaleMessage guards the regression where a
// git-fetch error or nonzero item count persisted in CatalogSource status after
// a later successful sync. message and itemCount carry `omitempty`, so an empty
// value is dropped from a JSON merge patch and the stale prior value survives
// in etcd.
func TestPatchCatalogSourceStatus_ClearsStaleMessage(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "CatalogSource"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("CatalogSourceList"), &unstructured.UnstructuredList{})

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "CatalogSource",
		"metadata":   map[string]interface{}{"name": "src", "namespace": "ns"},
		"status": map[string]interface{}{
			"phase":     "Error",
			"itemCount": int64(5),
			"message":   "git fetch failed: some stale error",
		},
	}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{catalogSourceGVR: "CatalogSourceList"},
		existing,
	)
	c := &ClientsetClient{dynamic: dyn}

	// A successful sync: no error, no message, zero items.
	err := c.PatchObjectStatus(context.Background(), catalogSourceGVR, "ns", "src", &v1alpha1.CatalogSourceStatus{
		Phase:     v1alpha1.CatalogSyncReady,
		ItemCount: 0,
	})
	if err != nil {
		t.Fatalf("patch failed: %v", err)
	}

	got, err := dyn.Resource(catalogSourceGVR).Namespace("ns").Get(context.Background(), "src", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	status, _, _ := unstructured.NestedMap(got.Object, "status")

	if msg, found := status["message"]; found && msg != nil && msg != "" {
		t.Errorf("expected message cleared, got: %v", msg)
	}
	if phase, _, _ := unstructured.NestedString(status, "phase"); phase != "Ready" {
		t.Errorf("expected phase=Ready, got: %q", phase)
	}
	ic, found, _ := unstructured.NestedInt64(got.Object, "status", "itemCount")
	if !found || ic != 0 {
		t.Errorf("expected itemCount=0 (found=%v), got: %d", found, ic)
	}
}

func TestEnsureStatefulSetPodLabel(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	stsGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	scheme.AddKnownTypeWithName(stsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(stsGVK.GroupVersion().WithKind("StatefulSetList"), &unstructured.UnstructuredList{})

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": map[string]interface{}{
			"name": "controller", "namespace": "team-a", "labels": map[string]interface{}{"app": "object-label"},
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "controller"}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "controller"}},
			},
		},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{stsGVR: "StatefulSetList"}, existing)
	updates := 0
	dyn.PrependReactor("update", "statefulsets", func(clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})
	c := &ClientsetClient{dynamic: dyn}

	patched, err := c.EnsureStatefulSetPodLabel(ctx, "team-a", "controller", "app.kubernetes.io/managed-by", "varroa-operator")
	if err != nil {
		t.Fatalf("EnsureStatefulSetPodLabel: %v", err)
	}
	if !patched {
		t.Fatal("expected unlabeled StatefulSet to be patched")
	}
	if updates != 1 {
		t.Fatalf("expected one update, got %d", updates)
	}

	got, err := dyn.Resource(stsGVR).Namespace("team-a").Get(ctx, "controller", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get StatefulSet: %v", err)
	}
	podLabels, _, _ := unstructured.NestedStringMap(got.Object, "spec", "template", "metadata", "labels")
	if got := podLabels["app.kubernetes.io/managed-by"]; got != "varroa-operator" {
		t.Errorf("pod template managed-by label = %q, want varroa-operator", got)
	}
	selector, _, _ := unstructured.NestedStringMap(got.Object, "spec", "selector", "matchLabels")
	if len(selector) != 1 || selector["app"] != "controller" {
		t.Errorf("selector changed: %#v", selector)
	}
	if labels := got.GetLabels(); len(labels) != 1 || labels["app"] != "object-label" {
		t.Errorf("StatefulSet metadata labels changed: %#v", labels)
	}

	patched, err = c.EnsureStatefulSetPodLabel(ctx, "team-a", "controller", "app.kubernetes.io/managed-by", "varroa-operator")
	if err != nil {
		t.Fatalf("EnsureStatefulSetPodLabel second call: %v", err)
	}
	if patched {
		t.Fatal("expected labeled StatefulSet to be a no-op")
	}
	if updates != 1 {
		t.Errorf("expected no second update, got %d total", updates)
	}
}

// --- Server-side spec completion harness ------------------------------------
//
// The fake dynamic client does not implement server-side apply, so completion
// tests assert the PAYLOAD we SEND, never a simulated applied object. Each
// test seeds the fake's stored object (including metadata.managedFields) and
// captures the types.ApplyPatchType body via a PrependReactor on patch.

// ssaHarness bundles a ClientsetClient backed by a dynamic fake with a patch
// reactor that captures every ApplyPatchType body.
type ssaHarness struct {
	client   *ClientsetClient
	captured []map[string]any
}

// newSSAHarness builds the harness. current may be nil (no stored object),
// in which case the completion Get returns NotFound. appliedSpec, when given,
// overrides the applied object the patch reactor returns — the apiserver's
// post-SSA object, which may retain fields another manager owns (the default
// echoes the patch body). A nil appliedSpec returns the patch body as-is.
func newSSAHarness(t *testing.T, current *unstructured.Unstructured, appliedSpec ...map[string]any) *ssaHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "Controller"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControllerList"), &unstructured.UnstructuredList{})

	var seeds []runtime.Object
	if current != nil {
		seeds = append(seeds, current)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{controllerGVR: "ControllerList"},
		seeds...,
	)
	h := &ssaHarness{client: &ClientsetClient{dynamic: dyn}}
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchActionImpl)
		if pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		var body map[string]any
		if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		h.captured = append(h.captured, body)
		if len(appliedSpec) > 0 {
			// Return a fixed applied object carrying the given spec, so the
			// seam's unapplied-removal check runs against it.
			return true, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "varroa.dev/v1alpha1",
				"kind":       "Controller",
				"metadata":   map[string]any{"name": "ctrl1", "namespace": "ns1"},
				"spec":       appliedSpec[0],
			}}, nil
		}
		// Return a valid Controller unstructured so FromUnstructured conversion runs.
		return true, &unstructured.Unstructured{Object: body}, nil
	})
	return h
}

// managedFieldEntry builds one metadata.managedFields entry carrying an f:spec
// subtree of the given fieldsV1 shape.
func managedFieldEntry(manager, operation, apiVersion string, specFields map[string]any) map[string]any {
	return map[string]any{
		"manager":    manager,
		"operation":  operation,
		"apiVersion": apiVersion,
		"fieldsType": "FieldsV1",
		"fieldsV1":   map[string]any{"f:spec": specFields},
	}
}

// completionCurrent builds a stored Controller unstructured (name "ctrl1" in
// namespace "ns1") with the given spec and managedFields.
func completionCurrent(spec map[string]any, managedFields []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata": map[string]any{
			"name":          "ctrl1",
			"namespace":     "ns1",
			"managedFields": managedFields,
		},
		"spec": spec,
	}}
}

// mustCapturedSpec asserts exactly one ApplyPatchType body was sent and
// returns its spec map.
func (h *ssaHarness) mustCapturedSpec(t *testing.T) map[string]any {
	t.Helper()
	if len(h.captured) != 1 {
		t.Fatalf("expected exactly 1 captured ApplyPatchType body, got %d", len(h.captured))
	}
	spec, ok := h.captured[0]["spec"].(map[string]any)
	if !ok {
		t.Fatalf("captured patch has no spec map: %#v", h.captured[0])
	}
	return spec
}

// assertSpecJSONEqual compares two spec maps by canonical JSON.
func assertSpecJSONEqual(t *testing.T, got, want map[string]any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("spec mismatch:\n got: %s\nwant: %s", g, w)
	}
}

// TestClientsetClient_ApplyControllerSpecSSA_NoNamespaceInPatch verifies F1:
// when ApplyControllerSpecSSA receives a sparse specPatch, the serialized body
// must contain only the fields the caller set PLUS the leaves this manager
// already owns — never "namespace" (nor any other unset ControllerSpec field).
// Completion deliberately backfills owned leaves, so the pre-completion
// assertion "only user-set fields appear" is rewritten: owned leaves are
// present at their current value, unowned leaves are absent, and the F1 guard
// ("namespace never appears") still holds.
func TestClientsetClient_ApplyControllerSpecSSA_NoNamespaceInPatch(t *testing.T) {
	current := completionCurrent(map[string]any{
		"version":           "2.479",
		"ingressSpec":       map[string]any{"mode": "subdomain", "host": "c.example.org"}, // unowned
		"composedBundleRef": map[string]any{"name": "bundle-x"},                           // unowned
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:version": map[string]any{},
		}),
	})
	h := newSSAHarness(t, current)

	// Sparse spec: only "resources".
	specPatch := map[string]any{
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m"},
		},
	}

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1", specPatch, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)

	// User-set field present.
	if _, hasResources := spec["resources"]; !hasResources {
		t.Fatal("spec has no 'resources' key — user-set field missing")
	}

	// Owned leaf present at its current value.
	if v, ok := spec["version"].(string); !ok || v != "2.479" {
		t.Fatalf("owned leaf 'version' = %v, want backfilled current value %q", spec["version"], "2.479")
	}

	// F1: spec must NOT contain "namespace" (zero-value from a typed round-trip
	// would claim ownership).
	if _, hasNS := spec["namespace"]; hasNS {
		t.Fatal("spec contains 'namespace' key — SSA would claim ownership of namespace field")
	}

	// Unowned leaves are absent — completion never adds a leaf the manager does
	// not already own.
	for _, unowned := range []string{"ingressSpec", "composedBundleRef"} {
		if _, ok := spec[unowned]; ok {
			t.Errorf("unowned leaf %q must not appear in the completed patch", unowned)
		}
	}
}

// TestClientsetClient_ApplyControllerSpecSSA_OwnedLeafBackfilled (5.2): an
// owned leaf omitted from the patch is present in the payload at its current
// value; the patch wins for the leaf it does set.
func TestClientsetClient_ApplyControllerSpecSSA_OwnedLeafBackfilled(t *testing.T) {
	current := completionCurrent(map[string]any{
		"version":     "2.479",
		"ingressSpec": map[string]any{"mode": "subdomain", "host": "c.example.org"},
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:version":     map[string]any{},
			"f:ingressSpec": map[string]any{},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.5"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{
		"version":     "2.5",                                                        // patch wins
		"ingressSpec": map[string]any{"mode": "subdomain", "host": "c.example.org"}, // owned leaf omitted -> current value
	})
}

// TestClientsetClient_ApplyControllerSpecSSA_UnownedLeafAbsent (5.3): an
// unowned leaf omitted from the patch stays absent from the payload.
func TestClientsetClient_ApplyControllerSpecSSA_UnownedLeafAbsent(t *testing.T) {
	current := completionCurrent(map[string]any{
		"version":           "2.479",
		"composedBundleRef": map[string]any{"name": "bundle-x"},
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:version": map[string]any{},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.5"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{"version": "2.5"})
}

// TestClientsetClient_ApplyControllerSpecSSA_NoApplyEntry (5.4): with no Apply
// entry for the manager, the payload equals the patch exactly. The only entry
// belongs to a DIFFERENT manager and owns f:className — a leaf the patch does
// NOT set — so if the manager filter were (wrongly) dropped and that record
// matched, className would appear in the payload at its current value and this
// assertion would fail. (Seeding the patch's own field would be vacuous:
// backfill has nothing to contribute either way.)
func TestClientsetClient_ApplyControllerSpecSSA_NoApplyEntry(t *testing.T) {
	// Only an Apply entry for a DIFFERENT manager exists.
	current := completionCurrent(map[string]any{
		"version":   "2.479",
		"className": "standard",
	}, []any{
		managedFieldEntry("mgr-gitops", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:className": map[string]any{},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.5"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{"version": "2.5"})
}

// TestClientsetClient_ApplyControllerSpecSSA_UpdateEntryIgnored (5.5): an
// Update entry under the same manager name is a distinct ownership record; its
// leaves are NOT backfilled into the Apply patch. The entry owns f:className —
// a leaf the patch does NOT set — so if the Update record were (wrongly)
// treated as Apply ownership, className would appear in the payload at its
// current value and this assertion would fail. (Seeding the patch's own field
// would be vacuous: backfill has nothing to contribute either way.)
func TestClientsetClient_ApplyControllerSpecSSA_UpdateEntryIgnored(t *testing.T) {
	current := completionCurrent(map[string]any{
		"version":   "2.479",
		"className": "standard",
	}, []any{
		managedFieldEntry("varroa-ui", "Update", "varroa.dev/v1alpha1", map[string]any{
			"f:className": map[string]any{},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.5"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{"version": "2.5"})
}

// TestClientsetClient_ApplyControllerSpecSSA_WrongAPIVersionIgnored (5.6): an
// Apply entry recorded under a different apiVersion describes a different
// schema and is ignored. The entry owns f:className — a leaf the patch does
// NOT set — so if a mismatched-apiVersion record were (wrongly) matched,
// className would appear in the payload at its current value and this
// assertion would fail. (Seeding the patch's own field would be vacuous:
// backfill has nothing to contribute either way.)
func TestClientsetClient_ApplyControllerSpecSSA_WrongAPIVersionIgnored(t *testing.T) {
	current := completionCurrent(map[string]any{
		"version":   "2.479",
		"className": "standard",
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha2", map[string]any{
			"f:className": map[string]any{},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.5"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{"version": "2.5"})
}

// TestClientsetClient_ApplyControllerSpecSSA_NestedMapPreservesSiblings (5.7):
// a nested patch {hibernation:{gracePeriodMinutes:X}} must not drop
// hibernation.enabled / hibernation.activityIgnoreRegex.
func TestClientsetClient_ApplyControllerSpecSSA_NestedMapPreservesSiblings(t *testing.T) {
	current := completionCurrent(map[string]any{
		"hibernation": map[string]any{
			"enabled":             true,
			"gracePeriodMinutes":  float64(30),
			"activityIgnoreRegex": "cron",
		},
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:hibernation": map[string]any{
				"f:enabled":             map[string]any{},
				"f:gracePeriodMinutes":  map[string]any{},
				"f:activityIgnoreRegex": map[string]any{},
			},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"hibernation": map[string]any{"gracePeriodMinutes": float64(60)}}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{
		"hibernation": map[string]any{
			"gracePeriodMinutes":  float64(60),
			"enabled":             true,
			"activityIgnoreRegex": "cron",
		},
	})
}

// TestClientsetClient_ApplyControllerSpecSSA_DetachNullNotRestored (5.8):
// {composedBundleRef: null} must be absent from the payload — a removal is
// never backfilled from the current object, so a detach actually detaches.
func TestClientsetClient_ApplyControllerSpecSSA_DetachNullNotRestored(t *testing.T) {
	current := completionCurrent(map[string]any{
		"version":           "2.479",
		"composedBundleRef": map[string]any{"name": "bundle-x", "namespace": "ns1"},
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:version":           map[string]any{},
			"f:composedBundleRef": map[string]any{"f:name": map[string]any{}},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"composedBundleRef": nil}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	if _, ok := spec["composedBundleRef"]; ok {
		t.Fatal("composedBundleRef must be ABSENT from the payload (null removal, never backfilled)")
	}
	// Other owned leaves are still backfilled.
	assertSpecJSONEqual(t, spec, map[string]any{"version": "2.479"})
}

// TestClientsetClient_ApplyControllerSpecSSA_NestedNullKeepsContainer (5.9):
// {hibernation:{activityIgnoreRegex:null}} keeps hibernation present while
// releasing activityIgnoreRegex.
func TestClientsetClient_ApplyControllerSpecSSA_NestedNullKeepsContainer(t *testing.T) {
	current := completionCurrent(map[string]any{
		"hibernation": map[string]any{
			"enabled":             true,
			"gracePeriodMinutes":  float64(30),
			"activityIgnoreRegex": "cron",
		},
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:hibernation": map[string]any{
				"f:enabled":             map[string]any{},
				"f:gracePeriodMinutes":  map[string]any{},
				"f:activityIgnoreRegex": map[string]any{},
			},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"hibernation": map[string]any{"activityIgnoreRegex": nil}}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	hibernation, ok := spec["hibernation"].(map[string]any)
	if !ok {
		t.Fatalf("hibernation must remain present, got %#v", spec["hibernation"])
	}
	if _, has := hibernation["activityIgnoreRegex"]; has {
		t.Error("activityIgnoreRegex must be ABSENT (null removal)")
	}
	if enabled, ok := hibernation["enabled"].(bool); !ok || !enabled {
		t.Errorf("enabled = %v, want true (owned sibling preserved)", hibernation["enabled"])
	}
	if gp, ok := hibernation["gracePeriodMinutes"].(float64); !ok || gp != 30 {
		t.Errorf("gracePeriodMinutes = %v, want 30", hibernation["gracePeriodMinutes"])
	}
}

// TestClientsetClient_ApplyControllerSpecSSA_NullInListRejected (5.10, seam):
// a null reachable through a list is invalid input — either a direct null
// element or a null nested inside a list entry's map. translateNulls rejects
// it with ErrNullInList and NO apply is attempted (the patch reactor is never
// called). Both the BFF and brood routes map this sentinel (tested in their
// own packages); here we pin the seam's no-apply guarantee.
func TestClientsetClient_ApplyControllerSpecSSA_NullInListRejected(t *testing.T) {
	cases := []struct {
		name  string
		patch map[string]any
	}{
		{
			name: "direct null element",
			patch: map[string]any{
				"podOverrides": map[string]any{"tolerations": []any{nil}},
			},
		},
		{
			name: "null nested inside a list entry's map",
			patch: map[string]any{
				"resources": map[string]any{"claims": []any{
					map[string]any{"name": "a", "request": nil},
				}},
			},
		},
		{
			name: "null in a nested list",
			patch: map[string]any{
				"podOverrides": map[string]any{
					"volumes": []any{
						map[string]any{"name": "v", "secret": map[string]any{"items": []any{nil}}},
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSSAHarness(t, nil)
			_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
				tc.patch, "varroa-ui", false)
			if !errors.Is(err, ErrNullInList) {
				t.Fatalf("err = %v, want ErrNullInList", err)
			}
			if len(h.captured) != 0 {
				t.Fatalf("reactor was called %d time(s) — no apply may be attempted", len(h.captured))
			}
		})
	}
}

// TestClientsetClient_ApplyControllerSpecSSA_AssocListNoMatchingEntries pins
// that when none of the owned k: entries appear in the current list, the list
// is left ABSENT from the payload (never asserted as []), so a stale
// managedFields window cannot wipe entries another manager owns.
func TestClientsetClient_ApplyControllerSpecSSA_AssocListNoMatchingEntries(t *testing.T) {
	current := completionCurrent(map[string]any{
		"resources": map[string]any{
			"claims": []any{map[string]any{"name": "b", "request": "y"}},
		},
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:resources": map[string]any{
				"f:claims": map[string]any{
					"k:{\"name\":\"a\"}": map[string]any{
						"f:name":    map[string]any{},
						"f:request": map[string]any{},
					},
				},
			},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}
	spec := h.mustCapturedSpec(t)
	// Owned entry "a" is not in the current list: nothing to backfill, so the
	// whole resources subtree must stay absent — never asserted as [].
	assertSpecJSONEqual(t, spec, map[string]any{})
}

// TestTranslateNulls pins translateNulls and listContainsNull directly (the
// seam exercises them through ApplyControllerSpecSSA, but the list-rejection
// and removal-path shapes are clearer here).
func TestTranslateNulls(t *testing.T) {
	t.Run("nulls removed and recorded at any depth", func(t *testing.T) {
		cleaned, removals, err := translateNulls(map[string]any{
			"composedBundleRef": nil,
			"hibernation": map[string]any{
				"enabled":             true,
				"activityIgnoreRegex": nil,
			},
			"version": "2.479",
		})
		if err != nil {
			t.Fatalf("translateNulls: %v", err)
		}
		if !removals["composedBundleRef"] {
			t.Error("top-level removal not recorded")
		}
		if !removals["hibernation.activityIgnoreRegex"] {
			t.Error("nested removal path not recorded")
		}
		if _, ok := cleaned["composedBundleRef"]; ok {
			t.Error("null top-level key not deleted")
		}
		// A map emptied by null removal is KEPT as an empty map (4.1).
		hibernation, ok := cleaned["hibernation"].(map[string]any)
		if !ok {
			t.Fatalf("hibernation not kept: %#v", cleaned["hibernation"])
		}
		if enabled, _ := hibernation["enabled"].(bool); !enabled {
			t.Errorf("hibernation.enabled lost: %#v", hibernation)
		}
		if _, has := hibernation["activityIgnoreRegex"]; has {
			t.Error("hibernation.activityIgnoreRegex not deleted")
		}
		if v, _ := cleaned["version"].(string); v != "2.479" {
			t.Errorf("version lost: %v", cleaned["version"])
		}
	})

	t.Run("null inside a list rejected at any depth", func(t *testing.T) {
		cases := []map[string]any{
			{"podOverrides": map[string]any{"tolerations": []any{nil}}},
			{"resources": map[string]any{"claims": []any{map[string]any{"name": "a", "request": nil}}}},
			{"podOverrides": map[string]any{"volumes": []any{map[string]any{"name": "v", "secret": map[string]any{"items": []any{nil}}}}}},
		}
		for _, patch := range cases {
			if _, _, err := translateNulls(patch); !errors.Is(err, ErrNullInList) {
				t.Errorf("translateNulls(%v) err = %v, want ErrNullInList", patch, err)
			}
		}
	})

	t.Run("null-free list passes through verbatim", func(t *testing.T) {
		cleaned, removals, err := translateNulls(map[string]any{
			"resources": map[string]any{"claims": []any{map[string]any{"name": "a", "request": "x"}}},
		})
		if err != nil {
			t.Fatalf("translateNulls: %v", err)
		}
		if len(removals) != 0 {
			t.Errorf("unexpected removals: %v", removals)
		}
		if _, ok := cleaned["resources"].(map[string]any); !ok {
			t.Errorf("resources lost: %#v", cleaned)
		}
	})
}

// TestFilterSetValues pins the v: (set-type list) branch of the traversal: only
// the listed scalar values are backfilled, in list order. The current CRD has
// no set-type list, so the traversal is total over fieldsV1 and this function
// is unit-tested directly rather than through a seam apply.
func TestFilterSetValues(t *testing.T) {
	list := []any{"blue", "green", "red"}
	got := filterSetValues(list, []string{`"blue"`, `"red"`})
	want := []any{"blue", "red"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if filtered := filterSetValues([]any{"a", "b"}, []string{`"z"`}); filtered != nil {
		t.Errorf("no matches must return nil, got %v", filtered)
	}
}

// TestBackfillAssocList_DeterministicOrder guards the payload-order stability
// of associative-list backfill. Go map iteration is randomized, so emitting
// owned.entries in iteration order would put the entries in a different order
// on every run. The apiserver merges list-map entries by key, so the order
// itself never changes the apply result — but it would make the payload (and
// any assertion on it) unstable, so the merge keys are sorted instead. A
// single call cannot reliably catch map randomization, so the function is
// called many times and the exact sorted output (a, m, z) is pinned.
func TestBackfillAssocList_DeterministicOrder(t *testing.T) {
	owned := &completionTree{entries: map[string]*completionTree{
		`{"name":"z"}`: {children: map[string]*completionTree{
			"request": {copyWhole: true},
		}},
		`{"name":"a"}`: {children: map[string]*completionTree{
			"request": {copyWhole: true},
		}},
		`{"name":"m"}`: {children: map[string]*completionTree{
			"request": {copyWhole: true},
		}},
	}}
	cur := []any{
		map[string]any{"name": "z", "request": "zr"},
		map[string]any{"name": "a", "request": "ar"},
		map[string]any{"name": "m", "request": "mr"},
	}

	first := backfillAssocList(cur, owned, map[string]bool{}, "resources.claims")
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first result: %v", err)
	}
	for i := 0; i < 20; i++ {
		gotJSON, err := json.Marshal(backfillAssocList(cur, owned, map[string]bool{}, "resources.claims"))
		if err != nil {
			t.Fatalf("marshal iteration %d: %v", i, err)
		}
		if string(gotJSON) != string(firstJSON) {
			t.Fatalf("iteration %d produced a different order:\n first: %s\n   got: %s", i, firstJSON, gotJSON)
		}
	}

	// Pin the exact order too: a lexical sort of the merge-key JSON yields
	// entries a, m, z regardless of map iteration order.
	assertSpecJSONEqual(t, map[string]any{"claims": first}, map[string]any{"claims": []any{
		map[string]any{"name": "a", "request": "ar"},
		map[string]any{"name": "m", "request": "mr"},
		map[string]any{"name": "z", "request": "zr"},
	}})
}

// TestClientsetClient_ApplyControllerSpecSSA_AssocListOwnedEntries (5.11): only
// the owned associative-list entries are backfilled, and only their owned
// sub-fields.
func TestClientsetClient_ApplyControllerSpecSSA_AssocListOwnedEntries(t *testing.T) {
	current := completionCurrent(map[string]any{
		"resources": map[string]any{
			"claims": []any{
				map[string]any{"name": "a", "request": "x", "resizePolicy": "owned-by-other"},
				map[string]any{"name": "b", "request": "y"},
			},
		},
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:resources": map[string]any{
				"f:claims": map[string]any{
					"k:{\"name\":\"a\"}": map[string]any{
						"f:name":    map[string]any{},
						"f:request": map[string]any{},
					},
				},
			},
		}),
	})
	h := newSSAHarness(t, current)

	// Patch omits resources entirely.
	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{
		"resources": map[string]any{
			"claims": []any{
				map[string]any{"name": "a", "request": "x"}, // only owned entry, only owned sub-fields
			},
		},
	})
}

// TestClientsetClient_ApplyControllerSpecSSA_AssocListPatchVerbatim (5.12): a
// patch-supplied list is used verbatim; owned entries are NOT re-injected
// (otherwise removing one entry from an associative list would be impossible).
func TestClientsetClient_ApplyControllerSpecSSA_AssocListPatchVerbatim(t *testing.T) {
	current := completionCurrent(map[string]any{
		"resources": map[string]any{
			"claims": []any{map[string]any{"name": "a", "request": "x"}},
		},
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:resources": map[string]any{
				"f:claims": map[string]any{
					"k:{\"name\":\"a\"}": map[string]any{
						"f:name":    map[string]any{},
						"f:request": map[string]any{},
					},
				},
			},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"resources": map[string]any{"claims": []any{map[string]any{"name": "c", "request": "z"}}}},
		"varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{
		"resources": map[string]any{
			"claims": []any{map[string]any{"name": "c", "request": "z"}},
		},
	})
}

// TestClientsetClient_ApplyControllerSpecSSA_EmptyNodeCopiesWholeValue (5.13):
// a bare {} fieldsV1 node (opaque owned leaf — atomic map, atomic list, or
// scalar) backfills the ENTIRE current value. It must never assert an empty
// map, which would wipe a populated atomic map.
func TestClientsetClient_ApplyControllerSpecSSA_EmptyNodeCopiesWholeValue(t *testing.T) {
	t.Run("populated map copied, never emptied", func(t *testing.T) {
		current := completionCurrent(map[string]any{
			"podOverrides": map[string]any{
				"podAnnotations": map[string]any{"a": "1", "b": "2"},
			},
		}, []any{
			managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
				"f:podOverrides": map[string]any{
					"f:podAnnotations": map[string]any{},
				},
			}),
		})
		h := newSSAHarness(t, current)

		_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
			map[string]any{}, "varroa-ui", false)
		if err != nil {
			t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
		}
		spec := h.mustCapturedSpec(t)
		assertSpecJSONEqual(t, spec, map[string]any{
			"podOverrides": map[string]any{
				"podAnnotations": map[string]any{"a": "1", "b": "2"},
			},
		})
	})

	t.Run("atomic list copied whole", func(t *testing.T) {
		current := completionCurrent(map[string]any{
			"podOverrides": map[string]any{
				"tolerations": []any{
					map[string]any{"key": "t1", "value": "v1", "effect": "NoSchedule"},
					map[string]any{"key": "t2", "operator": "Exists"},
				},
			},
		}, []any{
			managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
				"f:podOverrides": map[string]any{
					"f:tolerations": map[string]any{},
				},
			}),
		})
		h := newSSAHarness(t, current)

		_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
			map[string]any{}, "varroa-ui", false)
		if err != nil {
			t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
		}
		spec := h.mustCapturedSpec(t)
		assertSpecJSONEqual(t, spec, map[string]any{
			"podOverrides": map[string]any{
				"tolerations": []any{
					map[string]any{"key": "t1", "value": "v1", "effect": "NoSchedule"},
					map[string]any{"key": "t2", "operator": "Exists"},
				},
			},
		})
	})
}

// TestClientsetClient_ApplyControllerSpecSSA_ForceStillCompletes (5.14):
// force=true changes conflict handling only, never what is applied —
// completion still runs.
func TestClientsetClient_ApplyControllerSpecSSA_ForceStillCompletes(t *testing.T) {
	current := completionCurrent(map[string]any{
		"version": "2.479",
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:version": map[string]any{},
		}),
	})
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "100m"}}},
		"varroa-ui", true)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	spec := h.mustCapturedSpec(t)
	assertSpecJSONEqual(t, spec, map[string]any{
		"resources": map[string]any{"requests": map[string]any{"cpu": "100m"}},
		"version":   "2.479",
	})
}

// TestClientsetClient_ApplyControllerSpecSSA_AppliedResultReflectsCompletion
// (5.16) documents the harness rule: a test that needs a realistic APPLIED
// result must have the reactor return a seeded object (the fake dynamic client
// does not implement SSA), rather than relying on the patch-echo reactor. Here
// the reactor returns the current object with the patch merged onto its spec,
// and we assert the returned Controller reflects the backfilled state.
func TestClientsetClient_ApplyControllerSpecSSA_AppliedResultReflectsCompletion(t *testing.T) {
	current := completionCurrent(map[string]any{
		"version": "2.479",
	}, []any{
		managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
			"f:version": map[string]any{},
		}),
	})

	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "Controller"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControllerList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{controllerGVR: "ControllerList"},
		current,
	)
	c := &ClientsetClient{dynamic: dyn}
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchActionImpl)
		if pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		var body map[string]any
		if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		// Simulate the applied object: the stored current object with the
		// completed spec merged in.
		applied := current.DeepCopy()
		applied.Object["spec"] = body["spec"]
		return true, applied, nil
	})

	applied, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "100m"}}},
		"varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}
	if applied.Spec.Version != "2.479" {
		t.Errorf("applied spec.version = %q, want backfilled %q", applied.Spec.Version, "2.479")
	}
	if applied.Spec.Resources == nil || applied.Spec.Resources.Requests == nil {
		t.Fatalf("applied spec.resources missing: %+v", applied.Spec.Resources)
	}
	if cpu, ok := applied.Spec.Resources.Requests[corev1.ResourceCPU]; !ok || cpu.String() != "100m" {
		t.Errorf("applied resources.requests.cpu = %v, want 100m", cpu)
	}
}

// TestClientsetClient_ApplyControllerSpecSSA_IdempotentReapply verifies that
// a second Apply of the same sparse spec succeeds (idempotent) and that the
// payload is correctly built.
func TestClientsetClient_ApplyControllerSpecSSA_IdempotentReapply(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "Controller"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControllerList"), &unstructured.UnstructuredList{})

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{controllerGVR: "ControllerList"},
	)
	c := &ClientsetClient{dynamic: dyn}

	var patches [][]byte
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchActionImpl)
		if pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		patch := pa.GetPatch()
		patches = append(patches, patch)
		var body map[string]any
		if err := json.Unmarshal(patch, &body); err != nil {
			return true, nil, err
		}
		obj := &unstructured.Unstructured{Object: body}
		return true, obj, nil
	})

	specPatch := map[string]any{
		"version": "2.479",
	}

	// First apply.
	_, _, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl2", specPatch, "varroa-ui", false)
	if err != nil {
		t.Fatalf("first apply failed: %v", err)
	}

	// Second apply (same spec).
	_, _, err = c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl2", specPatch, "varroa-ui", false)
	if err != nil {
		t.Fatalf("second apply failed: %v", err)
	}

	if len(patches) != 2 {
		t.Fatalf("expected 2 patches, got %d", len(patches))
	}

	// Both patches should contain spec.version but NOT spec.namespace.
	for i, patch := range patches {
		var body map[string]any
		if err := json.Unmarshal(patch, &body); err != nil {
			t.Fatalf("unmarshal patch[%d]: %v", i, err)
		}
		spec, ok := body["spec"].(map[string]any)
		if !ok {
			t.Fatalf("patch[%d] has no spec key", i)
		}
		if _, hasVer := spec["version"]; !hasVer {
			t.Errorf("patch[%d] spec missing 'version'", i)
		}
		if _, hasNS := spec["namespace"]; hasNS {
			t.Errorf("patch[%d] spec contains 'namespace' — SSA would claim ownership", i)
		}
	}
}

// TestClientsetClient_ApplyControllerSpecSSA_ReportsRetainedZeroValueRemoval
// pins the Finding-1 fix: a removal of spec.hibernation.enabled does NOT take
// effect when another manager retained the field at its zero value (false).
// The applied object (returned by the patch reactor) still carries
// hibernation.enabled: false, so the seam MUST report it as unapplied. The
// typed Controller would marshal enabled=false as ABSENT (omitempty bool), so
// computing presence from the marshalled typed object — the old approach —
// would falsely report the removal as successful.
func TestClientsetClient_ApplyControllerSpecSSA_ReportsRetainedZeroValueRemoval(t *testing.T) {
	h := newSSAHarness(t, nil, map[string]any{
		// Another manager owns spec.hibernation.enabled at its zero value; the
		// apiserver's applied object keeps it after the removal request.
		"hibernation": map[string]any{"enabled": false},
	})

	applied, unapplied, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"hibernation": map[string]any{"enabled": nil}}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	want := []bus.UnappliedRemoval{{Field: "spec.hibernation.enabled"}}
	if !reflect.DeepEqual(unapplied, want) {
		t.Fatalf("unapplied = %+v, want %+v (a retained zero value must be reported as unapplied)", unapplied, want)
	}

	// Confirm the failure mode the fix removes: presence computed from the
	// MARSHALLED TYPED applied object misses the retained zero value, because
	// omitempty drops enabled=false (the pointer still marshals to a bare
	// {hibernation:{}}). This is exactly why the seam checks the unstructured
	// applied spec rather than the typed conversion below.
	if fromTyped := UnappliedRemovals(applied, []string{"hibernation.enabled"}); len(fromTyped) != 0 {
		t.Fatalf("typed presence check reported %+v, want none — the marshalled typed object must miss the retained zero value", fromTyped)
	}
}

// TestParseResourcesFromContainerMap_NonCPUResourceKeys verifies that
// parseResourcesFromContainerMap iterates ALL keys present in the live
// requests/limits maps — not just cpu/memory — so non-cpu/memory resource
// keys (ephemeral-storage, hugepages-*, extended resources) survive the parse
// and participate in the drift comparison rather than being silently dropped.
// Dropping them from the live side would create a permanent delta (desired has
// the key, live does not) and an endless Provisioning roll loop.
func TestParseResourcesFromContainerMap_NonCPUResourceKeys(t *testing.T) {
	// A live container map that includes ephemeral-storage alongside cpu/memory.
	cm := map[string]interface{}{
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{
				"cpu":               "500m",
				"memory":            "1Gi",
				"ephemeral-storage": "2Gi",
			},
			"limits": map[string]interface{}{
				"cpu":               "2",
				"memory":            "4Gi",
				"ephemeral-storage": "8Gi",
			},
		},
	}

	rr := parseResourcesFromContainerMap(cm)
	if rr == nil {
		t.Fatal("expected non-nil ResourceRequirements")
	}

	// Requests: all three keys must survive.
	if rr.Requests == nil {
		t.Fatal("expected non-nil Requests")
	}
	if cpu, ok := rr.Requests[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("500m")) != 0 {
		t.Errorf("requests cpu: got %v, want 500m", cpu)
	}
	if mem, ok := rr.Requests[corev1.ResourceMemory]; !ok || mem.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("requests memory: got %v, want 1Gi", mem)
	}
	if es, ok := rr.Requests[corev1.ResourceEphemeralStorage]; !ok || es.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("requests ephemeral-storage: got %v, want 2Gi (was it dropped?)", es)
	}

	// Limits: all three keys must survive.
	if rr.Limits == nil {
		t.Fatal("expected non-nil Limits")
	}
	if cpu, ok := rr.Limits[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("limits cpu: got %v, want 2", cpu)
	}
	if mem, ok := rr.Limits[corev1.ResourceMemory]; !ok || mem.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("limits memory: got %v, want 4Gi", mem)
	}
	if es, ok := rr.Limits[corev1.ResourceEphemeralStorage]; !ok || es.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("limits ephemeral-storage: got %v, want 8Gi (was it dropped?)", es)
	}
}

// TestQuantityListMap_RendersAllKeys pins the render side of the same
// invariant: dropping any desired key (ephemeral-storage, extended resources)
// from the rendered container map livelocks the controller, because the
// all-keys comparator sees the key missing from the live StatefulSet on every
// Running tick and rolls forever while re-renders keep dropping it.
func TestQuantityListMap_RendersAllKeys(t *testing.T) {
	list := corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("500m"),
		corev1.ResourceMemory:           resource.MustParse("1Gi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		"nvidia.com/gpu":                resource.MustParse("1"),
	}

	m := quantityListMap(list)
	want := map[string]string{
		"cpu":               "500m",
		"memory":            "1Gi",
		"ephemeral-storage": "2Gi",
		"nvidia.com/gpu":    "1",
	}
	if len(m) != len(want) {
		t.Fatalf("got %d keys (%v), want %d", len(m), m, len(want))
	}
	for k, w := range want {
		if got, ok := m[k].(string); !ok || got != w {
			t.Errorf("key %s: got %v, want %s (was it dropped?)", k, m[k], w)
		}
	}
}

// TestParseResourcesFromContainerMap_UnparseableValueOmitted verifies
// invariant I4: a per-key parse failure omits that key (it is NOT inserted
// with a zero/garbage quantity), so the omission is a deliberate delta
// signal to resourceListsEqual.
func TestParseResourcesFromContainerMap_UnparseableValueOmitted(t *testing.T) {
	cm := map[string]interface{}{
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{
				"cpu":               "500m",
				"ephemeral-storage": "not-a-quantity",
			},
		},
	}

	rr := parseResourcesFromContainerMap(cm)
	if rr == nil {
		t.Fatal("expected non-nil ResourceRequirements (cpu is parseable)")
	}

	// cpu must survive.
	if cpu, ok := rr.Requests[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("500m")) != 0 {
		t.Errorf("requests cpu: got %v, want 500m", cpu)
	}

	// ephemeral-storage must be OMITTED (I4: unparseable → omitted, delta).
	if _, ok := rr.Requests[corev1.ResourceEphemeralStorage]; ok {
		t.Error("requests ephemeral-storage must be omitted when unparseable (I4)")
	}
}

// TestParseResourcesFromContainerMap_EmptyOrMissingResourcesReturnsNil verifies
// that an empty resources block or missing requests/limits returns nil (not an
// empty non-nil struct), matching the existing nil/empty convention.
func TestParseResourcesFromContainerMap_EmptyOrMissingReturnsNil(t *testing.T) {
	// No resources key at all.
	if rr := parseResourcesFromContainerMap(map[string]interface{}{"name": "jenkins"}); rr != nil {
		t.Errorf("expected nil for no resources key, got %+v", rr)
	}

	// Empty resources map.
	if rr := parseResourcesFromContainerMap(map[string]interface{}{
		"resources": map[string]interface{}{},
	}); rr != nil {
		t.Errorf("expected nil for empty resources, got %+v", rr)
	}

	// Resources with only empty sub-maps.
	if rr := parseResourcesFromContainerMap(map[string]interface{}{
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{},
			"limits":   map[string]interface{}{},
		},
	}); rr != nil {
		t.Errorf("expected nil for empty requests+limits, got %+v", rr)
	}
}

// TestApplyUserCRD_StripsResourceVersionOnCreate asserts that ApplyUserCRD
// clears Create-forbidden metadata fields before Create: passing a
// ResourceVersion-bearing object to Create() gets a
// "resourceVersion should not be set on objects to be created" Invalid error
// from Kubernetes.
func TestApplyUserCRD_StripsResourceVersionOnCreate(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "User"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("UserList"), &unstructured.UnstructuredList{})

	// --- Create path: input carries a resourceVersion ---
	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "oidc-alice",
			Namespace:       "varroa-system",
			ResourceVersion: "12345",
		},
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{userGVR: "UserList"},
	)
	// Simulate the real apiserver: reject Create() when resourceVersion is
	// non-empty with a StatusReasonInvalid error.  This is the error the buggy
	// code saw (not IsAlreadyExists), which the old code failed to handle.
	dyn.PrependReactor("create", "users", func(action clienttesting.Action) (bool, runtime.Object, error) {
		obj := action.(clienttesting.CreateAction).GetObject()
		u, ok := obj.(*unstructured.Unstructured)
		if ok && u.GetResourceVersion() != "" {
			return true, nil, apierrors.NewInvalid(
				schema.GroupKind{Group: "varroa.dev", Kind: "User"},
				u.GetName(),
				nil,
			)
		}
		return false, nil, nil
	})
	c := &ClientsetClient{dynamic: dyn}

	err := crdstore.Apply[v1alpha1.User](context.Background(), c, user)
	if err != nil {
		t.Fatalf("Create path with resourceVersion set: %v", err)
	}

	got, err := dyn.Resource(userGVR).Namespace("varroa-system").Get(context.Background(), "oidc-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created user: %v", err)
	}
	if got.GetName() != "oidc-alice" {
		t.Errorf("expected name oidc-alice, got %s", got.GetName())
	}

	// --- Update path: object exists, update takes effect ---
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "User",
		"metadata": map[string]interface{}{
			"name":      "oidc-bob",
			"namespace": "varroa-system",
		},
		"spec": map[string]interface{}{
			"displayName": "Bob Original",
		},
	}}

	dyn2 := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{userGVR: "UserList"},
		existing,
	)
	c2 := &ClientsetClient{dynamic: dyn2}

	updatedUser := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oidc-bob",
			Namespace: "varroa-system",
		},
		Spec: v1alpha1.UserSpec{
			DisplayName: "Bob Updated",
		},
	}

	err = crdstore.Apply[v1alpha1.User](context.Background(), c2, updatedUser)
	if err != nil {
		t.Fatalf("Update path: %v", err)
	}

	got2, err := dyn2.Resource(userGVR).Namespace("varroa-system").Get(context.Background(), "oidc-bob", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated user: %v", err)
	}
	displayName, _, _ := unstructured.NestedString(got2.Object, "spec", "displayName")
	if displayName != "Bob Updated" {
		t.Errorf("expected displayName Bob Updated, got %q", displayName)
	}
}

// TestPatchControllerStatus_ClearsLastReconcileErrorAt asserts that a merge
// patch must explicitly null a pointer field to clear it — merely omitting
// the key leaves the prior value in etcd. This test sets LastReconcileErrorAt
// on the server-side object, patches with nil, and asserts the field is
// absent after the patch.
func TestPatchControllerStatus_ClearsLastReconcileErrorAt(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "Controller"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControllerList"), &unstructured.UnstructuredList{})

	now := metav1.Now()
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata":   map[string]interface{}{"name": "ctrl", "namespace": "ns"},
		"status": map[string]interface{}{
			"phase":                "Running",
			"lastReconcileError":   "some old error",
			"lastReconcileErrorAt": now.Format(time.RFC3339Nano),
		},
	}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{controllerGVR: "ControllerList"},
		existing,
	)
	c := &ClientsetClient{dynamic: dyn}

	// Patch with a nil LastReconcileErrorAt — this must explicitly null the field.
	err := c.PatchControllerStatus(context.Background(), "ctrl", "ns", &v1alpha1.ControllerStatus{
		Phase:                v1alpha1.ControllerPhaseRunning,
		LastReconcileError:   "",
		LastReconcileErrorAt: nil,
	})
	if err != nil {
		t.Fatalf("PatchControllerStatus failed: %v", err)
	}

	got, err := dyn.Resource(controllerGVR).Namespace("ns").Get(context.Background(), "ctrl", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}

	// LastReconcileErrorAt is a *metav1.Time; clearing patches it as nil → JSON
	// null → the merge patch deletes the key, so it must be strictly ABSENT.
	// (A regression that force-included "" instead would leave a present string
	// and pass a lenient found&&!="" check — this is exactly what this test guards.)
	if lat, found, _ := unstructured.NestedString(got.Object, "status", "lastReconcileErrorAt"); found {
		t.Errorf("lastReconcileErrorAt should be absent (nulled) after clearing, but key is present: %q", lat)
	}
	// LastReconcileError is a plain string; force-included as "" when cleared (a
	// merge patch can't null a non-pointer string field), so it is PRESENT-EMPTY,
	// not absent. Assert it carries no stale non-empty value.
	if ler, found, _ := unstructured.NestedString(got.Object, "status", "lastReconcileError"); found && ler != "" {
		t.Errorf("lastReconcileError should be empty after clearing, got %q", ler)
	}
}

// TestPatchCatalogItemStatus_ClearsStaleContent mirrors
// TestPatchComposedBundleStatus_ClearsStaleErrors: content carries
// `omitempty`, so a merge patch that omits it would leave stale bytes in
// etcd after an item becomes invalid. The patch must clear it explicitly.
func TestPatchCatalogItemStatus_ClearsStaleContent(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "CatalogItem"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("CatalogItemList"), &unstructured.UnstructuredList{})

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "CatalogItem",
		"metadata":   map[string]interface{}{"name": "i", "namespace": "ns"},
		"status": map[string]interface{}{
			"content":     "stale-bytes",
			"contentHash": "oldhash",
			"valid":       true,
		},
	}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{catalogItemGVR: "CatalogItemList"},
		existing,
	)
	c := &ClientsetClient{dynamic: dyn}

	// Item became invalid: Content is now empty, must clear the stored bytes.
	err := c.PatchObjectStatus(context.Background(), catalogItemGVR, "ns", "i", &v1alpha1.CatalogItemStatus{
		ContentHash: "newhash",
		Valid:       false,
		Message:     "invalid content",
	})
	if err != nil {
		t.Fatalf("PatchCatalogItemStatus: %v", err)
	}

	got, err := dyn.Resource(catalogItemGVR).Namespace("ns").Get(context.Background(), "i", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get catalogitem: %v", err)
	}
	if content, found, _ := unstructured.NestedString(got.Object, "status", "content"); found && content != "" {
		t.Errorf("stale content should be cleared, got %q", content)
	}
	if valid, _, _ := unstructured.NestedBool(got.Object, "status", "valid"); valid {
		t.Error("valid should be false after the patch")
	}
	if hash, _, _ := unstructured.NestedString(got.Object, "status", "contentHash"); hash != "newhash" {
		t.Errorf("contentHash should be updated, got %q", hash)
	}
}

// TestPatchCatalogItemStatus_ClearsStaleListFields covers the three list-valued
// status fields a derived item carries. Each is `omitempty`, so a merge patch
// that omits it leaves the previous entries in etcd forever: an item that stops
// warning keeps its verdicts, and a closure that shrinks keeps the dependency it
// dropped. One assertion per field — a partial clear is the failure mode that
// would otherwise ship silently.
func TestPatchCatalogItemStatus_ClearsStaleListFields(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "CatalogItem"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("CatalogItemList"), &unstructured.UnstructuredList{})

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "CatalogItem",
		"metadata":   map[string]interface{}{"name": "i", "namespace": "ns"},
		"status": map[string]interface{}{
			"content": "stale-bytes",
			"valid":   true,
			"closure": []interface{}{
				map[string]interface{}{"artifactId": "mailer", "version": "1.5", "provenance": "store"},
			},
			"compat": []interface{}{
				map[string]interface{}{"profile": "lts-2.555", "verdict": "core-too-old"},
			},
			"conditions": []interface{}{
				map[string]interface{}{"type": "CompatWarning", "status": "True", "reason": "core-too-old"},
			},
		},
	}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{catalogItemGVR: "CatalogItemList"},
		existing,
	)
	c := &ClientsetClient{dynamic: dyn}

	// The new sync produced no closure, no verdicts, and no conditions.
	err := c.PatchObjectStatus(context.Background(), catalogItemGVR, "ns", "i", &v1alpha1.CatalogItemStatus{
		ContentHash: "newhash",
		Valid:       true,
	})
	if err != nil {
		t.Fatalf("PatchObjectStatus: %v", err)
	}

	got, err := dyn.Resource(catalogItemGVR).Namespace("ns").Get(context.Background(), "i", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get catalogitem: %v", err)
	}
	for _, field := range []string{"closure", "compat", "conditions"} {
		if v, found, _ := unstructured.NestedSlice(got.Object, "status", field); found && len(v) > 0 {
			t.Errorf("stale status.%s should be cleared, got %v", field, v)
		}
	}
}

// hibernationRVEnforcer intercepts status-subresource updates against the
// dynamic fake and enforces optimistic concurrency, which the plain fake does
// not. When conflicts is positive, it injects that many 409s (before any
// resourceVersion comparison) so tests can exercise the wake retry path.
type hibernationRVEnforcer struct {
	dyn       *dynamicfake.FakeDynamicClient
	conflicts int
}

func (e *hibernationRVEnforcer) react(action clienttesting.Action) (bool, runtime.Object, error) {
	sub, ok := action.(interface{ GetSubresource() string })
	if !ok || sub.GetSubresource() != "status" {
		return false, nil, nil
	}
	ua, ok := action.(clienttesting.UpdateAction)
	if !ok {
		return false, nil, nil
	}
	incoming, ok := ua.GetObject().(*unstructured.Unstructured)
	if !ok {
		return false, nil, nil
	}
	if e.conflicts > 0 {
		e.conflicts--
		return true, nil, apierrors.NewConflict(controllerGVR.GroupResource(), incoming.GetName(), fmt.Errorf("injected conflict"))
	}
	stored, err := e.dyn.Tracker().Get(controllerGVR, incoming.GetNamespace(), incoming.GetName())
	if err != nil {
		return true, nil, err
	}
	storedU, ok := stored.(*unstructured.Unstructured)
	if !ok {
		return true, nil, fmt.Errorf("tracked controller is %T, not *unstructured.Unstructured", stored)
	}
	if incoming.GetResourceVersion() != storedU.GetResourceVersion() {
		return true, nil, apierrors.NewConflict(controllerGVR.GroupResource(), incoming.GetName(),
			fmt.Errorf("resourceVersion mismatch: submitted %q, current %q", incoming.GetResourceVersion(), storedU.GetResourceVersion()))
	}
	return false, nil, nil
}

// newHibernationTestClient builds a ClientsetClient backed by a dynamic fake
// seeded with the given controller, with resourceVersion enforcement on
// status-subresource updates.
func newHibernationTestClient(t *testing.T, existing *unstructured.Unstructured) (*ClientsetClient, *dynamicfake.FakeDynamicClient, *hibernationRVEnforcer) {
	t.Helper()
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "Controller"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControllerList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{controllerGVR: "ControllerList"},
		existing.DeepCopy(),
	)
	enforcer := &hibernationRVEnforcer{dyn: dyn}
	dyn.PrependReactor("update", "controllers", enforcer.react)
	return &ClientsetClient{dynamic: dyn}, dyn, enforcer
}

// hibernationController builds a minimal Controller unstructured for the
// SetHibernated tests.
func hibernationController(hibernated bool, phase string, withConditions bool) *unstructured.Unstructured {
	status := map[string]interface{}{}
	if phase != "" {
		status["phase"] = phase
	}
	if hibernated {
		status["hibernated"] = true
		status["hibernatedAt"] = "2026-08-15T12:00:00Z"
	}
	if withConditions {
		status["conditions"] = []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True", "reason": "Connected"},
		}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata": map[string]interface{}{
			"name":            "ctrl",
			"namespace":       "ns",
			"resourceVersion": "10",
		},
		"status": status,
	}}
}

func TestSetHibernated_NoOpWhenAlreadyAtTarget(t *testing.T) {
	c, dyn, _ := newHibernationTestClient(t, hibernationController(true, "Hibernated", false))

	changed, err := c.SetHibernated(context.Background(), "ctrl", "ns", true)
	if err != nil {
		t.Fatalf("SetHibernated: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when already hibernated")
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "update" {
			t.Fatalf("expected no update, got action %v", a)
		}
	}
}

func TestSetHibernated_StaleResourceVersionConflicts(t *testing.T) {
	c, dyn, _ := newHibernationTestClient(t, hibernationController(false, "Connected", false))

	// Serve a stale resourceVersion from the read, simulating a concurrent
	// write landing between the Get and the Update.
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
		u.SetResourceVersion("9")
		return true, u, nil
	})

	changed, err := c.SetHibernated(context.Background(), "ctrl", "ns", true)
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict, got changed=%v err=%v", changed, err)
	}
	if changed {
		t.Fatal("expected changed=false on conflict")
	}

	got, err := dyn.Resource(controllerGVR).Namespace("ns").Get(context.Background(), "ctrl", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v, _, _ := unstructured.NestedBool(got.Object, "status", "hibernated"); v {
		t.Fatal("status.hibernated must remain false after a rejected write")
	}
}

func TestSetHibernated_PreservesPhaseAndConditions(t *testing.T) {
	c, dyn, _ := newHibernationTestClient(t, hibernationController(false, "Connected", true))

	changed, err := c.SetHibernated(context.Background(), "ctrl", "ns", true)
	if err != nil {
		t.Fatalf("SetHibernated: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}

	got, err := dyn.Resource(controllerGVR).Namespace("ns").Get(context.Background(), "ctrl", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if phase, _, _ := unstructured.NestedString(got.Object, "status", "phase"); phase != "Connected" {
		t.Fatalf("status.phase was wiped: %q", phase)
	}
	conditions, found, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	if err != nil || !found || len(conditions) != 1 {
		t.Fatalf("status.conditions were wiped: found=%v len=%d err=%v", found, len(conditions), err)
	}
	if v, _, _ := unstructured.NestedBool(got.Object, "status", "hibernated"); !v {
		t.Fatal("status.hibernated was not set")
	}
	if at, found, _ := unstructured.NestedString(got.Object, "status", "hibernatedAt"); !found || at == "" {
		t.Fatalf("status.hibernatedAt was not set: found=%v value=%q", found, at)
	}
}

func TestSetHibernated_WakeRetriesOnConflict(t *testing.T) {
	c, dyn, enforcer := newHibernationTestClient(t, hibernationController(true, "Hibernated", false))

	// The first status update loses the race to an in-flight auto-hibernate;
	// the wake must retry rather than being dropped.
	enforcer.conflicts = 1

	changed, err := c.SetHibernated(context.Background(), "ctrl", "ns", false)
	if err != nil {
		t.Fatalf("SetHibernated: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true after retry")
	}

	got, err := dyn.Resource(controllerGVR).Namespace("ns").Get(context.Background(), "ctrl", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v, _, _ := unstructured.NestedBool(got.Object, "status", "hibernated"); v {
		t.Fatal("status.hibernated should be cleared by the wake")
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "hibernatedAt"); found {
		t.Fatal("status.hibernatedAt should be cleared by the wake")
	}
}

func TestSetHibernated_HibernateRetriesOnConflict(t *testing.T) {
	c, dyn, enforcer := newHibernationTestClient(t, hibernationController(false, "Connected", false))

	// The first status update loses the race to a concurrent write (e.g. the
	// end-of-reconcile status merge patch). The set direction is reached by
	// HibernateController, a retryless request-reply action, so the conflict
	// must be retried here rather than surfaced to the caller as a 500.
	enforcer.conflicts = 1

	changed, err := c.SetHibernated(context.Background(), "ctrl", "ns", true)
	if err != nil {
		t.Fatalf("SetHibernated: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true after retry")
	}

	got, err := dyn.Resource(controllerGVR).Namespace("ns").Get(context.Background(), "ctrl", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v, _, _ := unstructured.NestedBool(got.Object, "status", "hibernated"); !v {
		t.Fatal("status.hibernated should be set by the hibernate")
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "hibernatedAt"); !found {
		t.Fatal("status.hibernatedAt should be set by the hibernate")
	}
}

// TestClientsetClient_ApplyControllerSpecSSA_MetadataNeverInManifest (5.9):
// the SSA manifest carries only metadata.name/namespace, never labels or
// annotations, even when the live object has them. If the manifest carried the
// labels, varroa-ui would own them and a later apply that omitted them would
// DELETE them — so this pins that create-time labels survive a later spec edit.
func TestClientsetClient_ApplyControllerSpecSSA_MetadataNeverInManifest(t *testing.T) {
	current := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata": map[string]any{
			"name":      "ctrl1",
			"namespace": "ns1",
			"labels":    map[string]any{"team": "platform"},
			"managedFields": []any{
				managedFieldEntry("varroa-ui", "Apply", "varroa.dev/v1alpha1", map[string]any{
					"f:version": map[string]any{},
				}),
			},
		},
		"spec": map[string]any{"version": "2.479"},
	}}
	h := newSSAHarness(t, current)

	_, _, err := h.client.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1",
		map[string]any{"version": "2.5"}, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	body := h.captured[0]
	meta, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("manifest has no metadata: %#v", body)
	}
	if len(meta) != 2 || meta["name"] != "ctrl1" || meta["namespace"] != "ns1" {
		t.Fatalf("manifest metadata = %#v, want exactly {name, namespace} — labels must never ride the SSA manifest", meta)
	}
}

// TestClientsetClient_UserAgentIsExplicit (5.8): both constructors set the
// rest.Config.UserAgent from a named constant, so the operator's field-manager
// identity does not change if the binary is renamed.
func TestClientsetClient_UserAgentIsExplicit(t *testing.T) {
	kubeconfig := writeTestKubeconfig(t)
	c, err := NewClientsetClientWithKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("NewClientsetClientWithKubeconfig: %v", err)
	}
	if got := c.RESTConfig().UserAgent; got != operatorUserAgent {
		t.Fatalf("RESTConfig().UserAgent = %q, want %q", got, operatorUserAgent)
	}
}

// writeTestKubeconfig writes a minimal kubeconfig to a temp file and returns
// its path. Enough for clientcmd.BuildConfigFromFlags to produce a rest.Config;
// no connection is ever made.
func writeTestKubeconfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/kubeconfig"
	content := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: test-token
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// TestPatchUpdateCenterStatus_ClearsStaleSeedDigests guards the regression where
// digests of refs removed from spec.seed.refs survived in
// status.seedImportedDigests. The field carries `omitempty`, so an empty list is
// dropped from a JSON merge patch and the stale prior value survives in etcd —
// after which a ref re-added at the same content digest is skipped forever as
// "already imported", even if the store was wiped in between. pluginCount is
// supplied here to check that clearing seedImportedDigests does not disturb
// another field carried by the same patch — not as evidence that pluginCount
// retains by design; it carries the same latent defect for a genuinely zero
// value, along with gaps and resolvedMetadataSources.
func TestPatchUpdateCenterStatus_ClearsStaleSeedDigests(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "UpdateCenter"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("UpdateCenterList"), &unstructured.UnstructuredList{})

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "UpdateCenter",
		"metadata":   map[string]interface{}{"name": "varroa-update-center"},
		"status": map[string]interface{}{
			"phase":               "Ready",
			"pluginCount":         int64(72),
			"seedImportedDigests": []interface{}{"sha256:deadbeef"},
		},
	}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{updateCentersGVR: "UpdateCenterList"},
		existing,
	)
	c := &ClientsetClient{dynamic: dyn}

	// Every seed ref removed: the reconciler patches a full status carrying no digests.
	err := c.PatchObjectStatus(context.Background(), updateCentersGVR, "", "varroa-update-center",
		&v1alpha1.UpdateCenterStatus{Phase: "Ready", PluginCount: 72})
	if err != nil {
		t.Fatalf("patch failed: %v", err)
	}

	got, err := dyn.Resource(updateCentersGVR).Get(context.Background(), "varroa-update-center", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	status, _, _ := unstructured.NestedMap(got.Object, "status")

	if d, found := status["seedImportedDigests"]; found && d != nil {
		if s, ok := d.([]interface{}); !ok || len(s) > 0 {
			t.Errorf("expected seedImportedDigests cleared, got: %v", d)
		}
	}
	if pc, _, _ := unstructured.NestedInt64(status, "pluginCount"); pc != 72 {
		t.Errorf("expected pluginCount=72 preserved, got: %d", pc)
	}
}
