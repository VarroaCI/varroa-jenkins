package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// reasonTriggerRejected is the run.trigger_failed reason code of a call
// Jenkins answered with a status other than 404 and 409. The backstop
// treats it as permanent; the unstarted-intent re-fire treats it as
// transient.
const reasonTriggerRejected = "TRIGGER_REJECTED"

// ReasonClaimExpired is the trigger reason code the claim-expiry sweep
// writes. No envelope carries it: the recorder itself marks an accepted
// run that no claim reached within ClaimExpiryAge.
const ReasonClaimExpired = "CLAIM_EXPIRED"

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

	attempt, err := triggerAttemptTx(ctx, tx, env, runID)
	if err != nil {
		return ApplyResult{}, err
	}
	triggerError := payload.Error
	if payload.ReasonCode == reasonTriggerUnreachable && strings.HasPrefix(env.IntentKey, backstopIntentPrefix) &&
		backstopRefireSpent(attempt) {
		triggerError = strings.TrimSpace(triggerError + "; " + backstopRefireSpentNote)
	}

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
				'trigger_failed_at', $9::timestamptz,
				'trigger_attempt', $14::int
			)
		)
		ON CONFLICT (created_at, run_id) DO NOTHING
	`,
		runID, createdAt, env.IntentKey, env.Cluster, env.Namespace, env.Cell, env.Tenant, env.JobPath,
		env.OccurredAt, nullableString(payload.Actor),
		triggerError, payload.ReasonCode, payload.HTTPStatus, attempt,
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

// triggerAttemptTx answers the attempt number of a failed trigger: one
// more than the TRIGGER_FAILED runs of the same cluster and tenant that
// already hold its intent key under another run identifier. A cron key
// names no cluster, so the count filters on it. An empty key answers 1.
// The predicate names the literal result, so the count uses
// runs_trigger_failed_intent_key_idx.
//
// A backstop key also counts the occurrence's fires, because the re-fire
// limit counts a fire that never answered. The value then matches the
// attempt number the fire's activity row names.
func triggerAttemptTx(ctx context.Context, tx pgx.Tx, env IngestEnvelope, runID uuid.UUID) (int, error) {
	if env.IntentKey == "" {
		return 1, nil
	}
	var prior int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM varroa.runs
		WHERE intent_key = $1 AND result = 'TRIGGER_FAILED' AND run_id <> $2
		  AND cluster = $3 AND tenant = $4
	`, env.IntentKey, runID, env.Cluster, env.Tenant).Scan(&prior); err != nil {
		return 0, fmt.Errorf("count failed triggers of %q: %w", env.IntentKey, err)
	}
	attempt := prior + 1

	occ, ok := backstopOccurrenceOfKey(env.IntentKey, env.JobPath)
	if !ok {
		return attempt, nil
	}
	var fires int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM varroa.activity
		WHERE type = 'build.scheduled.backstop' AND cluster = $1 AND item_path = $2 AND reason = $3 AND ts > $4
	`, env.Cluster, env.JobPath, occurrenceMinuteKey(occ), env.OccurredAt.Add(-maxBackstopOccurrenceAge)).Scan(&fires); err != nil {
		return 0, fmt.Errorf("count backstop fires of %q: %w", env.IntentKey, err)
	}
	return max(attempt, fires), nil
}

// healRefiredIntent answers a rejected trigger that followed a re-fire.
// It runs inside the caller's transaction, so the marker and the failed
// run's record commit together.
//
// A re-fire marks the intent and then publishes. Nothing after that
// publish ever cleared the marker, so one rejected trigger lost the
// build for good. TRIGGER_REJECTED and TRIGGER_UNREACHABLE are transient:
// the cell refused the call or never reached its Jenkins, which is what a
// mite with no Jenkins token yet or a booting Jenkins produces. Clearing the marker lets a later round
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
	if !isTransientTriggerReason(reasonCode) {
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
