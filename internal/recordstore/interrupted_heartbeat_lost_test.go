package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestEvaluateInterruptionHeartbeatLost asserts the heartbeat-lost test:
// it ends a RUNNING run silent past liveOwnerSilentWindow whatever the
// placement says, and it applies after every liveness test.
func TestEvaluateInterruptionHeartbeatLost(t *testing.T) {
	now := time.Now()
	pastWindow := now.Add(-liveOwnerSilentWindow - time.Minute)
	insideWindow := now.Add(-liveOwnerSilentWindow + time.Minute)
	twentyMinutes := now.Add(-20 * time.Minute)
	fresh := now.Add(-time.Minute)
	later := now.Add(time.Minute)
	requested := now.Add(-2 * time.Minute)

	liveRow := tenantPlacementLivenessRow{cell: "cell-a", cellInstance: "pod-a", cellAlive: true, cellAliveAt: &fresh}

	cases := []struct {
		name       string
		c          runInterruptCandidate
		row        tenantPlacementLivenessRow
		exists     bool
		wantOK     bool
		wantReason string
	}{
		{
			name: "a running run on a live owner past the window",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &pastWindow},
			row: liveRow, exists: true,
			wantOK: true, wantReason: ReasonHeartbeatLost,
		},
		{
			name: "a running run on a live owner inside the window",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &insideWindow},
			row: liveRow, exists: true,
			wantOK: false,
		},
		{
			name: "a queued run past the window",
			c: runInterruptCandidate{status: StatusQueued, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &pastWindow},
			row: liveRow, exists: true,
			wantOK: false,
		},
		{
			name: "a dead cell still names no-heartbeat",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &twentyMinutes},
			row: tenantPlacementLivenessRow{cell: "cell-a", cellInstance: "pod-a", cellAlive: false,
				cellAliveAt: &fresh, cellLastSeenAt: &twentyMinutes},
			exists: true,
			wantOK: true, wantReason: ReasonNoHeartbeat,
		},
		{
			name: "no placement row",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &pastWindow},
			exists: false,
			wantOK: true, wantReason: ReasonHeartbeatLost,
		},
		{
			name: "an owner outside the member list with no dead entry",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-gone",
				heartbeatReceivedAt: &pastWindow},
			row: liveRow, exists: true,
			wantOK: true, wantReason: ReasonHeartbeatLost,
		},
		{
			name: "a dispatched resume inside its deadline",
			c: runInterruptCandidate{status: StatusRunning, cell: "cell-a", cellInstance: "pod-a",
				heartbeatReceivedAt: &pastWindow,
				mesh:                &runMeshRecord{Resume: &runMeshResume{Instance: "pod-b", RequestedAt: &requested, Deadline: &later}}},
			row: liveRow, exists: true,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row
			if tc.exists {
				row = resolveLivenessMembers(row)
			}
			reason, ok := evaluateInterruption(now, tc.c, row, tc.exists, testGrace)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (reason %q), want %v", ok, reason, tc.wantOK)
			}
			if ok && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// seedSilentRunOnLiveOwner seeds one RUNNING run owned by pod-a whose
