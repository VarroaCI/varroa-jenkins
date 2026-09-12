package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedSyntheticTriggerFailedRun inserts one TRIGGER_FAILED runs row for
// jobPath, exactly as applyTriggerFailed writes one: no build number, and no
// run_intents row unless the caller seeded one.
func seedSyntheticTriggerFailedRun(t *testing.T, db *sql.DB, jobPath string, createdAt time.Time) uuid.UUID {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, ended_at, actor, payload
		) VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'COMPLETED', 'TRIGGER_FAILED', $2, 'user:alice',
			jsonb_build_object('trigger_reason_code', 'JOB_NOT_FOUND'))
	`, runID, createdAt, "manual:"+runID.String(), testCluster, testNamespace, testTenant, jobPath); err != nil {
		t.Fatalf("seed TRIGGER_FAILED run: %v", err)
	}
	return runID
}

// seedSyntheticRowForRun inserts a TRIGGER_FAILED row under an existing run
// identifier, which is the repeat-client-token case: the ledger already holds
// that run's intent.
func seedSyntheticRowForRun(t *testing.T, db *sql.DB, runID uuid.UUID, createdAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, ended_at, payload
		) VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'COMPLETED', 'TRIGGER_FAILED', $2,
			jsonb_build_object('trigger_reason_code', 'JOB_NOT_FOUND'))
	`, runID, createdAt, "manual:"+runID.String(), testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed TRIGGER_FAILED row for run: %v", err)
	}
}

// TestRehydrationSeedSkipsATriggerFailedRun asserts the rehydration seed
// carries only the real build. A synthetic row holds a null build number, and
// the mite refuses a seeded build numbered zero, which fails the whole bundle.
func TestRehydrationSeedSkipsATriggerFailedRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	jobPath := "acme/seeded"
	base := time.Now().UTC().Add(-2 * time.Hour)
	buildNumber := int64(11)
	seedRehydrationRun(t, db, jobPath, &buildNumber, base, "SUCCESS")
	// The refused trigger is newer, so it would otherwise take the newest
	// slot of every per-job budget.
	seedSyntheticTriggerFailedRun(t, db, jobPath, base.Add(time.Hour))

	records, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, []string{jobPath}, 5)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if got := len(records[0].Records); got != 1 {
		t.Fatalf("seeded records = %d, want only the real build", got)
	}
	rec := records[0].Records[0]
	if rec.BuildNumber != buildNumber {
		t.Fatalf("build number = %d, want %d", rec.BuildNumber, buildNumber)
	}
	if rec.Result != "SUCCESS" {
		t.Fatalf("result = %q, want SUCCESS", rec.Result)
	}
}

// TestRehydrationEnumerationSkipsATriggerFailedOnlyJob asserts a job whose
// only record is a refused trigger reaches no seed at all. It holds no
// job_numbers row either, because the trigger reached no claim gate.
func TestRehydrationEnumerationSkipsATriggerFailedOnlyJob(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	buildNumber := int64(3)
	seedRehydrationRun(t, db, "acme/real", &buildNumber, base, "SUCCESS")
	seedSyntheticTriggerFailedRun(t, db, "acme/refused", base)

	records, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, nil, 5)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}
	for _, r := range records {
		if r.JobPath == "acme/refused" {
			t.Fatalf("the enumeration carries a job whose only record is a refused trigger: %+v", r)
		}
	}
}

// TestClaimMovesOwnershipPastATriggerFailedRow asserts a repeat claim from a
// new cell instance still moves the intent's ownership. A synthetic row is not
// a recorded build, so it must not make the ledger keep the old owner and then
// judge the real completion foreign.
func TestClaimMovesOwnershipPastATriggerFailedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:owner-move-1", 0)
	granted := mustGrant(t, ledger, req)
	runID := uuid.MustParse(granted.RunID)

	_, createdAt, err := parseRunUUIDv7(granted.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	seedSyntheticRowForRun(t, db, runID, createdAt)

	// The tenant rolls onto a new cell instance and the caller repeats its
	// client token, so the ledger sees the same run identifier again.
	repeat := req
	repeat.CellInstance = "pod-b"
	repeat.Cell = "cell-b"
	again := ledger.Claim(ctx, repeat)
	if again.Decision != DecisionGranted {
		t.Fatalf("repeat claim = %+v, want granted", again)
	}
	if again.RunID != granted.RunID {
		t.Fatalf("repeat claim run id = %q, want %q", again.RunID, granted.RunID)
	}

	var cellInstance, cell string
	if err := db.QueryRow(`
		SELECT cell_instance, cell FROM varroa.run_intents WHERE run_id = $1
	`, runID).Scan(&cellInstance, &cell); err != nil {
		t.Fatalf("read intent ownership: %v", err)
	}
	if cellInstance != "pod-b" || cell != "cell-b" {
		t.Fatalf("intent ownership = %q/%q, want cell-b/pod-b", cell, cellInstance)
	}
}

