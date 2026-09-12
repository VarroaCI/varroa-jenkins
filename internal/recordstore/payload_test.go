package recordstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestRunStagesPayloadHoldsUntypedField asserts that a field with no typed
// column survives in run_stages.payload beside the typed columns. Writing
// the complete ingest envelope into payload is section 5's job; this test
// covers only the column and its round trip.
func TestRunStagesPayloadHoldsUntypedField(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	runID := uuid.New()
	stageID := "flow-node-1"
	envelope := `{"name": "Build", "custom_field": "keep-me", "nested": {"a": 1}}`

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.run_stages (run_id, stage_id, created_at, name, node_type, status, payload)
		VALUES ($1, $2, now(), 'Build', 'stage', 'SUCCESS', $3::jsonb)
	`, runID, stageID, envelope); err != nil {
		t.Fatalf("insert run_stages: %v", err)
	}

	var custom string
	if err := db.QueryRowContext(ctx, `
		SELECT payload ->> 'custom_field' FROM varroa.run_stages WHERE run_id = $1 AND stage_id = $2
	`, runID, stageID).Scan(&custom); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if custom != "keep-me" {
		t.Fatalf("want payload.custom_field %q, got %q", "keep-me", custom)
	}
}
