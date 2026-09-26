package controller

import (
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
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
