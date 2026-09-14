package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// seedRunningRun grants req through a real Ledger.Claim, applies
// run.started, and returns the run id. Unlike startRunForFinalize, the
// caller controls every ClaimRequest field.
func seedRunningRun(t *testing.T, pool *pgxpool.Pool, req ClaimRequest) uuid.UUID {
	t.Helper()
	ledger := NewLedger(pool, nil)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("seed claim: want granted, got %+v", resp)
	}
	runID := uuid.MustParse(resp.RunID)
	g := NewIngester(pool, nil, nil)
	started := newTestEnvelope(t, runID, req.IntentKey, KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	return runID
}

// fetchRunningCandidate reads runID's row back as a runInterruptCandidate,
// through the same query the sweep uses.
func fetchRunningCandidate(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) runInterruptCandidate {
	t.Helper()
	all, err := selectRunningCandidates(context.Background(), pool)
	if err != nil {
		t.Fatalf("select running candidates: %v", err)
	}
	for _, c := range all {
		if c.runID == runID {
			return c
		}
	}
	t.Fatalf("run %s not found among running candidates", runID)
	return runInterruptCandidate{}
}

// newTestActionConn starts an embedded NATS server and connects, for
// afterMark's action publishes.
func newTestActionConn(t *testing.T) *bus.Conn {
	t.Helper()
	s, err := server.NewServer(&server.Options{Port: -1, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(s.Shutdown)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// TestMarkInterruptedWritesTerminalRecordReasonAndActivity asserts the
// terminal write, the reason value, the no-summary finalize marker for a
// run that never had one, and the build.interrupted activity row.
func TestMarkInterruptedWritesTerminalRecordReasonAndActivity(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now())
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if !changed {
		t.Fatal("want the row changed")
	}

	var status, result, reason string
	var finalized, incomplete bool
	var incompleteReason string
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason',
		       (payload->'log_summary'->>'finalized')::boolean,
		       (payload->'log_summary'->>'incomplete')::boolean,
		       payload->'log_summary'->>'incomplete_reason'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason, &finalized, &incomplete, &incompleteReason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "COMPLETED" || result != "INTERRUPTED" || reason != ReasonNoHeartbeat {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result, reason, ReasonNoHeartbeat)
	}
	if !finalized || !incomplete || incompleteReason != "no-summary" {
		t.Fatalf("finalized=%v incomplete=%v incomplete_reason=%q, want true/true/no-summary", finalized, incomplete, incompleteReason)
	}

	var activityCount int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'build.interrupted'`).Scan(&activityCount); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if activityCount != 1 {
		t.Fatalf("activity rows = %d, want 1", activityCount)
	}
}

// TestMarkInterruptedRollsBackWhenActivityInsertFails asserts design
// D9's whole-transaction contract (gap 9.6a): when the terminal run
// update succeeds but the activity insert then fails, the transaction
// rolls back, so neither write lands. now is a timestamp outside every
// partition migration 00002 creates (only the previous month, the
// current month, and the next two months), so the activity insert fails
// against a real missing-partition error, with no seam added to
// markInterrupted.
func TestMarkInterruptedRollsBackWhenActivityInsertFails(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	far := time.Now().UTC().AddDate(0, 6, 0)
	changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, far)
	if err == nil {
		t.Fatal("want an error from the missing activity partition, got nil")
	}
	if changed {
		t.Fatal("want changed=false when the marking rolls back")
	}

	var status string
	var result sql.NullString
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &result); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "RUNNING" || result.Valid {
		t.Fatalf("status=%q result=%v, want the run still RUNNING with no result: the terminal write must roll back too", status, result)
	}

	var activityCount int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'build.interrupted'`).Scan(&activityCount); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if activityCount != 0 {
		t.Fatalf("activity rows = %d, want 0: a rolled-back marking writes no row", activityCount)
	}
}

// TestMarkInterruptedFinalizesIncompleteWhenSummaryExists asserts a run
// that already holds a log summary gets finalized with reason
// "not-finalized", not "no-summary", and both fields decode as the
// booleans the API's Finalized/Incomplete fields expect.
func TestMarkInterruptedFinalizesIncompleteWhenSummaryExists(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	if _, err := db.Exec(`
		UPDATE varroa.runs SET payload = jsonb_build_object('log_summary', jsonb_build_object('bytes', 100))
		WHERE run_id = $1
	`, runID); err != nil {
		t.Fatalf("seed an existing log summary: %v", err)
	}
	c := fetchRunningCandidate(t, pool, runID)

	if _, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now()); err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}

	var finalized, incomplete bool
	var incompleteReason string
	var bytes int
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'finalized')::boolean,
		       (payload->'log_summary'->>'incomplete')::boolean,
		       payload->'log_summary'->>'incomplete_reason',
		       (payload->'log_summary'->>'bytes')::int
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&finalized, &incomplete, &incompleteReason, &bytes); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if !finalized || !incomplete || incompleteReason != "not-finalized" {
		t.Fatalf("finalized=%v incomplete=%v incomplete_reason=%q, want true/true/not-finalized", finalized, incomplete, incompleteReason)
	}
	if bytes != 100 {
		t.Fatalf("bytes = %d, want the pre-existing summary preserved at 100", bytes)
	}
}

