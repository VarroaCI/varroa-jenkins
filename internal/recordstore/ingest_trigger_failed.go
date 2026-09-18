package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// reasonTriggerRejected is the one run.trigger_failed reason code a
// later re-fire can still fix. The gateway sets it for every Jenkins
// status but 404 and 409, which the two permanent codes take.
const reasonTriggerRejected = "TRIGGER_REJECTED"

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

	outcome, reason, err := g.healRefiredIntent(ctx, tx, env.IntentKey, payload.ReasonCode)
	if err != nil {
		return ApplyResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit ingest transaction: %w", err)
	}

	if outcome != "" {
		g.sweepMetrics.addRefired(ctx, outcome, reason)
	}
	return ApplyResult{
		TriggerFailedReason: payload.ReasonCode,
		TriggerFailedStored: tag.RowsAffected() > 0,
	}, nil
}

// healRefiredIntent answers a rejected trigger that followed a re-fire.
// It runs inside the caller's transaction, so the marker and the failed
// run's record commit together.
//
// A re-fire marks the intent and then publishes. Nothing after that
// publish ever cleared the marker, so one rejected trigger lost the
// build for good. TRIGGER_REJECTED is transient: the cell that answered
// it was reachable but refused the call, which is what a mite with no
// Jenkins token yet produces. Clearing the marker lets a later round
// retry, bounded by refire_attempts, which this clear never lowers.
//
// JOB_NOT_FOUND and JOB_NOT_BUILDABLE are permanent. No retry can fix a
// job that is absent or disabled, so the marker stays set and a person
// decides.
//
// It answers the empty outcome when the intent carries no marker, which
// is every trigger that no re-fire produced.
func (g *Ingester) healRefiredIntent(ctx context.Context, tx pgx.Tx,
	intentKey, reasonCode string) (outcome, reason string, err error) {
	if intentKey == "" {
		return "", "", nil
	}
	if reasonCode != reasonTriggerRejected {
		var marked bool
		err := tx.QueryRow(ctx, `
			SELECT refired_at IS NOT NULL FROM varroa.run_intents WHERE intent_key = $1
		`, intentKey).Scan(&marked)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return "", "", nil
		case err != nil:
			return "", "", fmt.Errorf("read the re-fire marker on intent %q: %w", intentKey, err)
		case !marked:
			return "", "", nil
		}
		return refireOutcomeSkipped, refireReasonRejectedPerm, nil
	}

	tag, err := tx.Exec(ctx, `
		UPDATE varroa.run_intents SET refired_at = NULL
		WHERE intent_key = $1 AND refired_at IS NOT NULL AND state = 'GRANTED'
		  AND refire_attempts < $2
	`, intentKey, maxRefireAttempts)
	if err != nil {
		return "", "", fmt.Errorf("clear the re-fire marker on intent %q: %w", intentKey, err)
	}
	if tag.RowsAffected() == 1 {
		return refireOutcomeCleared, refireReasonRejectedTemp, nil
	}

	// The clear matched nothing. Either the intent carries no marker, or
	// its attempts are spent: only the second is reportable.
	var spent bool
	err = tx.QueryRow(ctx, `
		SELECT refired_at IS NOT NULL AND refire_attempts >= $2
		FROM varroa.run_intents WHERE intent_key = $1
	`, intentKey, maxRefireAttempts).Scan(&spent)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("read the re-fire attempts on intent %q: %w", intentKey, err)
	case spent:
		return refireOutcomeSkipped, refireReasonExhausted, nil
	}
	return "", "", nil
}
