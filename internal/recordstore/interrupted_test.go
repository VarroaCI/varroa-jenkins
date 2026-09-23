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
	all, err := selectCandidates(context.Background(), pool)
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

// TestMarkInterruptedClearsThePendingSet proves the sweep's marking write
// clears a paused run's pending set, so a run that ends while paused
// holds none.
func TestMarkInterruptedClearsThePendingSet(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)

	pending := newTestEnvelope(t, runID, intentKey, KindRunInput,
		RunInputPayload{Seq: inputSeqPtr(3), Inputs: runInputSet("input-1")})
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), pending); err != nil {
		t.Fatalf("apply run.input: %v", err)
	}

	c := fetchRunningCandidate(t, pool, runID)
	changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now())
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if !changed {
		t.Fatal("want the row changed")
	}

	inputs, _ := readRunInputsRow(t, db, runID)
	if inputs != "" {
		t.Fatalf("stored inputs = %q, want NULL after the mark", inputs)
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

// TestEvaluateResumeGuardRows asserts design D14's three guard rows: an
// armed run inside its arm window (0a) and a dispatched run inside its
// deadline (0b) are both shielded, and a run past its applicable window
// (0c) is marked resume-timeout. A dispatched run whose last heartbeat
// is newer than the resume's own request time stays shielded even past
// the deadline, because a live resume proves itself by that heartbeat.
func TestEvaluateResumeGuardRows(t *testing.T) {
	now := time.Now()
	future := now.Add(5 * time.Minute)
	past := now.Add(-time.Minute)
	requestedAt := now.Add(-3 * time.Minute)
	newerHeartbeat := now.Add(-time.Minute)
	olderHeartbeat := now.Add(-4 * time.Minute)

	cases := []struct {
		name        string
		c           runInterruptCandidate
		wantDecided bool
		wantOK      bool
		wantReason  string
	}{
		{
			name:        "no mesh resume leaves the ordinary tests to answer",
			c:           runInterruptCandidate{},
			wantDecided: false,
		},
		{
			name:        "row 0a: an armed run inside its arm window is not marked",
			c:           runInterruptCandidate{mesh: &runMeshRecord{Resume: &runMeshResume{ArmExpires: &future}}},
			wantDecided: true, wantOK: false,
		},
		{
			name: "row 0b: a dispatched run inside its deadline is not marked",
			c: runInterruptCandidate{
				mesh: &runMeshRecord{Resume: &runMeshResume{Instance: "pod-live", Deadline: &future}},
			},
			wantDecided: true, wantOK: false,
		},
		{
			name:        "row 0c: an armed run past its arm window is marked resume-timeout",
			c:           runInterruptCandidate{mesh: &runMeshRecord{Resume: &runMeshResume{ArmExpires: &past}}},
			wantDecided: true, wantOK: true, wantReason: ReasonResumeTimeout,
		},
		{
			name: "row 0c: a dispatched run past its deadline with no fresher heartbeat is marked resume-timeout",
			c: runInterruptCandidate{
				mesh: &runMeshRecord{
					Resume: &runMeshResume{Instance: "pod-live", Deadline: &past, RequestedAt: &requestedAt},
				},
				heartbeatReceivedAt: &olderHeartbeat,
			},
			wantDecided: true, wantOK: true, wantReason: ReasonResumeTimeout,
		},
		{
			// A heartbeat newer than the request proves the target took
			// the run over, so the guard decides nothing and the
			// ordinary liveness tests apply. Without that fall-through
			// a resumed run would be shielded for the rest of its life,
			// and a later death of the target would never mark it.
			name: "a resume a fresher heartbeat proved leaves the ordinary tests to answer",
			c: runInterruptCandidate{
				mesh: &runMeshRecord{
					Resume: &runMeshResume{Instance: "pod-live", Deadline: &past, RequestedAt: &requestedAt},
				},
				heartbeatReceivedAt: &newerHeartbeat,
			},
			wantDecided: false,
		},
		{
			name: "row 0b: a dispatched run inside its deadline with a fresher heartbeat is not marked",
			c: runInterruptCandidate{
				mesh: &runMeshRecord{
					Resume: &runMeshResume{Instance: "pod-live", Deadline: &future, RequestedAt: &requestedAt},
				},
				heartbeatReceivedAt: &olderHeartbeat,
			},
			wantDecided: true, wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok, decided := evaluateResumeGuard(now, tc.c)
			if decided != tc.wantDecided {
				t.Fatalf("decided = %v, want %v", decided, tc.wantDecided)
			}
			if !decided {
				return
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestMarkInterruptedNoopsWhenAnArmRacesTheMark asserts a mark that
// arrives after an arm committed changes no row: the sweep read its
// candidate before the arm committed, but the mark's own SQL re-reads
// the committed payload and finds a live arm window.
func TestMarkInterruptedNoopsWhenAnArmRacesTheMark(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:"+newTestUUIDv7String())
	c := fetchRunningCandidate(t, pool, runID)

	future := time.Now().Add(10 * time.Minute)
	if _, err := db.Exec(`
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         jsonb_build_object('resume', jsonb_build_object('armed_at', now(), 'arm_expires', $2::timestamptz)))
		 WHERE run_id = $1
	`, runID, future); err != nil {
		t.Fatalf("commit an arm that races the mark: %v", err)
	}

	changed, err := markInterrupted(context.Background(), pool, c, ReasonNoHeartbeat, time.Now())
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if changed {
		t.Fatal("want changed=false: the committed arm must shield the run")
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "RUNNING" {
		t.Fatalf("status = %q, want RUNNING unchanged", status)
	}
}

// TestAfterMarkPublishesCancelForDispatchedResumeTimeoutOnly asserts a
// resume-timeout mark publishes one cancel_run when the mesh already
// dispatched the run to a live target, and none when it only armed one:
// an undispatched arm still names the dead instance, so no process
// remains to stop.
func TestAfterMarkPublishesCancelForDispatchedResumeTimeoutOnly(t *testing.T) {
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

	dispatched := runInterruptCandidate{
		runID: uuid.Must(uuid.NewV7()), cluster: "core", namespace: "team-a", cell: "cell-1", jobPath: "folder/job",
		mesh: &runMeshRecord{Resume: &runMeshResume{Instance: "pod-live"}},
	}
	afterMark(context.Background(), conn, nil, nil, dispatched, ReasonResumeTimeout, "", nil)

	select {
	case a := <-received:
		if a.Kind != LedgerActionCancelRun || a.RunID != dispatched.runID.String() {
			t.Fatalf("got %+v, want a cancel_run for %s", a, dispatched.runID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a cancel_run action for a dispatched resume-timeout")
	}

	armedOnly := runInterruptCandidate{
		runID: uuid.Must(uuid.NewV7()), cluster: "core", namespace: "team-a", cell: "cell-1", jobPath: "folder/job",
		mesh: &runMeshRecord{Resume: &runMeshResume{}},
	}
	afterMark(context.Background(), conn, nil, nil, armedOnly, ReasonResumeTimeout, "", nil)
	select {
	case a := <-received:
		t.Fatalf("want no action for an undispatched resume-timeout, got %+v", a)
	case <-time.After(300 * time.Millisecond):
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
	fortySeconds := now.Add(-40 * time.Second)
	confirmed := now.Add(-45 * time.Second)
	freshConfirmation := now.Add(-5 * time.Second)

	cases := []struct {
		name       string
		c          runInterruptCandidate
		row        tenantPlacementLivenessRow
		wantOK     bool
		wantReason string
	}{
		{
			name: "no-heartbeat",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-a", heartbeatReceivedAt: &oldHeartbeat},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-a", cellAlive: false,
				cellAliveAt: &fresh, cellLastSeenAt: &oldHeartbeat,
			},
			wantOK: true, wantReason: ReasonNoHeartbeat,
		},
		{
			name: "cell-rolled",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", heartbeatReceivedAt: &sixMinutes},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: sixMinutes}},
			},
			wantOK: true, wantReason: ReasonCellRolled,
		},
		{
			name: "tenant-moved",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", heartbeatReceivedAt: &sixMinutes},
			row: tenantPlacementLivenessRow{
				cell: "cell-b", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: sixMinutes}},
			},
			wantOK: true, wantReason: ReasonTenantMoved,
		},
		{
			name: "moved wins over silent when both hold",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", heartbeatReceivedAt: &oldHeartbeat},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new", cellAlive: false,
				cellAliveAt: &fresh, cellLastSeenAt: &oldHeartbeat,
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: oldHeartbeat}},
			},
			wantOK: true, wantReason: ReasonCellRolled,
		},
		{
			name: "a live run on a rolled cell is not marked yet",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", heartbeatReceivedAt: &fresh},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: sixMinutes}},
			},
			wantOK: false,
		},
		{
			// The heartbeat is 40 seconds old, far inside every other
			// window. The pod read decides on its own.
			name: "cell-lost with a fresh heartbeat",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", heartbeatReceivedAt: &fortySeconds},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{
					Instance: "pod-old", LastSeenAt: fortySeconds, ConfirmedGoneAt: &confirmed,
				}},
			},
			wantOK: true, wantReason: ReasonCellLost,
		},
		{
			name: "confirmed wins over moved when both hold",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", heartbeatReceivedAt: &sixMinutes},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{
					Instance: "pod-old", LastSeenAt: sixMinutes, ConfirmedGoneAt: &confirmed,
				}},
			},
			wantOK: true, wantReason: ReasonCellLost,
		},
		{
			name: "a fresh confirmation waits out the grace",
			c:    runInterruptCandidate{cell: "cell-a", cellInstance: "pod-old", heartbeatReceivedAt: &fortySeconds},
			row: tenantPlacementLivenessRow{
				cell: "cell-a", cellInstance: "pod-new",
				deadInstances: []TenantSyncDeadInstance{{
					Instance: "pod-old", LastSeenAt: fortySeconds, ConfirmedGoneAt: &freshConfirmation,
				}},
			},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := evaluateInterruption(now, tc.c, resolveLivenessMembers(tc.row), true, testGrace)
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

	// A cell-lost mark proves the pod gone, so no process remains to
	// stop and the cell that would receive the command no longer runs.
	afterMark(context.Background(), conn, nil, nil, run, ReasonCellLost, "", nil)
	select {
	case a := <-received:
		t.Fatalf("want no action for a cell-lost reason, got %+v", a)
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
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, past, runID); err != nil {
		t.Fatalf("backdate heartbeat_received_at: %v", err)
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

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil, DefaultSweepMaxPending, time.Now(), testGrace, nil)
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

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil, 1, time.Now(), testGrace, nil)
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

	// The skip needs work in hand: one unconsumed message leaves the
	// consumer with a pending count above zero and no ack floor at all.
	if _, err := conn.PublishJetStream("builds.core.ns.cell", []byte("{}")); err != nil {
		t.Fatalf("publish a pending message: %v", err)
	}

	reader, restore := swapMeterProvider(t)
	defer restore()
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil, DefaultSweepMaxPending, time.Now(), testGrace, nil)
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

