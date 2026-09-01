package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestEvaluateDispatchedTarget_Upgrade covers the two-stage release predicate:
// the release annotation must be gone AND the phase must have left
// {Connected, Running} in the SAME evaluation before the target is marked
// released; only then does a return to Connected mark it Succeeded.
func TestEvaluateDispatchedTarget_Upgrade(t *testing.T) {
	now := frozenNow
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-op", Namespace: "ns1", UID: types.UID("op-uid")},
	}
	timeout := broodVerbTimeouts[v1alpha1.BroodVerbUpgrade]

	t.Run("stays Dispatched while the release annotation is present", func(t *testing.T) {
		rec, _, _ := newBORec(t)
		ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseProvisioning)
		ctrl.Annotations = map[string]string{annotationUpgradeRelease: "jenkins/jenkins:2.570.2"}
		target := &v1alpha1.BroodTargetStatus{
			Namespace: "ns1", Name: "ctrl-a",
			State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl, v1alpha1.BroodVerbUpgrade, timeout, now)
		if target.State != v1alpha1.BroodTargetStateDispatched {
			t.Errorf("state = %s, want Dispatched (annotation still held)", target.State)
		}
	})

	t.Run("stays Dispatched while annotation is gone but phase has not left Connected/Running", func(t *testing.T) {
		rec, _, _ := newBORec(t)
		ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
		target := &v1alpha1.BroodTargetStatus{
			Namespace: "ns1", Name: "ctrl-a",
			State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl, v1alpha1.BroodVerbUpgrade, timeout, now)
		if target.State != v1alpha1.BroodTargetStateDispatched {
			t.Errorf("state = %s, want Dispatched (phase never left Connected)", target.State)
		}
	})

	t.Run("marked released, but not yet Succeeded, once both hold and phase is not yet Connected", func(t *testing.T) {
		rec, _, _ := newBORec(t)
		ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseProvisioning)
		target := &v1alpha1.BroodTargetStatus{
			Namespace: "ns1", Name: "ctrl-a",
			State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl, v1alpha1.BroodVerbUpgrade, timeout, now)
		if target.State != v1alpha1.BroodTargetStateDispatched {
			t.Errorf("state = %s, want Dispatched (released, but phase not yet back to Connected)", target.State)
		}
		if target.Reason != upgradeReleasedMarker {
			t.Errorf("reason = %q, want the internal released marker", target.Reason)
		}
	})

	t.Run("Succeeded once released and phase returns to Connected", func(t *testing.T) {
		rec, _, _ := newBORec(t)
		ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
		target := &v1alpha1.BroodTargetStatus{
			Namespace: "ns1", Name: "ctrl-a",
			State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
			Reason: upgradeReleasedMarker,
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl, v1alpha1.BroodVerbUpgrade, timeout, now)
		if target.State != v1alpha1.BroodTargetStateSucceeded {
			t.Errorf("state = %s, want Succeeded", target.State)
		}
		if target.Reason != "" {
			t.Errorf("reason = %q, want cleared on success", target.Reason)
		}
		if target.FinishedAt == nil {
			t.Error("a succeeded target must stamp FinishedAt")
		}
	})

	t.Run("released marker plus annotation reappearing does not undo release", func(t *testing.T) {
		// Once released is latched via the Reason marker, the annotation's
		// current state is irrelevant to the two-stage predicate.
		rec, _, _ := newBORec(t)
		ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseProvisioning)
		target := &v1alpha1.BroodTargetStatus{
			Namespace: "ns1", Name: "ctrl-a",
			State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: now.Add(-time.Minute)},
			Reason: upgradeReleasedMarker,
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl, v1alpha1.BroodVerbUpgrade, timeout, now)
		if target.State != v1alpha1.BroodTargetStateDispatched {
			t.Errorf("state = %s, want Dispatched (still not back to Connected)", target.State)
		}
		if target.Reason != upgradeReleasedMarker {
			t.Error("released marker must be preserved across re-evaluation")
		}
	})

	t.Run("times out at 30 minutes when stuck", func(t *testing.T) {
		rec, _, _ := newBORec(t)
		ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
		ctrl.Annotations = map[string]string{annotationUpgradeRelease: "jenkins/jenkins:2.570.2"}
		target := &v1alpha1.BroodTargetStatus{
			Namespace: "ns1", Name: "ctrl-a",
			State: v1alpha1.BroodTargetStateDispatched, DispatchedAt: &metav1.Time{Time: now.Add(-31 * time.Minute)},
		}
		rec.evaluateDispatchedTarget(context.Background(), op, target, ctrl, v1alpha1.BroodVerbUpgrade, timeout, now)
		if target.State != v1alpha1.BroodTargetStateFailed {
			t.Errorf("state = %s, want Failed", target.State)
		}
		if target.Reason != "upgrade timeout" {
			t.Errorf("reason = %q, want upgrade timeout", target.Reason)
		}
	})
}
