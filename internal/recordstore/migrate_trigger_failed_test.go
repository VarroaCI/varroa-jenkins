package recordstore

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRunsAcceptsTriggerFailedResult asserts that migration 00008 widens
// runs_result_check, so a runs row can hold the result TRIGGER_FAILED.
func TestRunsAcceptsTriggerFailedResult(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)

	runID := uuid.Must(uuid.NewV7()).String()
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	_, err := db.Exec(`
		INSERT INTO varroa.runs (
			created_at, run_id, intent_key, cluster, namespace, cell, tenant,
			job_path, status, result, ended_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'COMPLETED', 'TRIGGER_FAILED', $1)
	`, createdAt, runID, "manual:trigger-failed-1", "core", "varroa-system", "cell-a", "team-a", "team-a/job")
	if err != nil {
		t.Fatalf("insert TRIGGER_FAILED run: %v", err)
	}

	var result string
	if err := db.QueryRow(`SELECT result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&result); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	if result != ResultTriggerFailed {
		t.Fatalf("result = %q, want %q", result, ResultTriggerFailed)
	}
}
