package recordstore

import (
	"testing"
	"time"
)

// deriveCase builds a derivation over prior and announced, with no stored
// entry and the request instance taken from slot 0 of announced.
func deriveCase(prior, announced []member) deadInstanceDerivation {
	in := deadInstanceDerivation{prior: prior, announced: announced, now: time.Now()}
	if len(announced) > 0 {
		in.requestInstance = announced[0].Instance
	}
	return in
}

func TestDeriveDeadInstancesAddsARolledMemberInstance(t *testing.T) {
	in := deriveCase(
		[]member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1")},
		[]member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b2")},
	)
	got := deriveDeadInstances(in)
	if len(got) != 1 || got[0].Instance != "pod-b1" || got[0].Derived != DeadInstanceDerivedRolled {
		t.Fatalf("want one rolled entry for pod-b1, got %+v", got)
	}
	if got[0].ConfirmedGoneAt != nil {
		t.Fatalf("a derived entry carries no confirmation, got %v", got[0].ConfirmedGoneAt)
	}
	if !got[0].LastSeenAt.Equal(in.now) {
		t.Fatalf("last_seen_at = %v, want the derivation time %v", got[0].LastSeenAt, in.now)
	}
}

func TestDeriveDeadInstancesAddsADroppedMemberInstance(t *testing.T) {
	got := deriveDeadInstances(deriveCase(
		[]member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1")},
		[]member{activeMember("cell-a", 0, "pod-a1")},
	))
	if len(got) != 1 || got[0].Instance != "pod-b1" || got[0].Derived != DeadInstanceDerivedDropped {
		t.Fatalf("want one dropped entry for pod-b1, got %+v", got)
	}
}

func TestDeriveDeadInstancesTreatsAnEmptyInstanceAsARoll(t *testing.T) {
	got := deriveDeadInstances(deriveCase(
		[]member{activeMember("cell-a", 0, "pod-a1")},
		[]member{activeMember("cell-a", 0, "")},
	))
	if len(got) != 1 || got[0].Instance != "pod-a1" || got[0].Derived != DeadInstanceDerivedRolled {
		t.Fatalf("want one rolled entry for pod-a1, got %+v", got)
	}
}

func TestDeriveDeadInstancesSkipsAnInstanceAlreadyListed(t *testing.T) {
	in := deriveCase(
		[]member{activeMember("cell-a", 0, "pod-a1")},
		[]member{activeMember("cell-a", 0, "pod-a2")},
	)
	in.listed = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: in.now.Add(-time.Hour)}}
	if got := deriveDeadInstances(in); len(got) != 0 {
		t.Fatalf("an instance the operator lists already takes no derived entry, got %+v", got)
	}
}

func TestDeriveDeadInstancesSkipsAnInstanceTheRequestNames(t *testing.T) {
	// The tenant moves its slot 1 member to the cell slot 0 left, and the
	// pod of that cell keeps its identifier.
	got := deriveDeadInstances(deriveCase(
		[]member{activeMember("cell-a", 0, "pod-a1")},
		[]member{activeMember("cell-b", 0, "pod-a1")},
	))
	if len(got) != 0 {
		t.Fatalf("an instance the request names is never dead, got %+v", got)
	}
}

func TestDeriveDeadInstancesStaleRequestDerivesNothing(t *testing.T) {
	// The stored row holds pod-a2, which replaced pod-a1. A stale request
	// names pod-a1 again. Without the guard, pod-a2, the live pod, would
	// be listed dead.
	in := deriveCase(
		[]member{activeMember("cell-a", 0, "pod-a2"), activeMember("cell-b", 1, "pod-b1")},
		[]member{activeMember("cell-a", 0, "pod-a1")},
	)
	in.stale = true
	if got := deriveDeadInstances(in); len(got) != 0 {
		t.Fatalf("a stale request derives nothing, got %+v", got)
	}
}

// staleCase builds a staleAnnounce input whose stored row is at
// generation 3 and holds prior, and whose request names announced at
// generation 3.
func staleCase(prior, announced []member) staleAnnounceInput {
	req := TenantSyncRequest{Generation: 3, Members: announced}
	if len(announced) > 0 {
		req.CellInstance = announced[0].Instance
	}
	return staleAnnounceInput{storedGeneration: 3, prior: prior, req: req}
}

func TestStaleAnnounceLowerGeneration(t *testing.T) {
	in := staleCase([]member{activeMember("cell-b", 0, "pod-b1")}, []member{activeMember("cell-a", 0, "pod-a1")})
	in.req.Generation = 2
	if !staleAnnounce(in) {
		t.Fatal("a lower generation is stale")
	}
}

