package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// triggerFailedCell is the cell that refused every trigger these tests build.
const triggerFailedCell = "cell-a"

// newTriggerFailedEnvelope returns a run.trigger_failed envelope for runID.
func newTriggerFailedEnvelope(t *testing.T, runID, intentKey string) IngestEnvelope {
	t.Helper()
	raw, err := json.Marshal(RunTriggerFailedPayload{
		Error:      "trigger build acme/app: HTTP 404: no such job",
		ReasonCode: "JOB_NOT_FOUND",
		HTTPStatus: 404,
		Actor:      "user:alice",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunTriggerFailed,
		RunID:         runID,
		IntentKey:     intentKey,
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          triggerFailedCell,
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC().Truncate(time.Microsecond),
		Payload:       raw,
	}
}

// seedGrantedIntentOnCell claims intentKey against the ledger on the named
// cell and returns the granted run identifier.
func seedGrantedIntentOnCell(t *testing.T, pool *pgxpool.Pool, intentKey, cell string) uuid.UUID {
	t.Helper()
	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest(intentKey, 0)
	req.Cell = cell
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted claim, got %+v", resp)
	}
	return uuid.MustParse(resp.RunID)
}

// readRunPayload returns the runs row's payload as a decoded map.
func readRunPayload(t *testing.T, db *sql.DB, runID string) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`SELECT payload FROM varroa.runs WHERE run_id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("read runs payload: %v", err)
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode runs payload: %v", err)
	}
	return out
}

// TestApplyTriggerFailedWritesRun asserts one envelope stores one runs row
// with the columns and payload keys the design names, and writes no intent.
func TestApplyTriggerFailedWritesRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := newTestUUIDv7String()
	env := newTriggerFailedEnvelope(t, runID, "manual:tf-1")

	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}

	var status, result, cell, tenant, jobPath, actor, intentKey string
	var buildNumber, durationMs sql.NullInt64
	var queuedAt, startedAt, endedAt sql.NullTime
	var params sql.NullString
	var createdAt time.Time
	if err := db.QueryRow(`
		SELECT status, result, cell, tenant, job_path, actor, intent_key,
		       build_number, duration_ms, queued_at, started_at, ended_at, params, created_at
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &cell, &tenant, &jobPath, &actor, &intentKey,
		&buildNumber, &durationMs, &queuedAt, &startedAt, &endedAt, &params, &createdAt); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusCompleted {
		t.Fatalf("status = %q, want COMPLETED", status)
	}
	if result != ResultTriggerFailed {
		t.Fatalf("result = %q, want %q", result, ResultTriggerFailed)
	}
	if cell != triggerFailedCell || tenant != testTenant || jobPath != testJobPath {
		t.Fatalf("identity = %q/%q/%q", cell, tenant, jobPath)
	}
	if actor != "user:alice" {
		t.Fatalf("actor = %q", actor)
	}
	if intentKey != "manual:tf-1" {
		t.Fatalf("intent_key = %q", intentKey)
	}
	if buildNumber.Valid || durationMs.Valid || queuedAt.Valid || startedAt.Valid || params.Valid {
		t.Fatal("a failed trigger must leave build_number, duration_ms, queued_at, started_at and params null")
	}
	if !endedAt.Valid || !endedAt.Time.UTC().Equal(env.OccurredAt) {
		t.Fatalf("ended_at = %v, want %v", endedAt, env.OccurredAt)
	}
	_, wantCreated, err := parseRunUUIDv7(runID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	if !createdAt.UTC().Equal(wantCreated.Truncate(time.Microsecond)) {
		t.Fatalf("created_at = %v, want the UUIDv7 time %v", createdAt.UTC(), wantCreated)
	}

	payload := readRunPayload(t, db, runID)
	for _, key := range []string{"trigger_error", "trigger_reason_code", "trigger_http_status", "trigger_failed_at"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("payload is missing the key %q", key)
		}
	}

	var intents int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&intents); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if intents != 0 {
		t.Fatalf("run_intents rows = %d, want 0", intents)
	}
}

