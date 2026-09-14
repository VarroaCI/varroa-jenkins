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

// triggerAcceptedCell is the cell that accepts every trigger these tests
// build.
const triggerAcceptedCell = "cell-a"

// newTriggerAcceptedEnvelope returns a run.trigger_accepted envelope for
// runID.
func newTriggerAcceptedEnvelope(t *testing.T, runID, intentKey string) IngestEnvelope {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"http_status": 201,
		"actor":       "user:alice",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunTriggerAccepted,
		RunID:         runID,
		IntentKey:     intentKey,
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          triggerAcceptedCell,
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC().Truncate(time.Microsecond),
		Payload:       raw,
	}
}

// TestApplyTriggerAcceptedWritesQueuedRun asserts one envelope stores a
// QUEUED runs row with the two accepted keys, the actor, and every
// build column left null, and writes no intent.
func TestApplyTriggerAcceptedWritesQueuedRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := newTestUUIDv7String()
	env := newTriggerAcceptedEnvelope(t, runID, "manual:ta-1")

	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}

	var status, cell, tenant, jobPath, actor string
	var buildNumber sql.NullInt64
	var queuedAt, startedAt, endedAt sql.NullTime
	if err := db.QueryRow(`
		SELECT status, cell, tenant, job_path, actor, build_number, queued_at, started_at, ended_at
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &cell, &tenant, &jobPath, &actor, &buildNumber, &queuedAt, &startedAt, &endedAt); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED", status)
	}
	if cell != triggerAcceptedCell || tenant != testTenant || jobPath != testJobPath {
		t.Fatalf("identity = %q/%q/%q", cell, tenant, jobPath)
	}
	if actor != "user:alice" {
		t.Fatalf("actor = %q", actor)
	}
	if buildNumber.Valid || queuedAt.Valid || startedAt.Valid || endedAt.Valid {
		t.Fatal("an accepted trigger must leave build_number, queued_at, started_at and ended_at null")
	}

	payload := readRunPayload(t, db, runID)
	if _, ok := payload["accepted_at"]; !ok {
		t.Fatal("payload is missing accepted_at")
	}
	if _, ok := payload["accepted_instance"]; !ok {
		t.Fatal("payload is missing accepted_instance")
	}

	var intents int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&intents); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if intents != 0 {
		t.Fatalf("run_intents rows = %d, want 0", intents)
	}
}

// TestApplyTriggerAcceptedIsIdempotent asserts a redelivered envelope
// stores no second row.
func TestApplyTriggerAcceptedIsIdempotent(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := newTestUUIDv7String()
	env := newTriggerAcceptedEnvelope(t, runID, "manual:ta-2")

	first, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply the first delivery: %v", err)
	}
	if !first.TriggerAcceptedStored {
		t.Fatal("the first delivery reports no stored row")
	}

	second, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply the redelivery: %v", err)
	}
	if second.TriggerAcceptedStored {
		t.Fatal("the redelivery reports a stored row, but the conflict clause skipped it")
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

// TestApplyTriggerAcceptedYieldsToRealRun asserts an envelope that
// arrives after a real run writes nothing and leaves the payload
// untouched.
func TestApplyTriggerAcceptedYieldsToRealRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedGrantedIntent(t, pool, "manual:ta-3")

	buildNumber := int64(4)
	started := newTestEnvelope(t, runID, "manual:ta-3", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	var beforePayload []byte
	if err := db.QueryRow(`SELECT payload FROM varroa.runs WHERE run_id = $1`, runID).Scan(&beforePayload); err != nil {
		t.Fatalf("read payload before: %v", err)
	}

	res, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID.String(), "manual:ta-3"))
	if err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}
	if res.TriggerAcceptedStored {
		t.Fatal("the envelope reports a stored row, but a real run already holds that run id")
	}

	var count int
	var status string
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	var afterPayload []byte
	if err := db.QueryRow(`SELECT status, payload FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &afterPayload); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING", status)
	}
	if string(afterPayload) != string(beforePayload) {
		t.Fatalf("payload changed: before %s, after %s", beforePayload, afterPayload)
	}
}

