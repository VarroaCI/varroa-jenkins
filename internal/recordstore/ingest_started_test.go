package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestApplyRunStartedWritesRun asserts a run.started envelope raises
// status to RUNNING and writes its typed columns.
func TestApplyRunStartedWritesRun(t *testing.T) {
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

// TestApplyRunHeartbeatTouchesOneColumn asserts a heartbeat raises
// last_heartbeat_at and changes no other column.
func TestApplyRunHeartbeatTouchesOneColumn(t *testing.T) {
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
}