// heartbeat is age old, and a placement row that reports pod-a alive with
// policy.
func seedSilentRunOnLiveOwner(t *testing.T, db *sql.DB, runID uuid.UUID, age time.Duration, policy string) {
	t.Helper()
	past := time.Now().Add(-age)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, past, runID); err != nil {
		t.Fatalf("backdate heartbeat_received_at: %v", err)
	}
	fresh := time.Now()
	seedTenantPlacementLiveness(t, db, "pod-a", true, &fresh, &fresh, nil)
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET policy = $1::jsonb WHERE cluster = $2 AND tenant = $3`,
		policy, testCluster, testTenant); err != nil {
		t.Fatalf("seed placement policy: %v", err)
	}
}

// subscribeClusterActions collects every ledger action published for
// testCluster.
func subscribeClusterActions(t *testing.T, conn *bus.Conn) chan LedgerAction {
	t.Helper()
	received := make(chan LedgerAction, 4)
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return received
}

// TestSweepEndsASilentRunOnALiveOwner asserts the sweep ends a RUNNING
// run silent past the window on a live owner, with the heartbeat-lost
// reason and one activity row. It publishes no cancel and no retry, it
// counts the skipped retry, and it deletes no run-state objects.
func TestSweepEndsASilentRunOnALiveOwner(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedSilentRunOnLiveOwner(t, db, runID, liveOwnerSilentWindow+time.Minute, `{"interruptedRetry": "automatic"}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	received := subscribeClusterActions(t, conn)

	reader := useTestMeterProvider(t)
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	store := &prefixRecordingStore{}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, store,
		DefaultSweepMaxPending, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}

	var status, result, reason string
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" || reason != ReasonHeartbeatLost {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result, reason,
			ReasonHeartbeatLost)
	}

	var activityRows int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.activity WHERE type = 'build.interrupted' AND reason = $1
	`, ReasonHeartbeatLost).Scan(&activityRows); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if activityRows != 1 {
		t.Fatalf("build.interrupted activity rows = %d, want 1", activityRows)
	}

	select {
	case a := <-received:
		t.Fatalf("got ledger action %+v, want none for a heartbeat-lost mark", a)
	case <-time.After(time.Second):
	}

	if got := sumCounterByAttr(t, reader, "recorder_retry_skipped_total", "reason", retrySkipHeartbeatLost); got != 1 {
		t.Fatalf("recorder_retry_skipped_total{reason=heartbeat-lost} = %d, want 1", got)
	}
	if len(store.prefixes) != 0 || len(store.keys) != 0 {
		t.Fatalf("deleted prefixes=%v keys=%v, want no run-state delete", store.prefixes, store.keys)
	}
}

// TestSweepLeavesASilentRunOnALiveOwnerInsideTheWindow asserts a run on a
// live owner whose heartbeat is younger than the window stays RUNNING.
func TestSweepLeavesASilentRunOnALiveOwnerInsideTheWindow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedSilentRunOnLiveOwner(t, db, runID, liveOwnerSilentWindow-time.Minute, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, nil, nil, nil,
		DefaultSweepMaxPending, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 0 {
		t.Fatalf("marked = %d, want 0", marked)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING", status)
	}
}

// TestMarkMovedRunsForTenantSkipsHeartbeatLost asserts the announce path
// never applies the heartbeat-lost test: it runs the confirmed and moved
// tests alone.
func TestMarkMovedRunsForTenantSkipsHeartbeatLost(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedSilentRunOnLiveOwner(t, db, runID, liveOwnerSilentWindow+time.Minute, `{}`)

	fresh := time.Now()
	row := resolveLivenessMembers(tenantPlacementLivenessRow{cellInstance: "pod-a", cellAlive: true, cellAliveAt: &fresh})
	marked, err := MarkMovedRunsForTenant(context.Background(), pool, nil, nil, nil, nil, testCluster, testTenant,
		row, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("MarkMovedRunsForTenant: %v", err)
	}
	if marked != 0 {
		t.Fatalf("marked = %d, want 0", marked)
	}
}

// TestMarkInterruptedHeartbeatLostRereadsTheHeartbeat asserts the marking
// write re-reads the heartbeat receive stamp for a heartbeat-lost mark: a
// heartbeat that lands after the candidate read stops the mark.
func TestMarkInterruptedHeartbeatLostRereadsTheHeartbeat(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedSilentRunOnLiveOwner(t, db, runID, liveOwnerSilentWindow+time.Minute, `{}`)
	c := fetchRunningCandidate(t, pool, runID)

	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("raise heartbeat_received_at: %v", err)
	}
	changed, err := markInterrupted(context.Background(), pool, c, ReasonHeartbeatLost, time.Now())
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if changed {
		t.Fatal("changed = true, want the fresh heartbeat to stop the mark")
	}
}

// markHeartbeatLost seeds one RUNNING run owned by pod-a and marks it
// heartbeat-lost through the sweep's own write.
func markHeartbeatLost(t *testing.T) (*Ingester, *sql.DB, uuid.UUID, string) {
	t.Helper()
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRunningRun(t, pool, newTestClaimRequest(intentKey, 0))
	seedSilentRunOnLiveOwner(t, db, runID, liveOwnerSilentWindow+time.Minute, `{}`)
	c := fetchRunningCandidate(t, pool, runID)
	changed, err := markInterrupted(context.Background(), pool, c, ReasonHeartbeatLost, time.Now())
	if err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v, want a mark", changed, err)
	}
	return NewIngester(pool, nil, nil), db, runID, intentKey
}

// TestRunCompletedSupersedesAHeartbeatLostMark asserts the owner's late
// run.completed replaces a heartbeat-lost mark: the real result wins, the
// reason goes, and the log summary loses the mark's incomplete_reason
// once the real run.log reports the log complete.
func TestRunCompletedSupersedesAHeartbeatLostMark(t *testing.T) {
	t.Parallel()

	g, db, runID, intentKey := markHeartbeatLost(t)

	runLog := newTestEnvelope(t, runID, intentKey, KindRunLog, map[string]any{
		"chunk_count": 3, "dropped_bytes": 0, "incomplete": false, "finalized": true,
	})
	if _, err := g.Apply(context.Background(), runLog); err != nil {
		t.Fatalf("apply run.log: %v", err)
	}
	success := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &success})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var (
		status, result string
		reason         sql.NullString
		superseded     sql.NullTime
		logSummary     []byte
	)
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason', interrupted_superseded_at, payload->'log_summary'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason, &superseded, &logSummary); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || result != success || reason.Valid {
		t.Fatalf("status=%q result=%q reason=%v, want COMPLETED/SUCCESS with no reason", status, result, reason)
	}
	if !superseded.Valid {
		t.Fatal("interrupted_superseded_at is null, want the supersede time")
	}
	var summary map[string]any
	if err := json.Unmarshal(logSummary, &summary); err != nil {
		t.Fatalf("decode log_summary %s: %v", logSummary, err)
	}
	if _, ok := summary["incomplete_reason"]; ok || summary["incomplete"] != false || summary["chunk_count"] != float64(3) {
		t.Fatalf("log_summary = %v, want the real summary with no incomplete_reason", summary)
	}
}

// TestRunCompletedKeepsTheReasonOfAnIncompleteLog asserts the owner's
// completion keeps the log summary's incomplete_reason when the log is
// still incomplete, and removes only the mark's reason.
func TestRunCompletedKeepsTheReasonOfAnIncompleteLog(t *testing.T) {
	t.Parallel()

	g, db, runID, intentKey := markHeartbeatLost(t)

	success := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &success})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	var (
		reason           sql.NullString
		incompleteReason sql.NullString
	)
	if err := db.QueryRow(`
		SELECT payload->>'interrupted_reason', payload->'log_summary'->>'incomplete_reason'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&reason, &incompleteReason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if reason.Valid || !incompleteReason.Valid {
		t.Fatalf("interrupted_reason=%v incomplete_reason=%v, want no reason and a kept incomplete_reason",
			reason, incompleteReason)
	}
}

