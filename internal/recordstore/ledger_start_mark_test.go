package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// startMarkOf returns req as the start mark of the same run.
func startMarkOf(req ClaimRequest) ClaimRequest {
	mark := req
	mark.StartMark = true
	return mark
}

// readStartMarkedAt reads one intent's start_marked_at.
func readStartMarkedAt(t *testing.T, db *sql.DB, intentKey string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := db.QueryRow(`SELECT start_marked_at FROM varroa.run_intents WHERE intent_key = $1`,
		intentKey).Scan(&at); err != nil {
		t.Fatalf("read start_marked_at: %v", err)
	}
	return at
}

// mustMark sends the start mark of req and fails the test unless it
// answers granted.
func mustMark(t *testing.T, ledger *Ledger, req ClaimRequest) ClaimResponse {
	t.Helper()
	resp := ledger.Claim(context.Background(), startMarkOf(req))
	if resp.Decision != DecisionGranted {
		t.Fatalf("start mark of %q: want granted, got %+v", req.IntentKey, resp)
	}
	return resp
}

// noConsumerRead fails the test when the grant hold reads the consumer.
func noConsumerRead(t *testing.T) func(context.Context) (*nats.ConsumerInfo, error) {
	return func(context.Context) (*nats.ConsumerInfo, error) {
		t.Error("the claim read the ingest consumer")
		return &nats.ConsumerInfo{}, nil
	}
}

// TestMarkStartStoresMarkForOwner: the owner's mark answers granted with
// the stored number and writes start_marked_at.
func TestMarkStartStoresMarkForOwner(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:mark-owner", 1)
	grant := mustGrant(t, ledger, req)
	if at := readStartMarkedAt(t, db, req.IntentKey); at != nil {
		t.Fatalf("want no mark after the grant, got %v", at)
	}

	resp := mustMark(t, ledger, req)
	if resp.RunID != grant.RunID || resp.BuildNumber != grant.BuildNumber {
		t.Fatalf("want the grant's run and number %+v, got %+v", grant, resp)
	}
	if readStartMarkedAt(t, db, req.IntentKey) == nil {
		t.Fatal("want start_marked_at set")
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
}

// TestMarkStartRepeatIsIdempotent: a second mark from the owner answers
// granted and keeps the first mark time.
func TestMarkStartRepeatIsIdempotent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:mark-repeat", 1)
	mustGrant(t, ledger, req)
	mustMark(t, ledger, req)
	first := readStartMarkedAt(t, db, req.IntentKey)

	mustMark(t, ledger, req)
	if again := readStartMarkedAt(t, db, req.IntentKey); !again.Equal(*first) {
		t.Fatalf("a repeat mark moved start_marked_at from %v to %v", first, again)
	}
}

// TestMarkStartOtherInstanceAnswersDuplicate: a mark from an instance the
// intent does not name answers duplicate and writes nothing.
func TestMarkStartOtherInstanceAnswersDuplicate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:mark-other", 1)
	grant := mustGrant(t, ledger, req)

	resp := ledger.Claim(context.Background(), startMarkOf(repeatFrom(req, "pod-b")))
	if resp.Decision != DecisionDuplicate || resp.RunID != grant.RunID {
		t.Fatalf("want duplicate for run %s, got %+v", grant.RunID, resp)
	}
	if at := readStartMarkedAt(t, db, req.IntentKey); at != nil {
		t.Fatalf("want no mark, got %v", at)
	}
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-a" {
		t.Fatalf("want the owner kept at pod-a, got %s", owner)
	}
}

// TestMarkStartMovedRunAnswersDuplicate: after a transfer to a new run, a
// mark for the old run answers duplicate with the stored run.
func TestMarkStartMovedRunAnswersDuplicate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:mark-moved", 1)
	mustGrant(t, ledger, req)
	transfer := newTestClaimRequest(req.IntentKey, 2)
	mustGrant(t, ledger, transfer)

	old := req
	old.PlacementGeneration = 2
	resp := ledger.Claim(context.Background(), startMarkOf(old))
	if resp.Decision != DecisionDuplicate || resp.RunID != transfer.RunID {
		t.Fatalf("want duplicate naming run %s, got %+v", transfer.RunID, resp)
	}
	if at := readStartMarkedAt(t, db, req.IntentKey); at != nil {
		t.Fatalf("want no mark, got %v", at)
	}
}