// TestApplyTriggerAcceptedYieldsToTriggerFailed asserts an accepted
// envelope that arrives after a refused-trigger row writes nothing.
func TestApplyTriggerAcceptedYieldsToTriggerFailed(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := newTestUUIDv7String()

	if _, err := g.Apply(context.Background(), newTriggerFailedEnvelope(t, runID, "manual:ta-4")); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}
	res, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID, "manual:ta-4"))
	if err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}
	if res.TriggerAcceptedStored {
		t.Fatal("the envelope reports a stored row, but a TRIGGER_FAILED row already holds that run id")
	}

	var count int
	var result string
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if err := db.QueryRow(`SELECT result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&result); err != nil {
		t.Fatalf("read result: %v", err)
	}
	if result != ResultTriggerFailed {
		t.Fatalf("result = %q, want %q", result, ResultTriggerFailed)
	}
}

// TestRunStartedSupersedesAcceptedTrigger asserts a later run.started
// removes the accepted keys and moves the row to RUNNING.
func TestRunStartedSupersedesAcceptedTrigger(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedGrantedIntent(t, pool, "manual:ta-5")

	if _, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID.String(), "manual:ta-5")); err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}

	buildNumber := int64(9)
	started := newTestEnvelope(t, runID, "manual:ta-5", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	var status string
	var result sql.NullString
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &result); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning || result.Valid {
		t.Fatalf("status = %q, result = %v, want RUNNING and null", status, result)
	}

	payload := readRunPayload(t, db, runID.String())
	for _, key := range []string{"accepted_at", "accepted_instance"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("payload still holds %q after a supersede", key)
		}
	}
}

// TestPlaceholdersRemoveAcceptedKeys asserts each of run.log,
// run.changes, run.heartbeat, and run.tests removes the accepted keys
// from a row that still holds them.
func TestPlaceholdersRemoveAcceptedKeys(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		payload any
	}{
		{"run.log", KindRunLog, map[string]any{"chunk_count": 3}},
		{"run.changes", KindRunChanges, RunChangesPayload{Changes: json.RawMessage(`[]`)}},
		{"run.heartbeat", KindRunHeartbeat, map[string]any{}},
		{"run.tests", KindRunTests, RunTestsPayload{Suite: "unit", Total: 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, db := setupLedgerPool(t)
			g := NewIngester(pool, nil, nil)
			runID := seedGrantedIntent(t, pool, "manual:ta-placeholder-"+tc.name)

			if _, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID.String(), "manual:ta-placeholder-"+tc.name)); err != nil {
				t.Fatalf("apply run.trigger_accepted: %v", err)
			}

			env := newTestEnvelope(t, runID, "manual:ta-placeholder-"+tc.name, tc.kind, tc.payload)
			if _, err := g.Apply(context.Background(), env); err != nil {
				t.Fatalf("apply %s: %v", tc.kind, err)
			}

			payload := readRunPayload(t, db, runID.String())
			for _, key := range []string{"accepted_at", "accepted_instance"} {
				if _, ok := payload[key]; ok {
					t.Fatalf("%s: payload still holds %q", tc.kind, key)
				}
			}
		})
	}
}

// seedAcceptedOnlyMarkedRow seeds a granted intent, applies a
// run.trigger_accepted envelope for it, then marks the resulting row
// interrupted directly through SQL, the way the sweep's write does. It
// returns the run id.
func seedAcceptedOnlyMarkedRow(t *testing.T, pool *pgxpool.Pool, db *sql.DB, intentKey string) uuid.UUID {
	t.Helper()
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	if _, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID.String(), intentKey)); err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.runs
		SET status = 'COMPLETED',
		    result = 'INTERRUPTED',
		    ended_at = now(),
		    payload = payload || jsonb_build_object('interrupted_reason', 'cell-rolled')
		        || jsonb_build_object('log_summary', jsonb_build_object('finalized', true, 'incomplete', true, 'incomplete_reason', 'no-summary'))
		WHERE run_id = $1
	`, runID); err != nil {
		t.Fatalf("mark the accepted row interrupted: %v", err)
	}
	return runID
}

