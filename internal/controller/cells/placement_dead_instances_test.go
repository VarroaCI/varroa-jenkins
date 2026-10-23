package cells

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// rolledCell is appliedCell whose status records a replaced pod: instance
// "uid-<name>" is the running pod, and previous is the pod it replaced.
func rolledCell(name, previous string, lastSeen *time.Time, tenants ...string) CellCandidate {
	c := appliedCell(name, tenants...)
	c.PreviousInstance = previous
	c.PreviousInstanceLastSeen = lastSeen
	return c
}

// countDead answers how many entries name instance.
func countDead(dead []DeadInstance, instance string) int {
	n := 0
	for _, d := range dead {
		if d.Instance == instance {
			n++
		}
	}
	return n
}

// TestReannounceWithNoCacheReportsTheCellPreviousInstance asserts an
// operator with no record of the last announcement still reports the
// replaced pod its cell's status records.
//
// Without the entry the recorder never marks a run of the replaced pod,
// and the run stays RUNNING with no end.
func TestReannounceWithNoCacheReportsTheCellPreviousInstance(t *testing.T) {
	in := testInputs()
	tenant := testTenant()
	lastSeen := fixedNow.Add(-3 * time.Minute)
	in.Cells = []CellCandidate{rolledCell("cell-a", "uid-old", &lastSeen, tenant.Name)}
	status := placedStatus(1)
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	if seen[0].Cell != "cell-a" || seen[0].Generation != 1 {
		t.Fatalf("the announce left the placement: %+v", seen[0])
	}
	entry := deadFor(seen[0].DeadInstances, "uid-old")
	if entry == nil {
		t.Fatalf("no dead instance for the replaced pod: %+v", seen[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("ConfirmedGoneAt = %v, want this tick's clock %v", entry.ConfirmedGoneAt, fixedNow)
	}
	if !entry.LastSeenAt.Equal(lastSeen) {
		t.Fatalf("LastSeenAt = %v, want the recorded reading %v", entry.LastSeenAt, lastSeen)
	}
}

// TestReannounceWithNoCacheReportsEachMemberPreviousInstance asserts the
// rule holds for every member cell, active or standby, not slot 0 alone.
func TestReannounceWithNoCacheReportsEachMemberPreviousInstance(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 1)
	in.Cells = []CellCandidate{
		rolledCell("cell-0", "old-cell-0", nil, tenant.Name),
		rolledCell("cell-1", "old-cell-1", nil, tenant.Name),
		rolledCell("cell-2", "old-cell-2", nil, tenant.Name),
	}
	status := placedWithMembers([]v1alpha1.TenantMemberStatus{
		memberStatus("cell-0", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-1", v1alpha1.TenantMemberRoleActive, intPtr(1)),
		memberStatus("cell-2", v1alpha1.TenantMemberRoleStandby, nil),
	})
	status.Placement.Cell = "cell-0"
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	for _, old := range []string{"old-cell-0", "old-cell-1", "old-cell-2"} {
		entry := deadFor(seen[0].DeadInstances, old)
		if entry == nil {
			t.Fatalf("no dead instance for %s: %+v", old, seen[0].DeadInstances)
		}
		if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
			t.Fatalf("%s carries no confirmation: %+v", old, entry)
		}
	}
}

// TestWarmCacheAndPreviousInstanceReportOneEntry asserts an instance that
// the cache delta and the cell record both name appears once.
func TestWarmCacheAndPreviousInstanceReportOneEntry(t *testing.T) {
	in := testInputs()
	tenant := testTenant()
	in.Cells = []CellCandidate{rolledCell("cell-a", "uid-old", nil, tenant.Name)}
	status := placedStatus(1)
	in.LastSync = &SyncCache{
		Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1, CellInstance: "uid-old",
		Policy: SyncPolicyFromPipelines(tenant.Spec.Pipelines), At: fixedNow.Add(-time.Minute),
	}
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	if n := countDead(seen[0].DeadInstances, "uid-old"); n != 1 {
		t.Fatalf("entries for uid-old = %d, want 1: %+v", n, seen[0].DeadInstances)
	}
}

// TestPreviousInstanceIsNotReportedWhenNoPodWasReplaced asserts an empty
// record, or a record that names the running pod, reports nothing. A live
// pod must never be reported dead.
func TestPreviousInstanceIsNotReportedWhenNoPodWasReplaced(t *testing.T) {
	cases := []struct {
		name     string
		previous string
	}{
		{name: "an empty previous instance", previous: ""},
		{name: "a previous instance equal to the current one", previous: "uid-cell-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := testInputs()
			tenant := testTenant()
			in.Cells = []CellCandidate{rolledCell("cell-a", tc.previous, nil, tenant.Name)}
			status := placedStatus(1)
			var seen []AnnounceRequest
			in.Announce = recordingAnnounce(true, &seen)

			if _, err := Place(context.Background(), in, tenant, status); err != nil {
				t.Fatalf("Place: %v", err)
			}
			if len(seen) != 1 {
				t.Fatalf("announcements = %d, want 1", len(seen))
			}
			if len(seen[0].DeadInstances) != 0 {
				t.Fatalf("dead instances = %+v, want none", seen[0].DeadInstances)
			}
		})
	}
}

