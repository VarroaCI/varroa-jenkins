package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunStage upserts run_stages on the natural key (run_id, stage_id).
// A terminal snapshot replaces the whole row, including payload. An
// IN_PROGRESS event never replaces a terminal status: the WHERE clause on
// the conflict update leaves such a row untouched. payload stores the
// complete ingest envelope, not only its inner payload object, so no
// captured field is unrecoverable.
func applyRunStage(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	var payload RunStagePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("%w: decode run.stage payload: %v", ErrMalformedEnvelope, err)
	}

	envelopeJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.run_stages (
			run_id, stage_id, created_at, parent_id, name, node_type, status,
			started_at, ended_at, duration_ms, error, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12::jsonb
		)
		ON CONFLICT (run_id, stage_id) DO UPDATE SET
			parent_id   = EXCLUDED.parent_id,
			name        = EXCLUDED.name,
			node_type   = EXCLUDED.node_type,
			status      = EXCLUDED.status,
			started_at  = EXCLUDED.started_at,
			ended_at    = EXCLUDED.ended_at,
			duration_ms = EXCLUDED.duration_ms,
			error       = EXCLUDED.error,
			payload     = EXCLUDED.payload
		WHERE NOT (run_stages.status <> 'IN_PROGRESS' AND EXCLUDED.status = 'IN_PROGRESS')
	`,
		intent.runID, payload.StageID, intent.createdAt, payload.ParentID, payload.Name, payload.NodeType, payload.Status,
		payload.StartedAt, payload.EndedAt, payload.DurationMs, payload.Error,
		string(envelopeJSON),
	); err != nil {
		return fmt.Errorf("upsert run_stages: %w", err)
	}
	return nil
}
