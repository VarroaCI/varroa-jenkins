package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// heldUpgradeOp returns an upgrade operation with one Pending target on ctrl-a.
func heldUpgradeOp() *v1alpha1.BroodOperation {
	op := upgradeDispatchOp(&v1alpha1.BroodUpgradeAction{})
	op.Status.Targets = []v1alpha1.BroodTargetStatus{
		{Namespace: "ns1", Name: "ctrl-a", State: v1alpha1.BroodTargetStatePending},
	}
	return op
}

// undecidedCandidateStatus is a candidate whose PluginsServable is Unknown.
func undecidedCandidateStatus(phase v1alpha1.ProfileCandidatePhase, st metav1.ConditionStatus) v1alpha1.ProfileCandidateStatus {
	return v1alpha1.ProfileCandidateStatus{
		Phase: phase,
		Conditions: []v1alpha1.ProfileCandidateCondition{
			{Type: v1alpha1.ConditionCandidatePluginsServable, Status: st, Reason: "ResolutionDegraded", Message: "upstream metadata degraded"},
		},
	}
}

// TestDispatchLoop_UnknownServabilityHoldsTarget: an undecided candidate verdict
// returns the target to Pending. It is neither Dispatched nor Failed, and nothing
// is written to the controller.
func TestDispatchLoop_UnknownServabilityHoldsTarget(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	op := heldUpgradeOp()
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	crdstore.MustSeed(fc.store, &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: "p1-candidate"},
		Spec:       v1alpha1.ProfileCandidateSpec{ProfileRef: "p1", ObservedVersion: "2.555", ResolveVersion: "2.555.1"},
		Status:     undecidedCandidateStatus(v1alpha1.ProfileCandidatePhasePending, metav1.ConditionUnknown),
	})

	rec.dispatchLoop(context.Background(), op, testLogger())

	tgt := op.Status.Targets[0]
	if tgt.State != v1alpha1.BroodTargetStatePending {
		t.Fatalf("state = %s, want Pending (held)", tgt.State)
	}
	if tgt.DispatchedAt != nil {
		t.Error("a held target must not keep a DispatchedAt stamp")
	}
	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), fc.store, "ctrl-a", "ns1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := stored.Annotations[annotationUpgradeRelease]; ok {
		t.Error("release annotation must not be written while the target is held")
	}
	if len(fc.ssaApplies) != 0 {
		t.Error("spec.version must not be written while the target is held")
	}
}

// TestDispatchLoop_HeldTargetDispatchesOnceServable: after the candidate turns
// True, the same target dispatches on the next tick, and is not Failed.
func TestDispatchLoop_HeldTargetDispatchesOnceServable(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	op := heldUpgradeOp()
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	crdstore.MustSeed(fc.store, &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: "p1-candidate"},
		Spec:       v1alpha1.ProfileCandidateSpec{ProfileRef: "p1", ObservedVersion: "2.555", ResolveVersion: "2.555.1"},
		Status:     undecidedCandidateStatus(v1alpha1.ProfileCandidatePhasePending, metav1.ConditionUnknown),
	})
	rec.dispatchLoop(context.Background(), op, testLogger())
	if op.Status.Targets[0].State != v1alpha1.BroodTargetStatePending {
		t.Fatalf("setup: expected the target held Pending, got %s", op.Status.Targets[0].State)
	}

	ready := undecidedCandidateStatus(v1alpha1.ProfileCandidatePhaseReady, metav1.ConditionTrue)
	if err := crdstore.PatchStatus[v1alpha1.ProfileCandidate](context.Background(), fc.store, "p1-candidate", "", &ready); err != nil {
		t.Fatalf("PatchStatus: %v", err)
	}

	rec.dispatchLoop(context.Background(), op, testLogger())

	tgt := op.Status.Targets[0]
	if tgt.State == v1alpha1.BroodTargetStateFailed {
		t.Fatalf("a later True verdict must not fail the target: %s", tgt.Reason)
	}
	if tgt.State == v1alpha1.BroodTargetStatePending {
		t.Fatal("expected the target to dispatch once the profile is servable")
	}
}