// seedAcceptedOnlyRunRowInstance inserts one QUEUED runs row holding the
// accepted keys, with the given accepting instance, so a sweep test can
// control which instance the row names. The cell is fixed to "cell-a",
// the tenant to testTenant, and the job path to testJobPath, since none
// of these tests vary any of the three.
func seedAcceptedOnlyRunRowInstance(t *testing.T, db *sql.DB, instance string, createdAt time.Time) uuid.UUID {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, payload
		) VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'QUEUED',
			jsonb_build_object('accepted_at', $2::timestamptz, 'accepted_instance', $8::text))
	`, runID, createdAt, "manual:"+runID.String(), testCluster, testNamespace, testTenant, testJobPath, instance); err != nil {
		t.Fatalf("seed accepted-only run: %v", err)
	}
	return runID
}

// seedPlacementLivenessWithPolicy inserts one tenant_placements row with
// full control over cell_instance, dead_instances, and policy, for a
// sweep test that needs an automatic-retry policy the shared
// seedTenantPlacementLiveness helper cannot express. The cell is fixed
// to "cell-a", since none of these tests vary it.
func seedPlacementLivenessWithPolicy(t *testing.T, db *sql.DB, cellInstance string, dead []TenantSyncDeadInstance, policy string) {
	t.Helper()
	deadJSON, err := json.Marshal(dead)
	if err != nil {
		t.Fatalf("marshal dead instances: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, cell, cell_instance, cell_alive, dead_instances, policy)
		VALUES ($1, $2, 'cell-a', $3, true, $4::jsonb, $5::jsonb)
	`, testCluster, testTenant, cellInstance, string(deadJSON), policy); err != nil {
		t.Fatalf("seed tenant placement liveness row: %v", err)
	}
}

