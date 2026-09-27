package recordstore

import (
	"log/slog"
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

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
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

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
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

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
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

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	if got := readTenantPlacement(t, db, testTenant).generation; got != 9 {
		t.Fatalf("generation after a stale teardown retry = %d, want 9", got)
	}
}

// TestTenantPurgeIgnoresANewerIncarnation asserts a late teardown purge
// leaves the placement row of a tenant created again under the same name
// alone. The purge can arrive after the Team finalizer clears, and the
// new tenant announces inside that window, so the row already belongs to
// the new incarnation.
func TestTenantPurgeIgnoresANewerIncarnation(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		Incarnation: "uid-old",
	})
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-2",
		Incarnation: "uid-new",
	})

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
		Incarnation: "uid-old",
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	row := readTenantPlacement(t, db, testTenant)
	if row.generation != 1 {
		t.Fatalf("generation after a purge of the old incarnation = %d, want 1", row.generation)
	}
	if row.incarnation != "uid-new" {
		t.Fatalf("stored incarnation = %q, want uid-new", row.incarnation)
	}
}

// TestALowerGenerationTakesOverARowWithNoIncarnation asserts a tenant
// created again owns a row that names no incarnation. Such a row was
// written before the column existed, so the announced generation is the
// only proof: a tenant never lowers its own generation.
func TestALowerGenerationTakesOverARowWithNoIncarnation(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		RehydratedInstance: "pod-1",
	})

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
		RehydratedInstance: "pod-1", Incarnation: "uid-new",
	})

	row := readTenantPlacement(t, db, testTenant)
	if row.generation != 1 {
		t.Fatalf("generation after the take-over = %d, want 1", row.generation)
	}
	if row.rehydratedInstance != "" {
		t.Fatalf("seed after the take-over = %q, want it cleared", row.rehydratedInstance)
	}
	if row.incarnation != "uid-new" {
		t.Fatalf("stored incarnation = %q, want uid-new", row.incarnation)
	}
}

// TestTheFirstStampDropsTheSchedulesOfARowWithNoIncarnation asserts the
// first announcement to name an incarnation deletes the schedule rows of
// a row that names none. Such a row can hold the rows of a tenant that is
// gone, at the same generation the new tenant announces, so no generation
// test tells them apart. The live cell exports its own set again on its
// ten-minute poll.
func TestTheFirstStampDropsTheSchedulesOfARowWithNoIncarnation(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
	})
	seedTenantPurgeRows(t, NewIngester(pool, nil, nil), testTenant, 1, 1, "other")
	if got := countTenantRows(t, db, testTenant); got != 2 {
		t.Fatalf("seeded rows = %d, want 2", got)
	}

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
		Incarnation: "uid-new",
	})

	if got := countTenantRows(t, db, testTenant); got != 0 {
		t.Fatalf("rows after the first stamp = %d, want 0", got)
	}
	if got := countTenantRows(t, db, "other"); got != 1 {
		t.Fatalf("rows of the other tenant = %d, want 1", got)
	}
}

// TestAStampedRowKeepsItsSchedules asserts the delete runs once. A row
// that already names an incarnation keeps the schedule rows of its own
// tenant on every later announcement.
func TestAStampedRowKeepsItsSchedules(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	announce := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
		Incarnation: "uid-one",
	}
	sendTenantSync(t, pool, conn, announce)
	seedTenantPurgeRows(t, NewIngester(pool, nil, nil), testTenant, 1, 1, "other")
	sendTenantSync(t, pool, conn, announce)

	if got := countTenantRows(t, db, testTenant); got != 2 {
		t.Fatalf("rows after a later announcement = %d, want 2", got)
	}
}

// TestAHigherGenerationKeepsARowWithNoIncarnation asserts the first
// announcement after the column exists takes nothing over. The tenant
// carries on at its own generation, so its seed and its stored generation
// stay, and the announcement stamps the incarnation alone.
func TestAHigherGenerationKeepsARowWithNoIncarnation(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
	})
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		RehydratedInstance: "pod-1", Incarnation: "uid-same",
	})

	row := readTenantPlacement(t, db, testTenant)
	if row.generation != 6 {
		t.Fatalf("generation after the stamp = %d, want 6", row.generation)
	}
	if row.rehydratedInstance != "pod-1" {
		t.Fatalf("seed after the stamp = %q, want pod-1", row.rehydratedInstance)
	}
	if row.incarnation != "uid-same" {
		t.Fatalf("stored incarnation = %q, want uid-same", row.incarnation)
	}
}