// TestMarkInterruptedSummaryDecodesAsAPIExpects asserts the log_summary
// object markInterrupted writes decodes through the exact type the BFF
// uses to parse it — the regression this finding covers: a string
// "finalized" value fails that decode and silently drops the run's
// terminal "end" event and its incomplete/incomplete_reason fields.
func TestMarkInterruptedSummaryDecodesAsAPIExpects(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	if _, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now()); err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}

	var payloadRaw []byte
	if err := db.QueryRow(`SELECT payload FROM varroa.runs WHERE run_id = $1`, runID).Scan(&payloadRaw); err != nil {
		t.Fatalf("read payload: %v", err)
	}

	var decoded struct {
		LogSummary struct {
			Finalized        bool   `json:"finalized"`
			Incomplete       bool   `json:"incomplete"`
			IncompleteReason string `json:"incomplete_reason"`
		} `json:"log_summary"`
	}
	if err := json.Unmarshal(payloadRaw, &decoded); err != nil {
		t.Fatalf("decode payload as the API's runLogSummaryPayload shape: %v", err)
	}
	if !decoded.LogSummary.Finalized || !decoded.LogSummary.Incomplete || decoded.LogSummary.IncompleteReason != "no-summary" {
		t.Fatalf("decoded log_summary = %+v, want finalized/incomplete true and reason no-summary", decoded.LogSummary)
	}
}

// TestMarkInterruptedReasonReadableThroughGetRun asserts that the reason
// markInterrupted stores survives the Reader.GetRun path the BFF uses:
// the run's payload carries interrupted_reason after a mark, and a
// fresh read of the run returns that same payload.
func TestMarkInterruptedReasonReadableThroughGetRun(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	changed, err := markInterrupted(context.Background(), pool, c, ReasonCellRolled, time.Now())
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if !changed {
		t.Fatal("want the row changed")
	}

	run, _, err := reader.GetRun(context.Background(), runID.String())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Result == nil || *run.Result != "INTERRUPTED" {
		t.Fatalf("result = %v, want INTERRUPTED", run.Result)
	}

	var decoded struct {
		InterruptedReason string `json:"interrupted_reason"`
	}
	if err := json.Unmarshal(run.Payload, &decoded); err != nil {
		t.Fatalf("decode payload as the API's runInterruptedPayload shape: %v", err)
	}
	if decoded.InterruptedReason != ReasonCellRolled {
		t.Fatalf("interrupted_reason = %q, want %q", decoded.InterruptedReason, ReasonCellRolled)
	}
}

// TestMarkInterruptedSkipsAlreadyTerminalRun asserts the conditional
// WHERE status = 'RUNNING' guard leaves a row a real completion already
// won, and reports no change.
func TestMarkInterruptedSkipsAlreadyTerminalRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)
	if _, err := db.Exec(`UPDATE varroa.runs SET status = 'COMPLETED', result = 'SUCCESS' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("simulate a real completion: %v", err)
	}

	changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now())
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if changed {
		t.Fatal("want no change: a real terminal state already won")
	}

	var result string
	if err := db.QueryRow(`SELECT result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&result); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if result != "SUCCESS" {
		t.Fatalf("result = %q, want SUCCESS unchanged", result)
	}
}