// TestFinalizeStaleLogsSkipsATriggerFailedRun asserts the log-finalization
// sweep writes no log summary onto a refused trigger. Such a run holds no log,
// and a summary marked incomplete would survive a later supersede and mislabel
// the real run's log.
func TestFinalizeStaleLogsSkipsATriggerFailedRun(t *testing.T) {
	pool, db := setupLedgerPool(t)

	runID := seedSyntheticTriggerFailedRun(t, db, testJobPath, time.Now().UTC().Add(-2*time.Hour))
	if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`,
		time.Now().UTC().Add(-2*time.Hour), runID); err != nil {
		t.Fatalf("backdate ended_at: %v", err)
	}

	n, err := finalizeStaleLogs(context.Background(), pool, fakeLiveness{gone: true}, time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}
	if n != 0 {
		t.Fatalf("finalized %d runs, want 0", n)
	}
	if found, _ := readLogSummaryFinalized(t, db, runID); found {
		t.Fatal("the sweep wrote a log summary onto a refused trigger")
	}
}

// --- The same-token-retry edge: an intent and a synthetic row for one run ---
//
// A caller who repeats a client token reuses the run identifier. The trigger
// can fail once, write the synthetic row, and then succeed on the retry. Every
// reader below sees both rows for a short window, before the real run.started
// supersedes. None may read the synthetic row as a recorded build.

// TestOwnerLookupIgnoresATriggerFailedRow asserts the owner lookup reports no
// run state for a synthetic row. Its COMPLETED status and its failure time
// would otherwise become the real run's log-finalization anchor, and the first
// log chunk could then miss a deadline that already passed.
func TestOwnerLookupIgnoresATriggerFailedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:owner-anchor-1", 0)
	granted := mustGrant(t, ledger, req)
	runID := uuid.MustParse(granted.RunID)
	_, createdAt, err := parseRunUUIDv7(granted.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	seedSyntheticRowForRun(t, db, runID, createdAt)

	resp, err := lookupOwner(ctx, pool, granted.RunID)
	if err != nil {
		t.Fatalf("lookupOwner: %v", err)
	}
	if !resp.Found {
		t.Fatal("want found=true: the intent row exists")
	}
	if resp.Status != "" {
		t.Fatalf("status = %q, want empty: a refused trigger records no run", resp.Status)
	}
	if resp.AnchorUnixMs != 0 {
		t.Fatalf("anchor = %d, want 0: a refused trigger supplies no finalize anchor", resp.AnchorUnixMs)
	}

	// The real build then starts and supersedes the row.
	g := NewIngester(pool, nil, nil)
	started := newTestEnvelope(t, runID, "manual:owner-anchor-1", KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(ctx, started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	resp, err = lookupOwner(ctx, pool, granted.RunID)
	if err != nil {
		t.Fatalf("lookupOwner after run.started: %v", err)
	}
	if resp.Status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING", resp.Status)
	}
}

// TestSweepIntentsDeletesAnOrphanPastATriggerFailedRow asserts the
// orphan-intent rule keeps its meaning: a synthetic row is no recorded build,
// so a granted intent older than the orphan age still leaves the table.
func TestSweepIntentsDeletesAnOrphanPastATriggerFailedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:orphan-trigger-failed-1", 0)
	granted := mustGrant(t, ledger, req)
	runID := uuid.MustParse(granted.RunID)
	_, createdAt, err := parseRunUUIDv7(granted.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	seedSyntheticRowForRun(t, db, runID, createdAt)

	if err := EnsurePartitions(ctx, pool, time.Now().UTC(), 30*24*time.Hour); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}
	// Age the intent past the orphan window. The runs row stays where it is,
	// so only the orphan rule can delete the intent.
	if _, err := db.Exec(`UPDATE varroa.run_intents SET created_at = $1 WHERE run_id = $2`,
		time.Now().UTC().Add(-8*24*time.Hour), runID); err != nil {
		t.Fatalf("age the intent: %v", err)
	}

	if _, orphans, err := sweepIntents(ctx, pool, time.Now().UTC()); err != nil {
		t.Fatalf("sweepIntents: %v", err)
	} else if orphans != 1 {
		t.Fatalf("orphan intents deleted = %d, want 1", orphans)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count run_intents: %v", err)
	}
	if count != 0 {
		t.Fatalf("run_intents rows = %d, want 0", count)
	}
}

// TestCrossTenantRunStateIgnoresATriggerFailedRow asserts a cross-tenant status
// poll reads a run whose only row is synthetic as queued, never as completed.
// The run identifier repeats when the operator redelivers the same trigger
// action, so the intent can arrive after the refused trigger.
func TestCrossTenantRunStateIgnoresATriggerFailedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:cross-state-1", 0)
	granted := mustGrant(t, ledger, req)
	runID := uuid.MustParse(granted.RunID)
	_, createdAt, err := parseRunUUIDv7(granted.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	seedSyntheticRowForRun(t, db, runID, createdAt)

	rec, found, err := readCrossTenantRunRecord(ctx, pool, granted.RunID)
	if err != nil {
		t.Fatalf("readCrossTenantRunRecord: %v", err)
	}
	if !found {
		t.Fatal("want found=true: the intent row exists")
	}
	if got := crossTenantRunState(rec); got != CrossTenantStateQueued {
		t.Fatalf("state = %q, want %q", got, CrossTenantStateQueued)
	}
}