func TestStaleAnnounceNamesAConfirmedGoneInstance(t *testing.T) {
	now := time.Now()
	in := staleCase([]member{activeMember("cell-a", 0, "pod-a2")}, []member{activeMember("cell-a", 0, "pod-a1")})
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: now, ConfirmedGoneAt: &now}}
	if !staleAnnounce(in) {
		t.Fatal("a request that names a confirmed gone instance is stale")
	}
}

func TestStaleAnnounceNamesARolledInstance(t *testing.T) {
	in := staleCase([]member{activeMember("cell-a", 0, "pod-a2")}, []member{activeMember("cell-a", 0, "pod-a1")})
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: time.Now(), Derived: DeadInstanceDerivedRolled}}
	if !staleAnnounce(in) {
		t.Fatal("a request that names a rolled instance is stale")
	}
}

func TestStaleAnnounceNamesARolledInstanceOnlyAsTheScalar(t *testing.T) {
	in := staleCase([]member{activeMember("cell-a", 0, "pod-a2")}, nil)
	in.req.CellInstance = "pod-a1"
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: time.Now(), Derived: DeadInstanceDerivedRolled}}
	if !staleAnnounce(in) {
		t.Fatal("a request whose cell_instance names a rolled instance is stale")
	}
}

func TestStaleAnnounceIgnoresADroppedEntry(t *testing.T) {
	// The tenant left cell-a and comes back to pod-a1, which stayed live.
	in := staleCase([]member{activeMember("cell-b", 0, "pod-b1")},
		[]member{activeMember("cell-b", 0, "pod-b1"), activeMember("cell-a", 1, "pod-a1")})
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: time.Now(), Derived: DeadInstanceDerivedDropped}}
	if staleAnnounce(in) {
		t.Fatal("a dropped entry proves no replacement")
	}
}

func TestStaleAnnounceIgnoresAnUnconfirmedOperatorEntry(t *testing.T) {
	in := staleCase([]member{activeMember("cell-b", 0, "pod-b1")}, []member{activeMember("cell-a", 0, "pod-a1")})
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: time.Now()}}
	if staleAnnounce(in) {
		t.Fatal("an unconfirmed operator entry proves no replacement")
	}
}

func TestStaleAnnounceIgnoresAnInstanceAStoredMemberHolds(t *testing.T) {
	now := time.Now()
	in := staleCase([]member{activeMember("cell-a", 0, "pod-a1")}, []member{activeMember("cell-a", 0, "pod-a1")})
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: now, ConfirmedGoneAt: &now}}
	if staleAnnounce(in) {
		t.Fatal("a request that agrees with the stored members is not stale")
	}
}

func TestStaleAnnounceTakeOverIsNeverStale(t *testing.T) {
	now := time.Now()
	in := staleCase([]member{activeMember("cell-b", 0, "pod-b1")}, []member{activeMember("cell-a", 0, "pod-a1")})
	in.req.Generation = 1
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: now, ConfirmedGoneAt: &now}}
	in.takeOver = true
	if staleAnnounce(in) {
		t.Fatal("a take-over is never stale")
	}
}

func TestStaleAnnounceAcceptsARequestThatConfirmsAStoredInstanceGone(t *testing.T) {
	// The stored row holds pod-a1 and lists the live pod-a2 as rolled. The
	// request confirms pod-a1 gone, so the stored row is the older one.
	now := time.Now()
	in := staleCase([]member{activeMember("cell-a", 0, "pod-a1")}, []member{activeMember("cell-a", 0, "pod-a2")})
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a2", LastSeenAt: now, Derived: DeadInstanceDerivedRolled}}
	in.req.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: now, ConfirmedGoneAt: &now}}
	if staleAnnounce(in) {
		t.Fatal("a request that confirms a stored instance gone is not stale")
	}
}

func TestStaleAnnounceConfirmationOfAnUnheldInstanceProvesNothing(t *testing.T) {
	// The request confirms pod-a0 gone, but no stored member holds pod-a0,
	// so the confirmation proves nothing about the stored row.
	now := time.Now()
	in := staleCase([]member{activeMember("cell-a", 0, "pod-a2")}, []member{activeMember("cell-a", 0, "pod-a1")})
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: now, ConfirmedGoneAt: &now}}
	in.req.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-a0", LastSeenAt: now, ConfirmedGoneAt: &now}}
	if !staleAnnounce(in) {
		t.Fatal("a confirmation of an instance no stored member holds keeps the request stale")
	}
}

func TestStaleAnnounceUnconfirmedEntryOfAStoredInstanceProvesNothing(t *testing.T) {
	now := time.Now()
	in := staleCase([]member{activeMember("cell-a", 0, "pod-a2")}, []member{activeMember("cell-a", 0, "pod-a1")})
	in.stored = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: now, ConfirmedGoneAt: &now}}
	in.req.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-a2", LastSeenAt: now}}
	if !staleAnnounce(in) {
		t.Fatal("an unconfirmed entry of a stored instance keeps the request stale")
	}
}