// TestApplyTriggerFailedIsIdempotent asserts a redelivered envelope stores no
// second row, and reports that its insert wrote nothing. The recorder counts
// the metric either way, and the log line names the skipped insert.
func TestApplyTriggerFailedIsIdempotent(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := newTestUUIDv7String()
	env := newTriggerFailedEnvelope(t, runID, "manual:tf-2")

	first, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply the first delivery: %v", err)
	}
	if !first.TriggerFailedStored {
		t.Fatal("the first delivery reports no stored row")
	}
	if first.TriggerFailedReason != "JOB_NOT_FOUND" {
		t.Fatalf("reason code = %q, want JOB_NOT_FOUND", first.TriggerFailedReason)
	}

	second, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply the redelivery: %v", err)
	}
	if second.TriggerFailedStored {
		t.Fatal("the redelivery reports a stored row, but the conflict clause skipped it")
	}
	if second.TriggerFailedReason != "JOB_NOT_FOUND" {
		t.Fatalf("redelivery reason code = %q, want the reason code on every outcome", second.TriggerFailedReason)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

// TestApplyTriggerFailedMalformedRunID asserts an identifier that is not a
// UUIDv7 is a malformed envelope, so the consumer drops it.
func TestApplyTriggerFailedMalformedRunID(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	env := newTriggerFailedEnvelope(t, "not-a-uuid", "manual:tf-bad")
	if _, err := g.Apply(context.Background(), env); err == nil {
		t.Fatal("want an error for a malformed run identifier")
	}
}

// TestApplyTriggerFailedYieldsToRealRun asserts an envelope that arrives after
// a real run writes nothing.
func TestApplyTriggerFailedYieldsToRealRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedGrantedIntent(t, pool, "manual:tf-3")

	buildNumber := int64(4)
	started := newTestEnvelope(t, runID, "manual:tf-3", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	res, err := g.Apply(context.Background(), newTriggerFailedEnvelope(t, runID.String(), "manual:tf-3"))
	if err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}
	if res.TriggerFailedStored {
		t.Fatal("the envelope reports a stored row, but a real run already holds that run id")
	}

	var count int
	var status string
	var result sql.NullString
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &result); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning || result.Valid {
		t.Fatalf("status = %q, result = %v, want RUNNING and null", status, result)
	}
}

// TestRunStartedSupersedesTriggerFailed asserts a real run supersedes the
// synthetic row in place, and keeps an earlier log summary.
func TestRunStartedSupersedesTriggerFailed(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedGrantedIntentOnCell(t, pool, "manual:tf-4", "cell-b")

	if _, err := g.Apply(context.Background(), newTriggerFailedEnvelope(t, runID.String(), "manual:tf-4")); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}

	logEnv := newTestEnvelope(t, runID, "manual:tf-4", KindRunLog, map[string]any{"chunk_count": 3})
	if _, err := g.Apply(context.Background(), logEnv); err != nil {
		t.Fatalf("apply run.log: %v", err)
	}

	buildNumber := int64(7)
	started := newTestEnvelope(t, runID, "manual:tf-4", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	var status, cell string
	var result sql.NullString
	var endedAt sql.NullTime
	if err := db.QueryRow(`SELECT status, result, ended_at, cell FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&status, &result, &endedAt, &cell); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING", status)
	}
	if result.Valid {
		t.Fatalf("result = %q, want null", result.String)
	}
	if endedAt.Valid {
		t.Fatal("ended_at must become null on a supersede")
	}
	if cell != "cell-b" {
		t.Fatalf("cell = %q, want cell-b", cell)
	}

	payload := readRunPayload(t, db, runID.String())
	for _, key := range []string{"trigger_error", "trigger_reason_code", "trigger_http_status", "trigger_failed_at"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("payload still holds %q after a supersede", key)
		}
	}
	if _, ok := payload["log_summary"]; !ok {
		t.Fatal("a supersede must keep an earlier log summary")
	}
}

// TestOwningCompletionSupersedesTriggerFailed asserts an owning completion
// supersedes the synthetic row and keeps the failed trigger's actor, and that a
// later run.started then sets its own.
func TestOwningCompletionSupersedesTriggerFailed(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedGrantedIntent(t, pool, "manual:tf-5")

	if _, err := g.Apply(context.Background(), newTriggerFailedEnvelope(t, runID.String(), "manual:tf-5")); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}

	wantResult := "SUCCESS"
	endedAt := time.Now().UTC().Truncate(time.Microsecond)
	duration := int64(4200)
	completed := newTestEnvelope(t, runID, "manual:tf-5", KindRunCompleted, RunCompletedPayload{
		Result:     &wantResult,
		EndedAt:    &endedAt,
		DurationMs: &duration,
	})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var status, result, actor string
	var ended time.Time
	var durationGot int64
	if err := db.QueryRow(`SELECT status, result, ended_at, duration_ms, actor FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&status, &result, &ended, &durationGot, &actor); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusCompleted || result != wantResult {
		t.Fatalf("status = %q, result = %q", status, result)
	}
	if !ended.UTC().Equal(endedAt) || durationGot != duration {
		t.Fatalf("ended_at = %v, duration_ms = %d", ended.UTC(), durationGot)
	}
	if actor != "user:alice" {
		t.Fatalf("actor = %q, want the failed trigger's actor", actor)
	}

	startedActor := "user:bob"
	started := newTestEnvelope(t, runID, "manual:tf-5", KindRunStarted, RunStartedPayload{Actor: &startedActor})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	if err := db.QueryRow(`SELECT actor FROM varroa.runs WHERE run_id = $1`, runID).Scan(&actor); err != nil {
		t.Fatalf("read actor: %v", err)
	}
	if actor != startedActor {
		t.Fatalf("actor = %q, want %q", actor, startedActor)
	}
}

