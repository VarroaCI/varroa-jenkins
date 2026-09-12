package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// applyTriggerFailed writes the one runs row a run.trigger_failed envelope
// produces, inside its own transaction. It touches run_intents never: the
// ledger grants every intent, and this trigger reached no claim gate.
//
// created_at is the UUIDv7 time of the run identifier, not occurred_at. A
// real run's created_at holds that same moment, so one run identifier maps
// to one primary key whichever path writes it. The conflict clause alone
// then decides, and the store can hold no second row for that identifier.
func (g *Ingester) applyTriggerFailed(ctx context.Context, env IngestEnvelope) (ApplyResult, error) {
	// The route runs before Apply's own uuid.Parse, so no other code checks
	// the identifier. An unchecked identifier lands in no partition, and the
	// consumer would then retry that message forever.
	runID, createdAt, err := parseRunUUIDv7(env.RunID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrMalformedEnvelope, err)
	}

	var payload RunTriggerFailedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: decode run.trigger_failed payload: %v", ErrMalformedEnvelope, err)
	}

	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin ingest transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	tag, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, ended_at, actor, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			'COMPLETED', 'TRIGGER_FAILED', $9, $10,
			jsonb_build_object(
				'trigger_error', $11::text,
				'trigger_reason_code', $12::text,
				'trigger_http_status', $13::int,
				'trigger_failed_at', $9::timestamptz
			)
		)
		ON CONFLICT (created_at, run_id) DO NOTHING
	`,
		runID, createdAt, env.IntentKey, env.Cluster, env.Namespace, env.Cell, env.Tenant, env.JobPath,
		env.OccurredAt, nullableString(payload.Actor),
		payload.Error, payload.ReasonCode, payload.HTTPStatus,
	)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("insert failed trigger run: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit ingest transaction: %w", err)
	}

	return ApplyResult{
		TriggerFailedReason: payload.ReasonCode,
		TriggerFailedStored: tag.RowsAffected() > 0,
	}, nil
}
