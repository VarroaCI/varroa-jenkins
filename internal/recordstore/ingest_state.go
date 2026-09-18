package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// runStateCapBytes bounds the stored state of one run. The recorder is
// the authority on this bound: the gateway's own counter lives in
// memory and a restart loses it.
const runStateCapBytes = int64(64 * 1024 * 1024)

// runStateCapReason names the cap in mesh.rejected_reason.
const runStateCapReason = "state-cap"

// runStateFile is one entry of mesh.files. Bytes is the whole stored
// object's inflated size, not the last chunk's. Epoch names the Jenkins
// process whose object this entry points at.
type runStateFile struct {
	RelPath  string `json:"relpath"`
	Bytes    int64  `json:"bytes"`
	Seq      int64  `json:"seq"`
	StateGen int64  `json:"state_gen"`
	Epoch    int64  `json:"epoch"`
}

// newer reports whether f supersedes stored. A higher epoch always wins,
// because the lower epoch's process is gone. Inside one epoch the higher
// generation wins.
func (f runStateFile) newer(stored runStateFile) bool {
	if f.Epoch != stored.Epoch {
		return f.Epoch > stored.Epoch
	}
	return f.StateGen > stored.StateGen
}

// sameObject reports whether f and other name one stored object. The
// object key carries the path, the epoch, and the generation, so two
// entries name one object only when all three match.
func (f runStateFile) sameObject(other runStateFile) bool {
	return f.RelPath == other.RelPath && f.Epoch == other.Epoch && f.StateGen == other.StateGen
}

// applyRunState merges one run-state push outcome into
// runs.payload->'mesh'. A stored file sets the freshness anchor and
// upserts the file entry by its relative path. A gateway cap refusal
// sets mesh.cap_warned_at alone, because the gateway's counter is
// advisory and must not end a run's replication.
//
// It reports the run for a prefix delete once the recorded file sizes
// pass the cap, unless a dispatched resume holds that delete. Every
// write uses COALESCE, because runs.payload is nullable and a plain
// concatenation with NULL answers NULL.
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
	return recordRunStateCapWarning(ctx, tx, intent, env)
}

// recordRunStateCapWarning stamps mesh.cap_warned_at, an operator's one
// signal that a run passed the cap. It writes that key alone: both
// callers keep the run's replication in place.
func recordRunStateCapWarning(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
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
//
// The upsert reads the stored entry under a row lock first. Two recorder
// replicas can hold two reports of one run at one moment, and without
// the lock both would read one entry and the later write would lose the
// earlier decision. An UPDATE ... RETURNING cannot answer this: that
// array is already the written one.
func applyRunStateFile(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope, payload RunStatePayload) (dispatchResult, error) {
	incoming := runStateFile{
		RelPath:  payload.RelPath,
		Bytes:    payload.Bytes,
		Seq:      payload.Seq,
		StateGen: payload.StateGen,
		Epoch:    payload.PusherEpoch,
	}

	stored, found, dispatched, err := lockedRunStateEntry(ctx, tx, intent, payload.RelPath)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return dispatchResult{}, nil
	case err != nil:
		return dispatchResult{}, err
	}

	winner := incoming
	superseded := runStateFile{}
	if found {
		if incoming.newer(stored) {
			superseded = stored
		} else {
			winner, superseded = stored, incoming
		}
	}

	// Every push names its own key, so the superseded entry's key holds
	// an object no reader needs. A push that repeats both numbers is a
	// redelivery of the winner, and its key is the winner's own.
	//
	// A dispatched resume names the pushes it read, and its target
	// fetches them one at a time. The delete waits while such a resume is
	// in flight, so it can never remove an object the target still needs.
	// The run's own prefix delete removes that object when the run ends.
	var cleanupKey string
	if !dispatched && superseded.RelPath != "" && !superseded.sameObject(winner) {
		cleanupKey = blob.RunStateKey(
			intent.runID.String(), superseded.Epoch, superseded.StateGen, superseded.RelPath)
	}

	entry, err := json.Marshal(winner)
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
	if total > runStateCapBytes {
		if dispatched {
			return dispatchResult{}, holdRunStateCap(ctx, tx, intent, env)
		}
		// The cap deletes the whole prefix, which covers the superseded
		// object as well.
		return applyRunStateCap(ctx, tx, intent)
	}
	return dispatchResult{stateKeyCleanup: cleanupKey}, nil
}

// lockedRunStateEntry reads the run's stored entry for relpath under a
// row lock. It reports whether the record holds one, and whether the run
// holds a dispatched resume. The lock is what makes the precedence
// decision safe against a second recorder replica.
func lockedRunStateEntry(ctx context.Context, tx pgx.Tx, intent *intentRow, relpath string) (runStateFile, bool, bool, error) {
	var raw, resumeRaw []byte
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(payload->'mesh'->'files', '[]'::jsonb),
		       COALESCE(payload->'mesh'->'resume', 'null'::jsonb)
		  FROM varroa.runs
		 WHERE run_id = $1 AND created_at = $2
		   FOR UPDATE
	`, intent.runID, intent.createdAt).Scan(&raw, &resumeRaw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runStateFile{}, false, false, err
		}
		return runStateFile{}, false, false, fmt.Errorf("read run-state file list: %w", err)
	}

	var resume *runMeshResume
	if err := json.Unmarshal(resumeRaw, &resume); err != nil {
		return runStateFile{}, false, false, fmt.Errorf("decode run-state resume: %w", err)
	}
	// An armed run holds a resume that names no instance, so the named
	// instance is what proves a dispatch. The heartbeat ingest clears the
	// whole object once the target reports the run.
	dispatched := resume != nil && resume.Instance != ""

	entries, err := decodeRunStateFiles(raw)
	if err != nil {
		return runStateFile{}, false, dispatched, err
	}
	for _, e := range entries {
		if e.RelPath == relpath {
			return e, true, dispatched, nil
		}
	}
	return runStateFile{}, false, dispatched, nil
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

// holdRunStateCap holds the cap while a dispatched resume names a
// target. It records the cap warning alone and keeps mesh.state_at,
// mesh.resume, and every stored object: the target reads exactly the
// objects the cap deletes, and it reads them one at a time.
//
// The cap holds whole. It cannot bound the stored bytes by refusing the
// entry instead, because the gateway writes the object before it reports
// it: a refused entry loses the record of an object that exists, and
// frees no storage. The record therefore keeps every entry, and the
// first report after the resume clears ends the replication.
//
// The hold is bounded by three exits. The target's own heartbeat clears
// mesh.resume, a resume failure clears the named instance, and a resume
// that proves neither passes its deadline, which marks the run
// interrupted and ends the run.
func holdRunStateCap(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	return recordRunStateCapWarning(ctx, tx, intent, env)
}

// applyRunStateCap ends a run's replication. It clears the resume as
// well as the anchor: a run whose state is gone can never resume, and a
// stale arm would shield it from the sweep, and hold its fleet lock,
// for the whole arm window. Its caller holds it while a dispatched
// resume names a target.
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
