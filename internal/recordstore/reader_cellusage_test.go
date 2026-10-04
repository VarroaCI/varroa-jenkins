package recordstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cellUsageNamespace is the cell namespace every cell-usage test shares.
// cellUsageScheduleCell is the one cell whose schedule rows these tests
// seed.
const (
	cellUsageNamespace      = "cells"
	cellUsageScheduleCell   = "cell-a"
	cellUsageScheduleTenant = "acme"
)

// seedCellPlacement binds tenant to cell in varroa.tenant_placements.
func seedCellPlacement(t *testing.T, db *sql.DB, cell, tenant string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, namespace, cell)
		VALUES ($1, $2, $3, $4)
	`, testCluster, tenant, cellUsageNamespace, cell); err != nil {
		t.Fatalf("seed placement of %s on %s: %v", tenant, cell, err)
	}
}

// seedCellRun inserts one varroa.runs row with the given cell, tenant,
// status, and result. A nil result leaves the column null.
func seedCellRun(t *testing.T, db *sql.DB, namespace, cell, tenant, jobPath string, createdAt time.Time, status string, result *string) {
	t.Helper()
	runID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, result)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, runID, createdAt, "manual:cellusage-"+runID.String(), testCluster, namespace, cell, tenant, jobPath, status, result); err != nil {
		t.Fatalf("seed run for %s on %s: %v", tenant, cell, err)
	}
}

// seedCellSchedule inserts one varroa.job_schedules row for the
// cellUsageScheduleTenant tenant on the cellUsageScheduleCell cell.
func seedCellSchedule(t *testing.T, db *sql.DB, jobPath, triggerType string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedules (cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, now(), '{}')
	`, testCluster, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant, jobPath, triggerType); err != nil {
		t.Fatalf("seed schedule %s: %v", jobPath, err)
	}
}

// resultOf returns a pointer to a runs.result value.
func resultOf(value string) *string {
	return &value
}

// tenantCounts finds one tenant's counts in a CellUsage value.
func tenantCounts(t *testing.T, usage CellUsage, tenant string) CellUsageCounts {
	t.Helper()
	for _, row := range usage.Tenants {
		if row.Tenant == tenant {
			return row.CellUsageCounts
		}
	}
	t.Fatalf("tenant %s is absent from %+v", tenant, usage.Tenants)
	return CellUsageCounts{}
}

// wantCounts compares one tenant's four counts.
func wantCounts(t *testing.T, got CellUsageCounts, want CellUsageCounts) {
	t.Helper()
	if got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

// TestCellUsageCountsEachTenant asserts the four counts for a placed
// tenant with runs, and four zeros for a placed tenant with none.
func TestCellUsageCountsEachTenant(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	const cell = "cell-a"
	seedCellPlacement(t, db, cell, "acme")
	seedCellPlacement(t, db, cell, "quiet")

	recent := time.Now().UTC().Add(-time.Hour)
	seedCellRun(t, db, cellUsageNamespace, cell, "acme", "acme/app", recent, "COMPLETED", resultOf("SUCCESS"))
	seedCellRun(t, db, cellUsageNamespace, cell, "acme", "acme/app", recent, "COMPLETED", resultOf("FAILURE"))
	seedCellRun(t, db, cellUsageNamespace, cell, "acme", "acme/app", recent, "RUNNING", nil)
	seedCellSchedule(t, db, "acme/app", "cron")

	usage, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, cell)
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	if len(usage.Tenants) != 2 {
		t.Fatalf("Tenants = %+v, want two rows", usage.Tenants)
	}
	if usage.Tenants[0].Tenant != "acme" || usage.Tenants[1].Tenant != "quiet" {
		t.Errorf("Tenants order = %s, %s, want acme, quiet", usage.Tenants[0].Tenant, usage.Tenants[1].Tenant)
	}
	wantCounts(t, tenantCounts(t, usage, "acme"), CellUsageCounts{RunningNow: 1, RunsLast24h: 3, FailedLast24h: 1, Jobs: 1})
	wantCounts(t, tenantCounts(t, usage, "quiet"), CellUsageCounts{})
	wantCounts(t, usage.Totals, CellUsageCounts{RunningNow: 1, RunsLast24h: 3, FailedLast24h: 1, Jobs: 1})
	if usage.WindowStart.IsZero() {
		t.Error("WindowStart is zero, want the start of the fixed window")
	}
}

