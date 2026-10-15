package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestDispatchLoop_UnevaluatedCandidateHoldsTarget: an open candidate that has no
// PluginsServable condition yet has no verdict. The target is held, not dispatched.
func TestDispatchLoop_UnevaluatedCandidateHoldsTarget(t *testing.T) {
	ctrl := testCtrl2("ctrl-a", "ns1", v1alpha1.ControllerPhaseConnected)
	ctrl.Spec.Version = "2.555"
	op := heldUpgradeOp()
	rec, fc, _ := newBORec(t, op, ctrl)
	crdstore.MustSeed(fc.store, ctrl)
	crdstore.MustSeed(fc.store, lineProfile("2.555"))
	crdstore.MustSeed(fc.store, &v1alpha1.ProfileCandidate{
		ObjectMeta: metav1.ObjectMeta{Name: "p1-candidate"},
		Spec:       v1alpha1.ProfileCandidateSpec{ProfileRef: "p1", ObservedVersion: "2.555", ResolveVersion: "2.555.1"},
		Status:     v1alpha1.ProfileCandidateStatus{Phase: v1alpha1.ProfileCandidatePhasePending},
	})

	rec.dispatchLoop(context.Background(), op, testLogger())

	if state := op.Status.Targets[0].State; state != v1alpha1.BroodTargetStatePending {
		t.Fatalf("state = %s, want Pending (no servability verdict yet)", state)
	}
}
