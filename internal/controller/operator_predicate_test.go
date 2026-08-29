package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func TestPredicate_GenerationBump(t *testing.T) {
	// annotationBumped should return false when new value is empty.
	oldC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{}},
	}
	newC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 2, Annotations: map[string]string{}},
	}
	if annotationBumped(oldC, newC, annotationWakeRequested) {
		t.Error("annotationBumped should be false when new value is empty")
	}
}

func TestPredicate_AnnotationSet(t *testing.T) {
	oldC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{}},
	}
	newC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{
			"varroa.dev/wake-requested": "2026-07-05T12:00:00Z",
		}},
	}
	if !annotationBumped(oldC, newC, annotationWakeRequested) {
		t.Error("annotationBumped should be true when annotation is set from empty to non-empty")
	}
}

func TestPredicate_AnnotationChanged(t *testing.T) {
	oldC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{
			"varroa.dev/wake-requested": "2026-07-05T12:00:00Z",
		}},
	}
	newC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{
			"varroa.dev/wake-requested": "2026-07-05T12:01:00Z",
		}},
	}
	if !annotationBumped(oldC, newC, annotationWakeRequested) {
		t.Error("annotationBumped should be true when annotation value changes")
	}
}

func TestPredicate_AnnotationRemoved(t *testing.T) {
	oldC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{
			"varroa.dev/wake-requested": "2026-07-05T12:00:00Z",
		}},
	}
	newC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{}},
	}
	if annotationBumped(oldC, newC, annotationWakeRequested) {
		t.Error("annotationBumped should be false when annotation is removed (new value empty)")
	}
}

func TestPredicate_StatusOnly(t *testing.T) {
	oldC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{}},
	}
	newC := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Generation: 1, Annotations: map[string]string{}},
	}
	if annotationBumped(oldC, newC, annotationWakeRequested) {
		t.Error("annotationBumped should be false for status-only update (same annotations)")
	}
	if annotationBumped(oldC, newC, annotationForceReprovision) {
		t.Error("annotationBumped should be false for status-only update")
	}
}

// TestRouting_WakeController_NonOwned verifies that WakeController on a
// non-owned key patches the annotation and does NOT enqueue locally.
func TestRouting_WakeController_NonOwned(t *testing.T) {
	ring := sharding.NewRing(8)
	held := sharding.NewShardSet()
	held.Add(0) // hold only shard 0

	client := newTestClient()
	rec := newTestReconciler(client)
	rec.SetShardOwnership(ring, held)

	// Use a key that does NOT hash to shard 0.
	var key string
	for _, ns := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		for _, name := range []string{"x", "y", "z", "w"} {
			k := ns + "/" + name
			if !held.Held(ring.ShardFor(k)) {
				key = k
				break
			}
		}
		if key != "" {
			break
		}
	}
	if key == "" {
		t.Fatal("could not find a key that does not hash to shard 0")
	}
	ns, name, _ := strings.Cut(key, "/")

	rec.WakeController("core", ns, name)

	// Should have patched the annotation via the store.
	patches := client.store.MetaPatches(controllerGVR)
	if len(patches) != 1 {
		t.Fatalf("expected 1 annotation patch, got %d", len(patches))
	}
	patch := patches[0]
	if patch.Name != name || patch.Namespace != ns {
		t.Errorf("unexpected patch target: %s/%s", patch.Namespace, patch.Name)
	}
	anns, ok := patch.Meta["annotations"].(map[string]any)
	if !ok {
		t.Fatal("expected annotations in meta patch")
	}
	if _, ok := anns[annotationWakeRequested]; !ok {
		t.Error("patch should include annotationWakeRequested")
	}

	// Should NOT enqueue locally.
	select {
	case ev := <-rec.reconcileEvents:
		t.Fatalf("unexpected local enqueue for %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
	default:
	}
}

