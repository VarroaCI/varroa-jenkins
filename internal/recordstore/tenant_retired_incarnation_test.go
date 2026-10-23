package recordstore

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// announceIncarnation is one slot-0 announcement of testTenant on cell-a
// that names incarnation.
func announceIncarnation(generation int64, instance, incarnation string) TenantSyncRequest {
	return TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: generation, CellInstance: instance,
		Incarnation: incarnation,
	}
}

// TestATakeOverRetiresTheOldIncarnation asserts a take-over adds the
// stored incarnation to the retired list.
func TestATakeOverRetiresTheOldIncarnation(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceIncarnation(6, "pod-1", "uid-old"))
	sendTenantSync(t, pool, conn, announceIncarnation(1, "pod-2", "uid-new"))

	row := readTenantPlacement(t, db, testTenant)
	if row.incarnation != "uid-new" {
		t.Fatalf("stored incarnation = %q, want uid-new", row.incarnation)
	}
	if len(row.retired) != 1 || row.retired[0] != "uid-old" {
		t.Fatalf("retired incarnations = %v, want [uid-old]", row.retired)
	}
}

// TestARetiredIncarnationCannotTakeTheRowBack asserts a late announcement
// of a replaced tenant object is refused as stale. It changes no column
// and keeps the new tenant's schedule rows, and the log names the reason.
func TestARetiredIncarnationCannotTakeTheRowBack(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceIncarnation(6, "pod-1", "uid-old"))
	fresh := announceIncarnation(1, "pod-2", "uid-new")
	fresh.RehydratedInstance = "pod-2"
	sendTenantSync(t, pool, conn, fresh)
	sendTenantSync(t, pool, conn, fresh)
	seedTenantPurgeRows(t, NewIngester(pool, nil, nil), testTenant, 1, 1, "other")
	before := readTenantPlacement(t, db, testTenant)
	if before.rehydratedInstance != "pod-2" {
		t.Fatalf("setup: seed = %q, want pod-2", before.rehydratedInstance)
	}

	body, err := json.Marshal(announceIncarnation(7, "pod-1", "uid-old"))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var resp TenantSyncResponse
	if err := json.Unmarshal(handleTenantSync(conn, logger, pool, nil, nil, nil, nil, testGrace, nil, "", body), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Stale || resp.Ok {
		t.Fatalf("response = %+v, want stale and no ok", resp)
	}
	if !strings.Contains(logs.String(), "reason=\"retired incarnation\"") {
		t.Fatalf("log names no retired incarnation reason: %s", logs.String())
	}

	after := readTenantPlacement(t, db, testTenant)
	if after.incarnation != "uid-new" || after.generation != before.generation ||
		after.rehydratedInstance != "pod-2" || after.cellInstance != "pod-2" {
		t.Fatalf("row after the refused announce = %+v, want %+v", after, before)
	}
	if got := countTenantRows(t, db, testTenant); got != 2 {
		t.Fatalf("schedule rows of the new tenant = %d, want 2", got)
	}
}

// TestTenantPurgeRetiresNoIncarnation asserts a teardown purge that reports
// no deleted Tenant leaves the retired list alone. A Tenant that drops spec.pipelines keeps its uid, and
// its next announcement after it adds the field again is accepted.
func TestTenantPurgeRetiresNoIncarnation(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceIncarnation(6, "pod-1", "uid-a"))
	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6, Incarnation: "uid-a",
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}

	if resp := sendTenantSync(t, pool, conn, announceIncarnation(1, "pod-1", "uid-a")); !resp.Ok || resp.Stale {
		t.Fatalf("the same Tenant's announcement was refused: %+v", resp)
	}
	row := readTenantPlacement(t, db, testTenant)
	if row.generation != 1 || len(row.retired) != 0 {
		t.Fatalf("row = %+v, want generation 1 and no retired incarnation", row)
	}
}

// TestADeletedTenantPurgeRetiresItsIncarnation asserts a teardown purge for
// a deleted Tenant retires its incarnation. A late announcement of that Tenant
// is then stale, and the row keeps the reset generation.
func TestADeletedTenantPurgeRetiresItsIncarnation(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceIncarnation(6, "pod-1", "uid-a"))
	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6, Incarnation: "uid-a",
		TenantDeleted: true,
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}
	row := readTenantPlacement(t, db, testTenant)
	if row.generation != 0 || len(row.retired) != 1 || row.retired[0] != "uid-a" {
		t.Fatalf("row after the purge = %+v, want generation 0 and uid-a retired", row)
	}

	if resp := sendTenantSync(t, pool, conn, announceIncarnation(6, "pod-1", "uid-a")); !resp.Stale || resp.Ok {
		t.Fatalf("the late announcement of the deleted Tenant = %+v, want stale", resp)
	}
	if row := readTenantPlacement(t, db, testTenant); row.generation != 0 {
		t.Fatalf("generation after the late announcement = %d, want 0", row.generation)
	}

	if resp := sendTenantSync(t, pool, conn, announceIncarnation(1, "pod-2", "uid-b")); !resp.Ok || resp.Stale {
		t.Fatalf("a new Tenant of the same name was refused: %+v", resp)
	}
	row = readTenantPlacement(t, db, testTenant)
	if row.incarnation != "uid-b" || len(row.retired) != 1 || row.retired[0] != "uid-a" {
		t.Fatalf("row after the new Tenant = %+v, want uid-b with only uid-a retired", row)
	}
}

