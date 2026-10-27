package recordstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// tenantRetentionStore is a blob.Store fake for the tenant retention step.
// It records every Delete and DeletePrefix call. A failKeyPrefix makes
// every Delete under that prefix fail.
type tenantRetentionStore struct {
	recordingBlobStore
	prefixes      []string
	failKeyPrefix string
}

func newTenantRetentionStore(t *testing.T) *tenantRetentionStore {
	return &tenantRetentionStore{recordingBlobStore: recordingBlobStore{t: t}}
}

func (s *tenantRetentionStore) Delete(ctx context.Context, key string) error {
	if s.failKeyPrefix != "" && len(key) >= len(s.failKeyPrefix) && key[:len(s.failKeyPrefix)] == s.failKeyPrefix {
		return errors.New("injected delete failure")
	}
	return s.recordingBlobStore.Delete(ctx, key)
}

func (s *tenantRetentionStore) DeletePrefix(_ context.Context, prefix string) error {
	s.prefixes = append(s.prefixes, prefix)
	return nil
}

// seedTenantRun stores one run of the test cluster with the given tenant,
// job path, creation time and status. It returns the run id. The run gets
// an intent and a job number, the way a claim does, so a test can check
// that the step leaves them alone.
func seedTenantRun(t *testing.T, pool *pgxpool.Pool, tenant, jobPath string, createdAt time.Time, status string) uuid.UUID {
	t.Helper()
	intentKey := "manual:tenant-retention-" + uuid.NewString()
	runID := seedGrantedIntent(t, pool, intentKey)
	result := any(nil)
	if status == "COMPLETED" {
		result = "SUCCESS"
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO varroa.runs
			(run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, result, ended_at)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, $8, $9,
			CASE WHEN $8 IN ('COMPLETED', 'DUPLICATE') THEN $2::timestamptz END)
	`, runID, createdAt, intentKey, testCluster, testNamespace, tenant, jobPath, status, result); err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}
	return runID
}

// seedRunChildren gives one run a stage, a test suite, a test case and an
// artifact row. The artifact points at the given blob key.
func seedRunChildren(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, createdAt time.Time, artifactKey string) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO varroa.run_stages (run_id, stage_id, created_at, name, node_type, status)
		 VALUES ($1, 'stage-1', $2, 'build', 'stage', 'SUCCESS')`,
		`INSERT INTO varroa.run_tests (run_id, suite, created_at, total) VALUES ($1, 'unit', $2, 1)`,
		`INSERT INTO varroa.run_test_cases (run_id, suite, class_name, name, status, created_at)
		 VALUES ($1, 'unit', 'pkg.Class', 'test_one', 'FAILED', $2)`,
		`INSERT INTO varroa.run_artifacts (run_id, path, created_at, size_bytes, blob_key)
		 VALUES ($1, 'out.txt', $2, 1, $3)`,
		`INSERT INTO varroa.run_coverage (run_id, report_id, created_at) VALUES ($1, 'jacoco', $2)`,
		`INSERT INTO varroa.run_warnings (run_id, report_id, total, created_at) VALUES ($1, 'checkstyle', 0, $2)`,
	}
	for _, stmt := range stmts {
		args := []any{runID, createdAt}
		if stmt == stmts[3] {
			args = append(args, artifactKey)
		}
		if _, err := pool.Exec(ctx, stmt, args...); err != nil {
			t.Fatalf("seed run child: %v", err)
		}
	}
}

