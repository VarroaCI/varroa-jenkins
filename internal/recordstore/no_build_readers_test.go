package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedAcceptedOnlyRunRow inserts one QUEUED runs row holding the two
// accepted keys, directly through SQL, exactly as applyTriggerAccepted
// writes one: no build number, and no run_intents row unless the caller
// seeded one. The cell is fixed to "cell-a" and the tenant to
// testTenant, since none of these tests vary either.
func seedAcceptedOnlyRunRow(t *testing.T, db *sql.DB, namespace, jobPath string, createdAt time.Time) uuid.UUID {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, actor, payload
		) VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'QUEUED', 'user:alice',
			jsonb_build_object('accepted_at', $2::timestamptz, 'accepted_instance', 'pod-a'))
	`, runID, createdAt, "manual:"+runID.String(), testCluster, namespace, testTenant, jobPath); err != nil {
		t.Fatalf("seed accepted-only run: %v", err)
	}
	return runID
}

// markRunInterruptedRaw marks runID interrupted directly through SQL,
// the shape the sweep's write produces, leaving the accepted keys in
// place, exactly as the marking transaction does.
func markRunInterruptedRaw(t *testing.T, db *sql.DB, runID uuid.UUID) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE varroa.runs
		SET status = 'COMPLETED',
		    result = 'INTERRUPTED',
		    ended_at = now(),
		    payload = payload || jsonb_build_object('interrupted_reason', 'cell-rolled')
		        || jsonb_build_object('log_summary', jsonb_build_object('finalized', true, 'incomplete', true, 'incomplete_reason', 'no-summary'))
		WHERE run_id = $1
	`, runID); err != nil {
		t.Fatalf("mark accepted-only row interrupted: %v", err)
	}
}

// TestRunsRowExistsExcludesAcceptedOnly asserts runsRowExists answers
// false for an accepted-only row, before and after the sweep marks it.
func TestRunsRowExistsExcludesAcceptedOnly(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	runID := seedAcceptedOnlyRunRow(t, db, testNamespace, testJobPath, time.Now().UTC())

	exists, err := runsRowExistsForTest(ctx, pool, runID)
	if err != nil {
		t.Fatalf("runsRowExists: %v", err)
	}
	if exists {
		t.Fatal("want false: an accepted-only row records no build")
	}

	markRunInterruptedRaw(t, db, runID)
	exists, err = runsRowExistsForTest(ctx, pool, runID)
	if err != nil {
		t.Fatalf("runsRowExists after mark: %v", err)
	}
	if exists {
		t.Fatal("want false after the mark: the accepted keys stay in place")
	}
}

// TestRunsRowExistsAcceptsHeartbeatFirstPlaceholder asserts a
// heartbeat-first placeholder, whose payload is null, answers true: the
// COALESCE guard must not exclude a live run with no payload at all.
func TestRunsRowExistsAcceptsHeartbeatFirstPlaceholder(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	runID := seedGrantedIntent(t, pool, "manual:heartbeat-first-1")

	g := NewIngester(pool, nil, nil)
	heartbeat := newTestEnvelope(t, runID, "manual:heartbeat-first-1", KindRunHeartbeat, map[string]any{})
	if _, err := g.Apply(context.Background(), heartbeat); err != nil {
		t.Fatalf("apply run.heartbeat: %v", err)
	}

	var payload sql.NullString
	if err := db.QueryRow(`SELECT payload FROM varroa.runs WHERE run_id = $1`, runID).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if payload.Valid {
		t.Fatalf("payload = %q, want null for a heartbeat-first placeholder", payload.String)
	}

	exists, err := runsRowExistsForTest(ctx, pool, runID)
	if err != nil {
		t.Fatalf("runsRowExists: %v", err)
	}
	if !exists {
		t.Fatal("want true: a null payload must not be excluded by the accepted-key guard")
	}
}

// runsRowExistsForTest opens a transaction to call the tx-scoped
// runsRowExists helper, and commits it: the tests here only read.
func runsRowExistsForTest(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return runsRowExists(ctx, tx, runID)
}

