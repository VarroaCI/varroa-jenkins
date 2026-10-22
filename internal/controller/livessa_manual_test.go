//go:build livessa

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Live SSA verification against a real apiserver (tasks 10.3-10.6).
// Run: go test -tags livessa -count=1 -v ./internal/controller/ -run TestLiveSSA
func TestLiveSSA(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}
	c, err := NewClientsetClientWithKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	const ns, fm = "default", "varroa-ui"
	// Unique per run: the harness deletes this object unconditionally, so a
	// fixed name could clobber a real controller that happened to share it.
	name := fmt.Sprintf("ssa-live-probe-%d", time.Now().UnixNano())

	get := func() map[string]any {
		u, err := c.dynamic.Resource(controllerGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		spec, _, _ := unstructured.NestedMap(u.Object, "spec")
		return spec
	}
	defer func() {
		_ = c.dynamic.Resource(controllerGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
		t.Log("10.6 cleanup: deleted test controller")
	}()
	_ = c.dynamic.Resource(controllerGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})

	// --- 10.3a: first save creates + owns className ---
	if _, _, err := c.ApplyControllerSpecSSA(ctx, ns, name,
		map[string]any{"className": "live-probe", "version": "2.479.3"}, fm, false); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// --- 10.3b: second save of a DIFFERENT field must not delete the first ---
	if _, _, err := c.ApplyControllerSpecSSA(ctx, ns, name,
		map[string]any{"version": "2.504"}, fm, false); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	spec := get()
	if spec["className"] != "live-probe" {
		t.Fatalf("10.3 FAIL: className lost after sparse save: %#v", spec)
	}
	if spec["version"] != "2.504" {
		t.Fatalf("10.3 FAIL: version not updated: %#v", spec)
	}
	t.Logf("10.3 PASS: className survived a sparse save of version; spec=%v", spec)

	// --- 10.4: a foreign manager owns a field; our save must not claim or disturb it ---
	foreign := []byte(`{"apiVersion":"varroa.dev/v1alpha1","kind":"Controller",` +
		`"metadata":{"name":"` + name + `","namespace":"` + ns + `"},` +
		`"spec":{"composedBundleRef":{"name":"foreign-bundle"}}}`)
	if _, err := c.dynamic.Resource(controllerGVR).Namespace(ns).Patch(ctx, name,
		types.ApplyPatchType, foreign, metav1.PatchOptions{FieldManager: "argocd-test"}); err != nil {
		t.Fatalf("foreign apply: %v", err)
	}
	if _, _, err := c.ApplyControllerSpecSSA(ctx, ns, name,
		map[string]any{"className": "live-probe-2"}, fm, false); err != nil {
		t.Fatalf("10.4 FAIL: unrelated save conflicted with foreign owner: %v", err)
	}
	spec = get()
	cbr, _ := spec["composedBundleRef"].(map[string]any)
	if cbr == nil || cbr["name"] != "foreign-bundle" {
		t.Fatalf("10.4 FAIL: foreign-owned field disturbed: %#v", spec)
	}
	u, _ := c.dynamic.Resource(controllerGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	mf, _ := json.Marshal(u.GetManagedFields())
	t.Logf("10.4 PASS: foreign field intact, no conflict raised")

	// --- 10.5a: clearing a field only varroa-ui owns actually removes it ---
	_, unapplied, err := c.ApplyControllerSpecSSA(ctx, ns, name,
		map[string]any{"className": nil}, fm, false)
	if err != nil {
		t.Fatalf("10.5a apply: %v", err)
	}
	spec = get()
	if _, present := spec["className"]; present {
		t.Fatalf("10.5a FAIL: className still present after null removal: %#v", spec)
	}
	if len(unapplied) != 0 {
		t.Fatalf("10.5a FAIL: sole-owned removal wrongly reported unapplied: %v", unapplied)
	}
	t.Logf("10.5a PASS: sole-owned field removed, nothing reported unapplied")

	// --- 10.5b: clearing a foreign-owned field is retained AND reported ---
	if _, _, err := c.ApplyControllerSpecSSA(ctx, ns, name,
		map[string]any{"composedBundleRef": map[string]any{"name": "foreign-bundle"}}, fm, false); err != nil {
		t.Fatalf("10.5b co-own: %v", err)
	}
	_, unapplied, err = c.ApplyControllerSpecSSA(ctx, ns, name,
		map[string]any{"composedBundleRef": nil}, fm, false)
	if err != nil {
		t.Fatalf("10.5b apply: %v", err)
	}
	spec = get()
	if _, present := spec["composedBundleRef"]; !present {
		t.Fatalf("10.5b FAIL: foreign-owned field was removed: %#v", spec)
	}
	if len(unapplied) == 0 {
		t.Fatalf("10.5b FAIL: blocked removal NOT reported. managedFields=%s", mf)
	}
	t.Logf("10.5b PASS: blocked removal retained and reported: %v", unapplied)
}

// TestLiveSSACreateClaim checks CreateControllerSpecSSA against a real
// apiserver. The create fences the name, the claim drops the varroa-ui Update
// entry that carries f:spec, a later apply with force off does not conflict,
// and a repeat create answers AlreadyExists without changing the spec.
// Run: go test -tags livessa -count=1 -v ./internal/controller/ -run TestLiveSSACreateClaim
func TestLiveSSACreateClaim(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}
	c, err := NewClientsetClientWithKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	const ns, fm = "default", "varroa-ui"
	name := fmt.Sprintf("ssa-create-probe-%d", time.Now().UnixNano())
	defer func() {
		_ = c.dynamic.Resource(controllerGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
		t.Log("cleanup: deleted test controller")
	}()

	created, err := c.CreateControllerSpecSSA(ctx, ns, name, map[string]any{"version": "2.479.3"}, fm)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("create PASS: %s/%s spec.version=%s", ns, name, created.Spec.Version)

	u, err := c.dynamic.Resource(controllerGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for _, e := range u.GetManagedFields() {
		if e.Manager == fm && e.Operation == metav1.ManagedFieldsOperationUpdate && e.Subresource == "" &&
			e.FieldsV1 != nil && strings.Contains(string(e.FieldsV1.Raw), "f:spec") {
			t.Fatalf("claim FAIL: varroa-ui Update entry with f:spec remains: %s", e.FieldsV1.Raw)
		}
	}
	t.Log("claim PASS: no varroa-ui Update entry with f:spec remains")

	if _, _, err := c.ApplyControllerSpecSSA(ctx, ns, name, map[string]any{"version": "2.504"}, fm, false); err != nil {
		t.Fatalf("apply with a changed value after the claim: %v", err)
	}
	t.Log("apply PASS: changed value applied with force=false and no conflict")

	if _, err := c.CreateControllerSpecSSA(ctx, ns, name, map[string]any{"version": "2.479.3"}, fm); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("repeat create: err = %v, want AlreadyExists", err)
	}
	u, err = c.dynamic.Resource(controllerGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after repeat create: %v", err)
	}
	version, _, _ := unstructured.NestedString(u.Object, "spec", "version")
	if version != "2.504" {
		t.Fatalf("repeat create changed spec.version to %q, want 2.504", version)
	}
	t.Logf("repeat create PASS: AlreadyExists, spec.version still %s", version)
}

// TestLiveSSARepairUpdateEntry checks the spec-claim repair against a real
// apiserver. A raw create as varroa-ui leaves an Update entry that owns
// f:spec, the state a failed claim leaves behind. The next varroa-ui spec
// write merges that entry into the Apply entry, so a changed value applies
// with force off and no conflict. A merge patch as varroa-ui then rebuilds
// the shared ownership, and the IfExists write also applies with no conflict.
// Run: go test -tags livessa -count=1 -v ./internal/controller/ -run TestLiveSSARepairUpdateEntry
func TestLiveSSARepairUpdateEntry(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}
	c, err := NewClientsetClientWithKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	const ns, fm = "default", "varroa-ui"
	name := fmt.Sprintf("ssa-repair-probe-%d", time.Now().UnixNano())
	defer func() {
		_ = c.dynamic.Resource(controllerGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
		t.Log("cleanup: deleted test controller")
	}()

	raw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{"version": "2.479.3"},
	}}
	if _, err := c.dynamic.Resource(controllerGVR).Namespace(ns).Create(ctx, raw,
		metav1.CreateOptions{FieldManager: fm}); err != nil {
		t.Fatalf("raw create: %v", err)
	}
	if !hasSpecUpdateEntry(t, c, ns, name, fm) {
		t.Fatal("setup FAIL: raw create left no varroa-ui Update entry with f:spec")
	}

	if _, _, err := c.ApplyControllerSpecSSA(ctx, ns, name, map[string]any{"version": "2.504"}, fm, false); err != nil {
		t.Fatalf("repair FAIL: apply with a changed value answered %v", err)
	}
	if hasSpecUpdateEntry(t, c, ns, name, fm) {
		t.Fatal("repair FAIL: varroa-ui Update entry with f:spec remains after the apply")
	}
	t.Log("repair PASS: Update entry merged, changed value applied with force=false")

	// A merge patch as varroa-ui records a new Update entry on the main resource.
	if _, err := c.dynamic.Resource(controllerGVR).Namespace(ns).Patch(ctx, name,
		types.MergePatchType, []byte(`{"spec":{"version":"2.505"}}`),
		metav1.PatchOptions{FieldManager: fm}); err != nil {
		t.Fatalf("merge patch as %s: %v", fm, err)
	}
	if _, _, err := c.ApplyControllerSpecSSAIfExists(ctx, ns, name, map[string]any{"version": "2.506"}, fm, false); err != nil {
		t.Fatalf("IfExists apply with a changed value: %v", err)
	}
	t.Log("IfExists PASS: changed value applied with force=false and no conflict")
}

// hasSpecUpdateEntry reports whether the controller carries a fm Update entry
// on the main resource that owns f:spec.
func hasSpecUpdateEntry(t *testing.T, c *ClientsetClient, ns, name, fm string) bool {
	t.Helper()
	u, err := c.dynamic.Resource(controllerGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for _, e := range u.GetManagedFields() {
		if e.Manager == fm && e.Operation == metav1.ManagedFieldsOperationUpdate && e.Subresource == "" &&
			e.FieldsV1 != nil && strings.Contains(string(e.FieldsV1.Raw), "f:spec") {
			return true
		}
	}
	return false
}
