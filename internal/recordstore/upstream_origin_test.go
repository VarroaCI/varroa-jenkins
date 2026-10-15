package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedUpstreamOriginFor stores the origin row for intentKey that names the
// child run of tenant and jobPath, the way the recorder does before it
// publishes the key.
func seedUpstreamOriginFor(t *testing.T, pool *pgxpool.Pool, intentKey, tenant, jobPath string) {
	t.Helper()
	if err := recordUpstreamOrigin(context.Background(), pool, intentKey, testCluster, tenant, jobPath); err != nil {
		t.Fatalf("record upstream origin: %v", err)
	}
}

// seedUpstreamOrigin stores the origin row for intentKey that names the
// child run of the test coordinates.
func seedUpstreamOrigin(t *testing.T, pool *pgxpool.Pool, intentKey string) {
	t.Helper()
	seedUpstreamOriginFor(t, pool, intentKey, testTenant, testJobPath)
}

// countUpstreamOrigins counts the origin rows that intentKey holds.
func countUpstreamOrigins(t *testing.T, pool *pgxpool.Pool, intentKey string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM varroa.upstream_origins WHERE intent_key = $1`, intentKey).Scan(&n); err != nil {
		t.Fatalf("count upstream origins: %v", err)
	}
	return n
}

// TestApplyRunStartedForgedUpstreamKeyStampsNoLink asserts a child that
// claims an upstream key naming a run of another tenant stores no upstream
// link, when the recorder wrote no origin row for the key.
func TestApplyRunStartedForgedUpstreamKeyStampsNoLink(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	foreignID := uuid.Must(uuid.NewV7())
	_, created, err := parseRunUUIDv7(foreignID.String())
	if err != nil {
		t.Fatalf("parse foreign run id: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, build_number, status)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', 'zeta', 'zeta/app', 9, 'COMPLETED')
	`, foreignID, created, "manual:forged-foreign", testCluster, testNamespace); err != nil {
		t.Fatalf("seed foreign run: %v", err)
	}

	forgedKey := "upstream:" + foreignID.String() + ":s1"
	childID := seedGrantedIntent(t, pool, forgedKey)
	childNumber := int64(1)
	mustApply(t, g, newTestEnvelope(t, childID, forgedKey, KindRunStarted, RunStartedPayload{BuildNumber: &childNumber}))

	got := readUpstreamColumns(t, db, childID)
	if got.runID.Valid || got.tenant.Valid || got.job.Valid || got.number.Valid {
		t.Fatalf("upstream columns = %+v, want all NULL for a key with no origin row", got)
	}
}

// TestApplyRunStartedOriginForOtherJobStampsNoLink asserts an origin row
// names one child job. A child of another job with the same key stores no
// upstream link.
func TestApplyRunStartedOriginForOtherJobStampsNoLink(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	upstreamID := seedGrantedIntent(t, pool, "manual:other-job-producer")
	upNumber := int64(5)
	mustApply(t, g, newTestEnvelope(t, upstreamID, "manual:other-job-producer", KindRunStarted, RunStartedPayload{BuildNumber: &upNumber}))

	key := "upstream:" + upstreamID.String() + ":s1"
	seedUpstreamOriginFor(t, pool, key, testTenant, "acme/other")

	childID := seedGrantedIntent(t, pool, key)
	childNumber := int64(1)
	mustApply(t, g, newTestEnvelope(t, childID, key, KindRunStarted, RunStartedPayload{BuildNumber: &childNumber}))

	got := readUpstreamColumns(t, db, childID)
	if got.runID.Valid {
		t.Fatalf("upstream_run_id = %q, want NULL when the origin names another job", got.runID.String)
	}
}

