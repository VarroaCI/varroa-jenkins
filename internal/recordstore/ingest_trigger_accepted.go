package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// RunTriggerAcceptedPayload is the part of a run.trigger_accepted
// envelope the recorder stores. The gateway also sends http_status; the
// row keeps no outcome field for an accepted trigger, so the recorder
// does not decode it.
type RunTriggerAcceptedPayload struct {
	Actor string `json:"actor"`
}

// applyTriggerAccepted writes the one runs row a run.trigger_accepted
// envelope produces, inside its own transaction. It writes run_intents
// never: an accepted trigger can arrive before any claim exists.
//
// It does read run_intents for the row's status. An intent key that
// already belongs to another run identifier means the claim for this one
// answers duplicate, so this trigger never builds and the row is
// terminal at once. The claim path marks the same row when it wins the
// race the other way. Both writes are needed: either order can happen,
// and the interrupted sweep leaves a QUEUED row alone while its
// accepting instance is alive.
//
// Such a row carries ended_at with its status. Retention reclaims a
// terminal run by ended_at, so a DUPLICATE row with none would sit until
// its partition expires.
//
// created_at is the UUIDv7 time of the run identifier, not occurred_at. A
// real run's created_at holds that same moment, so one run identifier maps
// to one primary key whichever path writes it. The conflict clause alone
// then decides, and the store can hold no second row for that identifier.
func (g *Ingester) applyTriggerAccepted(ctx context.Context, env IngestEnvelope) (ApplyResult, error) {
	// The route runs before Apply's own uuid.Parse, so no other code checks
	// the identifier. An unchecked identifier lands in no partition, and the
	// consumer would then retry that message forever.
	runID, createdAt, err := parseRunUUIDv7(env.RunID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrMalformedEnvelope, err)
	}

	var payload RunTriggerAcceptedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: decode run.trigger_accepted payload: %v", ErrMalformedEnvelope, err)
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
			status, ended_at, actor, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			CASE WHEN EXISTS (
				SELECT 1 FROM varroa.run_intents ri
				WHERE ri.intent_key = $3 AND ri.run_id <> $1
			) THEN 'DUPLICATE' ELSE 'QUEUED' END,
			CASE WHEN EXISTS (
				SELECT 1 FROM varroa.run_intents ri
				WHERE ri.intent_key = $3 AND ri.run_id <> $1
			) THEN now() END,
			$9,
			jsonb_build_object('accepted_at', $10::timestamptz, 'accepted_instance', $11::text)
		)
		ON CONFLICT (created_at, run_id) DO NOTHING
	`,
		runID, createdAt, env.IntentKey, env.Cluster, env.Namespace, env.Cell, env.Tenant, env.JobPath,
		nullableString(payload.Actor),
		env.OccurredAt, env.CellInstance,
	)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("insert accepted trigger run: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit ingest transaction: %w", err)
	}

	return ApplyResult{
		TriggerAcceptedStored: tag.RowsAffected() > 0,
	}, nil
}
