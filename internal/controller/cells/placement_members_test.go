package cells

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// recordingAnnounce answers ok and records every request it received.
func recordingAnnounce(ok bool, seen *[]AnnounceRequest) Announcer {
	return func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		*seen = append(*seen, req)
		return AnnounceResult{OK: ok}, nil
	}
}

// TestPlaceAnnouncesTheMemberListBeforeTheStatusWrite asserts the announce
// is the pre-step: the recorder holds the new member list before
// Team.status.placement.members changes.
func TestPlaceAnnouncesTheMemberListBeforeTheStatusWrite(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), readyCell("cell-b")}
	status := placedStatus(3)

	var seen []AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		// The status must still hold the old member list at the moment
		// the announcement is sent.
		if len(status.Placement.Members) != 0 {
			t.Errorf("the status already names %d members when the announce runs", len(status.Placement.Members))
		}
		seen = append(seen, req)
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	if len(seen[0].Members) != 2 {
		t.Fatalf("the announcement carried %d members, want 2: %+v", len(seen[0].Members), seen[0].Members)
	}
	if len(status.Placement.Members) != 2 {
		t.Fatalf("the status names %d members, want 2", len(status.Placement.Members))
	}
}

// TestPlaceStopsTheMemberChangeOnAFailedAnnounce asserts a refused
// announcement leaves the member list unchanged, so the pass retries on a
// later tick.
func TestPlaceStopsTheMemberChangeOnAFailedAnnounce(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), readyCell("cell-b")}
	status := placedStatus(3)

	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(false, &seen)

	out, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	if len(status.Placement.Members) != 0 {
		t.Fatalf("a failed announcement wrote %d members", len(status.Placement.Members))
	}
	if status.Placement.Generation != 3 {
		t.Fatalf("a failed announcement raised the generation to %d", status.Placement.Generation)
	}
	for _, p := range out.CellPatches {
		if p.Name == "cell-b" {
			t.Fatal("a failed announcement patched the new member's cell")
		}
	}
	if out.SyncCacheUpdate != nil {
		t.Fatal("a failed announcement updated the sync cache")
	}
}

// TestPlaceAnnouncesSlotZeroAsTheFirstMember asserts slot 0's entry
// repeats the request's scalar fields, so the recorder's write path is
// unchanged for a single-member tenant.
func TestPlaceAnnouncesSlotZeroAsTheFirstMember(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 1)
	in.Cells = []CellCandidate{
		appliedCell("cell-a", team.Name), readyCell("cell-b"), readyCell("cell-c"),
	}
	status := placedStatus(3)

	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	first := seen[0].Members[0]
	if first.Cell != seen[0].Cell || first.Namespace != seen[0].Namespace {
		t.Fatalf("the first member %+v does not repeat the scalar cell %s/%s",
			first, seen[0].Namespace, seen[0].Cell)
	}
	if first.Slot == nil || *first.Slot != 0 || first.Role != v1alpha1.TeamMemberRoleActive {
		t.Fatalf("the first member is not slot 0: %+v", first)
	}
	last := seen[0].Members[len(seen[0].Members)-1]
	if last.Role != v1alpha1.TeamMemberRoleStandby || last.Slot != nil {
		t.Fatalf("the last member is not a standby with no slot: %+v", last)
	}
}

// TestPlaceRaisesTheGenerationOnlyOnACellSetChange asserts a repeat tick
// with the same member set leaves the generation alone.
func TestPlaceRaisesTheGenerationOnlyOnACellSetChange(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), appliedCell("cell-b", team.Name)}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if status.Placement.Generation != 3 {
		t.Fatalf("the generation rose to %d with no cell set change", status.Placement.Generation)
	}
}

// TestPlaceRaisesTheGenerationWhenASlotRebinds asserts a slot taking
// another cell raises the tenant's one generation.
func TestPlaceRaisesTheGenerationWhenASlotRebinds(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), readyCell("cell-new")}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-gone", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if status.Placement.Generation != 4 {
		t.Fatalf("the generation is %d, want 4: slot 1 rebound", status.Placement.Generation)
	}
}

// TestPlaceReportsADeadInstanceForEveryMember asserts the announcement
// names a dead instance for each member whose instance changed, and not
// for slot 0 alone.
func TestPlaceReportsADeadInstanceForEveryMember(t *testing.T) {
	in := testInputs()
	team := slotTeam(2, 0)
	rolled := appliedCell("cell-b", team.Name)
	rolled.Instance = "uid-b2"
	in.Cells = []CellCandidate{appliedCell("cell-a", team.Name), rolled}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TeamMemberStatus{
		memberStatus("cell-a", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	}
	in.LastSync = &SyncCache{
		Cell: "cell-a", CellNamespace: "cells-ns", Generation: 3, CellInstance: "uid-cell-a",
		At: fixedNow.Add(-time.Minute),
		Members: []AnnounceMember{
			{Cell: "cell-a", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleActive, Slot: intPtr(0), Instance: "uid-cell-a"},
			{Cell: "cell-b", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleActive, Slot: intPtr(1), Instance: "uid-b1"},
		},
	}

	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	found := false
	for _, d := range seen[0].DeadInstances {
		if d.Instance == "uid-b1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the announcement reports no dead instance for slot 1: %+v", seen[0].DeadInstances)
	}
}
