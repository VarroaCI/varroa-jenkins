package cells

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// patchFor finds the tenant patch a cell received, and answers nil when
// the cell took none.
func patchFor(out Outcome, cell string) *CellPatch {
	for i := range out.CellPatches {
		if out.CellPatches[i].Name == cell {
			return &out.CellPatches[i]
		}
	}
	return nil
}

// claimedCells answers the cell names the outcome claimed.
func claimedCells(out Outcome) map[string]bool {
	claimed := map[string]bool{}
	for _, c := range out.CellClaims {
		claimed[c.Name] = true
	}
	return claimed
}

// placedWithMembers builds a placed status carrying a member list.
func placedWithMembers(members []v1alpha1.TeamMemberStatus) *v1alpha1.TeamStatus {
	status := placedStatus(3)
	status.Placement.Members = members
	return status
}

// TestPlaceClaimsEveryMemberCell asserts every member cell takes a claim,
// not slot 0's alone. A pool drain reads the fence set before it re-reads
// a free cell, so only the claim closes the window in which the drain
// deletes a cell this pass is binding.
func TestPlaceClaimsEveryMemberCell(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 1)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name), readyCell("cell-b"), readyCell("cell-c"),
	}
	status := placedStatus(3)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	claimed := claimedCells(out)
	if !claimed["cell-b"] || !claimed["cell-c"] {
		t.Fatalf("claims = %+v, want cell-b and cell-c held", out.CellClaims)
	}
	for _, claim := range out.CellClaims {
		if claim.Tenant != team.Name {
			t.Fatalf("a claim names the tenant %q, want %q", claim.Tenant, team.Name)
		}
	}
}

// TestPlaceClaimsTheMoveTargetAndEveryMember asserts a move claims the new
// slot-0 cell beside every member cell it binds.
func TestPlaceClaimsTheMoveTargetAndEveryMember(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{readyCell("cell-new"), readyCell("cell-other")}
	status := placedStatus(3)
	status.Placement.Cell = "cell-gone"

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(out.CellClaims) != 2 {
		t.Fatalf("claims = %+v, want one for each bound cell", out.CellClaims)
	}
}

// TestPlaceReleasesAScaledDownSlot asserts a scale from two slots to one
// drops the tenant from the released slot's cell.
func TestPlaceReleasesAScaledDownSlot(t *testing.T) {
	in := testInputs()
	team := slotTeam(1, 0)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name), appliedCell("cell-b", team.Name),
	}
	status := placedWithMembers([]v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	})

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	patch := patchFor(out, "cell-b")
	if patch == nil {
		t.Fatalf("the released slot's cell took no patch: %+v", out.CellPatches)
	}
	if containsString(patch.Tenants, team.Name) {
		t.Fatalf("the released cell still names the tenant: %+v", patch.Tenants)
	}
	if len(status.Placement.Members) != 1 {
		t.Fatalf("status members = %+v, want one", status.Placement.Members)
	}
}

// TestPlaceReleasesAScaledDownStandby asserts a standby reduction drops
// the tenant from the released standby's cell.
func TestPlaceReleasesAScaledDownStandby(t *testing.T) {
	in := testInputs()
	team := slotTeam(1, 0)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name), appliedCell("cell-c", team.Name),
	}
	status := placedWithMembers([]v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-c", v1alpha1.TeamMemberRoleStandby, nil),
	})

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	patch := patchFor(out, "cell-c")
	if patch == nil {
		t.Fatalf("the released standby's cell took no patch: %+v", out.CellPatches)
	}
	if containsString(patch.Tenants, team.Name) {
		t.Fatalf("the released cell still names the tenant: %+v", patch.Tenants)
	}
}

// TestPlaceReleasesAReplacedMember asserts a member whose cell leaves the
// candidate set is released from the cell it moves to, and that the tenant
// leaves the cell it held.
func TestPlaceReleasesAReplacedMember(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	evacuating := appliedCell("cell-b", team.Name)
	evacuating.Evacuate = true
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name), evacuating, readyCell("cell-new"),
	}
	status := placedWithMembers([]v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	})

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	released := patchFor(out, "cell-b")
	if released == nil || containsString(released.Tenants, team.Name) {
		t.Fatalf("the replaced member's cell keeps the tenant: %+v", out.CellPatches)
	}
	bound := patchFor(out, "cell-new")
	if bound == nil || !containsString(bound.Tenants, team.Name) {
		t.Fatalf("the new member's cell does not name the tenant: %+v", out.CellPatches)
	}
}

// TestPlaceReleasesEveryMemberOnTeardown asserts teardown drops the tenant
// from every member cell, not slot 0's alone.
func TestPlaceReleasesEveryMemberOnTeardown(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 1)
	team.Spec.Pipelines = nil
	in.Cells = []CellCandidate{
		readyCell("cell-a", team.Name), readyCell("cell-b", team.Name), readyCell("cell-c", team.Name),
	}
	status := placedWithMembers([]v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
		memberStatus("cell-c", v1alpha1.TeamMemberRoleStandby, nil),
	})

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	for _, name := range []string{"cell-a", "cell-b", "cell-c"} {
		patch := patchFor(out, name)
		if patch == nil {
			t.Fatalf("%s took no teardown patch: %+v", name, out.CellPatches)
		}
		if containsString(patch.Tenants, team.Name) {
			t.Fatalf("%s still names the tenant after teardown: %+v", name, patch.Tenants)
		}
	}
}

// TestTeardownWaitsForEveryMemberToDrain asserts teardown holds while any
// member cell still reports the tenant applied.
func TestTeardownWaitsForEveryMemberToDrain(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	team.Spec.Pipelines = nil
	team.Finalizers = []string{v1alpha1.TeamFinalizerTenantPlacement}
	stillApplied := readyCell("cell-b", team.Name)
	stillApplied.AppliedTenants = []string{team.Name}
	in.Cells = []CellCandidate{readyCell("cell-a", team.Name), stillApplied}
	status := placedWithMembers([]v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	})

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if out.FinalizerRelease {
		t.Fatal("teardown released the finalizer while a member still holds the tenant")
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonTenantTeardownPending {
		t.Fatalf("condition = %+v, want TenantTeardownPending", cond)
	}
}

// TestPlaceReleasesAMemberWithACellScopedPurge asserts a released member
// takes a purge narrowed to its own cell, so the backstop stops firing
// that cell's rows and every other slot's rows stay.
func TestPlaceReleasesAMemberWithACellScopedPurge(t *testing.T) {
	in := testInputs()
	team := slotTeam(1, 0)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name), appliedCell("cell-b", team.Name),
	}
	var seen []PurgeRequest
	in.RequestPurge = func(_ context.Context, req PurgeRequest) (bool, error) {
		seen = append(seen, req)
		return true, nil
	}
	status := placedWithMembers([]v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	})

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	found := false
	for _, req := range seen {
		// The namespace is part of the reference: a schedule row is keyed
		// by both, and two namespaces can hold a cell of the same name.
		if req.Cell == "cell-b" && req.CellNamespace == "cells-ns" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no cell-scoped purge for the released member: %+v", seen)
	}
}