// TestSweepMarksQueuedRunWithDeadAcceptingInstance asserts the sweep
// marks a QUEUED accepted-only run cell-rolled when its accepting
// instance is listed dead older than five minutes, with the no-summary
// finalizer, one build.interrupted row carrying a null build number, and
// no cancel_run or retry_run even though the tenant's policy is
// automatic.
func TestSweepMarksQueuedRunWithDeadAcceptingInstance(t *testing.T) {
	pool, db := setupLedgerPool(t)

	past := time.Now().UTC().Add(-6 * time.Minute)
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-old", past)
	seedPlacementLivenessWithPolicy(t, db, "pod-new",
		[]TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: past}},
		`{"interruptedRetry": "automatic"}`)

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

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil, DefaultSweepMaxPending, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}

	var status, result, reason string
	var buildNumber sql.NullInt64
	var finalized bool
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason', build_number,
		       (payload->'log_summary'->>'finalized')::boolean
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason, &buildNumber, &finalized); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" || reason != ReasonCellRolled {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result, reason, ReasonCellRolled)
	}
	if buildNumber.Valid {
		t.Fatal("build_number must stay null for a queued mark")
	}
	if !finalized {
		t.Fatal("want the log summary finalized")
	}

	var activityCount int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.activity WHERE type = 'build.interrupted' AND build_number IS NULL
	`).Scan(&activityCount); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if activityCount != 1 {
		t.Fatalf("activity rows with a null build number = %d, want 1", activityCount)
	}

	select {
	case a := <-received:
		t.Fatalf("want no action published for a queued mark, got %+v", a)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSweepLeavesQueuedRunWithLiveAcceptingInstanceAlone asserts a live
// accepting instance marks nothing.
func TestSweepLeavesQueuedRunWithLiveAcceptingInstanceAlone(t *testing.T) {
	pool, db := setupLedgerPool(t)

	past := time.Now().UTC().Add(-time.Hour)
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-live", past)
	seedPlacementLivenessWithPolicy(t, db, "pod-live", nil, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil, DefaultSweepMaxPending, time.Now(), testGrace, nil)
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
	if status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED unchanged", status)
	}
}

// TestSweepSkipsQueuedPlaceholderWithNoOwningInstance asserts a QUEUED
// row with no run_intents row and no accepted_instance payload key is
// skipped and counted, rather than evaluated with an empty instance.
func TestSweepSkipsQueuedPlaceholderWithNoOwningInstance(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := uuid.Must(uuid.NewV7())
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, now(), $2, $3, $4, 'cell-a', $5, $6, 'QUEUED')
	`, runID, "manual:"+runID.String(), testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed a bare queued placeholder: %v", err)
	}

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	reader, restore := swapMeterProvider(t)
	defer restore()
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil, DefaultSweepMaxPending, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 0 {
		t.Fatalf("marked = %d, want 0", marked)
	}
	if got := sumCounterByAttr(t, reader, "recorder_sweep_skipped_total", "reason", "no-owning-instance"); got != 1 {
		t.Fatalf("recorder_sweep_skipped_total{reason=no-owning-instance} = %d, want 1", got)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED unchanged", status)
	}
}

