package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunTests upserts run_tests on the natural key (run_id, suite),
// copying created_at from the intent, then recomputes runs.tests_total,
// tests_failed, and tests_skipped as the sum over every suite the run
// has reported. Recomputing from run_tests, rather than accumulating in
// place, keeps a replayed or redelivered run.tests envelope from double
// counting one suite.
func applyRunTests(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	var payload RunTestsPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("%w: decode run.tests payload: %v", ErrMalformedEnvelope, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.run_tests (run_id, suite, created_at, total, failed, skipped, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (run_id, suite) DO UPDATE SET
			total       = EXCLUDED.total,
			failed      = EXCLUDED.failed,
			skipped     = EXCLUDED.skipped,
			duration_ms = EXCLUDED.duration_ms
	`, intent.runID, payload.Suite, intent.createdAt, payload.Total, payload.Failed, payload.Skipped, payload.DurationMs); err != nil {
		return fmt.Errorf("upsert run_tests: %w", err)
	}

	var total, failed, skipped int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(total), 0), COALESCE(SUM(failed), 0), COALESCE(SUM(skipped), 0)
		FROM varroa.run_tests WHERE run_id = $1
	`, intent.runID).Scan(&total, &failed, &skipped); err != nil {
		return fmt.Errorf("sum run_tests: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, tests_total, tests_failed, tests_skipped
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, 'QUEUED', $9, $10, $11
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			tests_total   = EXCLUDED.tests_total,
			tests_failed  = EXCLUDED.tests_failed,
			tests_skipped = EXCLUDED.tests_skipped
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath, total, failed, skipped,
	); err != nil {
		return fmt.Errorf("update runs test counters: %w", err)
	}
	return nil
}