// TestOrphanDeleteRemovesAcceptedOnlyKeepsMarked asserts the orphan
// rule deletes a GRANTED intent whose only row is accepted-only, and
// keeps one whose row the sweep has marked.
func TestOrphanDeleteRemovesAcceptedOnlyKeepsMarked(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	if err := EnsurePartitions(ctx, pool, time.Now().UTC(), 30*24*time.Hour); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}

	// An accepted-only intent: the orphan rule must remove it.
	orphanReq := newTestClaimRequest("manual:orphan-accepted-1", 0)
	orphanGranted := mustGrant(t, ledger, orphanReq)
	orphanRunID := uuid.MustParse(orphanGranted.RunID)
	_, orphanCreatedAt, err := parseRunUUIDv7(orphanGranted.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, payload)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'QUEUED',
			jsonb_build_object('accepted_at', $2::timestamptz, 'accepted_instance', 'pod-a'))
	`, orphanRunID, orphanCreatedAt, orphanReq.IntentKey, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed accepted-only row for the orphan intent: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.run_intents SET created_at = $1 WHERE run_id = $2`,
		time.Now().UTC().Add(-8*24*time.Hour), orphanRunID); err != nil {
		t.Fatalf("age the orphan intent: %v", err)
	}

	// A marked intent: the orphan rule must keep it, because the marked
	// row is a real terminal record, not one that records no build.
	markedReq := newTestClaimRequest("manual:orphan-marked-1", 0)
	markedGranted := mustGrant(t, ledger, markedReq)
	markedRunID := uuid.MustParse(markedGranted.RunID)
	_, markedCreatedAt, err := parseRunUUIDv7(markedGranted.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, result, payload)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'COMPLETED', 'SUCCESS', '{}'::jsonb)
	`, markedRunID, markedCreatedAt, markedReq.IntentKey, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed a real terminal row: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.run_intents SET created_at = $1 WHERE run_id = $2`,
		time.Now().UTC().Add(-8*24*time.Hour), markedRunID); err != nil {
		t.Fatalf("age the marked intent: %v", err)
	}

	if _, orphans, err := sweepIntents(ctx, pool, time.Now().UTC()); err != nil {
		t.Fatalf("sweepIntents: %v", err)
	} else if orphans != 1 {
		t.Fatalf("orphan intents deleted = %d, want 1", orphans)
	}

	assertIntentRowCount(t, db, orphanReq.IntentKey, 0)
	assertIntentRowCount(t, db, markedReq.IntentKey, 1)
}

// TestRehydrationExcludesAcceptedOnly asserts the rehydration
// enumeration and record selection both exclude an accepted-only row.
func TestRehydrationExcludesAcceptedOnly(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	buildNumber := int64(11)
	seedRehydrationRun(t, db, "acme/real-accepted", &buildNumber, base, "SUCCESS")
	seedAcceptedOnlyRunRow(t, db, testNamespace, "acme/only-accepted", base)

	records, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, nil, 5)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}
	for _, r := range records {
		if r.JobPath == "acme/only-accepted" {
			t.Fatalf("the enumeration carries a job whose only record is accepted-only: %+v", r)
		}
	}

	perPath, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, []string{"acme/only-accepted"}, 5)
	if err != nil {
		t.Fatalf("ListRehydrationRecords for the accepted-only path: %v", err)
	}
	if len(perPath) != 1 || len(perPath[0].Records) != 0 {
		t.Fatalf("records = %+v, want an empty record list", perPath)
	}
}

// TestJobWindowStatsExcludesAcceptedOnly asserts JobWindowStats counts
// an accepted-only row in nothing.
func TestJobWindowStatsExcludesAcceptedOnly(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	jobPath := "acme/window-accepted"
	createdAt := time.Now().UTC().Add(-time.Hour)
	seedAcceptedOnlyRunRow(t, db, testNamespace, jobPath, createdAt)

	stats, err := reader.JobWindowStats(ctx, testCluster, testTenant, jobPath, createdAt.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("JobWindowStats: %v", err)
	}
	if stats.Builds != 0 {
		t.Fatalf("builds = %d, want 0", stats.Builds)
	}
	if stats.PassRate != nil {
		t.Fatalf("pass rate = %v, want none", *stats.PassRate)
	}
}