// TestSweepMarksClaimedQueuedRunThroughIntentInstance asserts a QUEUED
// run that holds a granted intent, but no accepted_instance payload key,
// is marked through the intent's own cell_instance.
func TestSweepMarksClaimedQueuedRunThroughIntentInstance(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	req := newTestClaimRequest("manual:claimed-queued-1", 0)
	req.Cell = "cell-a"
	req.CellInstance = "pod-old"
	granted := mustGrant(t, NewLedger(pool, nil), req)
	runID := uuid.MustParse(granted.RunID)

	// A plain QUEUED placeholder row for the granted intent, with no
	// accepted_instance payload key: the intent's own cell_instance is
	// this run's only owning-instance source. Its created_at is the
	// liveness baseline, aged past the moved window.
	past := time.Now().UTC().Add(-6 * time.Minute)
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'QUEUED')
	`, runID, past, req.IntentKey, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed a queued placeholder for the granted intent: %v", err)
	}

	seedPlacementLivenessWithPolicy(t, db, "pod-new",
		[]TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: past}}, `{}`)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(ctx, pool, conn, metrics, nil, nil, DefaultSweepMaxPending, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}

	var status, reason string
	if err := db.QueryRow(`
		SELECT status, payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || reason != ReasonCellRolled {
		t.Fatalf("status=%q reason=%q, want COMPLETED/%q", status, reason, ReasonCellRolled)
	}
}