// TestEvaluateInterruptionReasons asserts the reason each named scenario
// answers: no-heartbeat for the silent test, cell-rolled when the run's
// stored cell matches the tenant's current one, and tenant-moved when it
// differs.
func TestEvaluateInterruptionReasons(t *testing.T) {
	now := time.Now()
	oldHeartbeat := now.Add(-20 * time.Minute)
	sixMinutes := now.Add(-6 * time.Minute)
	fresh := now.Add(-time.Minute)

	cases := []struct {
		name       string
		c          runInterruptCandidate
		row        tenantPlacementLivenessRow
		wantOK     bool
		wantReason string
	}{
		{
			name: "no-heartbeat",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-a", lastHeartbeatAt: &oldHeartbeat},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-a", cellAlive: false,
				cellAliveAt: &fresh, cellLastSeenAt: &oldHeartbeat,
			},
			wantOK: true, wantReason: ReasonNoHeartbeat,
		},
		{
			name: "cell-rolled",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", lastHeartbeatAt: &sixMinutes},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: sixMinutes}},
			},
			wantOK: true, wantReason: ReasonCellRolled,
		},
		{
			name: "tenant-moved",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", lastHeartbeatAt: &sixMinutes},
			row: tenantPlacementLivenessRow{
				cell: "cell-b", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: sixMinutes}},
			},
			wantOK: true, wantReason: ReasonTenantMoved,
		},
		{
			name: "moved wins over silent when both hold",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", lastHeartbeatAt: &oldHeartbeat},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new", cellAlive: false,
				cellAliveAt: &fresh, cellLastSeenAt: &oldHeartbeat,
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: oldHeartbeat}},
			},
			wantOK: true, wantReason: ReasonCellRolled,
		},
		{
			name: "a live run on a rolled cell is not marked yet",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", lastHeartbeatAt: &fresh},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: sixMinutes}},
			},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := evaluateInterruption(now, tc.c, tc.row, true)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestAfterMarkPublishesCancelOnlyForNoHeartbeat asserts a cancel_run