// TestTenantPurgeKeepsANewerIncarnationsSchedules asserts the same late
// purge keeps the schedule rows of the tenant created again. Those rows
// sit at generation 1, which the old tenant's generation test admits, so
// the incarnation test is the only thing that holds them.
func TestTenantPurgeKeepsANewerIncarnationsSchedules(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		Incarnation: "uid-old",
	})
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-2",
		Incarnation: "uid-new",
	})
	seedTenantPurgeRows(t, NewIngester(pool, nil, nil), testTenant, 1, 1, "other")

	if got := countTenantRows(t, db, testTenant); got != 2 {
		t.Fatalf("seeded rows of the new tenant = %d, want 2", got)
	}

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
		Incarnation: "uid-old",
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	if got := countTenantRows(t, db, testTenant); got != 2 {
		t.Fatalf("rows of the new tenant after the purge = %d, want 2", got)
	}
}

// TestTakeOverDropsTheOldIncarnationsSchedules asserts the announcement
// that takes the row over deletes the schedule rows the torn-down tenant
// left. The backstop scheduler admits a row by its job's home, and the
// new tenant's home can name the same cell, so a row left behind would
// keep firing the old tenant's cron under the new tenant's name. Another
// tenant's rows stay.
func TestTakeOverDropsTheOldIncarnationsSchedules(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		Incarnation: "uid-old",
	})
	// Both rows carry the old tenant's own generation, so no generation
	// test can tell them from a new tenant's rows.
	seedTenantPurgeRows(t, NewIngester(pool, nil, nil), testTenant, 6, 6, "other")
	if got := countTenantRows(t, db, testTenant); got != 2 {
		t.Fatalf("seeded rows of the old tenant = %d, want 2", got)
	}

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-2",
		Incarnation: "uid-new",
	})

	if got := countTenantRows(t, db, testTenant); got != 0 {
		t.Fatalf("rows after the take-over = %d, want 0", got)
	}
	if got := countTenantRows(t, db, "other"); got != 1 {
		t.Fatalf("rows of the other tenant = %d, want 1", got)
	}
}

// TestTenantPurgeResetsItsOwnIncarnation asserts the purge still resets
// the row it names. The tenant is gone and no new one has announced, so
// the stored incarnation is the one the purge carries.
func TestTenantPurgeResetsItsOwnIncarnation(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		Incarnation: "uid-old",
	})

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
		Incarnation: "uid-old",
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	if got := readTenantPlacement(t, db, testTenant).generation; got != 0 {
		t.Fatalf("generation after a purge of its own incarnation = %d, want 0", got)
	}
}

// TestTenantPurgeResetsARowWithNoIncarnation asserts a row written before
// the incarnation column existed still answers the reset. The stored
// value is empty, which means unknown, and an unknown value matches every
// purge.
func TestTenantPurgeResetsARowWithNoIncarnation(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
	})

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6,
		Incarnation: "uid-old",
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	if got := readTenantPlacement(t, db, testTenant).generation; got != 0 {
		t.Fatalf("generation after a purge of a row with no incarnation = %d, want 0", got)
	}
}

// TestAnnounceStoresTheIncarnation asserts the announce stamps the
// tenant's incarnation on its row, and that an announcement naming none
// keeps the stored value. The purge reads that column, so an empty report
// must never erase it.
func TestAnnounceStoresTheIncarnation(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		Incarnation: "uid-old",
	})
	if got := readTenantPlacement(t, db, testTenant).incarnation; got != "uid-old" {
		t.Fatalf("stored incarnation = %q, want uid-old", got)
	}

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
	})
	if got := readTenantPlacement(t, db, testTenant).incarnation; got != "uid-old" {
		t.Fatalf("stored incarnation after an announcement naming none = %q, want uid-old", got)
	}
}

// TestANewIncarnationTakesTheRowOver asserts the first announcement of a
// tenant created again owns the row at once. The announce takes the
// announced generation, even below the stored one, and clears the seed
// the old tenant left, so the new tenant's claims pass the fence on that
// first announcement.
func TestANewIncarnationTakesTheRowOver(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	slot := int32(0)
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 6, CellInstance: "pod-1",
		RehydratedInstance: "pod-1", Incarnation: "uid-old",
		Members: []member{{
			Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive,
			Slot: &slot, Instance: "pod-1", RehydratedInstance: "pod-1",
		}},
	})

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
		RehydratedInstance: "pod-1", Incarnation: "uid-new",
		Members: []member{{
			Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive,
			Slot: &slot, Instance: "pod-1", RehydratedInstance: "pod-1",
		}},
	})

	row := readTenantPlacement(t, db, testTenant)
	if row.generation != 1 {
		t.Fatalf("generation after the take-over = %d, want 1", row.generation)
	}
	if row.rehydratedInstance != "" {
		t.Fatalf("seed after the take-over = %q, want it cleared", row.rehydratedInstance)
	}

	members, exists, err := readTenantPlacementMembers(t.Context(), pool, testCluster, testTenant)
	if err != nil || !exists {
		t.Fatalf("read members: exists=%v err=%v", exists, err)
	}
	if len(members.members) != 1 || members.members[0].RehydratedInstance != "" {
		t.Fatalf("member seed after the take-over = %v, want it cleared", members.members)
	}
}