// TestMarkMovedRunsForTenantMarksQueuedRunOfOldCell asserts the
// announce-triggered fast path marks a queued run of the tenant's old
// cell, through its accepted_instance.
func TestMarkMovedRunsForTenantMarksQueuedRunOfOldCell(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	past := time.Now().UTC().Add(-6 * time.Minute)
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-old", past)

	resp := sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-new",
		DeadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: past}},
	})
	if resp.InterruptedMarked != 1 {
		t.Fatalf("InterruptedMarked = %d, want 1", resp.InterruptedMarked)
	}

	var status, result, reason string
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" || reason != ReasonCellRolled {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result, reason, ReasonCellRolled)
	}
}

// TestSweepConsumerSkip asserts the consumer gate tests a backlog and
// never quiet. A consumer with no pending message has processed
// everything the stream holds, so an old or absent AckFloor.Last means
// only that no build event arrived lately. A quiet tenant is exactly
// the case the sweep must serve: nothing else would refresh the floor,
// and a lost cell's runs would wait for a build that never comes.
func TestSweepConsumerSkip(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-5 * time.Second)
	old := now.Add(-10 * time.Minute)
	const maxPending = 1000

	cases := []struct {
		name       string
		pending    uint64
		floor      *time.Time
		wantSkip   bool
		wantReason string
	}{
		{name: "a backlog at the cap skips", pending: maxPending, floor: &fresh, wantSkip: true, wantReason: "pending"},
		{name: "a backlog past the cap skips", pending: maxPending + 1, floor: &fresh, wantSkip: true, wantReason: "pending"},
		{name: "work in hand with an old floor skips", pending: 5, floor: &old, wantSkip: true, wantReason: "stale"},
		{name: "work in hand with a nil floor skips", pending: 5, floor: nil, wantSkip: true, wantReason: "stale"},
		{name: "work in hand with a fresh floor runs", pending: 5, floor: &fresh, wantSkip: false},
		{name: "an idle consumer with an old floor runs", pending: 0, floor: &old, wantSkip: false},
		{name: "an idle consumer with a nil floor runs", pending: 0, floor: nil, wantSkip: false},
		{name: "an idle consumer with a fresh floor runs", pending: 0, floor: &fresh, wantSkip: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := &nats.ConsumerInfo{
				NumPending: tc.pending,
				AckFloor:   nats.SequenceInfo{Last: tc.floor},
			}
			reason, skip := sweepConsumerSkip(info, maxPending, now)
			if skip != tc.wantSkip {
				t.Fatalf("skip = %v, want %v", skip, tc.wantSkip)
			}
			if skip && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if !skip && reason != "" {
				t.Fatalf("reason = %q, want empty when the round runs", reason)
			}
		})
	}
}

// cellLossCountByPath sums every recorder_cell_loss_confirmed_total data
// point whose "path" attribute equals path.
func cellLossCountByPath(t *testing.T, reader *sdkmetric.ManualReader, path string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "recorder_cell_loss_confirmed_total" {
				continue
			}
			data, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range data.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("path")); ok && v.AsString() == path {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// useManualMeter installs a manual metric reader for the duration of the
// test and answers it.
func useManualMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return reader
}