// TestCellUsageExcludesTriggerFailed asserts a TRIGGER_FAILED row counts
// in no value.
func TestCellUsageExcludesTriggerFailed(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	const cell = "cell-a"
	seedCellPlacement(t, db, cell, "acme")
	recent := time.Now().UTC().Add(-time.Hour)
	seedCellRun(t, db, cellUsageNamespace, cell, "acme", "acme/app", recent, "COMPLETED", resultOf("TRIGGER_FAILED"))

	usage, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, cell)
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	wantCounts(t, tenantCounts(t, usage, "acme"), CellUsageCounts{})
}

// TestCellUsageCountsLongRunningBuild asserts a run started three days
// ago still counts in runningNow, and stays outside the 24-hour window.
func TestCellUsageCountsLongRunningBuild(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	const cell = "cell-a"
	seedCellPlacement(t, db, cell, "acme")
	old := time.Now().UTC().Add(-72 * time.Hour)
	seedCellRun(t, db, cellUsageNamespace, cell, "acme", "acme/app", old, "RUNNING", nil)

	usage, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, cell)
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	wantCounts(t, tenantCounts(t, usage, "acme"), CellUsageCounts{RunningNow: 1})
}

// TestCellUsageCountsDistinctJobPaths asserts one job with two trigger
// rows counts once, and a job with trigger type "none" counts.
func TestCellUsageCountsDistinctJobPaths(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	const cell = "cell-a"
	seedCellPlacement(t, db, cell, "acme")
	seedCellSchedule(t, db, "acme/app", "cron")
	seedCellSchedule(t, db, "acme/app", "varroa-upstream")
	seedCellSchedule(t, db, "acme/branch", "none")

	usage, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, cell)
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	wantCounts(t, tenantCounts(t, usage, "acme"), CellUsageCounts{Jobs: 2})
}

// TestCellUsageDropsDepartedTenant asserts a tenant that moved to
// another cell leaves the first cell's totals.
func TestCellUsageDropsDepartedTenant(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedCellPlacement(t, db, "cell-b", "acme")
	recent := time.Now().UTC().Add(-time.Hour)
	seedCellRun(t, db, cellUsageNamespace, "cell-a", "acme", "acme/app", recent, "COMPLETED", resultOf("SUCCESS"))

	usage, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, "cell-a")
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	if len(usage.Tenants) != 0 {
		t.Fatalf("Tenants = %+v, want none", usage.Tenants)
	}
	wantCounts(t, usage.Totals, CellUsageCounts{})
}

// TestCellUsageCountsTransferredRun asserts a run whose intent moved to
// a cell in another namespace counts on the new cell, because the
// aggregate carries no namespace term.
func TestCellUsageCountsTransferredRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	const cell = "cell-new"
	seedCellPlacement(t, db, cell, "acme")
	recent := time.Now().UTC().Add(-time.Hour)
	seedCellRun(t, db, "old-namespace", cell, "acme", "acme/app", recent, "COMPLETED", resultOf("SUCCESS"))

	usage, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, cell)
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	wantCounts(t, tenantCounts(t, usage, "acme"), CellUsageCounts{RunsLast24h: 1})
}

// sumJobUsage adds the four counts of every row, the sum a caller with
// an unnarrowed grant reads.
func sumJobUsage(rows []CellJobUsage) CellUsageCounts {
	var total CellUsageCounts
	for _, row := range rows {
		total.RunningNow += row.RunningNow
		total.RunsLast24h += row.RunsLast24h
		total.FailedLast24h += row.FailedLast24h
		total.Jobs += row.Jobs
	}
	return total
}

