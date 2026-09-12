package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// applyRunStarted upserts the runs row for a run.started envelope, moves
// the intent to CONSUMED, raises the build-number watermark, and runs
// the duplicate backstop, all inside tx. nullBuildNumber forces a null
// build_number on every write; Apply sets it true on the retry after a
// build-number conflict, and skips the watermark raise in that case,
// because a rejected number must not become the new floor. It stores
// the complete envelope in runs.payload under the key "run.started", so
// a later kind's write never replaces it.
func applyRunStarted(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope, nullBuildNumber bool) (*LedgerAction, error) {
	var payload RunStartedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode run.started payload: %v", ErrMalformedEnvelope, err)
	}

	buildNumber := payload.BuildNumber
	if nullBuildNumber {
		buildNumber = nil
	}

	envelopeJSON, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}

	var status string
	err = tx.QueryRow(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			build_number, branch, scm_revision, status, queued_at, started_at, actor, cause, params, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, 'RUNNING', $12, $13, $14, $15::jsonb, $16::jsonb,
			jsonb_build_object('run.started', $17::jsonb)
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			build_number = COALESCE(EXCLUDED.build_number, runs.build_number),
			branch       = COALESCE(EXCLUDED.branch, runs.branch),
			scm_revision = COALESCE(EXCLUDED.scm_revision, runs.scm_revision),
			queued_at    = COALESCE(EXCLUDED.queued_at, runs.queued_at),
			started_at   = COALESCE(EXCLUDED.started_at, runs.started_at),
			actor        = COALESCE(EXCLUDED.actor, runs.actor),
			cause        = COALESCE(EXCLUDED.cause, runs.cause),
			params       = COALESCE(EXCLUDED.params, runs.params),
			status       = `+runsStatusMergeSQL("TRUE")+`,
			cell         = CASE WHEN `+supersedeTriggerFailedSQL("TRUE")+` THEN EXCLUDED.cell ELSE runs.cell END,
			result       = CASE WHEN `+supersedeTriggerFailedSQL("TRUE")+` THEN NULL::text ELSE runs.result END,
			ended_at     = CASE WHEN `+supersedeTriggerFailedSQL("TRUE")+` THEN NULL::timestamptz ELSE runs.ended_at END,
			payload      = (CASE WHEN `+supersedeTriggerFailedSQL("TRUE")+`
			                     THEN COALESCE(runs.payload, '{}'::jsonb) `+triggerPayloadKeysSQL+`
			                     ELSE COALESCE(runs.payload, '{}'::jsonb) END) || EXCLUDED.payload
		RETURNING status
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
		buildNumber, payload.Branch, payload.SCMRevision,
		payload.QueuedAt, payload.StartedAt, payload.Actor,
		jsonArg(payload.Cause), jsonArg(payload.Params), string(envelopeJSON),
	).Scan(&status)
	if err != nil {
		return nil, fmt.Errorf("upsert started run: %w", err)
	}

	consumedAt := env.OccurredAt
	if buildNumber == nil {
		if err := consumeIntentNullBuildNumber(ctx, tx, intent.runID, consumedAt); err != nil {
			return nil, err
		}
	} else {
		if err := consumeIntent(ctx, tx, intent.runID, *buildNumber, consumedAt); err != nil {
			return nil, err
		}
		if !nullBuildNumber {
			if err := RaiseWatermarkForStartedBuild(ctx, tx, intent.cluster, intent.tenant, intent.jobPath, *buildNumber); err != nil {
				return nil, err
			}
		}
	}

	if status != StatusQueued && status != StatusRunning {
		// The arriving run is no longer live: an out-of-order run.completed
		// already finished it. The duplicate backstop does not apply.
		return nil, nil
	}

	revision := ""
	if payload.SCMRevision != nil {
		revision = *payload.SCMRevision
	}
	return runDuplicateBackstop(ctx, tx, intent, revision)
}

// consumeIntentNullBuildNumber moves a GRANTED intent to CONSUMED with a
// null build_number. Apply calls it after a run.started write hits the
// run_intents build-number UNIQUE constraint, so the intent is marked
// consumed without claiming a number that collides with another run.
func consumeIntentNullBuildNumber(ctx context.Context, db DBTX, runID uuid.UUID, consumedAt time.Time) error {
	if _, err := db.Exec(ctx, `
		UPDATE varroa.run_intents
		SET state = $1, consumed_at = $2, build_number = NULL
		WHERE run_id = $3
	`, stateConsumed, consumedAt, runID); err != nil {
		return fmt.Errorf("consume intent with null build number: %w", err)
	}
	return nil
}
