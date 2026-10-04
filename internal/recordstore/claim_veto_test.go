package recordstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newVetoMarkingLedger returns a Ledger over pool with fence and the veto
// mark wired to notifier.
func newVetoMarkingLedger(pool *pgxpool.Pool, fence Fence, notifier *RunNotifier) *Ledger {
	l := NewLedger(pool, fence)
	l.SetVetoMarking(notifier)
	return l
}

// claimThroughResponder runs req through handleClaim, as the NATS
// responder does, and decodes the answer. The returned buffer holds the
// log lines.
func claimThroughResponder(t *testing.T, ledger *Ledger, metrics *ClaimMetrics, req ClaimRequest) (ClaimResponse, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal claim request: %v", err)
	}
	var resp ClaimResponse
	if err := json.Unmarshal(handleClaim(logger, ledger, metrics, body), &resp); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	return resp, &logs
}

// newVarroaClaim returns a claim request with origin VARROA_INTENT for
// runID.
func newVarroaClaim(runID string) ClaimRequest {
	req := newTestClaimRequest("manual:"+runID, 0)
	req.RunID = runID
	req.Origin = ClaimOriginVarroaIntent
	return req
}

// acceptRun stores the QUEUED row a run.trigger_accepted envelope writes
// for runID.
func acceptRun(t *testing.T, pool *pgxpool.Pool, runID string) ApplyResult {
	t.Helper()
	res, err := NewIngester(pool, nil, nil).Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID, "manual:"+runID))
	if err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}
	return res
}

// runRowCount counts the runs rows for runID.
func runRowCount(t *testing.T, db *sql.DB, runID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count runs rows: %v", err)
	}
	return n
}

