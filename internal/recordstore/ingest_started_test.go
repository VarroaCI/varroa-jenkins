package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestApplyRunStartedWritesRun asserts a run.started envelope raises
// status to RUNNING and writes its typed columns.
func TestApplyRunStartedWritesRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:started-1")
	g := NewIngester(pool, nil, nil)

	branch := "main"
	buildNumber := int64(1)
	env := newTestEnvelope(t, runID, "manual:started-1", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber,
		Branch:      &branch,
	})

	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	var status, branchGot string
	if err := db.QueryRow(`SELECT status, branch FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&status, &branchGot); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING", status)
	}
	if branchGot != branch {
		t.Fatalf("branch = %q, want %q", branchGot, branch)
	}
}

// TestApplyRunCompletedBeforeStarted asserts run.completed arriving
// before run.started still leaves one row at COMPLETED with an intact
// result.
func TestApplyRunCompletedBeforeStarted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:outoforder-1")
	g := NewIngester(pool, nil, nil)

	result := "SUCCESS"
	completed := newTestEnvelope(t, runID, "manual:outoforder-1", KindRunCompleted, RunCompletedPayload{
		Result: &result,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	buildNumber := int64(9)
	started := newTestEnvelope(t, runID, "manual:outoforder-1", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber,
	})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	var status, resultGot string
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&status, &resultGot); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusCompleted {
		t.Fatalf("status = %q, want COMPLETED", status)
	}
	if resultGot != result {
		t.Fatalf("result = %q, want %q (run.started must not clear it)", resultGot, result)
	}
}

// TestApplyRunLogMergesUnderKey asserts run.log merges into
// runs.payload.log_summary and touches no other column.
func TestApplyRunLogMergesUnderKey(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:log-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	started := newTestEnvelope(t, runID, "manual:log-1", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	logEnv := newTestEnvelope(t, runID, "manual:log-1", KindRunLog, map[string]any{
		"chunk_count":   3,
		"dropped_bytes": 0,
	})
	if _, err := g.Apply(context.Background(), logEnv); err != nil {
		t.Fatalf("apply run.log: %v", err)
	}

	var status string
	var chunkCount int
	if err := db.QueryRow(`
		SELECT status, (payload->'log_summary'->>'chunk_count')::int
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &chunkCount); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING (run.log must not change status)", status)
	}
	if chunkCount != 3 {
		t.Fatalf("chunk_count = %d, want 3", chunkCount)
	}
}

// TestApplyRunLogNeverReopensFinalized asserts a late run.log carrying
// finalized:false cannot clear a stored finalized:true, while its other
// summary fields still merge. It sets finalized true directly, standing
// in for the log-finalization sweep.
// TestApplyRunLogSweepThenLateRunLogNeverReopensFinalized runs the real
// sweep instead of this stand-in update.
func TestApplyRunLogNeverReopensFinalized(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:log-finalized-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	started := newTestEnvelope(t, runID, "manual:log-finalized-1", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	// Stand-in for the log-finalization sweep (task 6.6): mark the
	// summary finalized directly.
	if _, err := db.Exec(`
		UPDATE varroa.runs
		SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object(
			'log_summary', COALESCE(payload->'log_summary', '{}'::jsonb) || '{"finalized": true}'::jsonb)
		WHERE run_id = $1
	`, runID); err != nil {
		t.Fatalf("stand-in sweep update: %v", err)
	}

	late := newTestEnvelope(t, runID, "manual:log-finalized-1", KindRunLog, map[string]any{
		"finalized":     false,
		"dropped_bytes": 5,
	})
	if _, err := g.Apply(context.Background(), late); err != nil {
		t.Fatalf("apply late run.log: %v", err)
	}

	var finalized bool
	var droppedBytes int
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'finalized')::bool, (payload->'log_summary'->>'dropped_bytes')::int
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&finalized, &droppedBytes); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if !finalized {
		t.Fatal("finalized went back to false; the marker must stay monotonic")
	}
	if droppedBytes != 5 {
		t.Fatalf("dropped_bytes = %d, want 5 (other summary fields must still merge)", droppedBytes)
	}
}