// TestCellTenantUsageReadsOneTenant asserts the tenant-scoped method
// returns that tenant's rows alone, and no row for a tenant the
// placement table binds to another cell.
func TestCellTenantUsageReadsOneTenant(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	const cell = "cell-a"
	seedCellPlacement(t, db, cell, "acme")
	seedCellPlacement(t, db, cell, "beta")
	seedCellPlacement(t, db, "cell-b", "gamma")

	recent := time.Now().UTC().Add(-time.Hour)
	seedCellRun(t, db, cellUsageNamespace, cell, "acme", "acme/app", recent, "RUNNING", nil)
	seedCellRun(t, db, cellUsageNamespace, cell, "beta", "beta/app", recent, "COMPLETED", resultOf("FAILURE"))

	rows, err := reader.CellTenantUsage(ctx, testCluster, cellUsageNamespace, cell, "acme")
	if err != nil {
		t.Fatalf("CellTenantUsage: %v", err)
	}
	if len(rows) != 1 || rows[0].JobPath != "acme/app" {
		t.Fatalf("rows = %+v, want one row for acme/app", rows)
	}
	wantCounts(t, sumJobUsage(rows), CellUsageCounts{RunningNow: 1, RunsLast24h: 1})

	away, err := reader.CellTenantUsage(ctx, testCluster, cellUsageNamespace, cell, "gamma")
	if err != nil {
		t.Fatalf("CellTenantUsage for a tenant on another cell: %v", err)
	}
	if len(away) != 0 {
		t.Errorf("rows = %+v, want none for a tenant bound to another cell", away)
	}
}

// TestCellTenantUsageSplitsTheLoadByJob asserts one row for each job, so
// a caller that may read part of a tenant sums part of its load. A job
// with a schedule row and no run appears, and a job with a run and no
// schedule row appears as well.
func TestCellTenantUsageSplitsTheLoadByJob(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedCellPlacement(t, db, cellUsageScheduleCell, cellUsageScheduleTenant)

	recent := time.Now().UTC().Add(-time.Hour)
	seedCellRun(t, db, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant, "acme/release", recent, "RUNNING", nil)
	seedCellRun(t, db, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant, "acme/release", recent, "COMPLETED", resultOf("FAILURE"))
	seedCellRun(t, db, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant, "acme/build", recent, "COMPLETED", resultOf("SUCCESS"))
	seedCellSchedule(t, db, "acme/release", "cron")
	seedCellSchedule(t, db, "acme/idle", "none")

	rows, err := reader.CellTenantUsage(ctx, testCluster, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant)
	if err != nil {
		t.Fatalf("CellTenantUsage: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want three job rows", rows)
	}
	// The rows order by job path, so the reader answers a stable list.
	want := []CellJobUsage{
		{JobPath: "acme/build", CellUsageCounts: CellUsageCounts{RunsLast24h: 1}},
		{JobPath: "acme/idle", CellUsageCounts: CellUsageCounts{Jobs: 1}},
		{JobPath: "acme/release", CellUsageCounts: CellUsageCounts{RunningNow: 1, RunsLast24h: 2, FailedLast24h: 1, Jobs: 1}},
	}
	for i, row := range rows {
		if row != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, row, want[i])
		}
	}

	// The sum of every row equals the tenant's own row of the cell-wide
	// aggregate, so an unnarrowed caller reads the same four counts.
	usage, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, cellUsageScheduleCell)
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	wantCounts(t, sumJobUsage(rows), tenantCounts(t, usage, cellUsageScheduleTenant))
}

// TestCellTenantUsageExcludesTriggerFailedPerJob asserts a failed trigger
// counts in no job's row, exactly as it counts in no cell total.
func TestCellTenantUsageExcludesTriggerFailedPerJob(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedCellPlacement(t, db, cellUsageScheduleCell, cellUsageScheduleTenant)
	recent := time.Now().UTC().Add(-time.Hour)
	seedCellRun(t, db, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant, "acme/app", recent, "COMPLETED", resultOf("TRIGGER_FAILED"))

	rows, err := reader.CellTenantUsage(ctx, testCluster, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant)
	if err != nil {
		t.Fatalf("CellTenantUsage: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none: a failed trigger started no build", rows)
	}
}

// TestCellTenantUsageCountsALongRunningBuild asserts a run older than the
// window still reaches its job's row, because the running scan carries no
// time bound.
func TestCellTenantUsageCountsALongRunningBuild(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	seedCellPlacement(t, db, cellUsageScheduleCell, cellUsageScheduleTenant)
	old := time.Now().UTC().Add(-72 * time.Hour)
	seedCellRun(t, db, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant, "acme/slow", old, "RUNNING", nil)

	rows, err := reader.CellTenantUsage(ctx, testCluster, cellUsageNamespace, cellUsageScheduleCell, cellUsageScheduleTenant)
	if err != nil {
		t.Fatalf("CellTenantUsage: %v", err)
	}
	if len(rows) != 1 || rows[0].JobPath != "acme/slow" {
		t.Fatalf("rows = %+v, want one row for acme/slow", rows)
	}
	wantCounts(t, rows[0].CellUsageCounts, CellUsageCounts{RunningNow: 1})
}