// TestADeletedTenantPurgeOfANewerIncarnationRetiresNothing asserts a purge
// for a deleted Tenant leaves a row that a newer Tenant of the same name
// already owns.
func TestADeletedTenantPurgeOfANewerIncarnationRetiresNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceIncarnation(2, "pod-2", "uid-new"))
	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 6, Incarnation: "uid-old",
		TenantDeleted: true,
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}
	row := readTenantPlacement(t, db, testTenant)
	if row.incarnation != "uid-new" || row.generation != 2 || len(row.retired) != 0 {
		t.Fatalf("row = %+v, want uid-new at generation 2 with nothing retired", row)
	}
}

// TestADeletedTenantPurgeWithNoRowWritesNoRow asserts a purge for a deleted
// Tenant with no placement row writes no row, so a deleted tenant never gets
// a row of its own.
func TestADeletedTenantPurgeWithNoRowWritesNoRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	if err := purgeTenant(t.Context(), slog.Default(), pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 3, Incarnation: "uid-a",
		TenantDeleted: true,
	}); err != nil {
		t.Fatalf("teardown purge: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.tenant_placements WHERE tenant = $1`, testTenant).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("placement rows = %d, want 0", n)
	}
}

// TestSingleTenantTakeOverRetiresTheOldUID asserts the generation-zero row
// of a controller with no cell role gets the same protection. A Controller
// deleted and created again under one name takes the row over, and a late
// announcement of the old object is refused.
func TestSingleTenantTakeOverRetiresTheOldUID(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	tenant := "jenkins/solo"
	single := func(instance, incarnation string) TenantSyncRequest {
		return TenantSyncRequest{
			Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: tenant,
			Namespace: "jenkins", Cell: "solo", Generation: 0, CellInstance: instance,
			RehydratedInstance: instance, Incarnation: incarnation,
		}
	}

	sendTenantSync(t, pool, conn, single("solo-0-a", "uid-a"))
	if resp := sendTenantSync(t, pool, conn, single("solo-0-b", "uid-b")); !resp.Ok {
		t.Fatalf("the take-over was refused: %+v", resp)
	}
	if resp := sendTenantSync(t, pool, conn, single("solo-0-a", "uid-a")); !resp.Stale || resp.Ok {
		t.Fatalf("the replaced Controller's announcement = %+v, want stale", resp)
	}

	row, exists, err := readTenantPlacementMembers(t.Context(), pool, testCluster, tenant)
	if err != nil || !exists {
		t.Fatalf("read row: exists=%v err=%v", exists, err)
	}
	if len(row.members) != 1 || row.members[0].Instance != "solo-0-b" {
		t.Fatalf("stored member = %+v, want solo-0-b", row.members)
	}
}

// TestATakeOverOfARowWithNoIncarnationRetiresNothing asserts the second
// take-over arm adds no entry. The stored row names no incarnation, so no
// value exists to retire.
func TestATakeOverOfARowWithNoIncarnationRetiresNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceIncarnation(6, "pod-1", ""))
	sendTenantSync(t, pool, conn, announceIncarnation(1, "pod-2", "uid-new"))

	row := readTenantPlacement(t, db, testTenant)
	if row.incarnation != "uid-new" || row.generation != 1 {
		t.Fatalf("row = %+v, want uid-new at generation 1", row)
	}
	if len(row.retired) != 0 {
		t.Fatalf("retired incarnations = %v, want none", row.retired)
	}
}

// TestAStaleAnnouncementRemovesNoEntry asserts a refused announcement
// keeps an unconfirmed entry for an instance it names. A stale request
// writes nothing, so it proves nothing live.
func TestAStaleAnnouncementRemovesNoEntry(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceIncarnation(6, "pod-1", "uid-old"))
	sendTenantSync(t, pool, conn, announceIncarnation(1, "pod-2", "uid-new"))
	sendTenantSync(t, pool, conn, announceIncarnation(1, "pod-3", "uid-new"))
	if _, ok := derivedEntry(t, db, "pod-2"); !ok {
		t.Fatal("setup: pod-2 is not listed")
	}

	if resp := sendTenantSync(t, pool, conn, announceIncarnation(7, "pod-2", "uid-old")); !resp.Stale {
		t.Fatalf("response = %+v, want stale", resp)
	}
	if _, ok := derivedEntry(t, db, "pod-2"); !ok {
		t.Fatal("a stale announcement removed the entry of the instance it names")
	}
}
