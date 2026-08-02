package controller

import (
	"context"
	"encoding/json"
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

// TestClientsetClient_ApplyControllerSpecSSA_NoNamespaceInPatch verifies F1:
// when ApplyControllerSpecSSA receives a sparse specPatch containing only a
// single field (e.g. "resources"), the serialized body must contain only that
// field — spec must NOT contain "namespace" (nor any other unset ControllerSpec
// field). This guards against typed round-trip reintroducing zero-valued
// non-omitempty fields.
//
// Uses a PrependReactor to capture the actual ApplyPatchType payload that
// ApplyControllerSpecSSA sends over the wire.
func TestClientsetClient_ApplyControllerSpecSSA_NoNamespaceInPatch(t *testing.T) {
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "varroa.dev", Version: "v1alpha1", Kind: "Controller"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControllerList"), &unstructured.UnstructuredList{})

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{controllerGVR: "ControllerList"},
	)
	c := &ClientsetClient{dynamic: dyn}

	// Capture the apply patch payload via a reactor.
	var capturedPatch []byte
	dyn.PrependReactor("patch", "controllers", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchActionImpl)
		if pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		capturedPatch = pa.GetPatch()
		// Return a valid Controller unstructured so FromUnstructured conversion runs.
		var body map[string]any
		if err := json.Unmarshal(capturedPatch, &body); err != nil {
			return true, nil, err
		}
		obj := &unstructured.Unstructured{Object: body}
		return true, obj, nil
	})

	// Sparse spec: only "resources", not "namespace".
	specPatch := map[string]any{
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m"},
		},
	}

	_, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl1", specPatch, "varroa-ui", false)
	if err != nil {
		t.Fatalf("ApplyControllerSpecSSA failed: %v", err)
	}

	if capturedPatch == nil {
		t.Fatal("reactor was not called — no patch captured")
	}

	// Unmarshal captured patch bytes and assert the spec content.
	var body map[string]any
	if err := json.Unmarshal(capturedPatch, &body); err != nil {
		t.Fatalf("unmarshal captured patch: %v", err)
	}

	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatal("captured patch has no spec key")
	}

	// spec must contain "resources" (the one user-set field).
	if _, hasResources := spec["resources"]; !hasResources {
		t.Fatal("spec has no 'resources' key — user-set field missing")
	}

	// spec must NOT contain "namespace" (the F1 bug: non-omitempty zero-value
	// from typed ControllerSpec round-trip would claim ownership).
	if _, hasNS := spec["namespace"]; hasNS {
		t.Fatal("spec contains 'namespace' key — SSA would claim ownership of namespace field")
	}

	// Verify no other unintended keys are present.
	for k := range spec {
		if k != "resources" {
			t.Errorf("unexpected key %q in spec patch — only user-set fields should appear", k)
		}
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
	_, err := c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl2", specPatch, "varroa-ui", false)
	if err != nil {
		t.Fatalf("first apply failed: %v", err)
	}

	// Second apply (same spec).
	_, err = c.ApplyControllerSpecSSA(context.Background(), "ns1", "ctrl2", specPatch, "varroa-ui", false)
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

// TestApplyUserCRD_StripsResourceVersionOnCreate guards against bug #390: on
// OIDC login the BFF failed to record the signed-in user because ApplyUserCRD
// passed a ResourceVersion-bearing object to Create(), which Kubernetes rejects
// with a "resourceVersion should not be set on objects to be created" Invalid
// error.  The fix clears Create-forbidden metadata fields before Create.
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

// TestPatchControllerStatus_ClearsLastReconcileErrorAt guards the #391/#400
// regression shape: a merge patch must explicitly null a pointer field to
// clear it — merely omitting the key leaves the prior value in etcd. This
// test sets LastReconcileErrorAt on the server-side object, patches with nil,
// and asserts the field is absent after the patch.
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
