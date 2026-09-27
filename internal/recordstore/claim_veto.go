package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// SetVetoMarking wires the veto mark: after a permanent veto of a
// Varroa-dispatched run, the claim responder marks that run
// TRIGGER_FAILED at once. notifier publishes the change and may be nil.
// cmd/recorder calls this once, after NewLedger. A Ledger this method
// never touches marks nothing, which every test that only exercises
// Claim relies on.
func (l *Ledger) SetVetoMarking(notifier *RunNotifier) {
	l.vetoMarking = true
	l.vetoNotifier = notifier
}

// vetoedRunMessage is the trigger_error text of a veto that carries no
// message of its own.
const vetoedRunMessage = "the record store refused the build claim"

// markVetoedRun records a permanent veto on the requesting run's own
// row, in its own transaction, after the claim transaction rolled back.
// It reports whether it wrote the row.
//
// It marks only a veto the plugin cancels on (markableVeto), and only for
// a claim with origin VARROA_INTENT: a Jenkins-native trigger has no row
// that waits for its claim, and a synthetic row would record a build no
// Varroa path asked for. It always names req.RunID. A FENCED answer from
// the intent table names the stored run in resp.RunID, which is not the
// refused run.
//
// One upsert covers both orders of the race with run.trigger_accepted.
// With no row yet, the insert writes the failed row, and the later
// accepted insert keeps it through its own ON CONFLICT DO NOTHING. With
// the QUEUED row already stored, the update marks it. Both halves refuse
// a run that holds a ledger intent, a build number, a mesh resume
// record, or any status but QUEUED. A later run.started supersedes the
// row through the ordinary TRIGGER_FAILED supersede.
func (l *Ledger) markVetoedRun(ctx context.Context, req ClaimRequest, resp ClaimResponse) (bool, error) {
	if !l.vetoMarking || l.pool == nil {
		return false, nil
	}
	if resp.Decision != DecisionVetoed || !markableVeto(resp.ReasonCode) {
		return false, nil
	}
	if req.Origin != ClaimOriginVarroaIntent {
		return false, nil
	}
	runID, createdAt, err := parseRunUUIDv7(req.RunID)
	if err != nil {
		return false, nil
	}

	message := resp.Message
	if message == "" {
		message = fmt.Sprintf("%s (%s)", vetoedRunMessage, resp.ReasonCode)
	}
	now := time.Now().UTC()

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin veto marking: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs AS r (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, ended_at, actor, payload
		)
		SELECT $1::uuid, $2::timestamptz, $3::text, $4::text, $5::text, $6::text, $7::text, $8::text,
			'COMPLETED', '`+ResultTriggerFailed+`', $9::timestamptz, $10::text,
			jsonb_build_object(
				'trigger_error', $11::text,
				'trigger_reason_code', $12::text,
				'trigger_failed_at', $9::timestamptz)
		WHERE NOT EXISTS (SELECT 1 FROM varroa.run_intents ri WHERE ri.run_id = $1)
		ON CONFLICT (created_at, run_id) DO UPDATE
		SET status = 'COMPLETED',
		    result = '`+ResultTriggerFailed+`',
		    ended_at = EXCLUDED.ended_at,
		    payload = COALESCE(r.payload, '{}'::jsonb) || jsonb_build_object(
		        'trigger_error', $11::text,
		        'trigger_reason_code', $12::text,
		        'trigger_failed_at', $9::timestamptz)
		WHERE `+unclaimedQueuedRunSQL,
		runID, createdAt, req.IntentKey, req.Cluster, req.Namespace, req.Cell, req.Tenant, req.JobPath,
		now, nullableString(req.Actor), message, resp.ReasonCode)
	if err != nil {
		return false, fmt.Errorf("mark vetoed run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	event := activity.Event{
		Timestamp:  now,
		Type:       "build.claim_vetoed",
		Source:     "recorder",
		Severity:   "warning",
		Cluster:    req.Cluster,
		Namespace:  req.Namespace,
		Controller: req.Cell,
		Actor:      req.Actor,
		// The run id keeps the event key unique. Two vetoed runs of one
		// job can share the time, the cell, and an empty build number.
		Message: fmt.Sprintf("%s run %s was refused a build claim and was marked failed (%s)",
			req.JobPath, runID, resp.ReasonCode),
		Reason:   resp.ReasonCode,
		ItemPath: req.JobPath,
		Result:   ResultTriggerFailed,
	}
	if err := insertRunActivityTx(ctx, tx, event); err != nil {
		return false, fmt.Errorf("veto activity row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit veto marking: %w", err)
	}

	l.vetoNotifier.Notify(RunNotification{
		Kind:    "run",
		Cluster: req.Cluster,
		Tenant:  req.Tenant,
		JobPath: req.JobPath,
		RunID:   runID.String(),
		Status:  StatusCompleted,
		Result:  ResultTriggerFailed,
	})
	return true, nil
}