// TestApplyRunLogSweepThenLateRunLogNeverReopensFinalized runs the real
// log-finalization sweep to mark a completed run's log finalized, then
// delivers a late run.log carrying finalized:false, and asserts the
// marker stays true while the other summary fields still merge.
func TestApplyRunLogSweepThenLateRunLogNeverReopensFinalized(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:log-real-sweep-1")
	g := NewIngester(pool, nil, nil)

	started := newTestEnvelope(t, runID, "manual:log-real-sweep-1", KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	completed := newTestEnvelope(t, runID, "manual:log-real-sweep-1", KindRunCompleted, RunCompletedPayload{})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	oldEnded := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`, oldEnded, runID); err != nil {
		t.Fatalf("backdate ended_at: %v", err)
	}

	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("finalizeStaleLogs finalized %d runs, want 1", n)
	}

	late := newTestEnvelope(t, runID, "manual:log-real-sweep-1", KindRunLog, map[string]any{
		"finalized":     false,
		"dropped_bytes": 5,
	})
	if _, err := g.Apply(context.Background(), late); err != nil {
		t.Fatalf("apply late run.log: %v", err)
	}

	var finalized bool
	var droppedBytes int
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'finalized')::bool, (payload->'log_summary'->>'dropped_bytes')::int
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&finalized, &droppedBytes); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if !finalized {
		t.Fatal("finalized went back to false after the real sweep closed it")
	}
	if droppedBytes != 5 {
		t.Fatalf("dropped_bytes = %d, want 5 (other summary fields must still merge)", droppedBytes)
	}
}

// TestApplyRunHeartbeatTouchesTheTwoHeartbeatColumns asserts a heartbeat
// raises last_heartbeat_at from the envelope's occurred_at, raises
// heartbeat_received_at from the recorder's own clock, and changes no
// other column.
func TestApplyRunHeartbeatTouchesTheTwoHeartbeatColumns(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:heartbeat-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	started := newTestEnvelope(t, runID, "manual:heartbeat-1", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	first := newTestEnvelope(t, runID, "manual:heartbeat-1", KindRunHeartbeat, map[string]any{})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply heartbeat: %v", err)
	}

	var lastHeartbeat time.Time
	var status string
	var resultNull *string
	if err := db.QueryRow(`SELECT last_heartbeat_at, status, result FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&lastHeartbeat, &status, &resultNull); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if !lastHeartbeat.Equal(first.OccurredAt) {
		t.Fatalf("last_heartbeat_at = %v, want %v", lastHeartbeat, first.OccurredAt)
	}
	if status != StatusRunning {
		t.Fatalf("status changed to %q", status)
	}
	if resultNull != nil {
		t.Fatalf("result changed to %v, want nil", *resultNull)
	}

	// An older heartbeat must not move the column backward.
	older := first
	older.OccurredAt = first.OccurredAt.Add(-time.Hour)
	if _, err := g.Apply(context.Background(), older); err != nil {
		t.Fatalf("apply older heartbeat: %v", err)
	}
	var stillLatest time.Time
	if err := db.QueryRow(`SELECT last_heartbeat_at FROM varroa.runs WHERE run_id = $1`, runID).Scan(&stillLatest); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if !stillLatest.Equal(first.OccurredAt) {
		t.Fatalf("last_heartbeat_at moved backward to %v, want %v", stillLatest, first.OccurredAt)
	}

	// A heartbeat the pod stamped 30 minutes ago still reaches the
	// recorder now, so the two columns differ by that gap. The case needs
	// its own run: the monotonic raise would refuse an older occurred_at
	// on the run above, and last_heartbeat_at would keep its newer value.
	skewRunID := seedGrantedIntent(t, pool, "manual:heartbeat-skew-1")
	// The payload carries no build number. The ledger already reserved the
	// next one for this intent, so repeating the first run's number would
	// take the build-number conflict path instead of an ordinary start.
	skewStart := newTestEnvelope(t, skewRunID, "manual:heartbeat-skew-1", KindRunStarted,
		RunStartedPayload{})
	if _, err := g.Apply(context.Background(), skewStart); err != nil {
		t.Fatalf("apply run.started for the skew case: %v", err)
	}
	stale := newTestEnvelope(t, skewRunID, "manual:heartbeat-skew-1", KindRunHeartbeat, map[string]any{})
	stale.OccurredAt = time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Microsecond)
	before := time.Now().UTC()
	if _, err := g.Apply(context.Background(), stale); err != nil {
		t.Fatalf("apply a heartbeat with a skewed pod clock: %v", err)
	}
	var pod, received time.Time
	if err := db.QueryRow(`
		SELECT last_heartbeat_at, heartbeat_received_at FROM varroa.runs WHERE run_id = $1
	`, skewRunID).Scan(&pod, &received); err != nil {
		t.Fatalf("read the heartbeat columns: %v", err)
	}
	if !pod.Equal(stale.OccurredAt) {
		t.Fatalf("last_heartbeat_at = %v, want the pod's own %v", pod, stale.OccurredAt)
	}
	if received.Before(before) || received.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("heartbeat_received_at = %v, want the recorder's own current time", received)
	}
	if !received.After(pod) {
		t.Fatalf("heartbeat_received_at %v is not newer than the stale pod stamp %v", received, pod)
	}
}

