package cells

import (
	"context"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// storedMemberOf answers the stored member on cell, or a zero value when the
// status holds no member there.
func storedMemberOf(status *v1alpha1.TenantStatus, cell string) v1alpha1.TenantMemberStatus {
	for _, m := range status.Placement.Members {
		if m.Cell == cell {
			return m
		}
	}
	return v1alpha1.TenantMemberStatus{}
}

// editCell applies edit to the named candidate of the inputs.
func editCell(in *Inputs, name string, edit func(*CellCandidate)) {
	for i := range in.Cells {
		if in.Cells[i].Name == name {
			edit(&in.Cells[i])
			return
		}
	}
}

// simulateMemberDrain acts as the member cell does on a drain tick. While
// the stored member is draining, the cell drops the tenant's folder. Once
// the member is current, the cell applies the folder again.
func simulateMemberDrain(status *v1alpha1.TenantStatus, c *CellCandidate) {
	if storedMemberOf(status, c.Name).Draining {
		c.AppliedTenants = removeString(c.AppliedTenants, "payments")
		return
	}
	c.AppliedTenants = appendUnique(c.AppliedTenants, "payments")
}

// drainedHolder is a cell that still names the tenant but holds no folder
// and runs a pod, so it counts as drained.
func drainedHolder(name, tenant string) CellCandidate {
	c := readyCell(name, tenant)
	c.Instance = "uid-" + name
	return c
}

// TestPlaceWritesTheDrainFlagWithNoAnnouncement asserts a drain starts with
// no announcement and no claim on the replacement cell.
func TestPlaceWritesTheDrainFlagWithNoAnnouncement(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", "payments"), appliedCell("cell-b", "payments")}
	status := placedStatus(3)
	status.Placement.Cell = "cell-a"
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	}
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	steady, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place steady tick: %v", err)
	}
	in.LastSync = steady.SyncCacheUpdate
	seen = nil

	editCell(&in, "cell-b", func(c *CellCandidate) { *c = oldProfileCell("cell-b", "payments") })
	in.Cells = append(in.Cells, readyCell("cell-e"))
	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("a drain flag must not announce, got %d announce(s)", len(seen))
	}
	if !storedMemberOf(status, "cell-b").Draining {
		t.Errorf("cell-b should drain, got %+v", status.Placement.Members)
	}
	if patchFor(out, "cell-e") != nil || claimedCells(out)["cell-e"] {
		t.Errorf("the replacement cell-e must not be patched or claimed during the drain: %+v", out.CellPatches)
	}
}

// TestPlaceKeepsTheDrainWhenTheReplacementGoesAway asserts a replacement that
// disappears during the drain does not end it.
func TestPlaceKeepsTheDrainWhenTheReplacementGoesAway(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", "payments"), oldProfileCell("cell-b", "payments"), readyCell("cell-e"),
	}
	status := placedStatus(3)
	status.Placement.Cell = "cell-a"
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	}

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	in.LastSync = out.SyncCacheUpdate
	in.Cells = in.Cells[:2]
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	if !storedMemberOf(status, "cell-b").Draining {
		t.Errorf("the drain should hold without a replacement, got %+v", status.Placement.Members)
	}
}

// TestPlaceHoldsADrainedStaleMemberWithNoReplacement asserts a stale member
// whose cell has drained, and that has no replacement, keeps its slot and
// is not marked draining.
func TestPlaceHoldsADrainedStaleMemberWithNoReplacement(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", "payments"), oldProfileCell("cell-b")}
	status := placedStatus(3)
	status.Placement.Cell = "cell-a"
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	}

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	m := storedMemberOf(status, "cell-b")
	if m.Cell == "" || !m.Stale || m.Draining {
		t.Errorf("cell-b should hold its slot as stale and not draining, got %+v", m)
	}
	if strings.Contains(placedMessage(status), "(draining)") {
		t.Errorf("no member drains, got %q", placedMessage(status))
	}
}

// TestPlaceEndsTheDrainWhenTheMemberIsCurrentAgain asserts a draining member
// that serves the tenant's profile again stops draining.
func TestPlaceEndsTheDrainWhenTheMemberIsCurrentAgain(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", "payments"), oldProfileCell("cell-b", "payments"), readyCell("cell-e"),
	}
	status := placedStatus(3)
	status.Placement.Cell = "cell-a"
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	}

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if !storedMemberOf(status, "cell-b").Draining {
		t.Fatalf("cell-b should drain on tick 1, got %+v", status.Placement.Members)
	}
	in.LastSync = out.SyncCacheUpdate
	editCell(&in, "cell-b", func(c *CellCandidate) { c.ResolvedProfile = "jenkins-lts" })
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	m := storedMemberOf(status, "cell-b")
	if m.Stale || m.Draining {
		t.Errorf("cell-b is current again and should not drain, got %+v", m)
	}
}

