package cells

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// deadFor finds the dead-instance entry for one instance.
func deadFor(dead []DeadInstance, instance string) *DeadInstance {
	for i := range dead {
		if dead[i].Instance == instance {
			return &dead[i]
		}
	}
	return nil
}

// rolledMemberInputs builds a tenant whose slot 1 cell kept its name and
// reports a new pod instance, which is what a hard pod delete produces.
func rolledMemberInputs(t *testing.T) (Inputs, *v1alpha1.Team, *v1alpha1.TeamStatus, *[]AnnounceRequest) {
	t.Helper()
	in := testInputs()
	team := slotTeam(2, 0)
	slotZero := appliedCell("cell-0", team.Name)
	rolled := appliedCell("cell-2", team.Name)
	rolled.Instance = "fa69db7a"
	in.Cells = []CellCandidate{slotZero, rolled}

	status := placedWithMembers([]v1alpha1.TeamMemberStatus{
		memberStatus("cell-0", v1alpha1.TeamMemberRoleActive, intPtr(0)),
		memberStatus("cell-2", v1alpha1.TeamMemberRoleActive, intPtr(1)),
	})
	memberSeen := fixedNow.Add(-2 * time.Minute)
	in.LastSync = &SyncCache{
		Cell: "cell-0", CellNamespace: "cells-ns", Generation: 3, CellInstance: "uid-cell-0",
		At: fixedNow.Add(-time.Minute),
		Members: []AnnounceMember{
			{Cell: "cell-0", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleActive,
				Slot: intPtr(0), Instance: "uid-cell-0"},
			{Cell: "cell-2", Namespace: "cells-ns", Role: v1alpha1.TeamMemberRoleActive,
				Slot: intPtr(1), Instance: "2918db60", LastSeenAt: &memberSeen},
		},
	}

	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)
	return in, team, status, &seen
}