// TestForeignCompletionSupersedesNothing asserts a completion from another
// instance leaves the TRIGGER_FAILED state as it is.
func TestForeignCompletionSupersedesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedGrantedIntent(t, pool, "manual:tf-6")

	if _, err := g.Apply(context.Background(), newTriggerFailedEnvelope(t, runID.String(), "manual:tf-6")); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}

	wantResult := "FAILURE"
	completed := newTestEnvelope(t, runID, "manual:tf-6", KindRunCompleted, RunCompletedPayload{Result: &wantResult})
	completed.CellInstance = "pod-other"
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply foreign run.completed: %v", err)
	}

	var status, result string
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &result); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusCompleted || result != ResultTriggerFailed {
		t.Fatalf("status = %q, result = %q, want COMPLETED and TRIGGER_FAILED", status, result)
	}

	payload := readRunPayload(t, db, runID.String())
	if _, ok := payload["foreign_completion"]; !ok {
		t.Fatal("payload is missing foreign_completion")
	}
	if _, ok := payload["trigger_reason_code"]; !ok {
		t.Fatal("a foreign completion must strip no trigger key")
	}
}

// TestTriggerFailedWriteOrders asserts both write orders leave one row in a
// real run state. The harness runs the two writes one after the other, so it
// cannot reach the interleaving in which the run.started write evaluates its
// conflict clause while the run.trigger_failed insert is still open. The
// conflict clause is what makes that interleaving safe: both writes compute one
// primary key, so one blocks on the other and then re-evaluates.
func TestTriggerFailedWriteOrders(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	// Order one: the failure first, then the real run.
	first := seedGrantedIntent(t, pool, "manual:tf-order-1")
	if _, err := g.Apply(context.Background(), newTriggerFailedEnvelope(t, first.String(), "manual:tf-order-1")); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}
	startedFirst := newTestEnvelope(t, first, "manual:tf-order-1", KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(context.Background(), startedFirst); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	assertOneRealRun(t, db, first.String())

	// Order two: the real run first, then the failure.
	second := seedGrantedIntent(t, pool, "manual:tf-order-2")
	startedSecond := newTestEnvelope(t, second, "manual:tf-order-2", KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(context.Background(), startedSecond); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	if _, err := g.Apply(context.Background(), newTriggerFailedEnvelope(t, second.String(), "manual:tf-order-2")); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}
	assertOneRealRun(t, db, second.String())
}

// assertOneRealRun asserts the store holds one row for runID, in a real run
// state with no trigger key.
func assertOneRealRun(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	var status string
	var result sql.NullString
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &result); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning || result.Valid {
		t.Fatalf("status = %q, result = %v, want RUNNING and null", status, result)
	}
	payload := readRunPayload(t, db, runID)
	if _, ok := payload["trigger_reason_code"]; ok {
		t.Fatal("payload still holds a trigger key")
	}
}
