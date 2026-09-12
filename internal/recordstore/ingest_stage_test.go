package recordstore

import (
	"context"
	"testing"
)

// TestApplyRunStageReplaysToOneRow delivers a run.stage envelope twice
// and asserts one row per natural key (run_id, stage_id).
func TestApplyRunStageReplaysToOneRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:stage-replay-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:stage-replay-1", KindRunStage, RunStagePayload{
		StageID: "node-1", Name: "Build", NodeType: "stage", Status: "IN_PROGRESS",
	})
	for i := 0; i < 2; i++ {
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.stage delivery %d: %v", i, err)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2`,
		runID, "node-1").Scan(&count); err != nil {
		t.Fatalf("count run_stages: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

// TestApplyRunStageTerminalReplacesInProgress asserts an IN_PROGRESS
// then SUCCESS delivery for one stage_id leaves one row at SUCCESS, and
// a redelivered IN_PROGRESS leaves it there.
func TestApplyRunStageTerminalReplacesInProgress(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:stage-terminal-1")
	g := NewIngester(pool, nil, nil)

	inProgress := newTestEnvelope(t, runID, "manual:stage-terminal-1", KindRunStage, RunStagePayload{
		StageID: "node-1", Name: "Build", NodeType: "stage", Status: "IN_PROGRESS",
	})
	if _, err := g.Apply(context.Background(), inProgress); err != nil {
		t.Fatalf("apply IN_PROGRESS: %v", err)
	}

	success := newTestEnvelope(t, runID, "manual:stage-terminal-1", KindRunStage, RunStagePayload{
		StageID: "node-1", Name: "Build", NodeType: "stage", Status: "SUCCESS",
	})
	if _, err := g.Apply(context.Background(), success); err != nil {
		t.Fatalf("apply SUCCESS: %v", err)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2`,
		runID, "node-1").Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "SUCCESS" {
		t.Fatalf("status = %q, want SUCCESS", status)
	}

	// A redelivered IN_PROGRESS must not reopen the finished stage.
	if _, err := g.Apply(context.Background(), inProgress); err != nil {
		t.Fatalf("apply redelivered IN_PROGRESS: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2`,
		runID, "node-1").Scan(&status); err != nil {
		t.Fatalf("read status after redelivery: %v", err)
	}
	if status != "SUCCESS" {
		t.Fatalf("status = %q after redelivered IN_PROGRESS, want SUCCESS", status)
	}
}

// TestApplyRunStageUntypedFieldSurvivesInPayload asserts a run.stage
// envelope carrying a field run_stages has no column for still lands in
// run_stages.payload, and that payload holds the complete envelope, not
// only the inner payload object.
func TestApplyRunStageUntypedFieldSurvivesInPayload(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:stage-untyped-1")
	g := NewIngester(pool, nil, nil)

	raw := map[string]any{
		"stage_id":     "node-1",
		"name":         "Build",
		"node_type":    "stage",
		"status":       "SUCCESS",
		"custom_field": "keep-me",
	}
	env := newTestEnvelope(t, runID, "manual:stage-untyped-1", KindRunStage, raw)
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.stage: %v", err)
	}

	var name, custom, kind string
	if err := db.QueryRow(`
		SELECT name, payload->'payload'->>'custom_field', payload->>'kind'
		FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2
	`, runID, "node-1").Scan(&name, &custom, &kind); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if name != "Build" {
		t.Fatalf("name = %q, want Build (typed column)", name)
	}
	if custom != "keep-me" {
		t.Fatalf("payload.payload.custom_field = %q, want keep-me", custom)
	}
	if kind != KindRunStage {
		t.Fatalf("payload.kind = %q, want %q (payload holds the complete envelope)", kind, KindRunStage)
	}
}

// TestApplyRunStageSkippedAndPausedAreStored asserts NOT_EXECUTED and
// PAUSED stage statuses both insert.
func TestApplyRunStageSkippedAndPausedAreStored(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:stage-skip-1")
	g := NewIngester(pool, nil, nil)

	for _, status := range []string{"NOT_EXECUTED", "PAUSED"} {
		env := newTestEnvelope(t, runID, "manual:stage-skip-1", KindRunStage, RunStagePayload{
			StageID: "node-" + status, Name: "Stage", NodeType: "stage", Status: status,
		})
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply %s: %v", status, err)
		}
		var got string
		if err := db.QueryRow(`SELECT status FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2`,
			runID, "node-"+status).Scan(&got); err != nil {
			t.Fatalf("read %s row: %v", status, err)
		}
		if got != status {
			t.Fatalf("status = %q, want %q", got, status)
		}
	}
}
