package cells

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func intPtr(n int32) *int32 { return &n }

// slotTeam builds a pipelines tenant asking for members active slots and
// standby warm members.
func slotTeam(members, standby int32) *v1alpha1.Team {
	team := testTeam()
	team.Spec.Pipelines.Members = &members
	team.Spec.Pipelines.Standby = &standby
	return team
}

// appliedCell is readyCell whose appliedTenants names the tenant, so the
// member reads as applied.
func appliedCell(name string, tenants ...string) CellCandidate {
	c := readyCell(name, tenants...)
	c.AppliedTenants = tenants
	c.Instance = "uid-" + name
	return c
}

// memberStatus builds one stored member entry.
func memberStatus(cell, role string, slot *int32) v1alpha1.TeamMemberStatus {
	return v1alpha1.TeamMemberStatus{
		Cell: cell, CellNamespace: "cells-ns", Role: role, Slot: slot, Applied: true,
	}
}

// findMember answers the decided member bound to cell.
func findMember(members []MemberBinding, cell string) *MemberBinding {
	for i := range members {
		if members[i].Cell == cell {
			return &members[i]
		}
	}
	return nil
}

// TestDesiredMembersBindsEverySlotAndStandby asserts a tenant asking for
// two slots and one standby takes three cells, one member each.
func TestDesiredMembersBindsEverySlotAndStandby(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 1)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name), readyCell("cell-b"), readyCell("cell-c"),
	}
	status := placedStatus(3)
	slotZero := in.Cells[0]

	members, reason := desiredMembers(in, team, status, &slotZero, fixedNow)
	if len(members) != 3 {
		t.Fatalf("members = %d, want 3: %+v", len(members), members)
	}
	cells := map[string]bool{}
	for _, m := range members {
		if cells[m.Cell] {
			t.Fatalf("cell %s holds two members of one tenant", m.Cell)
		}
		cells[m.Cell] = true
	}
	if reason != v1alpha1.TeamReasonStandbyPending && reason != "" &&
		reason != v1alpha1.TeamReasonMembersApplied {
		t.Fatalf("reason = %q, want no pending slot", reason)
	}
}

// TestDesiredMembersReportsSlotPendingOnAFullPool asserts a pool with one
// free cell leaves slot 1 empty and reports SlotPending, and that
// SlotPending outranks StandbyPending.
func TestDesiredMembersReportsSlotPendingOnAFullPool(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 1)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name)}
	status := placedStatus(3)
	slotZero := in.Cells[0]

	members, reason := desiredMembers(in, team, status, &slotZero, fixedNow)
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1: %+v", len(members), members)
	}
	if reason != v1alpha1.TeamReasonSlotPending {
		t.Fatalf("reason = %q, want SlotPending", reason)
	}
}

// TestDesiredMembersReportsStandbyPending asserts a tenant whose every
// active slot holds a cell, and whose standby does not, reports
// StandbyPending.
func TestDesiredMembersReportsStandbyPending(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 1)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), readyCell("cell-b")}
	status := placedStatus(3)
	slotZero := in.Cells[0]

	members, reason := desiredMembers(in, team, status, &slotZero, fixedNow)
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2: %+v", len(members), members)
	}
	if reason != v1alpha1.TeamReasonStandbyPending {
		t.Fatalf("reason = %q, want StandbyPending", reason)
	}
}

// TestDesiredMembersReportsMembersApplied asserts every member's cell
// reporting the tenant applied answers MembersApplied.
func TestDesiredMembersReportsMembersApplied(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), appliedCell("cell-b", team.Name)}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	}
	slotZero := in.Cells[0]

	_, reason := desiredMembers(in, team, status, &slotZero, fixedNow)
	if reason != v1alpha1.TeamReasonMembersApplied {
		t.Fatalf("reason = %q, want MembersApplied", reason)
	}
}

// TestDesiredMembersPromotesTheNewestAppliedStandby asserts the promotion
// choice: the standby with the newer applied time takes the empty slot.
func TestDesiredMembersPromotesTheNewestAppliedStandby(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 1)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name),
		appliedCell("cell-old", team.Name),
		appliedCell("cell-new", team.Name),
	}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-old", v1alpha1.TeamMemberRoleStandby, nil),
		memberStatus("cell-new", v1alpha1.TeamMemberRoleStandby, nil),
	}
	older := fixedNow.Add(-time.Hour)
	newer := fixedNow.Add(-time.Minute)
	in.LastSync = &SyncCache{Members: []AnnounceMember{
		{Cell: "cell-old", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleStandby, AppliedAt: &older},
		{Cell: "cell-new", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleStandby, AppliedAt: &newer},
	}}
	slotZero := in.Cells[0]

	members, _ := desiredMembers(in, team, status, &slotZero, fixedNow)
	promoted := findMember(members, "cell-new")
	if promoted == nil || promoted.Role != v1alpha1.TeamMemberRoleActive ||
		promoted.Slot == nil || *promoted.Slot != 1 {
		t.Fatalf("cell-new did not take slot 1: %+v", members)
	}
	stayed := findMember(members, "cell-old")
	if stayed == nil || stayed.Role != v1alpha1.TeamMemberRoleStandby {
		t.Fatalf("cell-old did not stay a standby: %+v", members)
	}
}

// TestPromotionBreaksATieByCellName asserts two standbys with the same
// applied time promote the lower cell name.
func TestPromotionBreaksATieByCellName(t *testing.T) {
	at := fixedNow
	standbys := []MemberBinding{
		{Cell: "cell-z", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleStandby, Applied: true, AppliedAt: &at},
		{Cell: "cell-b", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleStandby, Applied: true, AppliedAt: &at},
	}
	chosen := takePromotionSource(&standbys, map[string]bool{})
	if chosen == nil || chosen.Cell != "cell-b" {
		t.Fatalf("chosen = %+v, want cell-b", chosen)
	}
	if len(standbys) != 1 || standbys[0].Cell != "cell-z" {
		t.Fatalf("the remaining standbys = %+v, want cell-z alone", standbys)
	}
}

