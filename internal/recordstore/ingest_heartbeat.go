package recordstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunHeartbeat raises runs.last_heartbeat_at to the envelope's
// occurred_at when that value is later than the stored one. It changes
// no other column: no status, no result, and no payload merge, so a
// heartbeat for a run with no runs row yet still creates only the base
// row a later event needs.
func applyRunHeartbeat(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, last_heartbeat_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, 'QUEUED', $9
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			last_heartbeat_at = CASE
				WHEN runs.last_heartbeat_at IS NULL OR EXCLUDED.last_heartbeat_at > runs.last_heartbeat_at
				THEN EXCLUDED.last_heartbeat_at
				ELSE runs.last_heartbeat_at
			END
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath, env.OccurredAt,
	); err != nil {
		return fmt.Errorf("raise last_heartbeat_at: %w", err)
	}
	return nil
}
