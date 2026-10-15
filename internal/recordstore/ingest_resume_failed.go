package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// resumeHeldCodes are the two reason codes that prove the target's own
// Jenkins process reached the build load. A run that carries one keeps
// its dispatch: a second target must never adopt a build the first
// target may already hold, and only the run's own resume deadline can
// decide that case.
var resumeHeldCodes = map[string]bool{
	"RESUME_LOAD_FAILED":  true,
	"RESUME_NOT_BUILDING": true,
}

// applyResumeFailed returns a run to the armed state after its target
// failed the resume command, so a later round can pick another target
// while the arm window lasts.
//
// The write clears the dispatch and keeps the arm. It never moves
// arm_expires: a failure buys the run no time, and the window bounds
// how long the run holds its fleet lock.
//
// It also records the failed instance. resumeTargetForRun never picks
// an instance on that list, which is the bound on re-dispatch: a
// further dispatch needs an instance that is new, so a pod must roll or
// a job home must move.
//
// The run's owner is not moved back. The failed target keeps the
// ownership the dispatch gave it, because the dead instance can never
// run the build and an owner that names a gone pod would open a claim
// fence for a run that is not queued. The next dispatch moves the owner
// again, with the guard that reads the run's current owner.
func (g *Ingester) applyResumeFailed(ctx context.Context, env IngestEnvelope) (ApplyResult, error) {
	// The route runs before Apply's own uuid.Parse, so no other code checks
	// the identifier. An unchecked identifier lands in no partition, and the
	// consumer would then retry that message forever.
	runID, createdAt, err := parseRunUUIDv7(env.RunID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrMalformedEnvelope, err)
	}

	var payload RunResumeFailedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: decode run.resume_failed payload: %v", ErrMalformedEnvelope, err)
	}
	// The guard reads this value. A record that names no instance could
	// match no row, and retrying it forever would cost the consumer.
	if payload.Instance == "" {
		return ApplyResult{}, fmt.Errorf("%w: run.resume_failed names no instance", ErrMalformedEnvelope)
	}

	result := ApplyResult{
		ResumeFailedReason:   payload.ReasonCode,
		ResumeFailedInstance: payload.Instance,
	}
	if resumeHeldCodes[payload.ReasonCode] {
		g.sweepMetrics.addResume(ctx, resumeOutcomeHeld, resumeReasonBuildLoaded)
		return result, nil
	}

	// One statement commits on its own, so this write opens no
	// transaction. Every part of the guard earns its place: the primary
	// key, a status that still needs a target, and the instance the
	// record names. The instance clause makes a repeat a no-op, and it
	// protects a newer dispatch from a result an earlier attempt sent.
	tag, err := g.pool.Exec(ctx, `
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb) || jsonb_build_object('resume',
		           ((payload->'mesh'->'resume') - 'instance' - 'requested_at' - 'deadline')
		           || jsonb_build_object('failed',
		                CASE WHEN COALESCE(payload->'mesh'->'resume'->'failed', '[]'::jsonb) @> to_jsonb($3::text)
		                     THEN payload->'mesh'->'resume'->'failed'
		                     ELSE COALESCE(payload->'mesh'->'resume'->'failed', '[]'::jsonb)
		                          || jsonb_build_array($3::text) END)))
		 WHERE run_id = $1 AND created_at = $2 AND status = 'RUNNING'
		   AND payload->'mesh'->'resume'->>'instance' = $3
	`, runID, createdAt, payload.Instance)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("re-arm a failed resume: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return result, nil
	}

	g.sweepMetrics.addResume(ctx, resumeOutcomeFailed, resumeReasonNone)
	result.ResumeFailedCleared = true
	return result, nil
}