// TestMarkStartMovedRunMarksItsRowDuplicate: a mark for a run the intent
// no longer names ends that run's accepted-trigger row DUPLICATE, and a
// mark from a former owner of the same run leaves the run's row alone.
func TestMarkStartMovedRunMarksItsRowDuplicate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	status := func(runID string) string {
		t.Helper()
		var s string
		if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&s); err != nil {
			t.Fatalf("read run status: %v", err)
		}
		return s
	}

	req := newTestClaimRequest("manual:mark-moved-row", 1)
	mustGrant(t, ledger, req)
	seedAcceptedRunRow(t, db, req)
	transfer := repeatFrom(newTestClaimRequest(req.IntentKey, 2), "pod-b")
	mustGrant(t, ledger, transfer)

	old := req
	old.PlacementGeneration = 2
	if resp := ledger.Claim(context.Background(), startMarkOf(old)); resp.Decision != DecisionDuplicate {
		t.Fatalf("want duplicate, got %+v", resp)
	}
	if got := status(req.RunID); got != "DUPLICATE" {
		t.Fatalf("want the moved run's row DUPLICATE, got %s", got)
	}

	same := newTestClaimRequest("manual:mark-moved-owner", 1)
	mustGrant(t, ledger, same)
	seedAcceptedRunRow(t, db, same)
	if resp := ledger.Claim(context.Background(), startMarkOf(repeatFrom(same, "pod-b"))); resp.Decision != DecisionDuplicate {
		t.Fatalf("want duplicate from another owner, got %+v", resp)
	}
	if got := status(same.RunID); got != "QUEUED" {
		t.Fatalf("want the run's own row kept QUEUED, got %s", got)
	}
}

// TestMarkStartConsumedOwnerAnswersGranted: a mark for a consumed intent
// of the same owner answers granted and writes nothing.
func TestMarkStartConsumedOwnerAnswersGranted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:mark-consumed", 1)
	mustGrant(t, ledger, req)
	if _, err := db.Exec(`UPDATE varroa.run_intents SET state = 'CONSUMED', consumed_at = now() WHERE intent_key = $1`,
		req.IntentKey); err != nil {
		t.Fatalf("consume the intent: %v", err)
	}

	mustMark(t, ledger, req)
	if at := readStartMarkedAt(t, db, req.IntentKey); at != nil {
		t.Fatalf("want no mark on a consumed intent, got %v", at)
	}
}

// TestMarkStartLowerGenerationFenced: a mark with a generation below the
// stored one answers FENCED and writes nothing.
func TestMarkStartLowerGenerationFenced(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:mark-fenced", 2)
	mustGrant(t, ledger, req)

	stale := req
	stale.PlacementGeneration = 1
	resp := ledger.Claim(context.Background(), startMarkOf(stale))
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonFenced {
		t.Fatalf("want vetoed FENCED, got %+v", resp)
	}
	if at := readStartMarkedAt(t, db, req.IntentKey); at != nil {
		t.Fatalf("want no mark, got %v", at)
	}
}

// TestMarkStartMissingIntentVetoed: a mark for a key no grant holds
// answers INVALID_KEY and inserts no row.
func TestMarkStartMissingIntentVetoed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:mark-missing", 1)
	resp := ledger.Claim(context.Background(), startMarkOf(req))
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want vetoed INVALID_KEY, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)
}

// TestMarkStartFenceVetoWritesNothing: a fence refusal reaches the caller
// unchanged and stores no mark.
func TestMarkStartFenceVetoWritesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	req := newTestClaimRequest("manual:mark-fence", 1)
	mustGrant(t, NewLedger(pool, nil), req)

	resp := NewLedger(pool, denyingFence{reasonCode: ReasonFenced}).Claim(context.Background(), startMarkOf(req))
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonFenced {
		t.Fatalf("want vetoed FENCED, got %+v", resp)
	}
	if at := readStartMarkedAt(t, db, req.IntentKey); at != nil {
		t.Fatalf("want no mark, got %v", at)
	}
}

