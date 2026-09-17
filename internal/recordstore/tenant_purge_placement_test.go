package recordstore

import (
	"testing"
)

// TestTeardownPurgeResetsThePlacementGeneration asserts a whole-tenant
// purge, which only a teardown sends, resets the tenant's stored
// placement generation. A Team created again under the same name starts
// at generation 1, and the announce never lowers a stored generation, so
// without the reset every claim of the new tenant answers NOT_READY.
func TestTeardownPurgeResetsThePlacementGeneration(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
	})

	if err := purgeTenant(t.Context(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-b", Generation: 3, CellInstance: "pod-2",
	})

	if got := readTenantPlacement(t, db, testTenant).generation; got != 3 {
		t.Fatalf("generation after the tenant is created again = %d, want 3", got)
	}
}

// TestATenantCreatedAgainInheritsNoSeed asserts the new tenant starts
// unseeded even when it lands on the same cell pod the old one used. The
// teardown never rolls that pod, so the member instance is unchanged, and
// the generation raise is the only thing that clears the seed.
func TestATenantCreatedAgainInheritsNoSeed(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	slot := int32(0)
	seeded := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		RehydratedInstance: "pod-1",
		Members: []member{{
			Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive,
			Slot: &slot, Instance: "pod-1", RehydratedInstance: "pod-1",
		}},
	}
	sendTenantSync(t, pool, conn, seeded)
	sendTenantSync(t, pool, conn, seeded)

	if err := purgeTenant(t.Context(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	fresh := seeded
	fresh.Generation = 1
	sendTenantSync(t, pool, conn, fresh)

	row, exists, err := readTenantPlacementMembers(t.Context(), pool, testCluster, testTenant)
	if err != nil || !exists {
		t.Fatalf("read members: exists=%v err=%v", exists, err)
	}
	if len(row.members) != 1 || row.members[0].RehydratedInstance != "" {
		t.Fatalf("member seed of the new tenant = %v, want it cleared", row.members)
	}
}

// TestMovePurgeKeepsThePlacementGeneration asserts a purge that names a
// cell, which is a slot move, never resets the generation. The tenant
// lives on, and lowering its generation would open the fence to a claim
// the move has already left behind.
func TestMovePurgeKeepsThePlacementGeneration(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
	})

	if err := purgeTenant(t.Context(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
		Cell: "cell-a", CellNamespace: testNamespace,
	}); err != nil {
		t.Fatalf("move purge: %v", err)
	}

	if got := readTenantPlacement(t, db, testTenant).generation; got != 6 {
		t.Fatalf("generation after a move purge = %d, want 6", got)
	}
}

// TestTeardownPurgeKeepsANewerPlacementGeneration asserts a delayed
// teardown retry never resets a generation above its own. The tenant was
// created again and announced before the retry arrived, so its row
// belongs to the new incarnation.
func TestTeardownPurgeKeepsANewerPlacementGeneration(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 9, CellInstance: "pod-1",
	})

	if err := purgeTenant(t.Context(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	if got := readTenantPlacement(t, db, testTenant).generation; got != 9 {
		t.Fatalf("generation after a stale teardown retry = %d, want 9", got)
	}
}
