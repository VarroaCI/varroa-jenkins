package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// twoSlotLivenessRow builds a liveness row for a tenant whose slot 0 and
// slot 1 are both alive, with an instance of no member listed dead.
func twoSlotLivenessRow(now time.Time) tenantPlacementLivenessRow {
	aliveAt := now.Add(-time.Minute)
	lastSeen := now.Add(-20 * time.Minute)
	return tenantPlacementLivenessRow{
		cell:           "cell-a",
		cellInstance:   "pod-a",
		cellAlive:      true,
		cellAliveAt:    &aliveAt,
		cellLastSeenAt: &lastSeen,
		deadInstances: []TenantSyncDeadInstance{
			{Instance: "pod-gone", LastSeenAt: now.Add(-6 * time.Minute)},
		},
		members: placementMembers{
			members: []member{
				{Cell: "cell-a", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(0),
					Instance: "pod-a", RehydratedInstance: "pod-a"},
				{Cell: "cell-b", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(1),
					Instance: "pod-b", RehydratedInstance: "pod-b"},
			},
			jobHomes: map[string]int32{"acme/api": 1},
		},
	}
}

// TestCellLivenessALiveSecondSlotTakesTheSilentReading asserts a run owned
// by slot 1's live instance is not read as moved merely because its
// instance differs from slot 0's.
func TestCellLivenessALiveSecondSlotTakesTheSilentReading(t *testing.T) {
	now := time.Now()
	row := twoSlotLivenessRow(now)
	anchor := now.Add(-6 * time.Minute)

	if got := cellLivenessGone(now, anchor, "pod-b", row, true, testGrace); got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: slot 1's instance is a current member", got)
	}
}

// TestCellLivenessAnInstanceOfNoMemberTakesTheMovedReading asserts an
// instance no member names, listed dead past the moved window, is read as
// moved.
func TestCellLivenessAnInstanceOfNoMemberTakesTheMovedReading(t *testing.T) {
	now := time.Now()
	row := twoSlotLivenessRow(now)
	anchor := now.Add(-6 * time.Minute)

	if got := cellLivenessGone(now, anchor, "pod-gone", row, true, testGrace); got != livenessMoved {
		t.Fatalf("reading = %d, want livenessMoved: no member names that instance", got)
	}
}

// TestCellLivenessASingleMemberTenantIsUnchanged asserts a row with an
// empty stored member list reads exactly as it read before slots existed:
// its own instance takes the silent reading.
func TestCellLivenessASingleMemberTenantIsUnchanged(t *testing.T) {
	now := time.Now()
	lastSeen := now.Add(-20 * time.Minute)
	aliveAt := now.Add(-time.Minute)
	row := resolveLivenessMembers(tenantPlacementLivenessRow{
		cell: "cell-solo", cellInstance: "pod-a", cellAlive: false,
		cellAliveAt: &aliveAt, cellLastSeenAt: &lastSeen,
	})
	anchor := now.Add(-20 * time.Minute)

	if got := cellLivenessGone(now, anchor, "pod-a", row, true, testGrace); got != livenessSilent {
		t.Fatalf("reading = %d, want livenessSilent", got)
	}
}

// TestEvaluateInterruptionNamesCellRolledOnTheHomeSlot asserts the reason
// follows the member resolution: a run whose stored cell still holds its
// job's home slot is cell-rolled, and one on another cell is tenant-moved.
func TestEvaluateInterruptionNamesCellRolledOnTheHomeSlot(t *testing.T) {
	now := time.Now()
	row := twoSlotLivenessRow(now)

	rolled := runInterruptCandidate{
		cell: "cell-b", jobPath: "acme/api", cellInstance: "pod-gone",
		status: StatusRunning, createdAt: now.Add(-time.Hour),
	}
	reason, ok := evaluateInterruption(now, rolled, row, true, testGrace)
	if !ok || reason != ReasonCellRolled {
		t.Fatalf("reason = %q ok = %v, want cell-rolled", reason, ok)
	}

	moved := rolled
	moved.cell = "cell-old"
	reason, ok = evaluateInterruption(now, moved, row, true, testGrace)
	if !ok || reason != ReasonTenantMoved {
		t.Fatalf("reason = %q ok = %v, want tenant-moved", reason, ok)
	}
}

