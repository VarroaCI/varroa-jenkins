package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// markedLostOwner is the cell instance every marked-intent test names as
// the owner that marked the start and died.
const markedLostOwner = "pod-dead"

// seedMarkedIntent inserts one GRANTED intent for runID that owner
// marked, with build number 6. A zero createdAt takes the clock.
func seedMarkedIntent(t *testing.T, db *sql.DB, runID uuid.UUID, createdAt time.Time, owner, paramsDigest string, marked bool) {
	t.Helper()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var digest, markedAt any
	if paramsDigest != "" {
		digest = paramsDigest
	}
	if marked {
		markedAt = time.Now().UTC().Add(-2 * time.Minute)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.run_intents (
			intent_key, run_id, created_at, cluster, namespace, tenant, job_path,
			cell, cell_instance, placement_generation, state, actor, params_digest, build_number,
			start_marked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'cell-a', $8, 1, 'GRANTED', 'alice', $9, 6, $10)
	`, "manual:"+runID.String(), runID, createdAt, testCluster, testNamespace, testTenant, testJobPath,
		owner, digest, markedAt); err != nil {
		t.Fatalf("seed marked intent: %v", err)
	}
}

// seedMarkedPlacement inserts the tenant's placement row with
// markedLostOwner listed dead, confirmed gone past the grace when
// confirmed is true.
func seedMarkedPlacement(t *testing.T, db *sql.DB, confirmed bool, policy string) {
	t.Helper()
	now := time.Now().UTC()
	entry := TenantSyncDeadInstance{Instance: markedLostOwner, LastSeenAt: now.Add(-time.Minute)}
	if confirmed {
		at := now.Add(-45 * time.Second)
		entry.ConfirmedGoneAt = &at
	}
	seedPlacementLivenessWithPolicy(t, db, "pod-new", []TenantSyncDeadInstance{entry}, policy)
}

// runEndMarked runs one round of EndMarkedLostIntents against a fresh
// consumer.
func runEndMarked(t *testing.T, pool *pgxpool.Pool, conn *bus.Conn, notifier *RunNotifier,
	metrics *InterruptedMetrics) int {
	t.Helper()
	ended, err := EndMarkedLostIntents(context.Background(), pool, conn, notifier, metrics, nil, nil,
		DefaultSweepMaxPending, time.Now().UTC(), testGrace, nil)
	if err != nil {
		t.Fatalf("EndMarkedLostIntents: %v", err)
	}
	return ended
}

// countRunRows counts the runs rows that hold runID.
func countRunRows(t *testing.T, db *sql.DB, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count runs rows: %v", err)
	}
	return n
}

// countInterruptedActivity counts the build.interrupted activity rows.
func countInterruptedActivity(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'build.interrupted'`).Scan(&n); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	return n
}

