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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	reader := useTestMeterProvider(t)
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
	t.Parallel()

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
	t.Parallel()

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

// staleMarkedOwner is the live cell instance the start-lost tests name. It
// equals newTestEnvelope's sender, so the owner's own events reach the row.
const staleMarkedOwner = "pod-a"

// ageStartMark moves runID's start mark age into the past.
func ageStartMark(t *testing.T, db *sql.DB, runID uuid.UUID, age time.Duration) {
	t.Helper()
	if _, err := db.Exec(`UPDATE varroa.run_intents SET start_marked_at = $2 WHERE run_id = $1`,
		runID, time.Now().UTC().Add(-age)); err != nil {
		t.Fatalf("age the start mark: %v", err)
	}
}

// seedStaleMarkedRun seeds a marked intent of a live owner whose start
// mark is age old, and the tenant's placement with that owner alive.
func seedStaleMarkedRun(t *testing.T, db *sql.DB, age time.Duration, policy string) uuid.UUID {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, time.Time{}, staleMarkedOwner, "", true)
	ageStartMark(t, db, runID, age)
	seedPlacementLivenessWithPolicy(t, db, staleMarkedOwner, nil, policy)
	return runID
}

// readMarkedRun reads the columns the start-lost tests assert.
func readMarkedRun(t *testing.T, db *sql.DB, runID uuid.UUID) (status string, result, reason sql.NullString,
	endedAt, superseded sql.NullTime, logSummary bool) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason', ended_at, interrupted_superseded_at,
		       COALESCE(payload ? 'log_summary', false)
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason, &endedAt, &superseded, &logSummary); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return status, result, reason, endedAt, superseded, logSummary
}