// TestJobSummaryExcludesAcceptedOnly asserts the job health dots, the
// last-run tile, and the duration trend all skip an accepted-only row,
// before and after the sweep marks it.
func TestJobSummaryExcludesAcceptedOnly(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	jobPath := "acme/summary-accepted"
	runID := seedAcceptedOnlyRunRow(t, db, testNamespace, jobPath, time.Now().UTC().Add(-time.Hour))

	assertNoJobHealth := func() {
		t.Helper()
		summaries, err := reader.ListJobSummaries(ctx, testCluster, testTenant)
		if err != nil {
			t.Fatalf("ListJobSummaries: %v", err)
		}
		for _, s := range summaries {
			if s.JobPath != jobPath {
				continue
			}
			if s.LastRun != nil {
				t.Fatal("an accepted-only row must be no last run")
			}
			if len(s.RecentResults) != 0 {
				t.Fatalf("recent results = %v, want none", s.RecentResults)
			}
			if len(s.DurationTrendMs) != 0 {
				t.Fatalf("duration trend = %v, want none", s.DurationTrendMs)
			}
		}
	}

	assertNoJobHealth()
	markRunInterruptedRaw(t, db, runID)
	assertNoJobHealth()
}

// TestCellUsageExcludesAcceptedOnly asserts the cell-usage runs and
// failed counts skip an accepted-only row.
func TestCellUsageExcludesAcceptedOnly(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	const cell = "cell-a"
	seedCellPlacement(t, db, cell, "acme")
	recent := time.Now().UTC().Add(-time.Hour)
	seedAcceptedOnlyRunRow(t, db, cellUsageNamespace, "acme/app", recent)

	usage, err := reader.CellUsage(ctx, testCluster, cellUsageNamespace, cell)
	if err != nil {
		t.Fatalf("CellUsage: %v", err)
	}
	wantCounts(t, tenantCounts(t, usage, "acme"), CellUsageCounts{})
}

// TestLookupOwnerExcludesAcceptedOnly asserts lookupOwner reports no
// status and no finalization anchor for an accepted-only row, and that a
// later run.started still resolves the real state.
func TestLookupOwnerExcludesAcceptedOnly(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	runID := seedGrantedIntent(t, pool, "manual:owner-accepted-1")
	g := NewIngester(pool, nil, nil)
	if _, err := g.Apply(ctx, newTriggerAcceptedEnvelope(t, runID.String(), "manual:owner-accepted-1")); err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}

	resp, err := lookupOwner(ctx, pool, runID.String())
	if err != nil {
		t.Fatalf("lookupOwner: %v", err)
	}
	if !resp.Found {
		t.Fatal("want found=true: the intent row exists")
	}
	if resp.Status != "" {
		t.Fatalf("status = %q, want empty: an accepted-only row records no run", resp.Status)
	}
	if resp.AnchorUnixMs != 0 {
		t.Fatalf("anchor = %d, want 0", resp.AnchorUnixMs)
	}

	started := newTestEnvelope(t, runID, "manual:owner-accepted-1", KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(ctx, started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	resp, err = lookupOwner(ctx, pool, runID.String())
	if err != nil {
		t.Fatalf("lookupOwner after run.started: %v", err)
	}
	if resp.Status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING", resp.Status)
	}
	_ = db
}

// TestListRunsGetRunSearchRecordsKeepAcceptedOnly asserts ListRuns,
// GetRun, and SearchRecords all keep an accepted-only row.
func TestListRunsGetRunSearchRecordsKeepAcceptedOnly(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	jobPath := "acme/keep-accepted"
	runID := seedAcceptedOnlyRunRow(t, db, testNamespace, jobPath, time.Now().UTC().Add(-time.Hour))

	page, err := reader.ListRuns(ctx, RunQuery{Cluster: testCluster, Tenant: testTenant, JobPath: jobPath, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 1 || page.Runs[0].RunID != runID.String() {
		t.Fatalf("ListRuns returned %d runs, want the accepted-only row", len(page.Runs))
	}

	run, _, err := reader.GetRun(ctx, runID.String())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.RunID != runID.String() {
		t.Fatalf("run_id = %q, want %q", run.RunID, runID)
	}
	if run.Status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED", run.Status)
	}
}
