package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// runStateCapBytes bounds the stored state of one run. The recorder is
// the authority on this bound: the gateway's own counter lives in
// memory and a restart loses it.
const runStateCapBytes = int64(64 * 1024 * 1024)

// runStateCapReason names the cap in mesh.rejected_reason.
const runStateCapReason = "state-cap"

// runStateFile is one entry of mesh.files. Bytes is the whole stored
// object's inflated size, not the last chunk's.
type runStateFile struct {
	RelPath  string `json:"relpath"`
	Bytes    int64  `json:"bytes"`
	Seq      int64  `json:"seq"`
	StateGen int64  `json:"state_gen"`
}

// applyRunState merges one run-state push outcome into
// runs.payload->'mesh'. A stored file sets the freshness anchor and
// upserts the file entry by its relative path. A gateway cap refusal
// sets mesh.cap_warned_at alone, because the gateway's counter is
// advisory and must not end a run's replication.
//
// It reports the run for a prefix delete once the recorded file sizes
// pass the cap. Every write uses COALESCE, because runs.payload is
// nullable and a plain concatenation with NULL answers NULL.
func applyRunState(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) (dispatchResult, error) {
	var payload RunStatePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return dispatchResult{}, fmt.Errorf("%w: run.state payload: %v", ErrMalformedEnvelope, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, 'QUEUED'
		)
		ON CONFLICT (created_at, run_id) DO NOTHING
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
	); err != nil {
		return dispatchResult{}, fmt.Errorf("ensure run row for run.state: %w", err)
	}

	if payload.RelPath == "" {
		return dispatchResult{}, applyRunStateRefusal(ctx, tx, intent, env, payload)
	}
	return applyRunStateFile(ctx, tx, intent, env, payload)
}

// applyRunStateRefusal records a gateway refusal. It leaves state_at
// unchanged, so the run keeps whatever replication it already has.
func applyRunStateRefusal(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope, payload RunStatePayload) error {
	if payload.RejectedReason != runStateCapReason {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb) || jsonb_build_object('cap_warned_at', $3::timestamptz))
		 WHERE run_id = $1 AND created_at = $2
	`, intent.runID, intent.createdAt, env.OccurredAt); err != nil {
		return fmt.Errorf("record run-state cap warning: %w", err)
	}
	return nil
}

// applyRunStateFile upserts one stored file by its relative path and
// then applies the durable cap.
func applyRunStateFile(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope, payload RunStatePayload) (dispatchResult, error) {
	entry, err := json.Marshal(runStateFile{
		RelPath:  payload.RelPath,
		Bytes:    payload.Bytes,
		Seq:      payload.Seq,
		StateGen: payload.StateGen,
	})
	if err != nil {
		return dispatchResult{}, fmt.Errorf("encode run-state file entry: %w", err)
	}

	var files []byte
	err = tx.QueryRow(ctx, `
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb)
		         || jsonb_build_object('state_at', $3::timestamptz)
		         || jsonb_build_object('files',
		              COALESCE((
		                SELECT jsonb_agg(e)
		                  FROM jsonb_array_elements(COALESCE(payload->'mesh'->'files', '[]'::jsonb)) e
		                 WHERE e->>'relpath' IS DISTINCT FROM $4
		              ), '[]'::jsonb) || jsonb_build_array($5::jsonb)))
		 WHERE run_id = $1 AND created_at = $2
		RETURNING COALESCE(payload->'mesh'->'files', '[]'::jsonb)
	`, intent.runID, intent.createdAt, env.OccurredAt, payload.RelPath, string(entry)).Scan(&files)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return dispatchResult{}, nil
	case err != nil:
		return dispatchResult{}, fmt.Errorf("upsert run-state file: %w", err)
	}

	total, err := sumRunStateBytes(files)
	if err != nil {
		return dispatchResult{}, err
	}
	if total <= runStateCapBytes {
		return dispatchResult{}, nil
	}
	return applyRunStateCap(ctx, tx, intent)
}

// sumRunStateBytes sums the stored size of every file the record holds
// after the upsert, so a replaced path is counted once.
func sumRunStateBytes(files []byte) (int64, error) {
	var entries []runStateFile
	if err := json.Unmarshal(files, &entries); err != nil {
		return 0, fmt.Errorf("decode run-state file list: %w", err)
	}
	var total int64
	for _, e := range entries {
		total += e.Bytes
	}
	return total, nil
}

// applyRunStateCap ends a run's replication. It clears the resume as
// well as the anchor: a run whose state is gone can never resume, and a
// stale arm would shield it from the sweep, and hold its fleet lock,
// for the whole arm window.
func applyRunStateCap(ctx context.Context, tx pgx.Tx, intent *intentRow) (dispatchResult, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         (COALESCE(payload->'mesh', '{}'::jsonb) - 'resume')
		         || jsonb_build_object('state_at', NULL, 'rejected_reason', $3::text))
		 WHERE run_id = $1 AND created_at = $2
	`, intent.runID, intent.createdAt, runStateCapReason); err != nil {
		return dispatchResult{}, fmt.Errorf("end run-state replication at the cap: %w", err)
	}
	return dispatchResult{stateCleanup: intent.runID.String()}, nil
}