func TestStaleAnnounceSameStateIsNotStale(t *testing.T) {
	in := staleCase([]member{activeMember("cell-a", 0, "pod-a1")}, []member{activeMember("cell-a", 0, "pod-a1")})
	if staleAnnounce(in) {
		t.Fatal("a repeated announcement is not stale")
	}
}

func TestDeriveDeadInstancesSlotZeroScalarRowRolls(t *testing.T) {
	scalars := placementScalars{cell: "cell-a", namespace: "cells", cellInstance: "pod-a1"}
	prior := newPlacementMembers(nil, nil, scalars).members
	scalars.cellInstance = "pod-a2"
	announced := newPlacementMembers(nil, nil, scalars).members
	got := deriveDeadInstances(deriveCase(prior, announced))
	if len(got) != 1 || got[0].Instance != "pod-a1" || got[0].Derived != DeadInstanceDerivedRolled {
		t.Fatalf("want one rolled entry for pod-a1, got %+v", got)
	}
}

func TestDeriveDeadInstancesIgnoresAMemberWithNoInstance(t *testing.T) {
	got := deriveDeadInstances(deriveCase(
		[]member{activeMember("cell-a", 0, "")},
		[]member{activeMember("cell-b", 0, "pod-b1")},
	))
	if len(got) != 0 {
		t.Fatalf("a member with no instance derives nothing, got %+v", got)
	}
}

func TestMergeDeadInstancesClearsTheDerivedTagOnAnOperatorEntry(t *testing.T) {
	now := time.Now()
	stored := []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: now, Derived: DeadInstanceDerivedDropped}}
	confirmed := now.Add(-time.Minute)
	incoming := []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: now.Add(-time.Hour), ConfirmedGoneAt: &confirmed}}
	got := mergeDeadInstances(stored, incoming)
	if len(got) != 1 || got[0].Derived != "" {
		t.Fatalf("an operator entry clears the derived tag, got %+v", got)
	}
	if got[0].ConfirmedGoneAt == nil || !got[0].ConfirmedGoneAt.Equal(confirmed) {
		t.Fatalf("the merged entry takes the operator confirmation, got %+v", got[0])
	}
}

func TestFleetLockOwnerConfirmedGoneIgnoresADroppedEntry(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * time.Minute)
	row := tenantPlacementLivenessRow{cellInstance: "pod-live"}

	row.deadInstances = []TenantSyncDeadInstance{{Instance: "pod-left", LastSeenAt: old, Derived: DeadInstanceDerivedDropped}}
	if fleetLockOwnerConfirmedGone(row, "pod-left", now, testGrace) {
		t.Fatal("a dropped entry released a lock")
	}

	row.deadInstances = []TenantSyncDeadInstance{{Instance: "pod-left", LastSeenAt: old, Derived: DeadInstanceDerivedRolled}}
	if !fleetLockOwnerConfirmedGone(row, "pod-left", now, testGrace) {
		t.Fatal("a rolled entry past the moved window released no lock")
	}

	row.deadInstances = []TenantSyncDeadInstance{{Instance: "pod-left", LastSeenAt: old}}
	if !fleetLockOwnerConfirmedGone(row, "pod-left", now, testGrace) {
		t.Fatal("an operator entry past the moved window released no lock")
	}
}

func TestDeriveDeadInstancesDerivesAgainAfterAReturnToALivePod(t *testing.T) {
	// The tenant left cell-a, and pod-a1 stayed listed. The tenant came
	// back to pod-a1, so the stored members hold it again. A later drop
	// of cell-b derives as usual.
	in := deriveCase(
		[]member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1")},
		[]member{activeMember("cell-a", 0, "pod-a1")},
	)
	in.listed = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: in.now.Add(-time.Hour)}}
	got := deriveDeadInstances(in)
	if len(got) != 1 || got[0].Instance != "pod-b1" || got[0].Derived != DeadInstanceDerivedDropped {
		t.Fatalf("want one dropped entry for pod-b1, got %+v", got)
	}
}

func TestTrimDeadInstancesKeepsANewestEntryAppendedLast(t *testing.T) {
	now := time.Now()
	var merged []TenantSyncDeadInstance
	for i := 0; i < maxDeadInstances; i++ {
		merged = append(merged, TenantSyncDeadInstance{
			Instance: "pod-old-" + string(rune('a'+i)), LastSeenAt: now.Add(-time.Duration(i+1) * time.Hour),
		})
	}
	merged = append(merged, TenantSyncDeadInstance{Instance: "pod-derived", LastSeenAt: now, Derived: DeadInstanceDerivedRolled})
	got := trimDeadInstances(merged, nil, maxDeadInstances)
	if len(got) != maxDeadInstances || got[0].Instance != "pod-derived" {
		t.Fatalf("want the newest entry kept first, got %+v", got)
	}
}