// TestEndStaleMarkedIntentEndsALiveOwnerAfterTheAge asserts the case of a
// Jenkins process that restarted inside a live pod: a marked intent with
// no build record ends INTERRUPTED with the reason start-lost once its
// mark is older than ClaimExpiryAge. It publishes no action, counts the
// skipped retry, and a second round writes nothing.
func TestEndStaleMarkedIntentEndsALiveOwnerAfterTheAge(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useTestMeterProvider(t)
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge+time.Minute, `{"interruptedRetry": "automatic"}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	received := collectActions(t, conn)
	notifier, sent, _ := newCaptureNotifier(t)

	if ended := runEndMarked(t, pool, conn, notifier, metrics); ended != 1 {
		t.Fatalf("ended = %d, want 1", ended)
	}
	status, result, reason, _, _, _ := readMarkedRun(t, db, runID)
	if status != StatusCompleted || result.String != "INTERRUPTED" || reason.String != ReasonStartLost {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result.String,
			reason.String, ReasonStartLost)
	}
	if n := countInterruptedActivity(t, db); n != 1 {
		t.Fatalf("build.interrupted rows = %d, want 1", n)
	}
	if sent.count() != 1 {
		t.Fatalf("notifications = %d, want 1", sent.count())
	}
	select {
	case a := <-received:
		t.Fatalf("published %+v, want no cancel and no retry", a)
	case <-time.After(300 * time.Millisecond):
	}
	if got := sumCounterByAttr(t, reader, "recorder_retry_skipped_total", "reason", retrySkipStartLost); got != 1 {
		t.Fatalf("recorder_retry_skipped_total{reason=start-lost} = %d, want 1", got)
	}

	if ended := runEndMarked(t, pool, conn, notifier, metrics); ended != 0 {
		t.Fatalf("second round ended = %d, want 0", ended)
	}
	if n := countInterruptedActivity(t, db); n != 1 {
		t.Fatalf("build.interrupted rows after the second round = %d, want 1", n)
	}
}

// TestEndStaleMarkedIntentKeepsARecentMark asserts that a live owner's
// mark younger than ClaimExpiryAge ends nothing.
func TestEndStaleMarkedIntentKeepsARecentMark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge-time.Minute, `{}`)

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

// TestEndStaleMarkedIntentPrefersConfirmedGone asserts that an old mark
// whose owner is confirmed gone ends with cell-rolled, because the
// confirmed class runs first.
func TestEndStaleMarkedIntentPrefersConfirmedGone(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, time.Time{}, markedLostOwner, "", true)
	ageStartMark(t, db, runID, ClaimExpiryAge+time.Minute)
	seedMarkedPlacement(t, db, true, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	if ended := runEndMarked(t, pool, conn, nil, nil); ended != 1 {
		t.Fatalf("ended = %d, want 1", ended)
	}
	if _, _, reason, _, _, _ := readMarkedRun(t, db, runID); reason.String != ReasonCellRolled {
		t.Fatalf("reason = %q, want %q", reason.String, ReasonCellRolled)
	}
}

// TestEndStaleMarkedIntentLeavesAHeartbeatRow asserts that a row a
// heartbeat wrote records a build, so the start-lost class leaves it.
func TestEndStaleMarkedIntentLeavesAHeartbeatRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge+time.Minute, `{}`)
	var createdAt time.Time
	if err := db.QueryRow(`SELECT created_at FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&createdAt); err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
		                         status, last_heartbeat_at, heartbeat_received_at)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'QUEUED', now(), now())
	`, runID, createdAt, "manual:"+runID.String(), testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed heartbeat row: %v", err)
	}

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	if ended := runEndMarked(t, pool, conn, nil, nil); ended != 0 {
		t.Fatalf("ended = %d, want 0", ended)
	}
	if status, _, _, _, _, _ := readMarkedRun(t, db, runID); status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED", status)
	}
}

// TestEndStaleMarkedIntentHonoursTheConsumerGate asserts that a backlog
// on the recorder's consumer holds the start-lost class too.
func TestEndStaleMarkedIntentHonoursTheConsumerGate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge+time.Minute, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	if _, err := conn.PublishJetStream("builds.core.ns.cell", []byte("{}")); err != nil {
		t.Fatalf("publish backlog message: %v", err)
	}

	ended, err := EndMarkedLostIntents(context.Background(), pool, conn, nil, nil, nil, nil,
		1, time.Now().UTC(), testGrace, nil)
	if err != nil {
		t.Fatalf("EndMarkedLostIntents: %v", err)
	}
	if ended != 0 {
		t.Fatalf("ended = %d, want 0 behind a backlog", ended)
	}
	if n := countRunRows(t, db, runID); n != 0 {
		t.Fatalf("runs rows = %d, want 0", n)
	}
}

// TestRunStartedSupersedesAStartLostMark asserts that a slow but live
// build takes its record back: the owner's late run.started sets the row
// RUNNING, clears the result, the end, and the marking keys, and stamps
// interrupted_superseded_at. The owner's completion then writes the real
// result.
func TestRunStartedSupersedesAStartLostMark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge+time.Minute, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	if ended := runEndMarked(t, pool, conn, nil, nil); ended != 1 {
		t.Fatalf("ended = %d, want 1", ended)
	}

	g := NewIngester(pool, nil, nil)
	intentKey := "manual:" + runID.String()
	buildNumber := int64(6)
	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	status, result, reason, endedAt, superseded, logSummary := readMarkedRun(t, db, runID)
	if status != StatusRunning || result.Valid || endedAt.Valid {
		t.Fatalf("status=%q result=%v ended_at=%v, want RUNNING with no result and no end", status, result, endedAt)
	}
	if reason.Valid || logSummary {
		t.Fatalf("interrupted_reason=%v log_summary=%v, want both removed", reason, logSummary)
	}
	if !superseded.Valid {
		t.Fatal("interrupted_superseded_at is null, want the supersede time")
	}

	success := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &success})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if status, result, _, _, _, _ := readMarkedRun(t, db, runID); status != StatusCompleted || result.String != success {
		t.Fatalf("status=%q result=%q, want COMPLETED/SUCCESS", status, result.String)
	}
}

// TestRunCompletedClearsAStartLostReason asserts that the owner's
// completion, with no start event, replaces a start-lost mark and
// removes its reason.
func TestRunCompletedClearsAStartLostReason(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge+time.Minute, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	if ended := runEndMarked(t, pool, conn, nil, nil); ended != 1 {
		t.Fatalf("ended = %d, want 1", ended)
	}

	g := NewIngester(pool, nil, nil)
	success := "SUCCESS"
	completed := newTestEnvelope(t, runID, "manual:"+runID.String(), KindRunCompleted, RunCompletedPayload{Result: &success})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	status, result, reason, _, superseded, _ := readMarkedRun(t, db, runID)
	if status != StatusCompleted || result.String != success || reason.Valid {
		t.Fatalf("status=%q result=%q reason=%v, want COMPLETED/SUCCESS with no reason", status, result.String, reason)
	}
	if !superseded.Valid {
		t.Fatal("interrupted_superseded_at is null, want the supersede time")
	}
}

// TestRunStartedKeepsACellRolledMark asserts that the narrow supersede
// applies only to start-lost: a late start of a run marked cell-rolled
// changes no terminal column.
func TestRunStartedKeepsACellRolledMark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := uuid.Must(uuid.NewV7())
	seedMarkedIntent(t, db, runID, time.Time{}, markedLostOwner, "", true)
	seedMarkedPlacement(t, db, true, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	if ended := runEndMarked(t, pool, conn, nil, nil); ended != 1 {
		t.Fatalf("ended = %d, want 1", ended)
	}

	g := NewIngester(pool, nil, nil)
	buildNumber := int64(6)
	started := newTestEnvelope(t, runID, "manual:"+runID.String(), KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	started.CellInstance = markedLostOwner
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	status, result, reason, _, superseded, _ := readMarkedRun(t, db, runID)
	if status != StatusCompleted || result.String != "INTERRUPTED" || reason.String != ReasonCellRolled {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result.String,
			reason.String, ReasonCellRolled)
	}
	if superseded.Valid {
		t.Fatal("interrupted_superseded_at is set, want null")
	}
}

// TestEndStaleMarkedIntentRereadsTheAgeUnderTheLock asserts that a mark
// renewed after the candidate read keeps the run: the locked re-read
// applies the age cutoff too.
func TestEndStaleMarkedIntentRereadsTheAgeUnderTheLock(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedStaleMarkedRun(t, db, ClaimExpiryAge+time.Minute, `{}`)
	now := time.Now().UTC()
	candidates, err := selectStaleMarkedIntents(context.Background(), pool, now.Add(-ClaimExpiryAge), markedLostBatch)
	if err != nil {
		t.Fatalf("selectStaleMarkedIntents: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	ageStartMark(t, db, runID, time.Minute)

	changed, err := endMarkedLostIntent(context.Background(), pool, candidates[0], ReasonStartLost,
		now.Add(-ClaimExpiryAge), now)
	if err != nil {
		t.Fatalf("endMarkedLostIntent: %v", err)
	}
	if changed {
		t.Fatal("changed = true, want the renewed mark to keep the run")
	}
	if n := countRunRows(t, db, runID); n != 0 {
		t.Fatalf("runs rows = %d, want 0", n)
	}
}
