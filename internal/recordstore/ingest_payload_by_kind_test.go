package recordstore

import (
	"context"
	"testing"
)

// TestRunsPayloadKeyedByKind asserts runs.payload stores the complete
// envelope of the latest event of each kind under that kind's own key,
// so a later kind's write never replaces an earlier kind's fields.
func TestRunsPayloadKeyedByKind(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:payload-by-kind-1")
	g := NewIngester(pool, nil, nil)

	buildNumber := int64(1)
	branch := "main"
	started := newTestEnvelope(t, runID, "manual:payload-by-kind-1", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber, Branch: &branch,
	})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	result := "SUCCESS"
	completed := newTestEnvelope(t, runID, "manual:payload-by-kind-1", KindRunCompleted, RunCompletedPayload{
		Result: &result,
	})
	// completed's CellInstance equals the intent's owning cell instance
	// ("pod-a", newTestEnvelope's fixed value), so it takes the owner
	// branch and writes under the "run.completed" key.
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	changes := newTestEnvelope(t, runID, "manual:payload-by-kind-1", KindRunChanges, RunChangesPayload{
		Changes: []byte(`[{"commit":"abc"}]`),
	})
	if _, err := g.Apply(context.Background(), changes); err != nil {
		t.Fatalf("apply run.changes: %v", err)
	}

	logEnv := newTestEnvelope(t, runID, "manual:payload-by-kind-1", KindRunLog, map[string]any{"chunk_count": 1})
	if _, err := g.Apply(context.Background(), logEnv); err != nil {
		t.Fatalf("apply run.log: %v", err)
	}

	var startedKind, completedKind, changesKind, logChunkCount string
	if err := db.QueryRow(`
		SELECT payload->'run.started'->>'kind',
		       payload->'run.completed'->>'kind',
		       payload->'run.changes'->>'kind',
		       payload->'log_summary'->>'chunk_count'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&startedKind, &completedKind, &changesKind, &logChunkCount); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if startedKind != KindRunStarted {
		t.Fatalf("payload.'run.started'.kind = %q, want %q", startedKind, KindRunStarted)
	}
	if completedKind != KindRunCompleted {
		t.Fatalf("payload.'run.completed'.kind = %q, want %q", completedKind, KindRunCompleted)
	}
	if changesKind != KindRunChanges {
		t.Fatalf("payload.'run.changes'.kind = %q, want %q", changesKind, KindRunChanges)
	}
	if logChunkCount != "1" {
		t.Fatalf("payload.log_summary.chunk_count = %q, want 1", logChunkCount)
	}
}