// TestTenantPurgeScopesTheDeleteByCell asserts a slot move purges one
// cell's schedule rows and leaves every other slot's rows in place.
func TestTenantPurgeScopesTheDeleteByCell(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	seedSchedule := func(cell, jobPath string, generation int64) {
		seedTenantSchedule(t, db, "cells", cell, jobPath, generation)
	}
	seedSchedule("cell-a", "acme/web", 4)
	seedSchedule("cell-b", "acme/api", 4)

	if err := purgeTenantJobSchedules(ctx, pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 4,
		Cell: "cell-a", CellNamespace: "cells",
	}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.job_schedules WHERE cell = 'cell-b'`).Scan(&remaining); err != nil {
		t.Fatalf("count cell-b rows: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("cell-b rows: got %d, want 1: a slot move purges one cell", remaining)
	}
	if err := db.QueryRow(`SELECT count(*) FROM varroa.job_schedules WHERE cell = 'cell-a'`).Scan(&remaining); err != nil {
		t.Fatalf("count cell-a rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("cell-a rows: got %d, want 0", remaining)
	}
}

// seedTenantSchedule inserts one job_schedules row for the test tenant.
func seedTenantSchedule(t *testing.T, db *sql.DB, namespace, cell, jobPath string, generation int64) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedules
			(cluster, namespace, cell, tenant, job_path, trigger_type, spec, timezone,
			 disabled, observed_at, placement_generation, payload)
		VALUES ($1, $2, $3, $4, $5, 'cron', 'H * * * *', '', false, now(), $6, '{}'::jsonb)
	`, testCluster, namespace, cell, testTenant, jobPath, generation); err != nil {
		t.Fatalf("seed job_schedules %s: %v", jobPath, err)
	}
}

// TestTenantPurgeKeepsASameNamedCellInAnotherNamespace asserts the narrow
// delete tests the namespace as well as the cell name.
//
// A job_schedules row carries both columns, and two namespaces can hold a
// cell of the same name. A delete on the name alone would drop the other
// namespace's rows, and that tenant would lose every schedule it reported.
func TestTenantPurgeKeepsASameNamedCellInAnotherNamespace(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	seedTenantSchedule(t, db, "cells-a", "cell-2", "acme/web", 4)
	seedTenantSchedule(t, db, "cells-b", "cell-2", "acme/api", 4)

	if err := purgeTenantJobSchedules(ctx, pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 4,
		Cell: "cell-2", CellNamespace: "cells-a",
	}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var remaining int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.job_schedules WHERE namespace = 'cells-b'
	`).Scan(&remaining); err != nil {
		t.Fatalf("count cells-b rows: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("cells-b rows: got %d, want 1: another namespace keeps its rows", remaining)
	}
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.job_schedules WHERE namespace = 'cells-a'
	`).Scan(&remaining); err != nil {
		t.Fatalf("count cells-a rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("cells-a rows: got %d, want 0", remaining)
	}
}