// TestRunStartedSupersedesMarkedAcceptedRow asserts a later run.started
// over a row the sweep marked interrupted supersedes it to RUNNING with
// a clean payload, and stamps interrupted_superseded_at.
func TestRunStartedSupersedesMarkedAcceptedRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedAcceptedOnlyMarkedRow(t, pool, db, "manual:ta-6")

	g := NewIngester(pool, nil, nil)
	buildNumber := int64(3)
	started := newTestEnvelope(t, runID, "manual:ta-6", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	var status string
	var result sql.NullString
	var supersededAt sql.NullTime
	if err := db.QueryRow(`
		SELECT status, result, interrupted_superseded_at FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &supersededAt); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusRunning || result.Valid {
		t.Fatalf("status = %q, result = %v, want RUNNING and null", status, result)
	}
	if !supersededAt.Valid {
		t.Fatal("interrupted_superseded_at must be set on a supersede of a marked row")
	}

	payload := readRunPayload(t, db, runID.String())
	for _, key := range []string{"accepted_at", "accepted_instance", "interrupted_reason", "log_summary"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("payload still holds %q after a supersede", key)
		}
	}
}

// TestForeignCompletionLeavesAcceptedKeys asserts a completion from
// another instance leaves the accepted keys and the QUEUED status in
// place.
func TestForeignCompletionLeavesAcceptedKeys(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedGrantedIntent(t, pool, "manual:ta-7")

	if _, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID.String(), "manual:ta-7")); err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}

	wantResult := "FAILURE"
	completed := newTestEnvelope(t, runID, "manual:ta-7", KindRunCompleted, RunCompletedPayload{Result: &wantResult})
	completed.CellInstance = "pod-other"
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply foreign run.completed: %v", err)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED unchanged", status)
	}

	payload := readRunPayload(t, db, runID.String())
	for _, key := range []string{"accepted_at", "accepted_instance"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("a foreign completion must strip no accepted key: missing %q", key)
		}
	}
	if _, ok := payload["foreign_completion"]; !ok {
		t.Fatal("payload is missing foreign_completion")
	}
}

// TestLateStartedLeavesInterruptedRealRun asserts a late run.started into
// an interrupted real run supersedes nothing: the status stays COMPLETED,
// the result stays INTERRUPTED, and interrupted_superseded_at stays null.
func TestLateStartedLeavesInterruptedRealRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	runID := seedGrantedIntent(t, pool, "manual:ta-8")

	buildNumber := int64(4)
	started := newTestEnvelope(t, runID, "manual:ta-8", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.runs SET status = 'COMPLETED', result = 'INTERRUPTED', ended_at = now(),
		    payload = COALESCE(payload, '{}'::jsonb) || '{"interrupted_reason": "cell-rolled"}'::jsonb
		WHERE run_id = $1
	`, runID); err != nil {
		t.Fatalf("mark run: %v", err)
	}

	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply late run.started: %v", err)
	}

	var status string
	var result sql.NullString
	var supersededAt sql.NullTime
	if err := db.QueryRow(`
		SELECT status, result, interrupted_superseded_at FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &supersededAt); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if status != StatusCompleted || !result.Valid || result.String != "INTERRUPTED" {
		t.Fatalf("status = %q, result = %v, want COMPLETED and INTERRUPTED", status, result)
	}
	if supersededAt.Valid {
		t.Fatal("interrupted_superseded_at must stay null when nothing is superseded")
	}
}