// vetoActivityCount counts the build.claim_vetoed activity rows.
func vetoActivityCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'build.claim_vetoed'`).Scan(&n); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	return n
}

// assertVetoMarked asserts runID is COMPLETED with TRIGGER_FAILED and
// carries reason as trigger_reason_code.
func assertVetoMarked(t *testing.T, db *sql.DB, runID, reason string) map[string]json.RawMessage {
	t.Helper()
	status, result := readStatusResult(t, db, uuid.MustParse(runID))
	if status != StatusCompleted || result.String != ResultTriggerFailed {
		t.Fatalf("status = %q, result = %v, want COMPLETED and TRIGGER_FAILED", status, result)
	}
	payload := readRunPayload(t, db, runID)
	var code, msg string
	_ = json.Unmarshal(payload["trigger_reason_code"], &code)
	_ = json.Unmarshal(payload["trigger_error"], &msg)
	if code != reason {
		t.Fatalf("trigger_reason_code = %q, want %q", code, reason)
	}
	if msg == "" {
		t.Fatal("trigger_error is empty, want the veto message")
	}
	if _, ok := payload["trigger_failed_at"]; !ok {
		t.Fatal("payload holds no trigger_failed_at")
	}
	return payload
}

// TestMarkableVeto asserts the markable set is the set the plugin cancels
// on.
func TestMarkableVeto(t *testing.T) {
	for code, want := range map[string]bool{
		ReasonFenced:      true,
		ReasonCrossTenant: true,
		ReasonInvalidKey:  true,
		ReasonThrottled:   true,
		ReasonNotReady:    false,
		ReasonNotHome:     false,
		"":                false,
	} {
		if got := markableVeto(code); got != want {
			t.Errorf("markableVeto(%q) = %v, want %v", code, got, want)
		}
	}
}

// TestVetoMarksAnAcceptedRun asserts a CROSS_TENANT veto of an accepted
// Varroa run marks the row at once, keeps the accepted keys, writes one
// activity row, publishes one notification, and counts one mark.
func TestVetoMarksAnAcceptedRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useTestMeterProvider(t)
	metrics, err := NewClaimMetrics()
	if err != nil {
		t.Fatalf("new claim metrics: %v", err)
	}
	notifier, sent, _ := newCaptureNotifier(t)

	runID := newTestUUIDv7String()
	acceptRun(t, pool, runID)

	ledger := newVetoMarkingLedger(pool, denyingFence{reasonCode: ReasonCrossTenant}, notifier)
	resp, logs := claimThroughResponder(t, ledger, metrics, newVarroaClaim(runID))
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("want vetoed/CROSS_TENANT, got %+v", resp)
	}

	payload := assertVetoMarked(t, db, runID, ReasonCrossTenant)
	var msg string
	_ = json.Unmarshal(payload["trigger_error"], &msg)
	if msg != "denied by test fence" {
		t.Fatalf("trigger_error = %q, want the veto message", msg)
	}
	if _, ok := payload["accepted_at"]; !ok {
		t.Fatal("the mark dropped accepted_at")
	}
	if n := vetoActivityCount(t, db); n != 1 {
		t.Fatalf("build.claim_vetoed rows = %d, want 1", n)
	}
	if sent.count() != 1 {
		t.Fatalf("notifications = %d, want 1", sent.count())
	}
	if got := sent.payloads[0]; got.RunID != runID || got.Status != StatusCompleted || got.Result != ResultTriggerFailed {
		t.Fatalf("notification = %+v, want the run COMPLETED with TRIGGER_FAILED", got)
	}
	if c := fanoutMetricSum(t, reader, "recorder_claim_veto_marked_total",
		map[string]string{"reason": ReasonCrossTenant}); c != 1 {
		t.Fatalf("recorder_claim_veto_marked_total = %d, want 1", c)
	}
	if !strings.Contains(logs.String(), "vetoed run marked") {
		t.Fatalf("log holds no mark line:\n%s", logs.String())
	}
}

// TestVetoBeforeAcceptanceKeepsTheFailedRow asserts a veto that reaches
// the recorder before run.trigger_accepted inserts the failed row, and the
// later accepted insert keeps it.
func TestVetoBeforeAcceptanceKeepsTheFailedRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := newVetoMarkingLedger(pool, denyingFence{reasonCode: ReasonFenced}, nil)

	runID := newTestUUIDv7String()
	claimThroughResponder(t, ledger, nil, newVarroaClaim(runID))
	assertVetoMarked(t, db, runID, ReasonFenced)

	if res := acceptRun(t, pool, runID); res.TriggerAcceptedStored {
		t.Fatal("run.trigger_accepted stored a row over the vetoed run")
	}
	assertVetoMarked(t, db, runID, ReasonFenced)
	if n := runRowCount(t, db, runID); n != 1 {
		t.Fatalf("runs rows = %d, want 1", n)
	}
}

// TestNotReadyAndNotHomeMarkNothing asserts the two vetoes the plugin
// does not cancel on leave a QUEUED row as it is, and insert no row for
// a run with none.
func TestNotReadyAndNotHomeMarkNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	for _, reason := range []string{ReasonNotReady, ReasonNotHome} {
		ledger := newVetoMarkingLedger(pool, denyingFence{reasonCode: reason}, nil)

		accepted := newTestUUIDv7String()
		acceptRun(t, pool, accepted)
		claimThroughResponder(t, ledger, nil, newVarroaClaim(accepted))
		if status, _ := readStatusResult(t, db, uuid.MustParse(accepted)); status != StatusQueued {
			t.Fatalf("%s: status = %q, want QUEUED", reason, status)
		}

		unseen := newTestUUIDv7String()
		claimThroughResponder(t, ledger, nil, newVarroaClaim(unseen))
		if n := runRowCount(t, db, unseen); n != 0 {
			t.Fatalf("%s: runs rows = %d, want 0", reason, n)
		}
	}
	if n := vetoActivityCount(t, db); n != 0 {
		t.Fatalf("build.claim_vetoed rows = %d, want 0", n)
	}
}

// TestNativeOriginMarksNothing asserts a claim whose origin is not
// VARROA_INTENT marks no run and inserts no row.
func TestNativeOriginMarksNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := newVetoMarkingLedger(pool, denyingFence{reasonCode: ReasonFenced}, nil)

	for _, origin := range []string{"CRON", ""} {
		unseen := newTestUUIDv7String()
		req := newVarroaClaim(unseen)
		req.Origin = origin
		claimThroughResponder(t, ledger, nil, req)
		if n := runRowCount(t, db, unseen); n != 0 {
			t.Fatalf("origin %q: runs rows = %d, want 0", origin, n)
		}

		accepted := newTestUUIDv7String()
		acceptRun(t, pool, accepted)
		req = newVarroaClaim(accepted)
		req.Origin = origin
		claimThroughResponder(t, ledger, nil, req)
		if status, _ := readStatusResult(t, db, uuid.MustParse(accepted)); status != StatusQueued {
			t.Fatalf("origin %q: status = %q, want QUEUED", origin, status)
		}
	}
}

// TestVetoLeavesARunWithAnIntent asserts a veto never marks a run the
// ledger already granted.
func TestVetoLeavesARunWithAnIntent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	runID := newTestUUIDv7String()
	acceptRun(t, pool, runID)
	mustGrant(t, NewLedger(pool, nil), newVarroaClaim(runID))

	ledger := newVetoMarkingLedger(pool, denyingFence{reasonCode: ReasonFenced}, nil)
	claimThroughResponder(t, ledger, nil, newVarroaClaim(runID))
	if status, _ := readStatusResult(t, db, uuid.MustParse(runID)); status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED", status)
	}
}

// TestFencedRepeatMarksTheRequesterOnly asserts a FENCED answer from the
// intent table marks the requesting run, not the stored run it names.
func TestFencedRepeatMarksTheRequesterOnly(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := newVetoMarkingLedger(pool, nil, nil)

	stored := newTestUUIDv7String()
	acceptRun(t, pool, stored)
	key := "manual:shared-" + stored
	first := newVarroaClaim(stored)
	first.IntentKey = key
	first.PlacementGeneration = 2
	mustGrant(t, ledger, first)

	requester := newTestUUIDv7String()
	second := newVarroaClaim(requester)
	second.IntentKey = key
	second.PlacementGeneration = 1
	resp, _ := claimThroughResponder(t, ledger, nil, second)
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonFenced || resp.RunID != stored {
		t.Fatalf("want vetoed/FENCED naming the stored run, got %+v", resp)
	}

	assertVetoMarked(t, db, requester, ReasonFenced)
	if status, _ := readStatusResult(t, db, uuid.MustParse(stored)); status != StatusQueued {
		t.Fatalf("stored run status = %q, want QUEUED", status)
	}
}

// TestInvalidKeyMarksAParseableRun asserts an INVALID_KEY veto with a
// valid run identifier marks that run.
func TestInvalidKeyMarksAParseableRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := newVetoMarkingLedger(pool, nil, nil)

	runID := newTestUUIDv7String()
	req := newVarroaClaim(runID)
	req.IntentKey = "bogus"
	resp, _ := claimThroughResponder(t, ledger, nil, req)
	if resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want INVALID_KEY, got %+v", resp)
	}
	assertVetoMarked(t, db, runID, ReasonInvalidKey)
}

// TestInvalidRunIDMarksNothing asserts a run identifier that is not a
// UUIDv7 writes nothing.
func TestInvalidRunIDMarksNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := newVetoMarkingLedger(pool, nil, nil)

	req := newVarroaClaim("not-a-uuid")
	resp, logs := claimThroughResponder(t, ledger, nil, req)
	if resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want INVALID_KEY, got %+v", resp)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs`).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("runs rows = %d, want 0", n)
	}
	if strings.Contains(logs.String(), "vetoed run mark") {
		t.Fatalf("log holds a mark line:\n%s", logs.String())
	}
}