// TestApplyRunHeartbeatNeverLowersTheReceiveStamp asserts the receive
// stamp only rises. Two recorder replicas hold two clocks, and a
// liveness window that moved backward would mark a live run.
func TestApplyRunHeartbeatNeverLowersTheReceiveStamp(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:heartbeat-monotonic-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	started := newTestEnvelope(t, runID, "manual:heartbeat-monotonic-1", KindRunStarted,
		RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	if _, err := db.Exec(`
		UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2
	`, future, runID); err != nil {
		t.Fatalf("stamp a future receive time: %v", err)
	}

	beat := newTestEnvelope(t, runID, "manual:heartbeat-monotonic-1", KindRunHeartbeat, map[string]any{})
	if _, err := g.Apply(context.Background(), beat); err != nil {
		t.Fatalf("apply heartbeat: %v", err)
	}

	var got time.Time
	if err := db.QueryRow(`
		SELECT heartbeat_received_at FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&got); err != nil {
		t.Fatalf("read heartbeat_received_at: %v", err)
	}
	if !got.Equal(future) {
		t.Fatalf("heartbeat_received_at = %v, want the stored %v", got, future)
	}
}

// refreshedIntent grants intentKey from pod-a with counter 1, then
// repeats the claim from pod-b with a counter one past the grant, so the
// intent is owned by pod-b under a fresh number. It returns the run id,
// the old number, and the fresh number.
func refreshedIntent(t *testing.T, ledger *Ledger, intentKey string) (runID uuid.UUID, old, fresh int64) {
	t.Helper()
	req := newTestClaimRequest(intentKey, 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)

	repeat := req
	repeat.Cell = "cell-b"
	repeat.CellInstance = "pod-b"
	repeat.PlacementGeneration = 2
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 1
	second := mustGrant(t, ledger, repeat)
	if second.BuildNumber != first.BuildNumber+1 {
		t.Fatalf("want a refreshed number %d, got %+v", first.BuildNumber+1, second)
	}
	return uuid.MustParse(first.RunID), first.BuildNumber, second.BuildNumber
}

// readIntentStateAndNumber reads run_intents.state and build_number for
// runID; number is nil for a null column.
func readIntentStateAndNumber(t *testing.T, db *sql.DB, runID uuid.UUID) (state string, number *int64) {
	t.Helper()
	if err := db.QueryRow(`SELECT state, build_number FROM varroa.run_intents WHERE run_id = $1`, runID).
		Scan(&state, &number); err != nil {
		t.Fatalf("read intent: %v", err)
	}
	return state, number
}

// countRunsRows counts varroa.runs rows for runID.
func countRunsRows(t *testing.T, db *sql.DB, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count runs rows: %v", err)
	}
	return n
}

// TestSupersededStartWritesNothing asserts a run.started from the old
// instance for the old number, arriving after a move and refresh, is
// reported superseded and writes nothing; the new instance's start for
// the fresh number then records the run and consumes the intent.
func TestSupersededStartWritesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)
	const key = "manual:superseded-1"
	runID, old, fresh := refreshedIntent(t, ledger, key)
	before := readNextNumber(t, db)

	stale := newTestEnvelope(t, runID, key, KindRunStarted, RunStartedPayload{BuildNumber: &old})
	res, err := g.Apply(context.Background(), stale)
	if err != nil {
		t.Fatalf("apply stale run.started: %v", err)
	}
	if !res.SupersededStart || res.Conflict || res.SupersededReported != old || res.SupersededStored != fresh || res.SupersededOwner != "pod-b" {
		t.Fatalf("want a superseded start (%d < %d, owner pod-b), got %+v", old, fresh, res)
	}
	if n := countRunsRows(t, db, runID); n != 0 {
		t.Fatalf("want no runs row, got %d", n)
	}
	if state, number := readIntentStateAndNumber(t, db, runID); state != stateGranted || number == nil || *number != fresh {
		t.Fatalf("want GRANTED at %d, got %s/%v", fresh, state, number)
	}
	if after := readNextNumber(t, db); after != before {
		t.Fatalf("next_number moved from %d to %d", before, after)
	}

	owned := newTestEnvelope(t, runID, key, KindRunStarted, RunStartedPayload{BuildNumber: &fresh})
	owned.CellInstance = "pod-b"
	res, err = g.Apply(context.Background(), owned)
	if err != nil {
		t.Fatalf("apply real run.started: %v", err)
	}
	if res.SupersededStart || res.Conflict {
		t.Fatalf("want an ordinary write, got %+v", res)
	}
	var recorded int64
	if err := db.QueryRow(`SELECT build_number FROM varroa.runs WHERE run_id = $1`, runID).Scan(&recorded); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if recorded != fresh {
		t.Fatalf("runs.build_number = %d, want %d", recorded, fresh)
	}
	if state, number := readIntentStateAndNumber(t, db, runID); state != stateConsumed || number == nil || *number != fresh {
		t.Fatalf("want CONSUMED at %d, got %s/%v", fresh, state, number)
	}
}

// TestSupersededStartAfterRealStartChangesNothing asserts a redelivered
// old-instance start for the old number, arriving after the real start,
// changes neither table.
func TestSupersededStartAfterRealStartChangesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)
	const key = "manual:superseded-2"
	runID, old, fresh := refreshedIntent(t, ledger, key)

	owned := newTestEnvelope(t, runID, key, KindRunStarted, RunStartedPayload{BuildNumber: &fresh})
	owned.CellInstance = "pod-b"
	if _, err := g.Apply(context.Background(), owned); err != nil {
		t.Fatalf("apply real run.started: %v", err)
	}

	stale := newTestEnvelope(t, runID, key, KindRunStarted, RunStartedPayload{BuildNumber: &old})
	res, err := g.Apply(context.Background(), stale)
	if err != nil {
		t.Fatalf("apply stale run.started: %v", err)
	}
	if !res.SupersededStart {
		t.Fatalf("want a superseded start, got %+v", res)
	}
	var recorded int64
	if err := db.QueryRow(`SELECT build_number FROM varroa.runs WHERE run_id = $1`, runID).Scan(&recorded); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if recorded != fresh {
		t.Fatalf("runs.build_number = %d, want %d unchanged", recorded, fresh)
	}
	if state, number := readIntentStateAndNumber(t, db, runID); state != stateConsumed || number == nil || *number != fresh {
		t.Fatalf("want CONSUMED at %d unchanged, got %s/%v", fresh, state, number)
	}
}

// TestEqualNumberFromForeignInstanceIsARealStart asserts a start from a
// non-owning instance that reports the stored number is not superseded:
// an ownership move without a refresh leaves the number as it was, and
// the moved instance's own start must record the run.
func TestEqualNumberFromForeignInstanceIsARealStart(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)

	req := newTestClaimRequest("manual:equal-foreign", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	moved := req
	moved.CellInstance = "pod-b"
	moved.JenkinsNextBuildNumber = first.BuildNumber
	if resp := mustGrant(t, ledger, moved); resp.BuildNumber != first.BuildNumber {
		t.Fatalf("want a move without a refresh at %d, got %+v", first.BuildNumber, resp)
	}
	runID := uuid.MustParse(first.RunID)

	number := first.BuildNumber
	env := newTestEnvelope(t, runID, req.IntentKey, KindRunStarted, RunStartedPayload{BuildNumber: &number})
	env.CellInstance = "pod-c"
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	if res.SupersededStart || res.Conflict {
		t.Fatalf("want an ordinary write for an equal number, got %+v", res)
	}
	if n := countRunsRows(t, db, runID); n != 1 {
		t.Fatalf("want one runs row, got %d", n)
	}
	if state, stored := readIntentStateAndNumber(t, db, runID); state != stateConsumed || stored == nil || *stored != number {
		t.Fatalf("want CONSUMED at %d, got %s/%v", number, state, stored)
	}
}

// TestLowerNumberFromOwnerTakesOrdinaryPath asserts a lower number from
// the owning instance is not superseded: it takes the ordinary write.
// Another intent of the job holds that number as an unstarted grant, so
// the owner takes it and the holder keeps no number.
func TestLowerNumberFromOwnerTakesOrdinaryPath(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)

	reqA := newTestClaimRequest("manual:owner-lower-a", 1)
	reqA.JenkinsNextBuildNumber = 1
	a := mustGrant(t, ledger, reqA)
	reqB := newTestClaimRequest("manual:owner-lower-b", 1)
	reqB.JenkinsNextBuildNumber = 1
	b := mustGrant(t, ledger, reqB)
	if b.BuildNumber != a.BuildNumber+1 {
		t.Fatalf("want B at %d, got %d", a.BuildNumber+1, b.BuildNumber)
	}

	repeatA := reqA
	repeatA.Cell = "cell-b"
	repeatA.CellInstance = "pod-b"
	repeatA.PlacementGeneration = 2
	repeatA.JenkinsNextBuildNumber = b.BuildNumber + 1
	refreshed := mustGrant(t, ledger, repeatA)
	if refreshed.BuildNumber != b.BuildNumber+1 {
		t.Fatalf("want A refreshed to %d, got %d", b.BuildNumber+1, refreshed.BuildNumber)
	}
	runA := uuid.MustParse(a.RunID)

	lower := b.BuildNumber
	env := newTestEnvelope(t, runA, reqA.IntentKey, KindRunStarted, RunStartedPayload{BuildNumber: &lower})
	env.CellInstance = "pod-b"
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply owner run.started: %v", err)
	}
	if res.SupersededStart || res.Conflict || res.NumberTakenFrom != b.RunID {
		t.Fatalf("want the take from run %s and no conflict, got %+v", b.RunID, res)
	}
	var recorded *int64
	if err := db.QueryRow(`SELECT build_number FROM varroa.runs WHERE run_id = $1`, runA).Scan(&recorded); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if recorded == nil || *recorded != lower {
		t.Fatalf("runs.build_number = %v, want %d", recorded, lower)
	}
	if state, number := readIntentStateAndNumber(t, db, runA); state != stateConsumed || number == nil || *number != lower {
		t.Fatalf("want CONSUMED at %d, got %s/%v", lower, state, number)
	}
	if state, number := readIntentStateAndNumber(t, db, uuid.MustParse(b.RunID)); state != stateGranted || number != nil {
		t.Fatalf("want B GRANTED with a null number, got %s/%v", state, number)
	}
	if next := readNextNumber(t, db); next != refreshed.BuildNumber+1 {
		t.Fatalf("next_number = %d, want %d", next, refreshed.BuildNumber+1)
	}
}

// readReplayOf answers the stored replay_of column of one run, and false
// when the column is NULL.
func readReplayOf(t *testing.T, db *sql.DB, runID uuid.UUID) (string, bool) {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRow(`SELECT replay_of::text FROM varroa.runs WHERE run_id = $1`, runID).Scan(&got); err != nil {
		t.Fatalf("read replay_of: %v", err)
	}
	return got.String, got.Valid
}

// TestApplyRunStartedWritesReplayOf asserts a run.started envelope that
// carries replay_of stores the source run identifier in the column.
func TestApplyRunStartedWritesReplayOf(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:replay-of-1")
	g := NewIngester(pool, nil, nil)

	source := "0199aa11-2222-7333-8444-555566667777"
	buildNumber := int64(1)
	env := newTestEnvelope(t, runID, "manual:replay-of-1", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber,
		ReplayOf:    &source,
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	if got, ok := readReplayOf(t, db, runID); !ok || got != source {
		t.Fatalf("replay_of = %q (valid %v), want %q", got, ok, source)
	}
}

// TestApplyRunStartedLeavesReplayOfNull asserts a run.started envelope with
// no replay_of key, or with a value that is not a UUID, leaves the column
// NULL and still records the run.
func TestApplyRunStartedLeavesReplayOfNull(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	absentID := seedGrantedIntent(t, pool, "manual:replay-of-2")
	buildNumber := int64(1)
	absent := newTestEnvelope(t, absentID, "manual:replay-of-2", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), absent); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	if got, ok := readReplayOf(t, db, absentID); ok {
		t.Fatalf("replay_of = %q, want NULL for a build with no source", got)
	}

	malformedID := seedGrantedIntent(t, pool, "manual:replay-of-3")
	secondNumber := int64(2)
	bad := "not-a-uuid"
	malformed := newTestEnvelope(t, malformedID, "manual:replay-of-3", KindRunStarted, RunStartedPayload{
		BuildNumber: &secondNumber,
		ReplayOf:    &bad,
	})
	if _, err := g.Apply(context.Background(), malformed); err != nil {
		t.Fatalf("apply run.started with a malformed replay_of: %v", err)
	}
	if got, ok := readReplayOf(t, db, malformedID); ok {
		t.Fatalf("replay_of = %q, want NULL for a value that is not a UUID", got)
	}
}

// TestRedeliveredStartKeepsReplayOf asserts a later run.started with no
// replay_of key keeps the stored source run identifier.
func TestRedeliveredStartKeepsReplayOf(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:replay-of-4")
	g := NewIngester(pool, nil, nil)

	source := "0199aa11-2222-7333-8444-555566667777"
	buildNumber := int64(1)
	first := newTestEnvelope(t, runID, "manual:replay-of-4", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber,
		ReplayOf:    &source,
	})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.started: %v", err)
	}

	again := newTestEnvelope(t, runID, "manual:replay-of-4", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), again); err != nil {
		t.Fatalf("apply redelivered run.started: %v", err)
	}
	if got, ok := readReplayOf(t, db, runID); !ok || got != source {
		t.Fatalf("replay_of = %q (valid %v), want %q kept", got, ok, source)
	}
}
