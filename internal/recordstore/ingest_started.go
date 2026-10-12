package recordstore

import (
	"context"
	"encoding/json"
	"errors"
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
//
// A run.started write always establishes a real run, so it supersedes a
// stored row that records no build: a TRIGGER_FAILED row, or a row that
// still holds the accepted keys. It removes the accepted keys from the
// stored payload either way, and stamps interrupted_superseded_at when
// the row it supersedes was marked INTERRUPTED. A late started write into
// an interrupted real run supersedes nothing and leaves the stamp alone.
//
// A start-lost mark is the one interrupted real run a started write
// supersedes (startLostRunSQL). The mark records only that no start event
// arrived in time, and this event is that start. The write sets the row
// RUNNING, clears the result, the end, and the marking keys, and stamps
// interrupted_superseded_at.
//
// A run.started that reports a number below the intent's stored number,
// from an instance other than the intent's current owner, describes a
// build of a superseded grant: the ledger moved and refreshed the intent
// after that build was authorized. Such a write would set the stale
// number back over the fresh one, and the owner's completion carries no
// number to restore it, so the function writes nothing and reports the
// superseded fields instead. An envelope that meets only one of the two
// conditions takes the ordinary path.
func applyRunStarted(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope, nullBuildNumber bool) (dispatchResult, error) {
	var payload RunStartedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return dispatchResult{}, fmt.Errorf("%w: decode run.started payload: %v", ErrMalformedEnvelope, err)
	}

	if superseded, res := supersededStart(intent, env, payload); superseded {
		return res, nil
	}

	res := dispatchResult{}
	if payload.BuildNumber != nil && payload.GrantedBuildNumber != nil && *payload.GrantedBuildNumber != *payload.BuildNumber {
		res.numberMismatch = true
		res.mismatchGranted = *payload.GrantedBuildNumber
		res.mismatchReported = *payload.BuildNumber
	}

	buildNumber := payload.BuildNumber
	if nullBuildNumber {
		buildNumber = nil
		if payload.BuildNumber != nil {
			holder, err := numberHolder(ctx, tx, intent, *payload.BuildNumber)
			if err != nil {
				return dispatchResult{}, err
			}
			res.conflictRunID = holder
			res.conflictNumber = *payload.BuildNumber
		}
	} else if payload.BuildNumber != nil && (intent.buildNumber == nil || *intent.buildNumber != *payload.BuildNumber) {
		taken, err := takeUnstartedGrant(ctx, tx, intent, *payload.BuildNumber)
		if err != nil {
			return dispatchResult{}, err
		}
		res.numberTakenFrom = taken
	}

	envelopeJSON, err := json.Marshal(env)
	if err != nil {
		return dispatchResult{}, fmt.Errorf("marshal envelope: %w", err)
	}

	upstream, err := deriveUpstream(ctx, tx, intent, payload)
	if err != nil {
		return dispatchResult{}, err
	}

	startedSupersede := "(" + supersedeSQL("TRUE") + " OR " + startLostRunSQL + ")"
	var status string
	err = tx.QueryRow(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			build_number, branch, scm_revision, status, queued_at, started_at, actor, cause, params, payload,
			replay_of, upstream_run_id, upstream_tenant, upstream_job, upstream_number
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, 'RUNNING', $12, $13, $14, $15::jsonb, $16::jsonb,
			jsonb_build_object('run.started', $17::jsonb),
			$18::uuid, $19::uuid, $20, $21, $22
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
			replay_of    = COALESCE(EXCLUDED.replay_of, runs.replay_of),
			upstream_run_id = COALESCE(EXCLUDED.upstream_run_id, runs.upstream_run_id),
			upstream_tenant = COALESCE(EXCLUDED.upstream_tenant, runs.upstream_tenant),
			upstream_job    = COALESCE(EXCLUDED.upstream_job, runs.upstream_job),
			upstream_number = COALESCE(EXCLUDED.upstream_number, runs.upstream_number),
			status       = CASE WHEN `+startLostRunSQL+` THEN EXCLUDED.status ELSE `+runsStatusMergeSQL("TRUE")+` END,
			cell         = CASE WHEN `+startedSupersede+` THEN EXCLUDED.cell ELSE runs.cell END,
			namespace    = CASE WHEN `+startedSupersede+` THEN EXCLUDED.namespace ELSE runs.namespace END,
			result       = CASE WHEN `+startedSupersede+` THEN NULL::text ELSE runs.result END,
			ended_at     = CASE WHEN `+startedSupersede+` THEN NULL::timestamptz ELSE runs.ended_at END,
			interrupted_superseded_at = CASE WHEN `+startedSupersede+` AND runs.result = 'INTERRUPTED' THEN now() ELSE runs.interrupted_superseded_at END,
			payload      = (CASE
			                     WHEN runs.result IS NOT DISTINCT FROM '`+ResultTriggerFailed+`' THEN COALESCE(runs.payload, '{}'::jsonb) `+triggerPayloadKeysSQL+`
			                     WHEN COALESCE(runs.payload ? 'accepted_at', false) OR `+startLostRunSQL+` THEN COALESCE(runs.payload, '{}'::jsonb) `+acceptedSupersedeKeysSQL+`
			                     ELSE COALESCE(runs.payload, '{}'::jsonb) END) `+acceptedPayloadKeysSQL+` || EXCLUDED.payload
		RETURNING status
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
		buildNumber, payload.Branch, payload.SCMRevision,
		payload.QueuedAt, payload.StartedAt, payload.Actor,
		jsonArg(payload.Cause), jsonArg(payload.Params), string(envelopeJSON),
		replayOfArg(payload.ReplayOf),
		upstream.runID, upstream.tenant, upstream.job, upstream.number,
	).Scan(&status)
	if err != nil {
		return dispatchResult{}, fmt.Errorf("upsert started run: %w", err)
	}
	if err := fillDownstreamUpstream(ctx, tx, intent, buildNumber); err != nil {
		return dispatchResult{}, err
	}

	consumedAt := env.OccurredAt
	if buildNumber == nil {
		if err := consumeIntentNullBuildNumber(ctx, tx, intent.runID, consumedAt); err != nil {
			return dispatchResult{}, err
		}
	} else {
		if err := consumeIntent(ctx, tx, intent.runID, *buildNumber, consumedAt); err != nil {
			return dispatchResult{}, err
		}
		if !nullBuildNumber {
			if err := RaiseWatermarkForStartedBuild(ctx, tx, intent.cluster, intent.tenant, intent.jobPath, *buildNumber); err != nil {
				return dispatchResult{}, err
			}
		}
	}

	if status != StatusQueued && status != StatusRunning {
		// The arriving run is no longer live: an out-of-order run.completed
		// already finished it. The duplicate backstop does not apply.
		return res, nil
	}

	revision := ""
	if payload.SCMRevision != nil {
		revision = *payload.SCMRevision
	}
	action, err := runDuplicateBackstop(ctx, tx, intent, revision)
	res.action = action
	return res, err
}