// TestSweepUpstreamOriginsDeletesRowsBeforeOldestPartition asserts the
// sweep deletes an origin row older than the oldest runs partition and
// keeps a fresh row.
func TestSweepUpstreamOriginsDeletesRowsBeforeOldestPartition(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()

	stale := "upstream:" + uuid.Must(uuid.NewV7()).String() + ":s1"
	fresh := "upstream:" + uuid.Must(uuid.NewV7()).String() + ":s1"
	if _, err := pool.Exec(ctx, `
		INSERT INTO varroa.upstream_origins (intent_key, cluster, tenant, job_path, created_at)
		VALUES ($1, $2, $3, $4, '2000-01-01T00:00:00Z')
	`, stale, testCluster, testTenant, testJobPath); err != nil {
		t.Fatalf("seed stale origin: %v", err)
	}
	seedUpstreamOrigin(t, pool, fresh)

	deleted, err := sweepUpstreamOrigins(ctx, pool)
	if err != nil {
		t.Fatalf("sweepUpstreamOrigins: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if n := countUpstreamOrigins(t, pool, stale); n != 0 {
		t.Fatalf("stale origin rows = %d, want 0", n)
	}
	if n := countUpstreamOrigins(t, pool, fresh); n != 1 {
		t.Fatalf("fresh origin rows = %d, want 1", n)
	}
}

// TestRecordUpstreamOriginKeepsFirstRow asserts a second write for the same
// key does not change the stored row.
func TestRecordUpstreamOriginKeepsFirstRow(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	key := "upstream:" + uuid.Must(uuid.NewV7()).String() + ":s1"
	seedUpstreamOriginFor(t, pool, key, testTenant, testJobPath)
	seedUpstreamOriginFor(t, pool, key, "zeta", "zeta/app")

	var tenant, job string
	if err := pool.QueryRow(context.Background(),
		`SELECT tenant, job_path FROM varroa.upstream_origins WHERE intent_key = $1`, key).Scan(&tenant, &job); err != nil {
		t.Fatalf("read origin: %v", err)
	}
	if tenant != testTenant || job != testJobPath {
		t.Fatalf("origin = %q/%q, want %q/%q", tenant, job, testTenant, testJobPath)
	}
}

// TestApplyRunStartedRetryOfOtherTenantCopiesNothing asserts a retry copies
// the upstream columns of its source only when the source belongs to the
// retry's own tenant.
func TestApplyRunStartedRetryOfOtherTenantCopiesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	sourceID := uuid.Must(uuid.NewV7())
	_, created, err := parseRunUUIDv7(sourceID.String())
	if err != nil {
		t.Fatalf("parse source run id: %v", err)
	}
	upstreamID := uuid.Must(uuid.NewV7()).String()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, build_number, status,
			upstream_run_id, upstream_tenant, upstream_job, upstream_number)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', 'zeta', 'zeta/app', 9, 'COMPLETED', $6, 'zeta', 'zeta/app', 3)
	`, sourceID, created, "manual:foreign-source", testCluster, testNamespace, upstreamID); err != nil {
		t.Fatalf("seed foreign source run: %v", err)
	}

	retryKey := "retry:" + sourceID.String()
	retryID := seedGrantedIntent(t, pool, retryKey)
	retryNumber := int64(2)
	mustApply(t, g, newTestEnvelope(t, retryID, retryKey, KindRunStarted, RunStartedPayload{BuildNumber: &retryNumber}))

	got := readUpstreamColumns(t, db, retryID)
	if got.runID.Valid || got.tenant.Valid || got.job.Valid || got.number.Valid {
		t.Fatalf("retry of another tenant's run copied %+v, want all NULL", got)
	}
}

// insertCallerIntent stores the run_intents row that the ledger writes when
// it grants runID to tenant and jobPath in the core cluster.
func insertCallerIntent(t *testing.T, db *sql.DB, runID uuid.UUID, tenant, jobPath string) {
	t.Helper()
	insertCallerIntentIn(t, db, runID, "core", tenant, jobPath)
}

// insertCallerIntentIn stores the run_intents row that the ledger writes
// when it grants runID to cluster, tenant, and jobPath.
func insertCallerIntentIn(t *testing.T, db *sql.DB, runID uuid.UUID, cluster, tenant, jobPath string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_intents (intent_key, run_id, created_at, cluster, namespace, tenant, job_path, state)
		VALUES ($1, $2, $3, $4, 'ns-a', $5, $6, 'GRANTED')
	`, "manual:"+runID.String(), runID, time.Now().UTC(), cluster, tenant, jobPath); err != nil {
		t.Fatalf("seed caller intent: %v", err)
	}
}