// TestRunLogPreciseReasonReplacesTheHeartbeatLostMarkReason asserts the
// owner's real run.log replaces the mark's generic reason with its own
// precise one, and the owner's completion keeps it.
func TestRunLogPreciseReasonReplacesTheHeartbeatLostMarkReason(t *testing.T) {
	t.Parallel()

	g, db, runID, intentKey := markHeartbeatLost(t)

	runLog := newTestEnvelope(t, runID, intentKey, KindRunLog, map[string]any{
		"chunk_count": 3, "incomplete": true, "incomplete_reason": "plugin-drop",
	})
	if _, err := g.Apply(context.Background(), runLog); err != nil {
		t.Fatalf("apply run.log: %v", err)
	}
	success := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &success})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var incompleteReason sql.NullString
	if err := db.QueryRow(`
		SELECT payload->'log_summary'->>'incomplete_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&incompleteReason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if incompleteReason.String != "plugin-drop" {
		t.Fatalf("incomplete_reason = %v, want plugin-drop", incompleteReason)
	}
}

// TestForeignCompletionLeavesAHeartbeatLostMark asserts a completion from
// another instance changes no terminal column of a heartbeat-lost mark.
func TestForeignCompletionLeavesAHeartbeatLostMark(t *testing.T) {
	t.Parallel()

	g, db, runID, intentKey := markHeartbeatLost(t)

	success := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &success})
	completed.CellInstance = "pod-z"
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	var result, reason string
	if err := db.QueryRow(`
		SELECT result, payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&result, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if result != "INTERRUPTED" || reason != ReasonHeartbeatLost {
		t.Fatalf("result=%q reason=%q, want INTERRUPTED/%q", result, reason, ReasonHeartbeatLost)
	}
}