// TestPromotionSkipsAnUnappliedStandby asserts a standby whose cell does
// not report the tenant applied is no promotion source, so the slot falls
// through to the existing move path.
func TestPromotionSkipsAnUnappliedStandby(t *testing.T) {
	standbys := []MemberBinding{
		{Cell: "cell-b", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleStandby, Applied: false},
	}
	if chosen := takePromotionSource(&standbys, map[string]bool{}); chosen != nil {
		t.Fatalf("an unapplied standby was promoted: %+v", chosen)
	}
}

// TestDesiredMembersScaleDownReleasesTheHighestSlots asserts a scale from
// three slots to two keeps slots 0 and 1, so the surviving indexes stay
// dense.
func TestDesiredMembersScaleDownReleasesTheHighestSlots(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name), appliedCell("cell-b", team.Name), appliedCell("cell-c", team.Name),
	}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
		memberStatus("cell-c", v1alpha1.TeamMemberRoleActive, intPtr(2)),
	}
	slotZero := in.Cells[0]

	members, _ := desiredMembers(in, team, status, &slotZero, fixedNow)
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2: %+v", len(members), members)
	}
	if findMember(members, "cell-c") != nil {
		t.Fatalf("the highest slot was not released: %+v", members)
	}
	slots := map[int32]bool{}
	for _, m := range members {
		if m.Slot == nil {
			t.Fatalf("an active member carries no slot: %+v", m)
		}
		slots[*m.Slot] = true
	}
	if !slots[0] || !slots[1] {
		t.Fatalf("the surviving slots are not 0 and 1: %+v", slots)
	}
}

// TestDesiredMembersCountsAStandbyAgainstCapacity asserts a standby
// consumes one unit of its cell's maxTenantsPerCell: a cell already at its
// cap is no candidate.
func TestDesiredMembersCountsAStandbyAgainstCapacity(t *testing.T) {
	in := testInputs()
	in.MaxTenantsDefault = 1
	team := slotTeam(1, 1)
	full := readyCell("cell-b", "other-tenant")
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), full}
	status := placedStatus(3)
	slotZero := in.Cells[0]

	members, reason := desiredMembers(in, team, status, &slotZero, fixedNow)
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1: a full cell is no candidate: %+v", len(members), members)
	}
	if reason != v1alpha1.TeamReasonStandbyPending {
		t.Fatalf("reason = %q, want StandbyPending", reason)
	}
}

// TestDesiredMembersNeverPicksACellHoldingAMember asserts the added
// selection test: a cell that already holds a member of this tenant is
// never selected for a second slot.
func TestDesiredMembersNeverPicksACellHoldingAMember(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name)}
	status := placedStatus(3)
	slotZero := in.Cells[0]

	members, _ := desiredMembers(in, team, status, &slotZero, fixedNow)
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1: the one cell already holds slot 0: %+v", len(members), members)
	}
}

// TestDesiredMembersDropsAnEvacuatingCell asserts a member whose cell is
// evacuating leaves the set, and the slot takes a fresh candidate.
func TestDesiredMembersDropsAnEvacuatingCell(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	evacuating := appliedCell("cell-b", team.Name)
	evacuating.Evacuate = true
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), evacuating, readyCell("cell-c")}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	}
	slotZero := in.Cells[0]

	members, _ := desiredMembers(in, team, status, &slotZero, fixedNow)
	if findMember(members, "cell-b") != nil {
		t.Fatalf("an evacuating cell kept its member: %+v", members)
	}
	if findMember(members, "cell-c") == nil {
		t.Fatalf("slot 1 did not take a fresh candidate: %+v", members)
	}
}

// TestPlaceAppliesEveryMemberOnItsCell asserts the pass names the tenant
// on every member's cell, so the tenant's items and its placement
// rehydration seed apply on each of them.
func TestPlaceAppliesEveryMemberOnItsCell(t *testing.T) {
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
	patched := map[string]bool{}
	for _, p := range out.CellPatches {
		patched[p.Name] = true
		if !containsString(p.Tenants, team.Name) {
			t.Fatalf("the patch for %s does not name the tenant: %+v", p.Name, p.Tenants)
		}
	}
	if !patched["cell-b"] || !patched["cell-c"] {
		t.Fatalf("the pass named the tenant on %v, want cell-b and cell-c as well", patched)
	}
	if len(status.Placement.Members) != 3 {
		t.Fatalf("status members = %+v, want three", status.Placement.Members)
	}
}

// TestPlaceReportsSlotPendingOnTheCondition asserts the member reason
// reaches the Placed condition, and that it outranks the reason the
// existing keep path set.
func TestPlaceReportsSlotPendingOnTheCondition(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name)}
	status := placedStatus(3)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonSlotPending {
		t.Fatalf("condition = %+v, want SlotPending", cond)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("status = %q, want False", cond.Status)
	}
}

// TestPlaceKeepsTenantPlacedForASingleMemberTenant asserts a tenant that
// asks for one member and no standby keeps today's reason, so every
// earlier rule is unchanged for it.
func TestPlaceKeepsTenantPlacedForASingleMemberTenant(t *testing.T) {
	in := testInputs()
	team := testTeam()
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name)}
	status := placedStatus(3)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	cond := condition(status)
	if cond == nil || cond.Reason != v1alpha1.TeamReasonTenantPlaced {
		t.Fatalf("condition = %+v, want TenantPlaced", cond)
	}
}
