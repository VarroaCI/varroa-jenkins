package controller

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestNextRequeueDuration_IdlePhasesUseInterval proves a Hibernated or
// Stopped controller requeues at the regular interval, not the one-second
// active-phase backoff, so an idle fleet does not reconcile in a tight loop.
func TestNextRequeueDuration_IdlePhasesUseInterval(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	for _, phase := range []v1alpha1.ControllerPhase{v1alpha1.ControllerPhaseHibernated, v1alpha1.ControllerPhaseStopped} {
		cr := testController("idle", "ns1", phase)
		if got := rec.nextRequeueDuration(cr); got < defaultReconcileInterval {
			t.Fatalf("phase %s: requeue %s, want at least %s", phase, got, defaultReconcileInterval)
		}
	}
	active := testController("busy", "ns1", v1alpha1.ControllerPhaseProvisioning)
	if got := rec.nextRequeueDuration(active); got >= 2*time.Second {
		t.Fatalf("Provisioning: requeue %s, want the short active backoff", got)
	}
}

// TestNextRequeueDuration_StreamLostBacksOff proves a Running controller that
// lost its mite stream requeues at streamLostRequeue, not the one-second
// backoff. The pod is unchanged, so a tight loop only repeats the same work.
func TestNextRequeueDuration_StreamLostBacksOff(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	cr := testController("lost", "ns1", v1alpha1.ControllerPhaseRunning)
	cr.Status.Conditions = []v1alpha1.ControllerCondition{streamLostCond(time.Now())}
	if got := rec.nextRequeueDuration(cr); got < streamLostRequeue {
		t.Fatalf("requeue %s, want at least %s", got, streamLostRequeue)
	}
}

// TestReconcile_TickOwnWritesKeepTheEndWrite pins that the tick's own
// writes before its end write move its resourceVersion chain forward. A
// first tick adds the finalizer and the controller label, and its end status
// write still lands in the same pass.
func TestReconcile_TickOwnWritesKeepTheEndWrite(t *testing.T) {
	client := newTestClientWithBundle()
	client.statusToStore = true
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhasePending)
	cr.ResourceVersion = "1"
	cr.Finalizers = nil
	cr.Labels = nil
	crdstore.MustSeed(client.store, cr)

	res, err := rec.Reconcile(context.Background(), mineRequest)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == tickConflictRequeue {
		t.Fatal("the tick conflicted on its own writes")
	}
	if client.patchCount == 0 {
		t.Fatal("the tick wrote no status")
	}
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "mine", "ns")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Finalizers) == 0 {
		t.Fatal("the tick added no finalizer")
	}
	if got.Status.Phase == v1alpha1.ControllerPhasePending || got.Status.Phase == "" {
		t.Fatalf("phase = %q, want the phase the end write sent", got.Status.Phase)
	}
	if got.Labels[v1alpha1.LabelControllerName] != "mine" {
		t.Fatalf("controller label = %q, want mine", got.Labels[v1alpha1.LabelControllerName])
	}
}

// TestWriteTickStatus_ConflictCancelsTheTick pins that a conflict of any
// tick write cancels the tick context, so a later write in the same tick,
// such as SetHibernated with its fresh read, cannot run. A write outside a
// tick only returns the conflict.
func TestWriteTickStatus_ConflictCancelsTheTick(t *testing.T) {
	client := newTestClient()
	client.statusToStore = true
	rec := newTestReconciler(client)
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	crdstore.MustSeed(client.store, cr)
	gvr, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("gvr: %v", err)
	}
	if err := client.store.PatchObjectStatus(context.Background(), gvr, "ns", "mine", map[string]any{"configHash": "newer"}); err != nil {
		t.Fatalf("newer write: %v", err)
	}

	stale := cr.DeepCopy()
	if err := rec.writeTickStatus(context.Background(), stale); err == nil {
		t.Fatal("a stale write outside a tick landed")
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx = context.WithValue(ctx, tickFenceKey{}, cancel)
	stale = cr.DeepCopy()
	stale.Status.WakeToken = "token"
	if err := rec.writeTickStatus(ctx, stale); err == nil {
		t.Fatal("a stale tick write landed")
	}
	if ctx.Err() == nil {
		t.Fatal("the conflict did not cancel the tick context")
	}
	res, fenced := rec.tickFenced(ctx, stale)
	if !fenced || res.RequeueAfter != tickConflictRequeue {
		t.Fatalf("tickFenced = %v, %v; want true and %v", res, fenced, tickConflictRequeue)
	}
}