// TestRunStartedLeavesAHeartbeatLostMark asserts a run.started envelope
// does not supersede a heartbeat-lost mark: only the owner's completion
// replaces it.
func TestRunStartedLeavesAHeartbeatLostMark(t *testing.T) {
	t.Parallel()

	g, db, runID, intentKey := markHeartbeatLost(t)

	buildNumber := int64(7)
	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	var status, result, reason string
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" || reason != ReasonHeartbeatLost {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result, reason,
			ReasonHeartbeatLost)
	}
}

// TestHeartbeatLostMarkKeepsThePendingInputs asserts a heartbeat-lost mark
// keeps the run's pending input set, a settled run.input still clears it,
// and the owner's completion clears it.
func TestHeartbeatLostMarkKeepsThePendingInputs(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRunningRun(t, pool, newTestClaimRequest(intentKey, 0))
	seedSilentRunOnLiveOwner(t, db, runID, liveOwnerSilentWindow+time.Minute, `{}`)
	if _, err := db.Exec(`UPDATE varroa.runs SET inputs = '[{"id":"approve"}]'::jsonb WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("seed pending inputs: %v", err)
	}
	c := fetchRunningCandidate(t, pool, runID)
	if changed, err := markInterrupted(context.Background(), pool, c, ReasonHeartbeatLost, time.Now()); err != nil || !changed {
		t.Fatalf("markInterrupted: changed=%v err=%v, want a mark", changed, err)
	}
	var inputs sql.NullString
	if err := db.QueryRow(`SELECT inputs::text FROM varroa.runs WHERE run_id = $1`, runID).Scan(&inputs); err != nil {
		t.Fatalf("read inputs: %v", err)
	}
	if !inputs.Valid {
		t.Fatal("inputs is null after the mark, want the pending set kept")
	}

	// A settled input still leaves the kept set: the owner reports an
	// empty set after a proceed.
	seq := time.Now().UnixMilli()
	settled := newTestEnvelope(t, runID, intentKey, KindRunInput, RunInputPayload{Seq: &seq})
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), settled); err != nil {
		t.Fatalf("apply run.input: %v", err)
	}
	if err := db.QueryRow(`SELECT inputs::text FROM varroa.runs WHERE run_id = $1`, runID).Scan(&inputs); err != nil {
		t.Fatalf("read inputs: %v", err)
	}
	if inputs.Valid {
		t.Fatalf("inputs = %s after the settled run.input, want null", inputs.String)
	}
	if _, err := db.Exec(`UPDATE varroa.runs SET inputs = '[{"id":"approve"}]'::jsonb WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("reseed pending inputs: %v", err)
	}

	success := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &success})
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if err := db.QueryRow(`SELECT inputs::text FROM varroa.runs WHERE run_id = $1`, runID).Scan(&inputs); err != nil {
		t.Fatalf("read inputs: %v", err)
	}
	if inputs.Valid {
		t.Fatalf("inputs = %s after the completion, want null", inputs.String)
	}
}