// countingTracer counts the statements a pgx connection runs.
type countingTracer struct {
	queries atomic.Int64
}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.queries.Add(1)
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestCellUsageManyRunsOneStatement asserts the batch read answers eight
// cells with the schema check plus one aggregate statement.
func TestCellUsageManyRunsOneStatement(t *testing.T) {
	db, dsn := setupTestDBDSN(t)

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	tracer := &countingTracer{}
	config.ConnConfig.Tracer = tracer

	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)

	cells := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		cell := fmt.Sprintf("cell-%d", i)
		cells = append(cells, cell)
		seedCellPlacement(t, db, cell, fmt.Sprintf("tenant-%d", i))
	}

	reader := NewReader(pool)
	tracer.queries.Store(0)
	usage, err := reader.CellUsageMany(context.Background(), testCluster, cellUsageNamespace, cells)
	if err != nil {
		t.Fatalf("CellUsageMany: %v", err)
	}
	if got := tracer.queries.Load(); got != 2 {
		t.Errorf("statements = %d, want 2: the schema check and one aggregate", got)
	}
	if len(usage) != 8 {
		t.Fatalf("cells = %d, want 8", len(usage))
	}
	for _, cell := range cells {
		if len(usage[cell].Tenants) != 1 {
			t.Errorf("cell %s tenants = %+v, want one row", cell, usage[cell].Tenants)
		}
	}
}

// TestCellUsageMethodsRefuseAnOldSchema asserts each cell-usage method
// answers ErrSchemaTooOld below the compiled minimum.
func TestCellUsageMethodsRefuseAnOldSchema(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	if _, err := db.Exec(`UPDATE varroa.goose_db_version SET version_id = 0 WHERE is_applied`); err != nil {
		t.Fatalf("lower schema version: %v", err)
	}

	if _, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, "cell-a"); !errors.Is(err, ErrSchemaTooOld) {
		t.Errorf("CellUsage error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.CellUsageMany(ctx, testCluster, cellUsageNamespace, []string{"cell-a"}); !errors.Is(err, ErrSchemaTooOld) {
		t.Errorf("CellUsageMany error = %v, want ErrSchemaTooOld", err)
	}
	if _, err := reader.CellTenantUsage(ctx, testCluster, cellUsageNamespace, "cell-a", "acme"); !errors.Is(err, ErrSchemaTooOld) {
		t.Errorf("CellTenantUsage error = %v, want ErrSchemaTooOld", err)
	}
}

// TestCellUsageCarriesPlacementGenerationAndAgentNamespace asserts each
// tenant row carries its placement row's generation and announced agent
// namespace, and an empty namespace for a row that names none.
func TestCellUsageCarriesPlacementGenerationAndAgentNamespace(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	const cell = "cell-a"
	seedCellPlacement(t, db, cell, "acme")
	seedCellPlacement(t, db, cell, "quiet")
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET generation = 7, agent_namespace = 'acme-agents'
		WHERE cluster = $1 AND tenant = 'acme'
	`, testCluster); err != nil {
		t.Fatalf("stamp acme's placement: %v", err)
	}

	usage, err := NewReader(pool).CellUsage(ctx, testCluster, cellUsageNamespace, cell)
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	if len(usage.Tenants) != 2 {
		t.Fatalf("Tenants = %+v, want two rows", usage.Tenants)
	}
	acme, quiet := usage.Tenants[0], usage.Tenants[1]
	if acme.Tenant != "acme" || acme.PlacementGeneration != 7 || acme.AgentNamespace != "acme-agents" {
		t.Errorf("acme row = %+v, want generation 7 and namespace acme-agents", acme)
	}
	if quiet.Tenant != "quiet" || quiet.PlacementGeneration != 0 || quiet.AgentNamespace != "" {
		t.Errorf("quiet row = %+v, want generation 0 and no namespace", quiet)
	}
}