// TestEndMarkedLostIntentEndsTheRun asserts the issue's case: a marked
// GRANTED intent whose owner is confirmed gone and whose start event was
// lost gets one INTERRUPTED run record with the reason cell-rolled, one
// activity row, one notification, and one automatic retry. A second
// round writes nothing.
func TestEndMarkedLostIntentEndsTheRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, time.Time{}, markedLostOwner, "", true)
	seedMarkedPlacement(t, db, true, `{"interruptedRetry": "automatic"}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	received := collectActions(t, conn)
	notifier, sent, _ := newCaptureNotifier(t)

	if ended := runEndMarked(t, pool, conn, notifier, nil); ended != 1 {
		t.Fatalf("ended = %d, want 1", ended)
	}

	var (
		status, result, reason string
		buildNumber            sql.NullInt64
		accepted               bool
		startedAt              sql.NullTime
	)
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason', build_number,
		       COALESCE(payload ? 'accepted_at', false), started_at
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason, &buildNumber, &accepted, &startedAt); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" || reason != ReasonCellRolled {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result, reason, ReasonCellRolled)
	}
	if !buildNumber.Valid || buildNumber.Int64 != 6 {
		t.Fatalf("build_number = %v, want 6", buildNumber)
	}
	if accepted {
		t.Fatal("the row holds the accepted keys, want a row that records a build")
	}
	if !startedAt.Valid {
		t.Fatal("started_at is null, want the start mark time")
	}
	if n := countInterruptedActivity(t, db); n != 1 {
		t.Fatalf("build.interrupted rows = %d, want 1", n)
	}
	if sent.count() != 1 {
		t.Fatalf("notifications = %d, want 1", sent.count())
	}
	if got := sent.payloads[0]; got.RunID != runID.String() || got.Status != StatusCompleted || got.Result != "INTERRUPTED" {
		t.Fatalf("notification = %+v, want the run COMPLETED with INTERRUPTED", got)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionRetryRun || a.IntentKey != "retry:"+runID.String() {
			t.Fatalf("action = %+v, want one retry_run for the run", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("want one retry_run action")
	}

	if ended := runEndMarked(t, pool, conn, notifier, nil); ended != 0 {
		t.Fatalf("second round ended = %d, want 0", ended)
	}
	if n := countInterruptedActivity(t, db); n != 1 {
		t.Fatalf("build.interrupted rows after the second round = %d, want 1", n)
	}
	if sent.count() != 1 {
		t.Fatalf("notifications after the second round = %d, want 1", sent.count())
	}
	select {
	case a := <-received:
		t.Fatalf("second round published %+v, want nothing", a)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestEndMarkedLostIntentLeavesAnUnconfirmedOwner asserts that a dead
// entry with no pod-read confirmation ends nothing.
func TestEndMarkedLostIntentLeavesAnUnconfirmedOwner(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, time.Time{}, markedLostOwner, "", true)
	seedMarkedPlacement(t, db, false, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	if ended := runEndMarked(t, pool, conn, nil, nil); ended != 0 {
		t.Fatalf("ended = %d, want 0", ended)
	}
	if n := countRunRows(t, db, runID); n != 0 {
		t.Fatalf("runs rows = %d, want 0", n)
	}
}

// TestEndMarkedLostIntentSkipsAnUnmarkedIntent asserts that an unmarked
// intent stays with the re-fire and never reaches this end.
func TestEndMarkedLostIntentSkipsAnUnmarkedIntent(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, time.Time{}, markedLostOwner, "", false)
	seedMarkedPlacement(t, db, true, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	if ended := runEndMarked(t, pool, conn, nil, nil); ended != 0 {
		t.Fatalf("ended = %d, want 0", ended)
	}
	if n := countRunRows(t, db, runID); n != 0 {
		t.Fatalf("runs rows = %d, want 0", n)
	}
	candidates, err := selectRefireCandidates(context.Background(), pool, testCluster, testTenant)
	if err != nil {
		t.Fatalf("selectRefireCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].runID != runID.String() {
		t.Fatalf("re-fire candidates = %+v, want the unmarked intent", candidates)
	}
}

// TestEndMarkedLostIntentSupersedesAnAcceptedRow asserts that a QUEUED
// row which records no build becomes the run's RUNNING record before the
// mark, so the interrupted sweep never marks it as a queued run.
func TestEndMarkedLostIntentSupersedesAnAcceptedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	createdAt := time.Now().UTC().Add(-3 * time.Minute)
	runID := seedAcceptedOnlyRunRowInstance(t, db, markedLostOwner, createdAt)
	seedMarkedIntent(t, db, runID, createdAt, markedLostOwner, "", true)
	seedMarkedPlacement(t, db, true, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	if ended := runEndMarked(t, pool, conn, nil, nil); ended != 1 {
		t.Fatalf("ended = %d, want 1", ended)
	}
	var (
		reason   string
		accepted bool
	)
	if err := db.QueryRow(`
		SELECT payload->>'interrupted_reason', COALESCE(payload ? 'accepted_at', false)
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&reason, &accepted); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if reason != ReasonCellRolled || accepted {
		t.Fatalf("reason=%q accepted=%v, want %q and no accepted keys", reason, accepted, ReasonCellRolled)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, nil, nil, nil,
		DefaultSweepMaxPending, time.Now().UTC(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 0 {
		t.Fatalf("sweep marked = %d, want 0", marked)
	}
	if n := countInterruptedActivity(t, db); n != 1 {
		t.Fatalf("build.interrupted rows = %d, want 1", n)
	}
}

// TestEndMarkedLostIntentSkipsTheRetryOfAParameterizedIntent asserts that
// the run ends and no retry runs with declared defaults.
func TestEndMarkedLostIntentSkipsTheRetryOfAParameterizedIntent(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader, restore := swapMeterProvider(t)
	defer restore()
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, time.Time{}, markedLostOwner, "abc123", true)
	seedMarkedPlacement(t, db, true, `{"interruptedRetry": "automatic"}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	received := collectActions(t, conn)

	if ended := runEndMarked(t, pool, conn, nil, metrics); ended != 1 {
		t.Fatalf("ended = %d, want 1", ended)
	}
	select {
	case a := <-received:
		t.Fatalf("published %+v, want no retry for a parameterized intent", a)
	case <-time.After(300 * time.Millisecond):
	}
	if got := sumCounterByAttr(t, reader, "recorder_retry_skipped_total", "reason", retrySkipParameterized); got != 1 {
		t.Fatalf("recorder_retry_skipped_total{reason=parameterized} = %d, want 1", got)
	}
}

// TestSelectMarkedLostIntentsReadsOnlyConfirmedOwners asserts that an
// intent whose owner holds no confirmation takes no place in the batch,
// so it never holds a confirmed loss out of it.
func TestSelectMarkedLostIntentsReadsOnlyConfirmedOwners(t *testing.T) {
	pool, db := setupLedgerPool(t)
	confirmed := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, confirmed, time.Time{}, markedLostOwner, "", true)
	if _, err := db.Exec(`UPDATE varroa.run_intents SET build_number = 5 WHERE run_id = $1`, confirmed); err != nil {
		t.Fatalf("free build number 6: %v", err)
	}
	seedMarkedIntent(t, db, uuid.Must(uuid.NewV7()), time.Time{}, "pod-alive", "", true)
	seedMarkedPlacement(t, db, true, `{}`)

	got, err := selectMarkedLostIntents(context.Background(), pool, time.Now().UTC().Add(-testGrace))
	if err != nil {
		t.Fatalf("selectMarkedLostIntents: %v", err)
	}
	if len(got) != 1 || got[0].runID != confirmed {
		t.Fatalf("candidates = %+v, want only the confirmed owner's intent", got)
	}
}

// TestExpireUnclaimedRunsSkipsAMarkedIntent asserts that the
// claim-abandoned class leaves a run whose intent holds a start mark.
func TestExpireUnclaimedRunsSkipsAMarkedIntent(t *testing.T) {
	pool, db := setupLedgerPool(t)
	accepted := claimExpiryNow.Add(-ClaimExpiryAge - time.Minute)
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-a", accepted)
	seedMarkedIntent(t, db, runID, accepted, "pod-a", "", true)

	n, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("expire unclaimed runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired = %d, want 0", n)
	}
	if status, _ := readStatusResult(t, db, runID); status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED", status)
	}

	if _, err := db.Exec(`UPDATE varroa.run_intents SET start_marked_at = NULL WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("clear the start mark: %v", err)
	}
	n, err = ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("expire unclaimed runs: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired without the mark = %d, want 1", n)
	}
	payload := readRunPayload(t, db, runID.String())
	var code string
	_ = json.Unmarshal(payload["trigger_reason_code"], &code)
	if code != ReasonClaimAbandoned {
		t.Fatalf("trigger_reason_code = %q, want %q", code, ReasonClaimAbandoned)
	}
}