// originBuildRequest is one cross-tenant build of alpha/release from the
// caller run callerRunID, whose own tenant and job path the caller names.
func originBuildRequest(callerRunID, callerTenant, callerJobPath string) CrossTenantRequest {
	return CrossTenantRequest{
		Cluster:             "core",
		Namespace:           "ns-a",
		Cell:                "cell-a",
		CallerTenant:        callerTenant,
		PlacementGeneration: 1,
		Kind:                CrossTenantKindBuild,
		CallerRunID:         callerRunID,
		CallerJobPath:       callerJobPath,
		Seq:                 "s1",
		TargetTenant:        "alpha",
		JobPath:             "release",
		Params:              json.RawMessage(`{}`),
	}
}

// countOriginsFor counts the origin rows that intentKey holds in db.
func countOriginsFor(t *testing.T, db *sql.DB, intentKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.upstream_origins WHERE intent_key = $1`, intentKey).Scan(&n); err != nil {
		t.Fatalf("count upstream origins: %v", err)
	}
	return n
}

// TestCrossTenantBuild_VerifiedCallerRunWritesOrigin asserts a build from a
// caller run that the ledger granted to the caller's own job writes the
// origin row for the child run, and publishes the build.
func TestCrossTenantBuild_VerifiedCallerRunWritesOrigin(t *testing.T) {
	t.Parallel()

	deps, db := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)
	callerRun := uuid.Must(uuid.NewV7())
	insertCallerIntent(t, db, callerRun, "alpha", "alpha/caller")

	if _, err := handleCrossTenantBuild(context.Background(), deps, originBuildRequest(callerRun.String(), "alpha", "alpha/caller")); err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}

	key := "upstream:" + callerRun.String() + ":s1"
	var tenant, job string
	if err := db.QueryRow(`SELECT tenant, job_path FROM varroa.upstream_origins WHERE intent_key = $1`, key).Scan(&tenant, &job); err != nil {
		t.Fatalf("read origin row: %v", err)
	}
	if tenant != "alpha" || job != "alpha/release" {
		t.Fatalf("origin = %q/%q, want alpha/alpha/release", tenant, job)
	}
	select {
	case action := <-actions:
		if action.IntentKey != key {
			t.Fatalf("intent_key = %q, want %q", action.IntentKey, key)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the published trigger_run action")
	}
}

// TestCrossTenantBuild_ForeignCallerRunWritesNoOrigin asserts a caller run
// that belongs to another tenant still publishes the build, but writes no
// origin row, so the child run stores no upstream link.
func TestCrossTenantBuild_ForeignCallerRunWritesNoOrigin(t *testing.T) {
	t.Parallel()

	deps, db := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)
	callerRun := uuid.Must(uuid.NewV7())
	insertCallerIntent(t, db, callerRun, "beta", "beta/caller")

	if _, err := handleCrossTenantBuild(context.Background(), deps, originBuildRequest(callerRun.String(), "alpha", "alpha/caller")); err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}

	key := "upstream:" + callerRun.String() + ":s1"
	if n := countOriginsFor(t, db, key); n != 0 {
		t.Fatalf("origin rows = %d, want 0 for a caller run of another tenant", n)
	}
	select {
	case <-actions:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the published trigger_run action")
	}
}

// TestCrossTenantBuild_CallerRunInOtherClusterWritesNoOrigin asserts a caller
// run of the same tenant and job in another cluster writes no origin row.
func TestCrossTenantBuild_CallerRunInOtherClusterWritesNoOrigin(t *testing.T) {
	t.Parallel()

	deps, db := buildTestDeps(t)
	actions := subscribeLedgerActions(t, deps)
	callerRun := uuid.Must(uuid.NewV7())
	insertCallerIntentIn(t, db, callerRun, "edge", "alpha", "alpha/caller")

	if _, err := handleCrossTenantBuild(context.Background(), deps, originBuildRequest(callerRun.String(), "alpha", "alpha/caller")); err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}

	key := "upstream:" + callerRun.String() + ":s1"
	if n := countOriginsFor(t, db, key); n != 0 {
		t.Fatalf("origin rows = %d, want 0 for a caller run of another cluster", n)
	}
	select {
	case <-actions:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the published trigger_run action")
	}
}

// TestApplyRunStartedOriginInOtherClusterStampsNoLink asserts an origin row
// names one cluster. A child of the same tenant and job claimed in another
// cluster stores no upstream link.
func TestApplyRunStartedOriginInOtherClusterStampsNoLink(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	upstreamID := seedGrantedIntent(t, pool, "manual:other-cluster-producer")
	upNumber := int64(5)
	mustApply(t, g, newTestEnvelope(t, upstreamID, "manual:other-cluster-producer", KindRunStarted, RunStartedPayload{BuildNumber: &upNumber}))

	key := "upstream:" + upstreamID.String() + ":s1"
	if err := recordUpstreamOrigin(context.Background(), pool, key, "edge", testTenant, testJobPath); err != nil {
		t.Fatalf("record upstream origin: %v", err)
	}

	childID := seedGrantedIntent(t, pool, key)
	childNumber := int64(1)
	mustApply(t, g, newTestEnvelope(t, childID, key, KindRunStarted, RunStartedPayload{BuildNumber: &childNumber}))

	got := readUpstreamColumns(t, db, childID)
	if got.runID.Valid {
		t.Fatalf("upstream_run_id = %q, want NULL when the origin names another cluster", got.runID.String)
	}
}

// TestCrossTenantBuild_ForwardWritesNoOrigin asserts an intra-tenant forward
// keeps its supplied key and writes no origin row.
func TestCrossTenantBuild_ForwardWritesNoOrigin(t *testing.T) {
	t.Parallel()

	deps, db := buildForwardTestDeps(t)
	key := "upstream:" + forwardCallerRunID + ":s1"
	if _, err := handleCrossTenantBuild(context.Background(), deps, forwardRequest(key)); err != nil {
		t.Fatalf("handleCrossTenantBuild: %v", err)
	}
	if n := countOriginsFor(t, db, key); n != 0 {
		t.Fatalf("origin rows = %d, want 0 for a forward", n)
	}
}

// TestPurgeTenant_DeletesOnlyTheTenantsUpstreamOrigins asserts a tenant purge
// deletes the origin rows of the purged tenant and keeps the others.
func TestPurgeTenant_DeletesOnlyTheTenantsUpstreamOrigins(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	purgedKey := "upstream:" + uuid.Must(uuid.NewV7()).String() + ":s1"
	keptKey := "upstream:" + uuid.Must(uuid.NewV7()).String() + ":s1"
	seedUpstreamOriginFor(t, pool, purgedKey, "payments", "payments/build")
	seedUpstreamOriginFor(t, pool, keptKey, "billing", "billing/build")

	if err := purgeTenant(context.Background(), slog.Default(), pool,
		TenantPurgeRequest{Cluster: testCluster, Tenant: "payments", PlacementGeneration: 1}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := countOriginsFor(t, db, purgedKey); n != 0 {
		t.Fatalf("purged tenant keeps %d origin rows, want 0", n)
	}
	if n := countOriginsFor(t, db, keptKey); n != 1 {
		t.Fatalf("other tenant has %d origin rows, want 1", n)
	}
}