// setTenantRetention stores a tenant's retention policy in its placement
// row. The row is created when absent.
func setTenantRetention(t *testing.T, pool *pgxpool.Pool, policyJSON string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO varroa.tenant_placements (cluster, tenant, policy)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (cluster, tenant) DO UPDATE SET policy = EXCLUDED.policy
	`, testCluster, testTenant, policyJSON); err != nil {
		t.Fatalf("set tenant retention: %v", err)
	}
}

// runExists reports whether a runs row with the given id is present.
func runExists(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count run: %v", err)
	}
	return n > 0
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string, runID uuid.UUID) int {
	t.Helper()
	var n int
	q := fmt.Sprintf(`SELECT count(*) FROM varroa.%s WHERE run_id = $1`, table)
	if err := pool.QueryRow(context.Background(), q, runID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// tenantRetentionFixture prepares a database whose partitions cover
// every run a test seeds.
func tenantRetentionFixture(t *testing.T) (*pgxpool.Pool, time.Time) {
	t.Helper()
	pool, _ := setupLedgerPool(t)
	now := time.Now().UTC()
	if err := EnsurePartitions(context.Background(), pool, now, 420*24*time.Hour); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	// EnsurePartitions covers only the current window of runs. A test
	// seeds runs up to about fourteen months old, so it creates the rest.
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 14; i++ {
		from := monthStart.AddDate(0, -i, 0)
		if err := createPartition(context.Background(), pool, "runs", from.Format("200601"), from, from.AddDate(0, 1, 0)); err != nil {
			t.Fatalf("create runs partition: %v", err)
		}
	}
	return pool, now
}

const storeWindow = 365 * 24 * time.Hour

func TestDeleteTenantExpiredRunsPurgesRunAndChildren(t *testing.T) {
	t.Parallel()
	pool, now := tenantRetentionFixture(t)
	ctx := context.Background()
	setTenantRetention(t, pool, `{"retention":{"runs":"7d"}}`)

	old := now.Add(-10 * 24 * time.Hour)
	runID := seedTenantRun(t, pool, testTenant, testJobPath, old, "COMPLETED")
	artifactKey := "artifacts/" + runID.String() + "/out.txt"
	seedRunChildren(t, pool, runID, old, artifactKey)

	store := newTenantRetentionStore(t)
	deleted, err := deleteTenantExpiredRuns(ctx, pool, store, now, storeWindow, tenantRetentionRoundBudget)
	if err != nil {
		t.Fatalf("deleteTenantExpiredRuns: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if runExists(t, pool, runID) {
		t.Fatalf("expired run row still present")
	}
	for _, table := range []string{"run_stages", "run_tests", "run_test_cases", "run_coverage", "run_warnings", "run_artifacts"} {
		if n := countRows(t, pool, table, runID); n != 0 {
			t.Fatalf("%s rows = %d, want 0", table, n)
		}
	}

	wantKeys := map[string]bool{
		blob.LogKey(runID.String()):      false,
		blob.LogIndexKey(runID.String()): false,
		artifactKey:                      false,
	}
	for _, k := range store.deleted {
		if _, ok := wantKeys[k]; ok {
			wantKeys[k] = true
		}
	}
	for k, seen := range wantKeys {
		if !seen {
			t.Fatalf("blob %q was not deleted; deleted = %v", k, store.deleted)
		}
	}
	wantPrefixes := []string{
		blob.RunStatePrefix(runID.String()),
		blob.RestartSourcePrefix(runID.String()),
	}
	if len(store.prefixes) != len(wantPrefixes) ||
		store.prefixes[0] != wantPrefixes[0] || store.prefixes[1] != wantPrefixes[1] {
		t.Fatalf("DeletePrefix calls = %v, want %v", store.prefixes, wantPrefixes)
	}

	var intents, numbers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&intents); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM varroa.job_numbers WHERE tenant = $1`, testTenant).Scan(&numbers); err != nil {
		t.Fatalf("count job numbers: %v", err)
	}
	if intents != 1 {
		t.Fatalf("run_intents rows = %d, want 1", intents)
	}
	if numbers == 0 {
		t.Fatalf("job_numbers rows for tenant = 0, want the watermark kept")
	}
}

