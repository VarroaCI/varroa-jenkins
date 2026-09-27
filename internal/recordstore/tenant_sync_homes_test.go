package recordstore

import "testing"

// TestTenantSyncHomesAnswersTheMemberListAndTheMap asserts the "homes"
// kind answers what the announce stored, so the operator computes no home
// of its own.
func TestTenantSyncHomesAnswersTheMemberListAndTheMap(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	seedJobHomePaths(t, db, "acme/api", "acme/web")

	sendTenantSync(t, pool, conn, announceMembers(2, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b"),
		{Cell: "cell-c", Namespace: "cells", Role: MemberRoleStandby, Instance: "uid-c"},
	}))

	resp := sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindHomes, Cluster: testCluster, Tenant: testTenant,
	})
	if resp.Error != "" || !resp.Ok {
		t.Fatalf("homes: %+v", resp)
	}
	if len(resp.Members) != 3 {
		t.Fatalf("member count: want 3, got %d: %+v", len(resp.Members), resp.Members)
	}
	if len(resp.JobHomes) != 2 {
		t.Fatalf("home map: want 2 entries, got %v", resp.JobHomes)
	}
	stored := readStoredJobHomes(t, db)
	if !jobHomesEqual(stored, resp.JobHomes) {
		t.Fatalf("the reply differs from the stored map: %v and %v", resp.JobHomes, stored)
	}
}

// TestTenantSyncHomesOnAnUnannouncedTenant asserts a tenant with no row
// answers an empty list and an empty map, never an error.
func TestTenantSyncHomesOnAnUnannouncedTenant(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	resp := sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindHomes, Cluster: testCluster, Tenant: "never-announced",
	})
	if resp.Error != "" || !resp.Ok {
		t.Fatalf("homes: %+v", resp)
	}
	if len(resp.Members) != 0 {
		t.Fatalf("an unannounced tenant answered %d members", len(resp.Members))
	}
	if len(resp.JobHomes) != 0 {
		t.Fatalf("an unannounced tenant answered %v", resp.JobHomes)
	}
}

// TestTenantSyncHomesOnASingleMemberTenant asserts a tenant the operator
// announced with no member list answers the synthetic slot-0 member, so
// the caller needs no branch.
func TestTenantSyncHomesOnASingleMemberTenant(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: "cells", Cell: "cell-solo", Generation: 1, CellInstance: "uid-solo",
	})

	resp := sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindHomes, Cluster: testCluster, Tenant: testTenant,
	})
	if len(resp.Members) != 1 {
		t.Fatalf("member count: want 1, got %d: %+v", len(resp.Members), resp.Members)
	}
	m := resp.Members[0]
	if m.Cell != "cell-solo" || m.Role != MemberRoleActive || m.Slot == nil || *m.Slot != 0 {
		t.Fatalf("the synthetic member: %+v", m)
	}
}

// TestTenantSyncHomesNeedsAClusterAndATenant asserts a request with no
// tenant answers an error and reads nothing.
func TestTenantSyncHomesNeedsAClusterAndATenant(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	resp := sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindHomes, Cluster: testCluster,
	})
	if resp.Error == "" {
		t.Fatal("want an error for a homes request with no tenant")
	}
}

// TestAnnounceScaleMovesTheRehomedScheduleRows asserts an announce that
// grows the tenant from one slot to two moves each rehomed job's schedule
// row to the new slot's cell at the new generation, and leaves each other
// row as it is.
func TestAnnounceScaleMovesTheRehomedScheduleRows(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	moved := pathOnSlot(t, 1)
	stays := pathOnSlot(t, 0)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{activeMember("cell-a", 0, "uid-a")}))
	seedScheduleRowAt(t, db, moved, 1)
	seedScheduleRowAt(t, db, stays, 1)

	resp := sendTenantSync(t, pool, conn, announceMembers(2, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b"),
	}))
	if resp.Error != "" || !resp.Ok {
		t.Fatalf("announce: %+v", resp)
	}

	got, ok := readScheduleRowAddress(t, db, moved)
	if !ok {
		t.Fatal("the announce deleted the rehomed job's row")
	}
	if want := (scheduleRowAddress{namespace: "cells", cell: "cell-b", generation: 2}); got != want {
		t.Fatalf("rehomed row = %+v, want %+v", got, want)
	}
	got, _ = readScheduleRowAddress(t, db, stays)
	if want := (scheduleRowAddress{namespace: "cells", cell: "cell-a", generation: 1}); got != want {
		t.Fatalf("unmoved row = %+v, want %+v", got, want)
	}
}
