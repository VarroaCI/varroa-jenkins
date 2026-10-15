package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// Command names a run.command_refused envelope carries. The literal
// values must match internal/mite's own refused command names, and the
// API's run detail answers them unchanged.
const (
	RefusedCommandCancel  = "cancel"
	RefusedCommandProceed = "proceed_input"
	RefusedCommandAbort   = "abort_input"
)

// refusedCommands lists every command name a run.command_refused envelope
// may carry.
var refusedCommands = map[string]bool{
	RefusedCommandCancel:  true,
	RefusedCommandProceed: true,
	RefusedCommandAbort:   true,
}

// applyCommandRefused merges one refusal notice into the run's payload
// under the command_refusal key: the command, the actor, the HTTP status,
// the error, and the refusal time.
//
// The guard names the run's primary key and the envelope's cluster,
// tenant, and job path, so a cell cannot write a notice onto another
// tenant's run. A newer stored notice wins: an older envelope that
// arrives late changes nothing. A run with no row changes nothing either,
// because the notice is not a record of its own. A later successful
// command does not clear the notice, because the notice carries its own
// time.
func (g *Ingester) applyCommandRefused(ctx context.Context, env IngestEnvelope) (ApplyResult, error) {
	// The route runs before Apply's own uuid.Parse, so no other code checks
	// the identifier. An unchecked identifier lands in no partition, and the
	// consumer would then retry that message forever.
	runID, createdAt, err := parseRunUUIDv7(env.RunID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrMalformedEnvelope, err)
	}

	var payload RunCommandRefusedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: decode run.command_refused payload: %v", ErrMalformedEnvelope, err)
	}
	if !refusedCommands[payload.Command] {
		return ApplyResult{}, fmt.Errorf("%w: run.command_refused names unknown command %q", ErrMalformedEnvelope, payload.Command)
	}
	if env.OccurredAt.IsZero() {
		return ApplyResult{}, fmt.Errorf("%w: run.command_refused carries no occurred_at", ErrMalformedEnvelope)
	}

	// One statement commits on its own, so this write opens no
	// transaction.
	tag, err := g.pool.Exec(ctx, `
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('command_refusal',
		         jsonb_build_object(
		           'command', $6::text,
		           'actor', $7::text,
		           'http_status', $8::int,
		           'error', $9::text,
		           'refused_at', $10::timestamptz))
		 WHERE run_id = $1 AND created_at = $2
		   AND cluster = $3 AND tenant = $4 AND job_path = $5
		   AND (payload->'command_refusal'->>'refused_at' IS NULL
		        OR (payload->'command_refusal'->>'refused_at')::timestamptz < $10::timestamptz)
	`, runID, createdAt, env.Cluster, env.Tenant, env.JobPath,
		payload.Command, payload.Actor, payload.HTTPStatus, payload.Error, env.OccurredAt.UTC())
	if err != nil {
		return ApplyResult{}, fmt.Errorf("store a command refusal: %w", err)
	}
	return ApplyResult{CommandRefusedStored: tag.RowsAffected() == 1, CommandRefused: payload.Command}, nil
}
