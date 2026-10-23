package cells

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestTeardownWaitsForAMemberWhenTheScalarCellIsGone asserts a deleted
// scalar cell does not end the teardown while a member cell still holds
// the tenant.
//
// Releasing the finalizer over such a member would leave it the tenant
// folder and a unit of its capacity, and would remove the retry path that
// drops them.
func TestTeardownWaitsForAMemberWhenTheScalarCellIsGone(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	tenant.Spec.Pipelines = nil
	tenant.Finalizers = []string{v1alpha1.TenantFinalizerTenantPlacement}
	stillApplied := readyCell("cell-b", tenant.Name)
	stillApplied.AppliedTenants = []string{tenant.Name}
	// The scalar cell, cell-a, is absent from the candidate set.
	in.Cells = []CellCandidate{stillApplied}
	status := placedWithMembers([]v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	})

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if out.FinalizerRelease {
		t.Fatal("teardown released the finalizer while a member still holds the tenant")
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TenantReasonTenantTeardownPending {
		t.Fatalf("condition = %+v, want TenantTeardownPending", cond)
	}
	patch := patchFor(out, "cell-b")
	if patch == nil || containsString(patch.Tenants, tenant.Name) {
		t.Fatalf("the surviving member took no removal patch: %+v", out.CellPatches)
	}

	// The member drains, and the next tick completes the teardown.
	in.Cells = []CellCandidate{readyCell("cell-b")}
	out, err = Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place after the drain: %v", err)
	}
	if !out.FinalizerRelease {
		t.Fatalf("teardown held the finalizer after every member drained: %+v", out)
	}
	if status.Placement != nil {
		t.Fatalf("teardown kept the placement: %+v", status.Placement)
	}
}

// TestTeardownEndsWhenTheScalarCellIsGoneAndEveryMemberDrained asserts the
// unchanged case: a deleted scalar cell whose members have all drained
// completes the teardown at once.
func TestTeardownEndsWhenTheScalarCellIsGoneAndEveryMemberDrained(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	tenant.Spec.Pipelines = nil
	tenant.Finalizers = []string{v1alpha1.TenantFinalizerTenantPlacement}
	in.Cells = []CellCandidate{readyCell("cell-b")}
	status := placedWithMembers([]v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	})

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if !out.FinalizerRelease {
		t.Fatalf("teardown held the finalizer with no member left to drain: %+v", out)
	}
}