// takeUnstartedGrant frees number for the arriving run when another
// intent of the same job holds it as an unstarted grant. Jenkins already
// built the arriving run under number, so that number is the truth. The
// holder's run has not started, so it holds no Jenkins number yet: its
// own run.started writes one later, and its next claim reserves a fresh
// grant. The function sets the holder's number to null and answers the
// holder's run id. It answers "" and changes nothing when no intent holds
// the number, or when the holder already started; the write then meets
// the unique constraint.
//
// The caller already holds its own intent's lock. The holder's lock comes
// second, and a claim never holds two intent locks, so only two crossing
// ingest writes can deadlock; Postgres then fails one, and the message is
// delivered again.
func takeUnstartedGrant(ctx context.Context, tx pgx.Tx, intent *intentRow, number int64) (string, error) {
	var (
		holder uuid.UUID
		state  string
	)
	err := tx.QueryRow(ctx, `
		SELECT run_id, state FROM varroa.run_intents
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND build_number = $4 AND run_id <> $5
		FOR UPDATE
	`, intent.cluster, intent.tenant, intent.jobPath, number, intent.runID).Scan(&holder, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read build number holder: %w", err)
	}
	if state != stateGranted {
		return "", nil
	}
	started, err := runsRowExists(ctx, tx, holder)
	if err != nil || started {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.run_intents SET build_number = NULL WHERE run_id = $1
	`, holder); err != nil {
		return "", fmt.Errorf("release unstarted grant: %w", err)
	}
	return holder.String(), nil
}

// numberHolder answers the run id of the other intent of the job that
// holds number, or "" when none does. The null-number retry reads it for
// the conflict log line alone.
func numberHolder(ctx context.Context, tx pgx.Tx, intent *intentRow, number int64) (string, error) {
	var holder uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT run_id FROM varroa.run_intents
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND build_number = $4 AND run_id <> $5
	`, intent.cluster, intent.tenant, intent.jobPath, number, intent.runID).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read build number holder: %w", err)
	}
	return holder.String(), nil
}

// replayOfArg binds run.started's replay_of value. An absent, empty, or
// non-UUID value binds NULL, so a malformed source identifier never fails
// the upsert and never loses the rest of a decodable event.
func replayOfArg(v *string) *string {
	if v == nil {
		return nil
	}
	id, err := uuid.Parse(*v)
	if err != nil {
		return nil
	}
	s := id.String()
	return &s
}

// supersededStart reports whether env describes a build of a superseded
// grant: its build number is below the intent's stored number and its
// cell instance is not the intent's current owner. A null reported
// number, a null stored number, or the owning instance never counts.
func supersededStart(intent *intentRow, env IngestEnvelope, payload RunStartedPayload) (bool, dispatchResult) {
	if payload.BuildNumber == nil || intent.buildNumber == nil || intent.cellInstance == nil {
		return false, dispatchResult{}
	}
	if *payload.BuildNumber >= *intent.buildNumber || env.CellInstance == *intent.cellInstance {
		return false, dispatchResult{}
	}
	return true, dispatchResult{
		supersededStart:    true,
		supersededReported: *payload.BuildNumber,
		supersededStored:   *intent.buildNumber,
		supersededOwner:    *intent.cellInstance,
	}
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