// TestMarkStartFailsClosed: a fence that cannot answer, and a database
// that does not answer, give an empty decision with a code.
func TestMarkStartFailsClosed(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	req := newTestClaimRequest("manual:mark-closed", 1)
	mustGrant(t, NewLedger(pool, nil), req)

	resp := NewLedger(pool, erroringFence{}).Claim(context.Background(), startMarkOf(req))
	if resp.Decision != "" || resp.Code != CodeUnavailable {
		t.Fatalf("want an empty decision with code %s, got %+v", CodeUnavailable, resp)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp = NewLedger(pool, nil).Claim(ctx, startMarkOf(req))
	if resp.Decision != "" || resp.Code != CodeUnavailable {
		t.Fatalf("want an empty decision with code %s on a cancelled context, got %+v", CodeUnavailable, resp)
	}
}

// TestMarkedRepeatFromOtherInstanceAnswersDuplicate: a repeat claim from
// another instance for a marked intent answers duplicate at once. It
// reads no consumer, keeps the owner, and leaves the run's own row alone.
func TestMarkedRepeatFromOtherInstanceAnswersDuplicate(t *testing.T) {
	t.Parallel()

	ledger, db := newHeldLedgerWithDB(t, noConsumerRead(t))

	req := newTestClaimRequest("manual:marked-repeat", 1)
	grant := mustGrant(t, ledger, req)
	mustMark(t, ledger, req)
	if _, err := db.Exec(`UPDATE varroa.run_intents SET granted_at = now() - interval '10 minutes' WHERE intent_key = $1`,
		req.IntentKey); err != nil {
		t.Fatalf("age the grant: %v", err)
	}
	seedAcceptedRunRow(t, db, req)

	for _, instance := range []string{"pod-b", ""} {
		resp := ledger.Claim(context.Background(), repeatFrom(req, instance))
		if resp.Decision != DecisionDuplicate || resp.RunID != grant.RunID {
			t.Fatalf("repeat from %q: want duplicate for run %s, got %+v", instance, grant.RunID, resp)
		}
	}
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-a" {
		t.Fatalf("want the owner kept at pod-a, got %s", owner)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, req.RunID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "QUEUED" {
		t.Fatalf("want the run's own row kept QUEUED, got %s", status)
	}
}

// TestMarkedRepeatFromSameInstanceReanswers: the owner's own repeat for a
// marked intent answers granted with the stored number.
func TestMarkedRepeatFromSameInstanceReanswers(t *testing.T) {
	t.Parallel()

	ledger, _ := newHeldLedgerWithDB(t, noConsumerRead(t))

	req := newTestClaimRequest("manual:marked-same", 1)
	grant := mustGrant(t, ledger, req)
	mustMark(t, ledger, req)

	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted || resp.BuildNumber != grant.BuildNumber {
		t.Fatalf("want granted with number %d, got %+v", grant.BuildNumber, resp)
	}
}

// TestMarkedIntentNeverTransfers: a higher generation with a new run does
// not take over a marked intent. It answers duplicate and the row keeps
// the marked run.
func TestMarkedIntentNeverTransfers(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:marked-transfer", 1)
	grant := mustGrant(t, ledger, req)
	mustMark(t, ledger, req)

	transfer := repeatFrom(newTestClaimRequest(req.IntentKey, 2), "pod-b")
	resp := ledger.Claim(context.Background(), transfer)
	if resp.Decision != DecisionDuplicate || resp.RunID != grant.RunID {
		t.Fatalf("want duplicate naming run %s, got %+v", grant.RunID, resp)
	}
	assertStoredRunID(t, db, req.IntentKey, grant.RunID)
	if owner := readOwner(t, db, req.IntentKey); owner != "pod-a" {
		t.Fatalf("want the owner kept at pod-a, got %s", owner)
	}
}

// TestUnmarkedIntentStillTransfers: an unmarked intent moves to a new run
// at a higher generation, and the old owner's later mark answers
// duplicate.
func TestUnmarkedIntentStillTransfers(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:unmarked-transfer", 1)
	mustGrant(t, ledger, req)

	transfer := repeatFrom(newTestClaimRequest(req.IntentKey, 2), "pod-b")
	mustGrant(t, ledger, transfer)
	assertStoredRunID(t, db, req.IntentKey, transfer.RunID)

	old := req
	old.PlacementGeneration = 2
	resp := ledger.Claim(context.Background(), startMarkOf(old))
	if resp.Decision != DecisionDuplicate || resp.RunID != transfer.RunID {
		t.Fatalf("want the old owner's mark refused as duplicate, got %+v", resp)
	}
}

// TestClaimResponderStartMark: the responder routes a start mark on
// bus.LedgerClaimSubject to MarkStart and counts it by decision.
func TestClaimResponderStartMark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	mp, reader := newTestMeterProvider(t)
	metrics, err := NewClaimMetrics(mp)
	if err != nil {
		t.Fatalf("new claim metrics: %v", err)
	}

	s := startResponderTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	ledger := NewLedger(pool, nil)
	sub, err := RegisterClaimResponder(conn, ledger, metrics, nil)
	if err != nil {
		t.Fatalf("register claim responder: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	request := func(r ClaimRequest) ClaimResponse {
		t.Helper()
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		msg, err := conn.Request(bus.LedgerClaimSubject, body, 5*time.Second)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		var resp ClaimResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		return resp
	}

	if resp := request(req); resp.Decision != DecisionGranted {
		t.Fatalf("claim: want granted, got %+v", resp)
	}
	if resp := request(startMarkOf(req)); resp.Decision != DecisionGranted {
		t.Fatalf("start mark: want granted, got %+v", resp)
	}
	if readStartMarkedAt(t, db, req.IntentKey) == nil {
		t.Fatal("want start_marked_at set through the responder")
	}
	if got := sumCounterByAttr(t, reader, "recorder_start_marks_total", "decision", DecisionGranted); got != 1 {
		t.Fatalf("recorder_start_marks_total{decision=granted} = %d, want 1", got)
	}
}

// seedAcceptedRunRow inserts the QUEUED accepted-trigger row of req's run.
func seedAcceptedRunRow(t *testing.T, db *sql.DB, req ClaimRequest) {
	t.Helper()
	runID, createdAt, err := parseRunUUIDv7(req.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, payload)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'QUEUED',
		        jsonb_build_object('accepted_at', now(), 'accepted_instance', 'pod-a'))
	`, runID, createdAt, req.IntentKey, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed accepted run row: %v", err)
	}
}
