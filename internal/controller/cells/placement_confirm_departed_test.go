package cells

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// confirmedDeparted answers one confirmed departed instance on a cell the
// tenant left, confirmed at this tick's clock.
func confirmedDeparted(instance string) DeadInstance {
	confirmed := fixedNow
	return DeadInstance{
		Instance: instance, LastSeenAt: fixedNow.Add(-20 * time.Minute),
		ConfirmedGoneAt: &confirmed, Cell: "cell-gone", Namespace: "cells-ns",
	}
}

// warmInputs places testTenant on cell-a and answers inputs whose cache holds
// the first pass's announcement, so nothing else is due.
func warmInputs(t *testing.T) (Inputs, *v1alpha1.Tenant, *v1alpha1.TenantStatus) {
	t.Helper()
	in := testInputs()
	tenant := testTenant()
	in.Cells = []CellCandidate{appliedCell("cell-a", tenant.Name)}
	status := placedStatus(1)
	out, err := Place(context.Background(), in, tenant, status)
	if err != nil || out.SyncCacheUpdate == nil {
		t.Fatalf("first pass: err = %v, cache update = %v", err, out.SyncCacheUpdate)
	}
	in.LastSync = out.SyncCacheUpdate
	return in, tenant, status
}

// TestConfirmedDepartedMakesAnAnnounceDue asserts a confirmed departed
// instance is a trigger, and that the announcement carries it with its
// confirmation and its cell.
func TestConfirmedDepartedMakesAnAnnounceDue(t *testing.T) {
	in, tenant, status := warmInputs(t)
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("announcements = %d, want 0 with nothing due", len(seen))
	}

	in.ConfirmedDeparted = []DeadInstance{confirmedDeparted("uid-left")}
	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("announcements = %d, want 1", len(seen))
	}
	entry := deadFor(seen[0].DeadInstances, "uid-left")
	if entry == nil {
		t.Fatalf("no entry for the confirmed instance: %+v", seen[0].DeadInstances)
	}
	if entry.ConfirmedGoneAt == nil || !entry.ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("ConfirmedGoneAt = %v, want %v", entry.ConfirmedGoneAt, fixedNow)
	}
	if entry.Cell != "cell-gone" || entry.Namespace != "cells-ns" {
		t.Fatalf("cell = %s/%s, want cells-ns/cell-gone", entry.Namespace, entry.Cell)
	}
	if seen[0].Generation != 1 || seen[0].Cell != "cell-a" {
		t.Fatalf("the confirmation changed the placement: %+v", seen[0])
	}
}

// TestConfirmedDepartedNeverNamesALiveInstance asserts a confirmation for
// an instance an announced member runs is dropped. The recorder would
// otherwise refuse every later announcement as stale.
func TestConfirmedDepartedNeverNamesALiveInstance(t *testing.T) {
	in, tenant, status := warmInputs(t)
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)
	in.ConfirmedDeparted = []DeadInstance{confirmedDeparted("uid-cell-a")}

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	for _, req := range seen {
		if deadFor(req.DeadInstances, "uid-cell-a") != nil {
			t.Fatalf("the live instance was reported dead: %+v", req.DeadInstances)
		}
	}
}

// TestPlaceCachesTheUnconfirmedReply asserts the reply's unconfirmed list
// reaches the cache, so the caller reads those pods on the next tick.
func TestPlaceCachesTheUnconfirmedReply(t *testing.T) {
	in := testInputs()
	tenant := testTenant()
	in.Cells = []CellCandidate{appliedCell("cell-a", tenant.Name)}
	status := placedStatus(1)
	unconfirmed := []DeadInstance{{Instance: "uid-left", Cell: "cell-gone", Namespace: "cells-ns"}}
	in.Announce = func(context.Context, AnnounceRequest) (AnnounceResult, error) {
		return AnnounceResult{OK: true, Unconfirmed: unconfirmed}, nil
	}

	out, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if out.SyncCacheUpdate == nil || len(out.SyncCacheUpdate.Unconfirmed) != 1 ||
		out.SyncCacheUpdate.Unconfirmed[0].Instance != "uid-left" {
		t.Fatalf("cache update = %+v, want the unconfirmed reply", out.SyncCacheUpdate)
	}
}

// TestMoveCarriesConfirmedDeparted asserts the move path carries a
// confirmation too, so a move tick never drops one.
func TestMoveCarriesConfirmedDeparted(t *testing.T) {
	in := testInputs()
	tenant := testTenant()
	evacuating := appliedCell("cell-a", tenant.Name)
	evacuating.Evacuate = true
	in.Cells = []CellCandidate{evacuating, readyCell("cell-b")}
	in.ConfirmedDeparted = []DeadInstance{confirmedDeparted("uid-left")}
	status := placedStatus(1)
	var seen []AnnounceRequest
	in.Announce = recordingAnnounce(true, &seen)

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("Place: %v", err)
	}
	if len(seen) != 1 || seen[0].Cell != "cell-b" {
		t.Fatalf("announcements = %+v, want one move to cell-b", seen)
	}
	entry := deadFor(seen[0].DeadInstances, "uid-left")
	if entry == nil || entry.ConfirmedGoneAt == nil {
		t.Fatalf("the move dropped the confirmation: %+v", seen[0].DeadInstances)
	}
}

// TestAppendConfirmedDepartedConfirmsAnExistingEntry asserts a confirmation
// for an instance another producer already reports adds no second entry
// and stamps the existing one.
func TestAppendConfirmedDepartedConfirmsAnExistingEntry(t *testing.T) {
	dead := []DeadInstance{{Instance: "uid-left", LastSeenAt: fixedNow.Add(-time.Hour)}}
	got := appendConfirmedDeparted(dead, []DeadInstance{confirmedDeparted("uid-left")}, nil)
	if len(got) != 1 {
		t.Fatalf("entries = %+v, want one", got)
	}
	if got[0].ConfirmedGoneAt == nil || !got[0].ConfirmedGoneAt.Equal(fixedNow) {
		t.Fatalf("ConfirmedGoneAt = %v, want %v", got[0].ConfirmedGoneAt, fixedNow)
	}
	if got[0].Cell != "cell-gone" {
		t.Fatalf("cell = %q, want cell-gone", got[0].Cell)
	}

	earlier := fixedNow.Add(-time.Minute)
	dead = []DeadInstance{{Instance: "uid-left", ConfirmedGoneAt: &earlier, Cell: "cell-x"}}
	got = appendConfirmedDeparted(dead, []DeadInstance{confirmedDeparted("uid-left")}, nil)
	if !got[0].ConfirmedGoneAt.Equal(earlier) || got[0].Cell != "cell-x" {
		t.Fatalf("entry = %+v, want the earlier confirmation and cell kept", got[0])
	}
}

// TestMemberDeadInstancesNamesTheCell asserts an entry for a member that
// left names that member's cell, so the recorder can hand it back.
func TestMemberDeadInstancesNamesTheCell(t *testing.T) {
	last := &SyncCache{At: fixedNow, Members: []AnnounceMember{
		{Cell: "cell-b", Namespace: "cells-ns", Instance: "uid-b"},
	}}
	got := memberDeadInstances(last, nil, nil, nil, fixedNow)
	if len(got) != 1 || got[0].Cell != "cell-b" || got[0].Namespace != "cells-ns" || got[0].ConfirmedGoneAt != nil {
		t.Fatalf("entries = %+v, want one unconfirmed entry on cells-ns/cell-b", got)
	}
}