func TestDeleteTenantExpiredRunsKeepsYoungRunningAndOtherTenants(t *testing.T) {
	t.Parallel()
	pool, now := tenantRetentionFixture(t)
	ctx := context.Background()
	setTenantRetention(t, pool, `{"retention":{"runs":"7d"}}`)

	old := now.Add(-10 * 24 * time.Hour)
	young := seedTenantRun(t, pool, testTenant, testJobPath, now.Add(-24*time.Hour), "COMPLETED")
	running := seedTenantRun(t, pool, testTenant, testJobPath, old, "RUNNING")
	other := seedTenantRun(t, pool, "other-team", "other-team/build", old, "COMPLETED")

	store := newTenantRetentionStore(t)
	deleted, err := deleteTenantExpiredRuns(ctx, pool, store, now, storeWindow, tenantRetentionRoundBudget)
	if err != nil {
		t.Fatalf("deleteTenantExpiredRuns: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	for name, id := range map[string]uuid.UUID{"young run": young, "running run": running, "other tenant run": other} {
		if !runExists(t, pool, id) {
			t.Fatalf("%s was deleted", name)
		}
	}
}

func TestDeleteTenantExpiredRunsJobRules(t *testing.T) {
	t.Parallel()
	pool, now := tenantRetentionFixture(t)
	ctx := context.Background()
	setTenantRetention(t, pool,
		`{"retention":{"runs":"30d","jobs":[{"job":"pr-*","runs":"2d"},{"job":"release/**","runs":"365d"}]}}`)

	prRun := seedTenantRun(t, pool, testTenant, testTenant+"/pr-1", now.Add(-3*24*time.Hour), "COMPLETED")
	releaseRun := seedTenantRun(t, pool, testTenant, testTenant+"/release/1.0", now.Add(-100*24*time.Hour), "COMPLETED")
	deployRun := seedTenantRun(t, pool, testTenant, testTenant+"/deploy", now.Add(-40*24*time.Hour), "COMPLETED")

	store := newTenantRetentionStore(t)
	deleted, err := deleteTenantExpiredRuns(ctx, pool, store, now, storeWindow, tenantRetentionRoundBudget)
	if err != nil {
		t.Fatalf("deleteTenantExpiredRuns: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if runExists(t, pool, prRun) {
		t.Fatalf("pr run past its 2d job rule was kept")
	}
	if runExists(t, pool, deployRun) {
		t.Fatalf("deploy run past the 30d tenant window was kept")
	}
	if !runExists(t, pool, releaseRun) {
		t.Fatalf("release run inside its 365d job rule was deleted")
	}
}

func TestDeleteTenantExpiredRunsHonoursBudget(t *testing.T) {
	t.Parallel()
	pool, now := tenantRetentionFixture(t)
	ctx := context.Background()
	setTenantRetention(t, pool, `{"retention":{"runs":"7d"}}`)

	for i := 0; i < 5; i++ {
		seedTenantRun(t, pool, testTenant, testJobPath, now.Add(-time.Duration(10+i)*24*time.Hour), "COMPLETED")
	}

	store := newTenantRetentionStore(t)
	first, err := deleteTenantExpiredRuns(ctx, pool, store, now, storeWindow, 2)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if first != 2 {
		t.Fatalf("first round deleted %d, want 2", first)
	}
	second, err := deleteTenantExpiredRuns(ctx, pool, store, now, storeWindow, 2)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if second != 2 {
		t.Fatalf("second round deleted %d, want 2", second)
	}
}

func TestDeleteTenantExpiredRunsKeepsRowsWhenObjectDeleteFails(t *testing.T) {
	t.Parallel()
	pool, now := tenantRetentionFixture(t)
	ctx := context.Background()
	setTenantRetention(t, pool, `{"retention":{"runs":"7d"}}`)

	old := now.Add(-10 * 24 * time.Hour)
	runID := seedTenantRun(t, pool, testTenant, testJobPath, old, "COMPLETED")
	seedRunChildren(t, pool, runID, old, "artifacts/"+runID.String()+"/out.txt")

	store := newTenantRetentionStore(t)
	store.failKeyPrefix = "logs/"
	deleted, err := deleteTenantExpiredRuns(ctx, pool, store, now, storeWindow, tenantRetentionRoundBudget)
	if err == nil {
		t.Fatalf("deleteTenantExpiredRuns() error = nil, want the injected object failure")
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if !runExists(t, pool, runID) {
		t.Fatalf("run row was deleted although its blob delete failed")
	}
	if n := countRows(t, pool, "run_stages", runID); n != 1 {
		t.Fatalf("run_stages rows = %d, want 1", n)
	}

	store.failKeyPrefix = ""
	if _, err := deleteTenantExpiredRuns(ctx, pool, store, now, storeWindow, tenantRetentionRoundBudget); err != nil {
		t.Fatalf("retry after the fault cleared: %v", err)
	}
	if runExists(t, pool, runID) {
		t.Fatalf("retry did not delete the run")
	}
}

func TestDeleteTenantExpiredRunsSkipsWindowsAtOrAboveStore(t *testing.T) {
	t.Parallel()
	pool, now := tenantRetentionFixture(t)
	ctx := context.Background()
	setTenantRetention(t, pool, `{"retention":{"runs":"900d"}}`)

	runID := seedTenantRun(t, pool, testTenant, testJobPath, now.Add(-400*24*time.Hour), "COMPLETED")

	store := newTenantRetentionStore(t)
	deleted, err := deleteTenantExpiredRuns(ctx, pool, store, now, storeWindow, tenantRetentionRoundBudget)
	if err != nil {
		t.Fatalf("deleteTenantExpiredRuns: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 for a tenant window at or above the store window", deleted)
	}
	if !runExists(t, pool, runID) {
		t.Fatalf("run inside the store window was deleted")
	}
}

func TestRunMaintenanceRoundAppliesTenantRetention(t *testing.T) {
	t.Parallel()
	pool, now := tenantRetentionFixture(t)
	setTenantRetention(t, pool, `{"retention":{"runs":"7d"}}`)

	runID := seedTenantRun(t, pool, testTenant, testJobPath, now.Add(-10*24*time.Hour), "COMPLETED")
	store := newTenantRetentionStore(t)
	cfg := MaintenanceConfig{
		RunRetention:      storeWindow,
		LogRetention:      90 * 24 * time.Hour,
		ArtifactRetention: 30 * 24 * time.Hour,
		ActivityRetention: 30 * 24 * time.Hour,
		LogStore:          store,
		Now:               func() time.Time { return now },
	}
	ran, err := RunMaintenanceRound(context.Background(), pool, cfg)
	if err != nil {
		t.Fatalf("RunMaintenanceRound: %v", err)
	}
	if !ran {
		t.Fatalf("RunMaintenanceRound did not take the advisory lock")
	}
	if runExists(t, pool, runID) {
		t.Fatalf("round kept a run past its tenant window")
	}
}

func TestTenantRetentionReadsPolicy(t *testing.T) {
	t.Parallel()
	pool, _ := tenantRetentionFixture(t)
	ctx := context.Background()
	reader := NewReader(pool)

	got, err := reader.TenantRetention(ctx, testCluster, "no-such-tenant")
	if err != nil {
		t.Fatalf("TenantRetention for a missing row: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("missing row policy = %+v, want zero", got)
	}

	setTenantRetention(t, pool,
		`{"retention":{"runs":"30d","jobs":[{"job":"pr-*","runs":"7d"}]}}`)
	got, err = reader.TenantRetention(ctx, testCluster, testTenant)
	if err != nil {
		t.Fatalf("TenantRetention: %v", err)
	}
	if got.Runs != 30*24*time.Hour || len(got.Jobs) != 1 || got.Jobs[0].Job != "pr-*" || got.Jobs[0].Runs != 7*24*time.Hour {
		t.Fatalf("TenantRetention = %+v, want runs 30d and one pr-* rule of 7d", got)
	}
}