// TestTenantPurgeRefusesACellWithNoNamespace asserts the responder refuses
// a request that names a cell and no namespace, rather than deleting on
// the name alone.
func TestTenantPurgeRefusesACellWithNoNamespace(t *testing.T) {
	body, err := json.Marshal(TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 4, Cell: "cell-2",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var resp TenantPurgeResponse
	if err := json.Unmarshal(handleTenantPurge(slog.Default(), nil, body), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error == "" {
		t.Fatal("a cell with no namespace answered success")
	}
}

// TestTenantPurgeWithNoCellDeletesTheWholeTenant asserts teardown keeps
// today's whole-tenant delete.
func TestTenantPurgeWithNoCellDeletesTheWholeTenant(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	for _, row := range []struct{ cell, jobPath string }{{"cell-a", "acme/web"}, {"cell-b", "acme/api"}} {
		if _, err := db.Exec(`
			INSERT INTO varroa.job_schedules
				(cluster, namespace, cell, tenant, job_path, trigger_type, spec, timezone,
				 disabled, observed_at, placement_generation, payload)
			VALUES ($1, 'cells', $2, $3, $4, 'cron', 'H * * * *', '', false, now(), 4, '{}'::jsonb)
		`, testCluster, row.cell, testTenant, row.jobPath); err != nil {
			t.Fatalf("seed job_schedules %s: %v", row.jobPath, err)
		}
	}

	if err := purgeTenantJobSchedules(ctx, pool, TenantPurgeRequest{
		Cluster: testCluster, Tenant: testTenant, PlacementGeneration: 4,
	}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.job_schedules`).Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("rows: got %d, want 0: a teardown purge names no cell", remaining)
	}
}

// TestCheckPlacementAllowsACallerOnSlotOne asserts a caller on any slot of
// the tenant passes the placement orders, and that a cell the tenant has
// left still gets the permanent FENCED.
func TestCheckPlacementAllowsACallerOnSlotOne(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	evaluator := NewGrantEvaluator(pool)
	seedTenantPlacementMembers(t, db, testTenant, 3, fenceScenarioMembers(), map[string]int32{})

	onSlotOne := PlacementInput{
		Path: GrantPathLock, Cluster: testCluster, Namespace: testNamespace,
		Cell: "cell-b", Tenant: testTenant, PlacementGeneration: 3,
	}
	got, err := evaluator.CheckPlacement(ctx, onSlotOne)
	if err != nil {
		t.Fatalf("CheckPlacement: %v", err)
	}
	if !got.Allow {
		t.Fatalf("a caller on slot 1 answered %+v, want allow", got)
	}

	onStandby := onSlotOne
	onStandby.Cell = "cell-c"
	got, err = evaluator.CheckPlacement(ctx, onStandby)
	if err != nil {
		t.Fatalf("CheckPlacement: %v", err)
	}
	if !got.Allow {
		t.Fatalf("a caller on the standby answered %+v, want allow", got)
	}

	onLeftCell := onSlotOne
	onLeftCell.Cell = "cell-old"
	got, err = evaluator.CheckPlacement(ctx, onLeftCell)
	if err != nil {
		t.Fatalf("CheckPlacement: %v", err)
	}
	if got.Allow || got.ReasonCode != ReasonFenced {
		t.Fatalf("a caller on a cell the tenant left answered %+v, want FENCED", got)
	}
}

// TestCheckPlacementHoldsAStaleGenerationFromALiveMember asserts a caller
// on a current member whose resolver cache is behind gets the retryable
// NOT_READY, and that its next attempt is allowed.
func TestCheckPlacementHoldsAStaleGenerationFromALiveMember(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	evaluator := NewGrantEvaluator(pool)
	seedTenantPlacementMembers(t, db, testTenant, 4, fenceScenarioMembers(), map[string]int32{})

	stale := PlacementInput{
		Path: GrantPathLock, Cluster: testCluster, Namespace: testNamespace,
		Cell: "cell-b", Tenant: testTenant, PlacementGeneration: 3,
	}
	got, err := evaluator.CheckPlacement(ctx, stale)
	if err != nil {
		t.Fatalf("CheckPlacement: %v", err)
	}
	if got.Allow || got.ReasonCode != ReasonNotReady {
		t.Fatalf("a stale generation from a live member answered %+v, want NOT_READY", got)
	}

	current := stale
	current.PlacementGeneration = 4
	got, err = evaluator.CheckPlacement(ctx, current)
	if err != nil {
		t.Fatalf("CheckPlacement: %v", err)
	}
	if !got.Allow {
		t.Fatalf("the next attempt answered %+v, want allow", got)
	}
}

// TestReadTenantCellInstanceNamesTheHomeSlot asserts the lock acquire
// records the instance of the member that runs the build, not slot 0's.
func TestReadTenantCellInstanceNamesTheHomeSlot(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	seedTenantPlacementMembers(t, db, testTenant, 3, fenceScenarioMembers(),
		map[string]int32{"acme/api": 1})

	got, err := readTenantCellInstance(ctx, pool, testCluster, testTenant, "acme/api")
	if err != nil {
		t.Fatalf("readTenantCellInstance: %v", err)
	}
	if got != "pod-b" {
		t.Fatalf("instance = %q, want pod-b: the job is homed on slot 1", got)
	}

	got, err = readTenantCellInstance(ctx, pool, testCluster, testTenant, "acme/web")
	if err != nil {
		t.Fatalf("readTenantCellInstance: %v", err)
	}
	if got != "pod-a" {
		t.Fatalf("instance = %q, want pod-a: acme/web holds no entry, and its hash home is slot 0", got)
	}
}

// TestResolveCrossTenantTargetPlacementNamesTheHomeSlot asserts a
// cross-tenant trigger routes to the target job's home slot's holder.
func TestResolveCrossTenantTargetPlacementNamesTheHomeSlot(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	seedTenantPlacementMembers(t, db, "beta", 3, []member{
		{Cell: "beta-a", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(0), Instance: "pod-a"},
		{Cell: "beta-b", Namespace: "cells", Role: MemberRoleActive, Slot: slotPtr(1), Instance: "pod-b"},
	}, map[string]int32{"beta/deploy": 1})

	target, err := resolveCrossTenantTargetPlacement(ctx, pool, testCluster, "beta", "beta/deploy")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.cell != "beta-b" {
		t.Fatalf("cell = %q, want beta-b: the target job is homed on slot 1", target.cell)
	}

	// A lock call carries no job path. It resolves slot 0.
	target, err = resolveCrossTenantTargetPlacement(ctx, pool, testCluster, "beta", "")
	if err != nil {
		t.Fatalf("resolve with no job path: %v", err)
	}
	if target.cell != "beta-a" {
		t.Fatalf("cell = %q, want beta-a: a lock call resolves the tenant, not a job", target.cell)
	}
}