// TestRouting_WakeController_Owned verifies that WakeController on an owned
// key enqueues locally and does NOT patch any annotation.
func TestRouting_WakeController_Owned(t *testing.T) {
	ring := sharding.NewRing(8)
	held := sharding.NewShardSet()
	held.Add(0)

	client := newTestClient()
	rec := newTestReconciler(client)
	rec.SetShardOwnership(ring, held)

	// Find a key that hashes to shard 0.
	key := ""
	for _, ns := range []string{"a", "b", "c", "d", "e", "f"} {
		for _, name := range []string{"x", "y", "z"} {
			k := ns + "/" + name
			if held.Held(ring.ShardFor(k)) {
				key = k
				break
			}
		}
		if key != "" {
			break
		}
	}
	if key == "" {
		t.Fatal("could not find a key that hashes to shard 0")
	}

	ns, name, _ := strings.Cut(key, "/")
	rec.WakeController("core", ns, name)

	// Should NOT patch annotation (store should have no meta patches for owned key).
	if len(client.store.MetaPatches(controllerGVR)) > 0 {
		t.Fatalf("expected no annotation patches for owned key, got %d", len(client.store.MetaPatches(controllerGVR)))
	}

	// Should enqueue locally.
	select {
	case ev := <-rec.reconcileEvents:
		if ev.Object.GetNamespace() != ns || ev.Object.GetName() != name {
			t.Errorf("unexpected event: %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
		}
	default:
		t.Fatal("expected local enqueue for owned key")
	}
}

// TestRouting_Reprovision verifies that Reprovision stamps the force-reprovision
// annotation.
func TestRouting_Reprovision(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	rec.Reprovision("core", "ns", "ctrl")

	patches := client.store.MetaPatches(controllerGVR)
	if len(patches) != 1 {
		t.Fatalf("expected 1 annotation patch, got %d", len(patches))
	}
	patch := patches[0]
	if patch.Name != "ctrl" || patch.Namespace != "ns" {
		t.Errorf("unexpected patch target: %s/%s", patch.Namespace, patch.Name)
	}
	anns, ok := patch.Meta["annotations"].(map[string]any)
	if !ok {
		t.Fatal("expected annotations in meta patch")
	}
	if _, ok := anns[annotationForceReprovision]; !ok {
		t.Error("patch should include annotationForceReprovision")
	}
}

// TestRouting_TriggerReconcile delegates to wakeController.
func TestRouting_TriggerReconcile(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	// With nil gate, TriggerReconcile should enqueue locally (owns all).
	rec.TriggerReconcile("core", "ctrl", "ns")

	select {
	case ev := <-rec.reconcileEvents:
		if ev.Object.GetNamespace() != "ns" || ev.Object.GetName() != "ctrl" {
			t.Errorf("unexpected event: %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
		}
	default:
		t.Fatal("expected local enqueue")
	}
}

// TestReconcile_ConsumesForceReprovision verifies the annotation is consumed
// one-shot at the top of Reconcile: a single pass clears it via PATCH
// regardless of which phase path runs afterwards.
func TestReconcile_ConsumesForceReprovision(t *testing.T) {
	client := newTestClientWithBundle()
	cr := testController("test-ctrl", "test-ns", v1alpha1.ControllerPhaseConnected)
	cr.Annotations = map[string]string{annotationForceReprovision: "2026-07-06T00:00:00Z"}
	client.controllers = []*v1alpha1.Controller{cr}
	crdstore.MustSeed(client.store, cr)
	rec := newTestReconciler(client)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "test-ns", Name: "test-ctrl"}}
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatal(err)
	}
	var cleared bool
	for _, p := range client.store.MetaPatches(ctrlGVR) {
		ann, _ := p.Meta["annotations"].(map[string]any)
		if v, ok := ann[annotationForceReprovision]; ok && v == nil {
			cleared = true
		}
	}
	if !cleared {
		t.Error("expected Reconcile to clear annotationForceReprovision via PATCH")
	}
	// The in-memory copy must survive the consume so this pass still honors it.
	if cr.Annotations[annotationForceReprovision] == "" {
		t.Error("in-memory annotation should be preserved for the current pass")
	}
}

// TestReconcile_ForceReprovisionClearFailureRequeues verifies clear-before-act:
// when the consume PATCH fails, Reconcile returns the error (rate-limited
// retry) instead of acting on the annotation, guarding against a reload loop.
func TestReconcile_ForceReprovisionClearFailureRequeues(t *testing.T) {
	client := newTestClientWithBundle()
	cr := testController("test-ctrl", "test-ns", v1alpha1.ControllerPhaseConnected)
	cr.Annotations = map[string]string{annotationForceReprovision: "2026-07-06T00:00:00Z"}
	client.controllers = []*v1alpha1.Controller{cr}
	crdstore.MustSeed(client.store, cr)
	gvrC, gvrErr := crdstore.GVRFor[v1alpha1.Controller]()
	if gvrErr != nil {
		t.Fatal(gvrErr)
	}
	client.store.FailAlways("patchmeta", gvrC, fmt.Errorf("apiserver contention"))
	rec := newTestReconciler(client)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "test-ns", Name: "test-ctrl"}}
	if _, err := rec.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected error when the consume PATCH fails")
	}
	if len(client.statuses) != 0 {
		t.Errorf("reconcile must not act (no status patch) when the consume fails, got %v", client.statuses)
	}
}

// TestReconcile_ForceReprovisionStaysArmedInNonHonoringPhase asserts that a
// phase that cannot honor the request (Pending/Failed/Stopped) must NOT
// consume the annotation — it stays armed until Provisioning or Connected
// fires it.
func TestReconcile_ForceReprovisionStaysArmedInNonHonoringPhase(t *testing.T) {
	client := newTestClientWithBundle()
	cr := testController("test-ctrl", "test-ns", v1alpha1.ControllerPhasePending)
	cr.Annotations = map[string]string{annotationForceReprovision: "2026-07-06T00:00:00Z"}
	client.controllers = []*v1alpha1.Controller{cr}
	crdstore.MustSeed(client.store, cr)
	rec := newTestReconciler(client)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "test-ns", Name: "test-ctrl"}}
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gvrP, gvrPErr := crdstore.GVRFor[v1alpha1.Controller]()
	if gvrPErr != nil {
		t.Fatal(gvrPErr)
	}
	for _, p := range client.store.MetaPatches(gvrP) {
		ann, _ := p.Meta["annotations"].(map[string]any)
		if v, ok := ann[annotationForceReprovision]; ok && v == nil {
			t.Fatal("Pending-phase reconcile must not consume the force-reprovision annotation")
		}
	}
}
