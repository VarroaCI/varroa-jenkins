package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunChanges upserts runs.changes from a run.changes envelope. A
// late arrival never clears a set value, following the same COALESCE
// merge every other scalar column uses. It stores the complete
// envelope in payload under the key "run.changes", so a later kind's
// write never replaces it.
func applyRunChanges(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	var payload RunChangesPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("%w: decode run.changes payload: %v", ErrMalformedEnvelope, err)
	}

	envelopeJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, changes, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			'QUEUED', $9::jsonb, jsonb_build_object('run.changes', $10::jsonb)
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			changes = COALESCE(EXCLUDED.changes, runs.changes),
			payload = COALESCE(runs.payload, '{}'::jsonb) || EXCLUDED.payload
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
		jsonArg(payload.Changes), string(envelopeJSON),
	); err != nil {
		return fmt.Errorf("upsert run.changes: %w", err)
	}
	return nil
}