// action publishes for the silent reason and none for a moved reason.
func TestAfterMarkPublishesCancelOnlyForNoHeartbeat(t *testing.T) {
	conn := newTestActionConn(t)
	received := make(chan LedgerAction, 4)
	sub, err := conn.Subscribe(bus.LedgerActionSubject("core"), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	conn.NATSConn().Flush()

	run := runInterruptCandidate{
		runID: uuid.Must(uuid.NewV7()), cluster: "core", namespace: "team-a", cell: "cell-1", jobPath: "folder/job",
	}
	afterMark(context.Background(), conn, nil, nil, run, ReasonNoHeartbeat, "", nil)

	select {
	case a := <-received:
		if a.Kind != LedgerActionCancelRun || a.RunID != run.runID.String() {
			t.Fatalf("got %+v, want a cancel_run for %s", a, run.runID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a cancel_run action for the no-heartbeat reason")
	}

	afterMark(context.Background(), conn, nil, nil, run, ReasonTenantMoved, "", nil)
	select {
	case a := <-received:
		t.Fatalf("want no action for a moved reason, got %+v", a)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAfterMarkPublishesAutomaticRetry asserts an "automatic" policy
// publishes one retry_run action with the retry:<run-id> intent key, and
// that no run_intents row is written for it.
func TestAfterMarkPublishesAutomaticRetry(t *testing.T) {
	_, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := make(chan LedgerAction, 4)
	sub, err := conn.Subscribe(bus.LedgerActionSubject("core"), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	conn.NATSConn().Flush()

	runID := uuid.Must(uuid.NewV7())
	run := runInterruptCandidate{
		runID: runID, cluster: "core", namespace: "team-a", cell: "cell-1", jobPath: "folder/job", actor: "alice",
	}
	afterMark(context.Background(), conn, nil, nil, run, ReasonNoHeartbeat, interruptedRetryAutomatic, nil)

	var gotCancel, gotRetry bool
	for i := 0; i < 2; i++ {
		select {
		case a := <-received:
			switch a.Kind {
			case LedgerActionCancelRun:
				gotCancel = true
			case LedgerActionRetryRun:
				gotRetry = true
				if a.IntentKey != "retry:"+runID.String() {
					t.Fatalf("intent key = %q, want retry:%s", a.IntentKey, runID)
				}
				if a.RunID == runID.String() {
					t.Fatal("want a fresh run id for the retry, not the marked run's own id")
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for both actions")
		}
	}
	if !gotCancel || !gotRetry {
		t.Fatalf("gotCancel=%v gotRetry=%v, want both", gotCancel, gotRetry)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents`).Scan(&count); err != nil {
		t.Fatalf("count run_intents: %v", err)
	}
	if count != 0 {
		t.Fatalf("run_intents rows = %d, want 0: the recorder never inserts the retry intent itself", count)
	}
}

// freshenSweepConsumer publishes one message on the varroa_builds
// stream and pulls and acks it through the recorder consumer, so the
// consumer's AckFloor.Last becomes recent and its pending count returns
// to zero. Every sweep test that expects a round to actually run, rather
// than be gated, calls this once before it seeds the scenario it wants
// the sweep to mark.
func freshenSweepConsumer(t *testing.T, conn *bus.Conn) {
	t.Helper()
	if _, err := conn.PublishJetStream("builds.core.ns.cell", []byte("{}")); err != nil {
		t.Fatalf("publish freshening message: %v", err)
	}
	sub, err := conn.PullSubscribe(bus.BuildsWildcard, bus.BuildsStreamName, RecorderConsumerName)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	msgs, err := sub.Fetch(1, nats.MaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetch freshening message: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(msgs))
	}
	if err := msgs[0].Ack(); err != nil {
		t.Fatalf("ack freshening message: %v", err)
	}
}

// TestSweepInterruptedRunsMarksASilentRun is the sweep's end-to-end
// case: a fresh consumer, a RUNNING run whose heartbeat is silent, and a
// placement row confirming its owning cell instance is not alive. The
// sweep marks it, publishes one cancel_run action, and counts the mark
// under path "sweep".
func TestSweepInterruptedRunsMarksASilentRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))

	past := time.Now().Add(-17 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET last_heartbeat_at = $1 WHERE run_id = $2`, past, runID); err != nil {
		t.Fatalf("backdate last_heartbeat_at: %v", err)
	}
	seedTenantPlacementLiveness(t, db, "pod-a", false, &past, &past, nil)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

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
	conn.NATSConn().Flush()

	reader, restore := swapMeterProvider(t)
	defer restore()
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, DefaultSweepMaxPending, time.Now(), nil)
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
	if status != "COMPLETED" || result != "INTERRUPTED" || reason != ReasonNoHeartbeat {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result, reason, ReasonNoHeartbeat)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionCancelRun || a.RunID != runID.String() {
			t.Fatalf("got %+v, want a cancel_run for %s", a, runID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a cancel_run action published for the marked run")
	}

	if got := sumCounterByAttr(t, reader, "recorder_interrupted_marked_total", "path", "sweep"); got != 1 {
		t.Fatalf("recorder_interrupted_marked_total{path=sweep} = %d, want 1", got)
	}
}

// TestSweepInterruptedRunsSkipsOnPendingBacklog asserts the sweep reads
// no run and counts a skip when the consumer's pending count is at or
// above maxPending.
func TestSweepInterruptedRunsSkipsOnPendingBacklog(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	conn, cleanup := newSweepTestBus(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		if _, err := conn.PublishJetStream("builds.core.ns.cell", []byte("{}")); err != nil {
			t.Fatalf("publish backlog message %d: %v", i, err)
		}
	}

	reader, restore := swapMeterProvider(t)
	defer restore()
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, 1, time.Now(), nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 0 {
		t.Fatalf("marked = %d, want 0", marked)
	}
	if got := sumCounterByAttr(t, reader, "recorder_sweep_skipped_total", "reason", "pending"); got != 1 {
		t.Fatalf("recorder_sweep_skipped_total{reason=pending} = %d, want 1", got)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "RUNNING" {
		t.Fatalf("status = %q, want RUNNING unchanged", status)
	}
}

// TestSweepInterruptedRunsSkipsOnStaleConsumer asserts the sweep skips
// its round, and counts it, when the consumer has never processed a
// message.
func TestSweepInterruptedRunsSkipsOnStaleConsumer(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	conn, cleanup := newSweepTestBus(t)
	defer cleanup()

	reader, restore := swapMeterProvider(t)
	defer restore()
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, DefaultSweepMaxPending, time.Now(), nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 0 {
		t.Fatalf("marked = %d, want 0", marked)
	}
	if got := sumCounterByAttr(t, reader, "recorder_sweep_skipped_total", "reason", "stale"); got != 1 {
		t.Fatalf("recorder_sweep_skipped_total{reason=stale} = %d, want 1", got)
	}
}

// newSweepTestBus starts an embedded JetStream-enabled NATS server,
// ensures the varroa_builds stream and the recorder consumer exist, and
// returns a connected *bus.Conn plus a cleanup func.
func newSweepTestBus(t *testing.T) (*bus.Conn, func()) {
	t.Helper()
	s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		s.Shutdown()
		t.Fatalf("connect: %v", err)
	}
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure builds stream: %v", err)
	}
	if err := EnsureIngestConsumer(conn, bus.BuildsStreamName); err != nil {
		t.Fatalf("ensure ingest consumer: %v", err)
	}
	return conn, func() {
		conn.Close()
		s.Shutdown()
	}
}

// swapMeterProvider installs a fresh ManualReader-backed MeterProvider as
// the global provider, following the ledger_fence_metrics_test.go
// pattern, and returns a restore func.
func swapMeterProvider(t *testing.T) (*sdkmetric.ManualReader, func()) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	return reader, func() { otel.SetMeterProvider(prev) }
}

// sumCounterByAttr sums every data point of metricName whose attrKey
// attribute equals attrValue.
func sumCounterByAttr(t *testing.T, reader *sdkmetric.ManualReader, metricName, attrKey, attrValue string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != metricName {
				continue
			}
			data, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range data.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(attrKey)); ok && v.AsString() == attrValue {
					total += dp.Value
				}
			}
		}
	}
	return total
}
