//go:build livessa

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

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
