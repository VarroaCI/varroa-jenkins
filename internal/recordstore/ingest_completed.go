package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunCompleted upserts the runs row for a run.completed envelope
// under Contract G1's terminal-precedence rule. A real terminal event —
// one whose cell_instance equals the intent's owning cell_instance —
// always wins over a synthetic interruption C6's sweep wrote, and the
// write records the moment of that replacement in
// interrupted_superseded_at. It stores the complete envelope in
// payload under the key "run.completed", so a later kind's write never
// replaces it. A completion from any other instance is an audit fact:
// it merges into payload under foreign_completion instead and changes
// no terminal column.
//
// The owner arm also supersedes a row that records no build: a
// TRIGGER_FAILED row, or a row that still holds the accepted keys. It
// removes the accepted keys from every owner write, whether or not the
// stored row held them, because the owner arm is always a real write. A
// foreign completion leaves every stored key in place.
//
// owner reports whether this write was the real terminal write: only
// then does the caller run the fan-out round. A foreign completion
// changes no terminal column, so it is not "an ingested run.completed"
// in the sense the fleet-locks and cross-tenant-upstream-trigger
// capabilities mean: it would double-fire a downstream trigger or
// release a lock still protected by a live deployment.
//
// releasedLocks names every fleet lock the owner arm deleted, one entry
// per row, so the caller publishes one lock notification per released
// row. A foreign completion releases nothing and answers no entry.
func applyRunCompleted(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) (owner bool, payload RunCompletedPayload, releasedLocks []ReleasedFleetLock, err error) {
	if unmarshalErr := json.Unmarshal(env.Payload, &payload); unmarshalErr != nil {
		return false, RunCompletedPayload{}, nil, fmt.Errorf("%w: decode run.completed payload: %v", ErrMalformedEnvelope, unmarshalErr)
	}

	owner = intent.cellInstance != nil && *intent.cellInstance != "" && env.CellInstance == *intent.cellInstance

	envelopeJSON, err := json.Marshal(env)
	if err != nil {
		return false, RunCompletedPayload{}, nil, fmt.Errorf("marshal envelope: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, ended_at, duration_ms, scm_revision, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			CASE WHEN $13 THEN 'COMPLETED' ELSE 'QUEUED' END,
			CASE WHEN $13 THEN $9::text ELSE NULL::text END,
			CASE WHEN $13 THEN $10::timestamptz ELSE NULL::timestamptz END,
			CASE WHEN $13 THEN $11::bigint ELSE NULL::bigint END,
			$14,
			CASE WHEN $13 THEN jsonb_build_object('run.completed', $12::jsonb) ELSE jsonb_build_object('foreign_completion', $12::jsonb) END
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			status = `+runsStatusMergeSQL("$13")+`,
			cell = CASE WHEN `+supersedeSQL("$13")+` THEN EXCLUDED.cell ELSE runs.cell END,
			-- The owner arm below already supplies the supersede value for
			-- these three columns: a supersede needs $13, and $13 takes the
			-- event's own value whatever the row holds.
			result = CASE WHEN $13 THEN EXCLUDED.result ELSE COALESCE(EXCLUDED.result, runs.result) END,
			ended_at = CASE WHEN $13 THEN EXCLUDED.ended_at ELSE COALESCE(EXCLUDED.ended_at, runs.ended_at) END,
			duration_ms = CASE WHEN $13 THEN EXCLUDED.duration_ms ELSE COALESCE(EXCLUDED.duration_ms, runs.duration_ms) END,
			-- The fill-only revision rule: an absent value takes the
			-- newest changelog entry's revision, whatever the seq. A set
			-- value stays, so the rule never rewrites a known commit.
			scm_revision = COALESCE(NULLIF(runs.scm_revision, ''), EXCLUDED.scm_revision),
			interrupted_superseded_at = CASE
				WHEN $13 AND runs.result = 'INTERRUPTED' THEN now()
				ELSE runs.interrupted_superseded_at
			END,
			payload = (CASE
			                WHEN runs.result IS NOT DISTINCT FROM '`+ResultTriggerFailed+`' AND $13 THEN COALESCE(runs.payload, '{}'::jsonb) `+triggerPayloadKeysSQL+`
			                WHEN COALESCE(runs.payload ? 'accepted_at', false) AND $13 THEN COALESCE(runs.payload, '{}'::jsonb) `+acceptedSupersedeKeysSQL+`
			                WHEN $13 THEN COALESCE(runs.payload, '{}'::jsonb) `+acceptedPayloadKeysSQL+`
			                ELSE COALESCE(runs.payload, '{}'::jsonb) END) || EXCLUDED.payload
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
		payload.Result, payload.EndedAt, payload.DurationMs, string(envelopeJSON),
		owner, payload.SCMRevision,
	)
	if err != nil {
		return false, RunCompletedPayload{}, nil, fmt.Errorf("upsert completed run: %w", err)
	}

	// The fleet lock's first release confirmation (design.md D11): a
	// terminal run.completed for the holder deletes a held or a
	// releasing row of this run, whichever it finds. It runs only for
	// the real terminal write, in the same transaction as the runs row
	// it confirms, so a crash after commit never strands a lock a
	// finished run no longer protects.
	if owner {
		releasedLocks, err = deleteHeldFleetLocksForRun(ctx, tx, intent.runID.String())
		if err != nil {
			return false, RunCompletedPayload{}, nil, err
		}
	}

	return owner, payload, releasedLocks, nil
}

// ReleasedFleetLock is one fleet_locks row a terminal run.completed
// write deleted, carried up to Ingester.notify for that row's own lock
// notification. Each field is the deleted row's own value.
type ReleasedFleetLock struct {
	Cluster, Tenant, JobPath, LockName string
}

// deleteHeldFleetLocksForRun deletes every held or releasing fleet_locks
// row of one run, and answers each deleted row's own identity. The
// statement's own affected-row count cannot name a row, and the
// notification needs the deleted row's own cluster, tenant, job path,
// and qualified lock name. It drains and closes its rows before it
// returns, so the caller's transaction can commit.
func deleteHeldFleetLocksForRun(ctx context.Context, tx pgx.Tx, runID string) ([]ReleasedFleetLock, error) {
	rows, err := tx.Query(ctx, `
		DELETE FROM varroa.fleet_locks WHERE run_id = $1 AND state IN ('held', 'releasing')
		RETURNING cluster, tenant, job_path, name
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("release fleet locks for terminal run: %w", err)
	}
	defer rows.Close()

	var released []ReleasedFleetLock
	for rows.Next() {
		var lock ReleasedFleetLock
		if err := rows.Scan(&lock.Cluster, &lock.Tenant, &lock.JobPath, &lock.LockName); err != nil {
			return nil, fmt.Errorf("scan released fleet lock: %w", err)
		}
		released = append(released, lock)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("release fleet locks for terminal run: %w", err)
	}
	return released, nil
}