// TestPlaceReplacesAStaleMemberWithNoPodAtOnce asserts a stale member whose
// cell runs no pod has no builds to protect, so the slot moves at once.
func TestPlaceReplacesAStaleMemberWithNoPodAtOnce(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	b := oldProfileCell("cell-b", "payments")
	b.Instance = ""
	in.Cells = []CellCandidate{appliedCell("cell-a", "payments"), b, readyCell("cell-e")}
	status := placedStatus(3)
	status.Placement.Cell = "cell-a"
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	}

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	slot1 := findMember(memberBindingsOf(status), "cell-e")
	if slot1 == nil || slot1.Slot == nil || *slot1.Slot != 1 {
		t.Errorf("slot 1 should move to cell-e at once, got %+v", status.Placement.Members)
	}
	if !dropsTenant(patchFor(out, "cell-b")) {
		t.Errorf("cell-b should drop the tenant at once, got %+v", out.CellPatches)
	}
}

// TestPlaceDrainsAScaledDownSlotBeforeItsRelease asserts a slot removed by a
// scale-down releases its cell only after the cell drops the folder.
func TestPlaceDrainsAScaledDownSlotBeforeItsRelease(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(1, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", tenant.Name), appliedCell("cell-b", tenant.Name)}
	status := placedWithMembers([]v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	})
	var purges []PurgeRequest
	in.RequestPurge = func(_ context.Context, req PurgeRequest) (bool, error) {
		purges = append(purges, req)
		return true, nil
	}

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place tick 1: %v", err)
	}
	if patchFor(out, "cell-b") != nil || len(purges) != 0 {
		t.Errorf("tick 1 must not release cell-b: patches %+v purges %+v", out.CellPatches, purges)
	}
	if !storedMemberOf(status, "cell-b").Draining {
		t.Errorf("cell-b should drain, got %+v", status.Placement.Members)
	}

	in.LastSync = out.SyncCacheUpdate
	editCell(&in, "cell-b", func(c *CellCandidate) { simulateMemberDrain(status, c) })
	out, err = Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	if !dropsTenant(patchFor(out, "cell-b")) {
		t.Errorf("tick 2 should release cell-b, got %+v", out.CellPatches)
	}
	if status.Placement.Generation != 4 || len(status.Placement.Members) != 1 {
		t.Errorf("tick 2 generation = %d members = %+v, want 4 and one member",
			status.Placement.Generation, status.Placement.Members)
	}
	scoped := 0
	for _, p := range purges {
		if p.Cell == "cell-b" {
			scoped++
		}
	}
	if scoped == 0 {
		t.Errorf("tick 2 should purge cell-b's schedule rows: %+v", purges)
	}
}

// TestPlaceDrainsAScaledDownStandbyBeforeItsRelease asserts a standby removed
// by a scale-down drains its cell before the release.
func TestPlaceDrainsAScaledDownStandbyBeforeItsRelease(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(1, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", tenant.Name), appliedCell("cell-c", tenant.Name)}
	status := placedWithMembers([]v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-c", v1alpha1.TenantMemberRoleStandby, nil),
	})

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place tick 1: %v", err)
	}
	if patchFor(out, "cell-c") != nil || !storedMemberOf(status, "cell-c").Draining {
		t.Fatalf("tick 1 must drain cell-c first: patches %+v members %+v", out.CellPatches, status.Placement.Members)
	}

	in.LastSync = out.SyncCacheUpdate
	editCell(&in, "cell-c", func(c *CellCandidate) { simulateMemberDrain(status, c) })
	out, err = Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place tick 2: %v", err)
	}
	if !dropsTenant(patchFor(out, "cell-c")) {
		t.Errorf("tick 2 should release cell-c, got %+v", out.CellPatches)
	}
}

// TestStaleMembersMessageMarksADrainingMember asserts a draining member is
// named with the draining mark.
func TestStaleMembersMessageMarksADrainingMember(t *testing.T) {
	members := []v1alpha1.TenantMemberStatus{{
		Cell: "cell-b", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive,
		Slot: intPtr(1), Stale: true, Draining: true,
	}}
	msg := staleMembersMessage(testInputs(), slotTenant(2, 0), members)
	if !strings.Contains(msg, "slot 1 cells-ns/cell-b (draining)") {
		t.Errorf("message should mark the draining member, got %q", msg)
	}
}

// TestPromotionSkipsADrainingStandby asserts a draining standby never takes a
// slot.
func TestPromotionSkipsADrainingStandby(t *testing.T) {
	at := fixedNow
	standbys := []MemberBinding{{
		Cell: "cell-c", Namespace: "cells-ns", Applied: true, Draining: true, AppliedAt: &at,
	}}
	if got := takePromotionSource(&standbys, map[string]bool{}); got != nil {
		t.Errorf("a draining standby must not be a promotion source, got %+v", got)
	}
}