// TestPreviousInstanceWithNoLastSeenUsesTheTickClock asserts a record with
// no heartbeat reading falls back to this tick's clock. A zero time would
// read as older than any window.
func TestPreviousInstanceWithNoLastSeenUsesTheTickClock(t *testing.T) {
	in := testInputs()
	tenant := testTenant()
	in.Cells = []CellCandidate{rolledCell("cell-a", "uid-old", nil, tenant.Name)}
	status := placedStatus(1)
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor(seen[0].DeadInstances, "uid-old")
	if entry == nil {
		t.Fatalf("no dead instance for the replaced pod: %+v", seen[0].DeadInstances)
	}
	if !entry.LastSeenAt.Equal(fixedNow) {
		t.Fatalf("LastSeenAt = %v, want the tick clock %v", entry.LastSeenAt, fixedNow)
	}
}

// TestMoveWithNoCacheReportsTheOldCellPreviousInstance asserts a tenant
// that leaves a live cell still reports the pod that cell replaced.
func TestMoveWithNoCacheReportsTheOldCellPreviousInstance(t *testing.T) {
	in := testInputs()
	tenant := testTenant()
	evacuating := rolledCell("cell-a", "old-a", nil, tenant.Name)
	evacuating.Evacuate = true
	in.Cells = []CellCandidate{evacuating, readyCell("cell-b")}
	status := placedStatus(1)
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	if seen[0].Cell != "cell-b" {
		t.Fatalf("the announce names %q, want the new cell cell-b", seen[0].Cell)
	}
	entry := deadFor(seen[0].DeadInstances, "old-a")
	if entry == nil {
		t.Fatalf("no dead instance for the old cell's replaced pod: %+v", seen[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("the old cell's replaced pod carries no confirmation: %+v", entry)
	}
	if deadFor(seen[0].DeadInstances, "uid-cell-a") != nil {
		t.Fatalf("the old cell's running pod was reported dead: %+v", seen[0].DeadInstances)
	}
}

// TestPreviousInstanceConfirmsAnUnconfirmedCacheEntry asserts the cell
// record stamps a confirmation on an entry the cache producer reports
// without one. A member that leaves a live cell whose pod rolled is one
// such case. Without the stamp an unstarted intent of that pod never
// re-fires.
func TestPreviousInstanceConfirmsAnUnconfirmedCacheEntry(t *testing.T) {
	in := testInputs()
	tenant := slotTenant(2, 0)
	recordedSeen := fixedNow.Add(-10 * time.Second)
	evacuating := rolledCell("cell-b", "old-b", &recordedSeen, tenant.Name)
	evacuating.Evacuate = true
	in.Cells = []CellCandidate{appliedCell("cell-a", tenant.Name), evacuating, readyCell("cell-c")}
	status := placedStatus(3)
	status.Placement.Members = []v1alpha1.TenantMemberStatus{
		memberStatus("cell-a", v1alpha1.TenantMemberRoleActive, intPtr(0)),
		memberStatus("cell-b", v1alpha1.TenantMemberRoleActive, intPtr(1)),
	}
	in.LastSync = &SyncCache{
		Cell: "cell-a", CellNamespace: "cells-ns", Generation: 3, CellInstance: "uid-cell-a",
		Policy: SyncPolicyFromPipelines(tenant.Spec.Pipelines), At: fixedNow.Add(-time.Minute),
		Members: []AnnounceMember{
			{Cell: "cell-a", Namespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive,
				Slot: intPtr(0), Instance: "uid-cell-a"},
			{Cell: "cell-b", Namespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive,
				Slot: intPtr(1), Instance: "old-b"},
		},
	}
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	if n := countDead(seen[0].DeadInstances, "old-b"); n != 1 {
		t.Fatalf("entries for old-b = %d, want 1: %+v", n, seen[0].DeadInstances)
	}
	entry := deadFor(seen[0].DeadInstances, "old-b")
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("the rolled pod of the left cell carries no confirmation: %+v", entry)
	}
	if !entry.LastSeenAt.Equal(recordedSeen) {
		t.Fatalf("LastSeenAt = %v, want the newer recorded reading %v", entry.LastSeenAt, recordedSeen)
	}
}

// TestPreviousInstanceKeepsANewerCacheLastSeen asserts an older recorded
// heartbeat never moves the cache entry's LastSeenAt back.
func TestPreviousInstanceKeepsANewerCacheLastSeen(t *testing.T) {
	in := testInputs()
	tenant := testTenant()
	recordedSeen := fixedNow.Add(-10 * time.Minute)
	in.Cells = []CellCandidate{rolledCell("cell-a", "uid-old", &recordedSeen, tenant.Name)}
	status := placedStatus(1)
	cacheSeen := fixedNow.Add(-30 * time.Second)
	in.LastSync = &SyncCache{
		Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1, CellInstance: "uid-old",
		Policy: SyncPolicyFromPipelines(tenant.Spec.Pipelines), At: fixedNow.Add(-time.Minute),
		CellLastSeenAt: &cacheSeen,
	}
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	entry := deadFor(seen[0].DeadInstances, "uid-old")
	if entry == nil || !entry.LastSeenAt.Equal(cacheSeen) {
		t.Fatalf("entry = %+v, want LastSeenAt %v", entry, cacheSeen)
	}
}

// TestPreviousInstanceAddsNoAnnounce asserts the record is not a trigger:
// a warm cache with nothing due sends no announcement.
func TestPreviousInstanceAddsNoAnnounce(t *testing.T) {
	in := testInputs()
	tenant := testTenant()
	in.Cells = []CellCandidate{rolledCell("cell-a", "uid-old", nil, tenant.Name)}
	status := placedStatus(1)
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 || out.SyncCacheUpdate == nil {
		t.Fatalf("first pass: announcements = %d, cache update = %v", len(seen), out.SyncCacheUpdate)
	}

	in.LastSync = out.SyncCacheUpdate
	seen = nil
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("announcements = %d, want 0 with nothing due", len(seen))
	}
}
