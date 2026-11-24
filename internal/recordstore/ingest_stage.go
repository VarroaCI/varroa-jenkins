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
//
// A stage that an interrupted mark ended (markInterruptedTx) follows the
// same rule. A terminal fact of the owner replaces it, and an IN_PROGRESS
// fact never opens it again. A Jenkins process that resumes the build sends
// no second start for a stage that is open, so a start that arrives after
// the mark is a redelivery.
//
// An IN_PROGRESS fact that first arrives after the owner's run.completed
// opens no stage. The same transaction ends the row at once
// (endStagesAtCompletion), because no later fact of the owner ends it.
//
// dispatch calls senderOwnsRun first, so a stage envelope from a cell
// instance that does not own a cell run never reaches this write. The
// gateway also drops the stage envelopes of a COMPLETED event that a
// ledger answer names foreign.
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
	if payload.Status == "IN_PROGRESS" {
		stageID := payload.StageID
		return endStagesAtCompletion(ctx, tx, intent, &stageID)
	}
	return nil
}

// endStagesAtCompletion ends each stage of the run that has no real end
// after the owner's run.completed. It changes nothing before that
// completion: the runs row must hold the payload key run.completed, and
// only the owner arm of applyRunCompleted writes that key. A nil stageID
// covers every stage of the run. A set stageID covers that stage alone.
//
// It changes three kinds of row: an IN_PROGRESS row, a row that a mark
// ended (payload key interrupted_reason), and a row that an earlier owner
// completion ended (payload key open_at_completion). A row that holds a
// terminal fact of the owner holds neither key, so the write never
// changes it.
//
// The row takes SUCCESS when the run succeeded. Any other result, and no
// result, gives ABORTED. A failed run does not give its failure to an
// open parallel stage, so the run digest names the stage that failed. The
// row takes the run's end and a NULL
// duration, so no stage median counts it. The payload loses
// interrupted_reason and takes open_at_completion. A later terminal
// run.stage fact of the owner replaces the whole row (applyRunStage).
func endStagesAtCompletion(ctx context.Context, tx pgx.Tx, intent *intentRow, stageID *string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.run_stages rs
		SET status = CASE WHEN r.result = 'SUCCESS' THEN 'SUCCESS' ELSE 'ABORTED' END,
		    ended_at = r.ended_at,
		    duration_ms = NULL,
		    payload = (COALESCE(rs.payload, '{}'::jsonb) - 'interrupted_reason')
		              || jsonb_build_object('open_at_completion', true)
		FROM varroa.runs r
		WHERE r.created_at = $2 AND r.run_id = $1
		  AND r.payload ? 'run.completed'
		  AND rs.run_id = $1
		  AND ($3::text IS NULL OR rs.stage_id = $3::text)
		  AND (rs.status = 'IN_PROGRESS'
		       OR rs.payload ? 'interrupted_reason'
		       OR rs.payload ? 'open_at_completion')
	`, intent.runID, intent.createdAt, stageID); err != nil {
		return fmt.Errorf("end the open stages of the completed run: %w", err)
	}
	return nil
}