// TestAnnounceHookMarksCellLostWithAFreshHeartbeat asserts the announce
// hook does not skip a cell-lost reading. The run's heartbeat is 40
// seconds old, far inside the five-minute window, and the pod-read
// confirmation alone decides. It also asserts
// recorder_cell_loss_confirmed_total counts one "announce" mark.
func TestAnnounceHookMarksCellLostWithAFreshHeartbeat(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useManualMeter(t)
	conn := newTestSyncConn(t)

	now := time.Now().UTC()
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-old", now.Add(-40*time.Second))
	confirmed := now.Add(-45 * time.Second)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	resp := sendTenantSyncWithMetrics(t, pool, conn, metrics, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "",
		DeadInstances: []TenantSyncDeadInstance{{
			Instance: "pod-old", LastSeenAt: now.Add(-40 * time.Second), ConfirmedGoneAt: &confirmed,
		}},
	})
	if resp.InterruptedMarked != 1 {
		t.Fatalf("InterruptedMarked = %d, want 1: the announce hook must not skip a cell-lost reading", resp.InterruptedMarked)
	}

	var status, result, reason string
	if err := db.QueryRow(`
		SELECT status, result, payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" || reason != ReasonCellLost {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%q", status, result, reason, ReasonCellLost)
	}
	if got := cellLossCountByPath(t, reader, "announce"); got != 1 {
		t.Fatalf("recorder_cell_loss_confirmed_total{path=announce} = %d, want 1", got)
	}
}

// TestSweepMarksCellLostAndCountsIt asserts the periodic sweep marks a
// confirmed loss with the cell-lost reason and counts it under the
// "sweep" path.
func TestSweepMarksCellLostAndCountsIt(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useManualMeter(t)
	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	now := time.Now().UTC()
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-old", now.Add(-40*time.Second))
	confirmed := now.Add(-45 * time.Second)
	seedPlacementLivenessWithPolicy(t, db, "pod-new", []TenantSyncDeadInstance{{
		Instance: "pod-old", LastSeenAt: now.Add(-40 * time.Second), ConfirmedGoneAt: &confirmed,
	}}, `{}`)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil,
		DefaultSweepMaxPending, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}

	var reason string
	if err := db.QueryRow(`
		SELECT payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if reason != ReasonCellLost {
		t.Fatalf("interrupted_reason = %q, want %q", reason, ReasonCellLost)
	}
	if got := cellLossCountByPath(t, reader, "sweep"); got != 1 {
		t.Fatalf("recorder_cell_loss_confirmed_total{path=sweep} = %d, want 1", got)
	}
}

// TestSweepMarksCellLostOnAnIdleConsumer asserts the sweep marks a
// confirmed loss with no build event in the whole fleet. The consumer is
// idle, so it holds no pending message and its ack floor never moved.
// A quiet tenant is exactly the case the sweep must serve: nothing else
// would refresh the floor, so the lost run would otherwise wait for a
// build that never comes.
func TestSweepMarksCellLostOnAnIdleConsumer(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	// Deliberately no freshening publish and no fetch.

	now := time.Now().UTC()
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-old", now.Add(-40*time.Second))
	confirmed := now.Add(-45 * time.Second)
	seedPlacementLivenessWithPolicy(t, db, "pod-new", []TenantSyncDeadInstance{{
		Instance: "pod-old", LastSeenAt: now.Add(-40 * time.Second), ConfirmedGoneAt: &confirmed,
	}}, `{}`)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil,
		DefaultSweepMaxPending, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1: an idle consumer must not skip the round", marked)
	}

	var reason string
	if err := db.QueryRow(`
		SELECT payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if reason != ReasonCellLost {
		t.Fatalf("interrupted_reason = %q, want %q", reason, ReasonCellLost)
	}
}

// TestDeadInstancesTrimKeepsUnclaimedQueuedRunEntry asserts the trim
// keeps a dead_instances entry an unclaimed queued run names through its
// accepted_instance payload value.
func TestDeadInstancesTrimKeepsUnclaimedQueuedRunEntry(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	past := time.Now().UTC()
	seedAcceptedOnlyRunRowInstance(t, db, "pod-referenced", past)

	first := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current",
		DeadInstances: []TenantSyncDeadInstance{{Instance: "pod-referenced", LastSeenAt: past.Add(-time.Hour)}},
	}
	sendTenantSync(t, pool, conn, first)

	entries := make([]TenantSyncDeadInstance, 0, 12)
	for i := 0; i < 12; i++ {
		entries = append(entries, TenantSyncDeadInstance{
			Instance: "pod-extra-" + string(rune('a'+i)), LastSeenAt: past.Add(-time.Duration(i) * time.Minute),
		})
	}
	second := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current", DeadInstances: entries,
	}
	sendTenantSync(t, pool, conn, second)

	stored := readTenantPlacement(t, db, testTenant)
	var referencedKept bool
	for _, d := range stored.deadInstances {
		if d.Instance == "pod-referenced" {
			referencedKept = true
		}
	}
	if !referencedKept {
		t.Fatalf("want pod-referenced kept because an unclaimed QUEUED run names it, got %v", stored.deadInstances)
	}
	if len(stored.deadInstances) != 11 {
		t.Fatalf("want 1 kept entry plus 10 trimmed entries (11 total), got %d: %v", len(stored.deadInstances), stored.deadInstances)
	}
}