// TestMemberRollCarriesAConfirmation asserts a member whose cell keeps its
// name and reports a new pod instance carries a confirmation.
//
// Without the stamp the confirmed reading never holds for a member above
// slot 0: no run of that member is armed for a resume, and no unstarted
// intent of it is re-fired. The run instead waits out the five-minute
// moved window and is marked interrupted.
func TestMemberRollCarriesAConfirmation(t *testing.T) {
	in, team, status, seen := rolledMemberInputs(t)

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(*seen))
	}
	entry := deadFor((*seen)[0].DeadInstances, "2918db60")
	if entry == nil {
		t.Fatalf("no dead instance for the replaced pod: %+v", (*seen)[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt == nil {
		t.Fatalf("the replaced member's entry carries no confirmation: %+v", entry)
	}
	if !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("the confirmation is %v, want this tick's clock %v", entry.ConfirmedGoneAt, fixedNow)
	}
}

// TestMemberRollCarriesItsOwnLastSeen asserts the entry's LastSeenAt is
// that member's own presence reading, not slot 0's clock.
func TestMemberRollCarriesItsOwnLastSeen(t *testing.T) {
	in, team, status, seen := rolledMemberInputs(t)
	memberSeen := *in.LastSync.Members[1].LastSeenAt
	scalarSeen := fixedNow.Add(-30 * time.Second)
	in.LastSync.CellLastSeenAt = &scalarSeen

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor((*seen)[0].DeadInstances, "2918db60")
	if entry == nil {
		t.Fatalf("no dead instance for the replaced pod: %+v", (*seen)[0].DeadInstances)
	}
	if !entry.LastSeenAt.Equal(memberSeen) {
		t.Fatalf("LastSeenAt = %v, want the member's own reading %v", entry.LastSeenAt, memberSeen)
	}
}

// TestMemberRollFallsBackToTheScalarLastSeen asserts a cache entry written
// before the member reading existed falls back to the tenant's scalar
// reading.
func TestMemberRollFallsBackToTheScalarLastSeen(t *testing.T) {
	in, team, status, seen := rolledMemberInputs(t)
	in.LastSync.Members[1].LastSeenAt = nil
	scalarSeen := fixedNow.Add(-30 * time.Second)
	in.LastSync.CellLastSeenAt = &scalarSeen

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor((*seen)[0].DeadInstances, "2918db60")
	if entry == nil || !entry.LastSeenAt.Equal(scalarSeen) {
		t.Fatalf("LastSeenAt = %+v, want the scalar reading %v", entry, scalarSeen)
	}
}

// departedMemberInputs builds the tenant of rolledMemberInputs after its
// slot 1 cell left the candidate set. A fresh cell takes the slot.
func departedMemberInputs(t *testing.T) (Inputs, *v1alpha1.Team, *v1alpha1.TeamStatus, *[]AnnounceRequest) {
	t.Helper()
	in, team, status, seen := rolledMemberInputs(t)
	in.Cells = []CellCandidate{appliedCell("cell-0", team.Name), readyCell("cell-new")}
	return in, team, status, seen
}

// TestADepartedMemberWithNoPodIsConfirmedGone asserts a member whose cell
// left the candidate set carries a confirmation when the caller's own pod
// read answers no pod.
//
// A cell delete removes the Controller, the StatefulSet, and the pod
// within seconds. Without the confirmation the promoted standby never
// receives the slot's armed run, and the sweep marks that run interrupted
// five minutes later.
func TestADepartedMemberWithNoPodIsConfirmedGone(t *testing.T) {
	in, team, status, seen := departedMemberInputs(t)
	in.DepartedPods = map[string]bool{"cells-ns/cell-2": true}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor((*seen)[0].DeadInstances, "2918db60")
	if entry == nil {
		t.Fatalf("no dead instance for the departed member: %+v", (*seen)[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("the departed member carries no confirmation: %+v", entry)
	}
}

// TestADepartedMemberThatStillTerminatesCarriesNoConfirmation asserts a
// departed member whose pod still exists carries no confirmation. Such a
// pod can serve builds for its whole termination grace.
func TestADepartedMemberThatStillTerminatesCarriesNoConfirmation(t *testing.T) {
	in, team, status, seen := departedMemberInputs(t)
	in.DepartedPods = map[string]bool{"cells-ns/cell-2": false}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor((*seen)[0].DeadInstances, "2918db60")
	if entry == nil {
		t.Fatalf("no dead instance for the departed member: %+v", (*seen)[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt != nil {
		t.Fatalf("a terminating pod carries a confirmation: %+v", entry)
	}
}

// TestADepartedMemberWithNoReadingCarriesNoConfirmation asserts a departed
// member the caller could not read carries no confirmation, so an API
// failure never confirms a live pod gone.
func TestADepartedMemberWithNoReadingCarriesNoConfirmation(t *testing.T) {
	in, team, status, seen := departedMemberInputs(t)
	in.DepartedPods = nil

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor((*seen)[0].DeadInstances, "2918db60")
	if entry == nil {
		t.Fatalf("no dead instance for the departed member: %+v", (*seen)[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt != nil {
		t.Fatalf("an unread cell carries a confirmation: %+v", entry)
	}
}

// TestSlotZeroConfirmationIsUnchanged asserts the scalar path still stamps
// slot 0's replaced instance with this tick's clock.
func TestSlotZeroConfirmationIsUnchanged(t *testing.T) {
	in, team, status, seen := rolledMemberInputs(t)
	rolledZero := appliedCell("cell-0", team.Name)
	rolledZero.Instance = "uid-cell-0-new"
	in.Cells[0] = rolledZero

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor((*seen)[0].DeadInstances, "uid-cell-0")
	if entry == nil {
		t.Fatalf("no dead instance for slot 0's replaced pod: %+v", (*seen)[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("slot 0's confirmation changed: %+v", entry)
	}
}

// TestALiveMemberReportsNoDeadInstance asserts an unchanged member reports
// nothing, so a steady tick never confirms a live pod gone.
func TestALiveMemberReportsNoDeadInstance(t *testing.T) {
	in, team, status, seen := rolledMemberInputs(t)
	// cell-2 keeps the instance the cache names.
	steady := appliedCell("cell-2", team.Name)
	steady.Instance = "2918db60"
	in.Cells[1] = steady

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(*seen) == 0 {
		return
	}
	if entry := deadFor((*seen)[0].DeadInstances, "2918db60"); entry != nil {
		t.Fatalf("a live member was reported dead: %+v", entry)
	}
}

// TestADepartedSlotZeroCellIsConfirmedGone asserts the scalar path applies
// the same rule: slot 0's own cell, gone from the candidate set, carries a
// confirmation when the caller's pod read answers no pod. The evidence and
// the consequence are the members' own.
func TestADepartedSlotZeroCellIsConfirmedGone(t *testing.T) {
	in, team, status, seen := rolledMemberInputs(t)
	// cell-0 leaves the candidate set, and a fresh cell takes slot 0.
	in.Cells = []CellCandidate{appliedCell("cell-2", team.Name), readyCell("cell-new")}
	in.Cells[0].Instance = "2918db60"
	in.DepartedPods = map[string]bool{"cells-ns/cell-0": true}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor((*seen)[0].DeadInstances, "uid-cell-0")
	if entry == nil {
		t.Fatalf("no dead instance for the departed slot 0 cell: %+v", (*seen)[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("the departed slot 0 cell carries no confirmation: %+v", entry)
	}
}

// TestADepartedSlotZeroCellWithNoReadingCarriesNoConfirmation asserts the
// scalar path keeps the five-minute moved window when the caller confirms
// nothing.
func TestADepartedSlotZeroCellWithNoReadingCarriesNoConfirmation(t *testing.T) {
	in, team, status, seen := rolledMemberInputs(t)
	in.Cells = []CellCandidate{appliedCell("cell-2", team.Name), readyCell("cell-new")}
	in.Cells[0].Instance = "2918db60"
	in.DepartedPods = nil

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor((*seen)[0].DeadInstances, "uid-cell-0")
	if entry == nil {
		t.Fatalf("no dead instance for the departed slot 0 cell: %+v", (*seen)[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt != nil {
		t.Fatalf("an unread slot 0 cell carries a confirmation: %+v", entry)
	}
}