// TestVetoLeavesAStartedRun asserts a veto never marks a run that holds a
// build number.
func TestVetoLeavesAStartedRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := newVetoMarkingLedger(pool, denyingFence{reasonCode: ReasonFenced}, nil)

	parsed, createdAt, err := parseRunUUIDv7(newTestUUIDv7String())
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, build_number, payload)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'RUNNING', 7, '{}'::jsonb)
	`, parsed, createdAt, "manual:"+parsed.String(),
		testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("seed running run: %v", err)
	}

	claimThroughResponder(t, ledger, nil, newVarroaClaim(parsed.String()))
	status, result := readStatusResult(t, db, parsed)
	if status != StatusRunning || result.Valid {
		t.Fatalf("status = %q, result = %v, want RUNNING and null", status, result)
	}
}

// TestRunStartedSupersedesAVetoedRun asserts a late real start replaces
// a vetoed run's row.
func TestRunStartedSupersedesAVetoedRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := newTestUUIDv7String()
	acceptRun(t, pool, runID)
	claimThroughResponder(t, newVetoMarkingLedger(pool, denyingFence{reasonCode: ReasonFenced}, nil), nil,
		newVarroaClaim(runID))
	assertVetoMarked(t, db, runID, ReasonFenced)

	mustGrant(t, NewLedger(pool, nil), newVarroaClaim(runID))
	parsed := uuid.MustParse(runID)
	buildNumber := int64(1)
	started := newTestEnvelope(t, parsed, "manual:"+runID, KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	status, result := readStatusResult(t, db, parsed)
	if status != StatusRunning || result.Valid {
		t.Fatalf("status = %q, result = %v, want RUNNING and null", status, result)
	}
	payload := readRunPayload(t, db, runID)
	for _, key := range []string{"trigger_reason_code", "trigger_error", "accepted_at"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("payload still holds %q after run.started", key)
		}
	}
}

// TestClaimExpirySkipsAVetoedRun asserts the claim-expiry sweep leaves a
// vetoed run alone, whatever its accepted_at.
func TestClaimExpirySkipsAVetoedRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	runID := newTestUUIDv7String()
	acceptRun(t, pool, runID)
	claimThroughResponder(t, newVetoMarkingLedger(pool, denyingFence{reasonCode: ReasonCrossTenant}, nil), nil,
		newVarroaClaim(runID))
	if _, err := db.Exec(`
		UPDATE varroa.runs SET payload = payload || jsonb_build_object('accepted_at', $2::timestamptz)
		WHERE run_id = $1
	`, runID, claimExpiryNow.Add(-ClaimExpiryAge-time.Minute)); err != nil {
		t.Fatalf("age the vetoed row: %v", err)
	}

	n, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("expire unclaimed runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired = %d, want 0", n)
	}
	assertVetoMarked(t, db, runID, ReasonCrossTenant)
}

// TestUnwiredLedgerMarksNothing asserts a Ledger with no veto mark wired
// and no pool answers a veto normally.
func TestUnwiredLedgerMarksNothing(t *testing.T) {
	req := newVarroaClaim(newTestUUIDv7String())
	req.IntentKey = "bogus"
	resp, logs := claimThroughResponder(t, NewLedger(nil, nil), nil, req)
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want vetoed/INVALID_KEY, got %+v", resp)
	}
	if strings.Contains(logs.String(), "vetoed run mark") {
		t.Fatalf("log holds a mark line:\n%s", logs.String())
	}

	wired := NewLedger(nil, nil)
	wired.SetVetoMarking(nil)
	if marked, err := wired.markVetoedRun(context.Background(), req, resp); marked || err != nil {
		t.Fatalf("markVetoedRun with no pool = %v, %v, want false, nil", marked, err)
	}
}
