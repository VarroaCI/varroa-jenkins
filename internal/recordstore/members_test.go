package recordstore

import (
	"testing"
	"time"
)

func slotPtr(n int32) *int32 { return &n }

// twoSlotRow builds a decoded row with slot 0 on cell-a, slot 1 on
// cell-b, and a standby on cell-c.
func twoSlotRow() placementMembers {
	applied := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	return placementMembers{
		members: []member{
			{Cell: "cell-a", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(0),
				Instance: "uid-a", RehydratedInstance: "uid-a", AppliedAt: &applied},
			{Cell: "cell-b", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(1),
				Instance: "uid-b", RehydratedInstance: "uid-b", AppliedAt: &applied},
			{Cell: "cell-c", Namespace: "cells", Role: MemberRoleStandby,
				Instance: "uid-c", RehydratedInstance: "uid-c", AppliedAt: &applied},
		},
		jobHomes: map[string]int32{"acme/api": 1, "acme/web": 0},
	}
}

// TestDecodePlacementMembersSyntheticSlotZero asserts an empty members
// list answers one active member built from the row's scalar columns, so
// a controller with no cell role reads as it read before slots existed.
func TestDecodePlacementMembersSyntheticSlotZero(t *testing.T) {
	row, err := decodePlacementMembers([]byte(`[]`), []byte(`{}`), placementScalars{
		cell: "solo", namespace: "ns", cellInstance: "uid-1",
		rehydratedInstance: "uid-1", pendingJobs: []string{"ns/solo/job"},
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(row.members) != 1 {
		t.Fatalf("member count: want 1, got %d", len(row.members))
	}
	m := row.members[0]
	if m.Cell != "solo" || m.Namespace != "ns" || m.Role != MemberRoleActive {
		t.Fatalf("synthetic member: %+v", m)
	}
	if m.Slot == nil || *m.Slot != 0 {
		t.Fatalf("synthetic member slot: %+v", m.Slot)
	}
	if m.Instance != "uid-1" || m.RehydratedInstance != "uid-1" {
		t.Fatalf("synthetic member instances: %+v", m)
	}
	if got := memberForJob(row, "any/job"); got.Cell != "solo" {
		t.Fatalf("memberForJob on a synthetic row: %+v", got)
	}
}

// TestDecodePlacementMembersNullColumns asserts a nil raw column decodes
// to the same synthetic answer an empty list gives.
func TestDecodePlacementMembersNullColumns(t *testing.T) {
	row, err := decodePlacementMembers(nil, nil, placementScalars{cell: "solo", namespace: "ns"})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(row.members) != 1 || row.members[0].Cell != "solo" {
		t.Fatalf("members: %+v", row.members)
	}
	if row.jobHomes == nil {
		t.Fatal("job homes map is nil, so a lookup would panic on a write")
	}
}

// TestMemberForJobUsesTheHomeMap asserts a job resolves to its home slot's
// holder, and an unseen job resolves to its computed hash home.
func TestMemberForJobUsesTheHomeMap(t *testing.T) {
	row := twoSlotRow()
	if got := memberForJob(row, "acme/api"); got.Cell != "cell-b" {
		t.Fatalf("acme/api home: want cell-b, got %q", got.Cell)
	}
	if got := memberForJob(row, "acme/web"); got.Cell != "cell-a" {
		t.Fatalf("acme/web home: want cell-a, got %q", got.Cell)
	}
	// The jump hash homes acme/unseen on slot 1 of two.
	if got := memberForJob(row, "acme/unseen"); got.Cell != "cell-b" {
		t.Fatalf("an unseen job: want its hash home cell-b, got %q", got.Cell)
	}
}

// TestMemberForJob_MissingEntryUsesTheComputedHome asserts a job with no
// stored entry resolves to the slot the next recompute stores, so every
// reader reads one home from the moment the job exists.
func TestMemberForJob_MissingEntryUsesTheComputedHome(t *testing.T) {
	row := twoSlotRow()
	row.jobHomes = map[string]int32{}
	const jobPath = "smoke-b/xt-missing-artifact"

	want := computeJobHomes([]string{jobPath}, row.members)[jobPath]
	if want != 1 {
		t.Fatalf("fixture: the jump hash homes %s on slot %d, want 1", jobPath, want)
	}
	if got := homeSlotForJob(row, jobPath); got != want {
		t.Fatalf("homeSlotForJob = %d, want the recompute's %d", got, want)
	}
	if got := memberForJob(row, jobPath); got.Cell != "cell-b" {
		t.Fatalf("memberForJob = %q, want cell-b", got.Cell)
	}
}

// TestMemberForJob_StoredEntryWins asserts a stored entry wins over the
// computed hash home.
func TestMemberForJob_StoredEntryWins(t *testing.T) {
	row := twoSlotRow()
	// The jump hash homes acme/api on slot 0; the stored entry names 1.
	if jobHomeSlot("acme/api", 2) != 0 {
		t.Fatal("fixture: acme/api must hash to slot 0")
	}
	if got := memberForJob(row, "acme/api"); got.Cell != "cell-b" {
		t.Fatalf("memberForJob = %q, want the stored slot's cell-b", got.Cell)
	}
}

// TestMemberForJob_OneSlotAnswersSlotZero asserts a tenant with one
// active slot homes every job, known or not, on slot 0.
func TestMemberForJob_OneSlotAnswersSlotZero(t *testing.T) {
	row := placementMembers{
		members:  []member{{Cell: "solo", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(0)}},
		jobHomes: map[string]int32{},
	}
	for _, path := range []string{"acme/unseen", "smoke-b/xt-missing-artifact"} {
		if got := homeSlotForJob(row, path); got != 0 {
			t.Fatalf("%s: slot = %d, want 0", path, got)
		}
	}
}

// TestMemberForJob_EmptyPathAnswersSlotZero asserts a tenant-wide call,
// which carries no job path, resolves to slot 0.
func TestMemberForJob_EmptyPathAnswersSlotZero(t *testing.T) {
	row := twoSlotRow()
	if got := memberForJob(row, ""); got.Cell != "cell-a" {
		t.Fatalf("empty path: want slot 0's cell-a, got %q", got.Cell)
	}
}

// TestMemberForJobNeverAnswersAStandby asserts a tenant with one active
// slot and one standby resolves every job to the active member.
func TestMemberForJobNeverAnswersAStandby(t *testing.T) {
	row := placementMembers{
		members: []member{
			{Cell: "cell-a", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(0), Instance: "uid-a"},
			{Cell: "cell-c", Namespace: "cells", Role: MemberRoleStandby, Instance: "uid-c"},
		},
		jobHomes: map[string]int32{},
	}
	for _, path := range []string{"acme/api", "acme/web", ""} {
		if got := memberForJob(row, path); got.Cell != "cell-a" {
			t.Fatalf("%q home: want cell-a, got %q", path, got.Cell)
		}
	}
}

// TestMemberForJobFailsClosedOnAnUnboundSlot asserts a job homed on a slot
// that holds no cell answers a zero member, so every comparison fails.
func TestMemberForJobFailsClosedOnAnUnboundSlot(t *testing.T) {
	row := placementMembers{
		members:  []member{{Cell: "cell-a", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(0), Instance: "uid-a"}},
		jobHomes: map[string]int32{"acme/api": 1},
	}
	got := memberForJob(row, "acme/api")
	if got.Cell != "" || got.Instance != "" {
		t.Fatalf("an unbound slot: want a zero member, got %+v", got)
	}
}

// TestMemberForInstance asserts the hit, the miss, and the empty-instance
// case, which never matches.
func TestMemberForInstance(t *testing.T) {
	row := twoSlotRow()
	m, ok := memberForInstance(row, "uid-b")
	if !ok || m.Cell != "cell-b" {
		t.Fatalf("uid-b: ok=%v member=%+v", ok, m)
	}
	if _, ok := memberForInstance(row, "uid-gone"); ok {
		t.Fatal("an unknown instance resolved to a member")
	}
	if _, ok := memberForInstance(row, ""); ok {
		t.Fatal("an empty instance resolved to a member")
	}
}

// TestMemberForCell asserts the cell key is exact: the namespace must
// match as well, and an empty cell never matches.
func TestMemberForCell(t *testing.T) {
	row := twoSlotRow()
	m, ok := memberForCell(row, "cells", "cell-b")
	if !ok || m.Slot == nil || *m.Slot != 1 {
		t.Fatalf("cells/cell-b: ok=%v member=%+v", ok, m)
	}
	if _, ok := memberForCell(row, "other", "cell-b"); ok {
		t.Fatal("a cell in another namespace resolved to a member")
	}
	if _, ok := memberForCell(row, "cells", "cell-z"); ok {
		t.Fatal("an unknown cell resolved to a member")
	}
	if _, ok := memberForCell(row, "cells", ""); ok {
		t.Fatal("an empty cell resolved to a member")
	}
	if m, ok := memberForCell(row, "cells", "cell-c"); !ok || m.Role != MemberRoleStandby {
		t.Fatalf("the standby cell: ok=%v member=%+v", ok, m)
	}
}

// TestHasMemberInNamespace asserts the fence's namespace order sees a
// member on any slot.
func TestHasMemberInNamespace(t *testing.T) {
	row := twoSlotRow()
	if !hasMemberInNamespace(row, "cells") {
		t.Fatal("the member namespace answered no member")
	}
	if hasMemberInNamespace(row, "cells-old") {
		t.Fatal("a namespace the tenant left answered a member")
	}
}

// TestActiveSlotCount asserts the bucket count follows the highest active
// slot, and that a list with no active member answers one.
func TestActiveSlotCount(t *testing.T) {
	if got := activeSlotCount(twoSlotRow().members); got != 2 {
		t.Fatalf("two active slots answered %d", got)
	}
	standbyOnly := []member{{Cell: "cell-c", Role: MemberRoleStandby}}
	if got := activeSlotCount(standbyOnly); got != 1 {
		t.Fatalf("a standby-only list answered %d, want 1", got)
	}
	if got := activeSlotCount(nil); got != 1 {
		t.Fatalf("an empty list answered %d, want 1", got)
	}
}
