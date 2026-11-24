package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Interrupted-run reasons runs.payload.interrupted_reason holds. The
// confirmed test names cell-lost: the operator read the cell pod and
// found none. The moved test names cell-rolled when the run's stored
// cell equals the tenant's current cell (the same cell's pod rolled),
// and tenant-moved when they differ (the tenant now lives on a
// different cell).
const (
	ReasonCellLost    = "cell-lost"
	ReasonCellRolled  = "cell-rolled"
	ReasonTenantMoved = "tenant-moved"
	ReasonNoHeartbeat = "no-heartbeat"
	// ReasonResumeTimeout names a run the pipeline mesh claimed and could
	// not bring back. One reason covers both windows: in each case the
	// recorder tried to resume the run and the run did not come back.
	ReasonResumeTimeout = "resume-timeout"
	// ReasonResumeFailed names a run whose only home target failed its
	// resume command. No other member can take the run, so the mark
	// comes at once, not when the arm window passes.
	ReasonResumeFailed = "resume-failed"
	// ReasonStartLost names a run whose owner marked the start and whose
	// start event did not reach the store within ClaimExpiryAge. The owner
	// can still be alive, so the mark is provisional: a later run.started
	// supersedes it.
	ReasonStartLost = "start-lost"
	// ReasonHeartbeatLost names a RUNNING run whose heartbeat stopped for
	// liveOwnerSilentWindow while no liveness reading proves its owner
	// gone. The owner can still be alive behind a transport outage, so
	// the mark is provisional: the owner's run.completed replaces it.
	ReasonHeartbeatLost = "heartbeat-lost"
)

// retrySkipHeartbeatLost is recorder_retry_skipped_total's reason for a
// retry of a run marked heartbeat-lost. Its owner can still hold the
// build, and a retry would then run the job twice.
const retrySkipHeartbeatLost = "heartbeat-lost"

// interruptedRetryAutomatic is the Tenant.spec.pipelines.interruptedRetry
// value that makes a mark publish one automatic retry_run action.
const interruptedRetryAutomatic = "automatic"

// retrySkipUnplaced is recorder_retry_skipped_total's reason for a retry
// with no home cell to address.
const retrySkipUnplaced = "unplaced"

// DefaultSweepMaxPending is VARROA_SWEEP_MAX_PENDING's default. The
// interrupted sweep skips its round when the recorder's own
// varroa_builds consumer holds at least this many pending messages.
const DefaultSweepMaxPending = 1000

// pluginHeartbeatInterval is the fixed interval the Jenkins plugin emits
// one run.heartbeat (design D9). The sweep's consumer-freshness gate
// uses it as the newest-processed-event bound.
const pluginHeartbeatInterval = 60 * time.Second

// runInterruptCandidate is one QUEUED or RUNNING run's data the
// interrupted-run tests and the marking write need. status is the
// status the round observed, so the marking write can condition on it
// rather than a literal.
type runInterruptCandidate struct {
	runID               uuid.UUID
	createdAt           time.Time
	cluster             string
	namespace           string
	cell                string
	tenant              string
	jobPath             string
	status              string
	buildNumber         *int64
	actor               string
	params              json.RawMessage
	cellInstance        string
	heartbeatReceivedAt *time.Time
	acceptedAt          *time.Time
	mesh                *runMeshRecord
	// scmRevision and scmURL are the commit and repository of the run,
	// empty when the run recorded neither. The commit-status end needs them.
	scmRevision string
	scmURL      string
	// generationZero is true when the run's intent was granted under
	// placement generation zero, so a controller with no cell role owns
	// the run. A run with no intent answers false.
	generationZero bool
}

// livenessBaseline is the moved and silent tests' anchor: the recorder's
// own receive time for the run's last heartbeat, else its accepted_at
// payload value when it has one, else its creation time. A cell that
// died before a run's first heartbeat is still measured, and a queued
// run that never started is measured from the moment Jenkins accepted
// it.
//
// The anchor is the recorder's stamp, never the pod's. Every caller
// compares this value with a recorder timestamp, so a pod clock would
// add the skew between two machines to each window.
func livenessBaseline(c runInterruptCandidate) time.Time {
	if c.heartbeatReceivedAt != nil {
		return *c.heartbeatReceivedAt
	}
	if c.acceptedAt != nil {
		return *c.acceptedAt
	}
	return c.createdAt
}

// evaluateInterruption applies the mesh guard, then the confirmed test,
// then the moved test, then the silent test, to one QUEUED or RUNNING
// run. It answers the reason the first test that holds names. None of
// these tests fires without a cell-liveness reading naming the run's own
// owning cell instance. A RUNNING run that a controller with no cell role
// owns reads controllerLiveness instead, so only the silent test applies.
// A QUEUED run keeps every test: its build did not start, and the claim
// path moves it when the next pod claims it again.
//
// The heartbeat-lost test applies last, to a RUNNING run alone, and
// needs no liveness reading: no placement row, a live member, and an
// owner outside the member list all reach it. A Jenkins container that
// restarts inside a live pod keeps the pod's cell instance, so only the
// heartbeat gap shows that a build it held is gone.
func evaluateInterruption(now time.Time, c runInterruptCandidate, row tenantPlacementLivenessRow,
	exists bool, grace time.Duration) (reason string, ok bool) {
	if reason, ok, decided := evaluateResumeGuard(now, c); decided {
		return reason, ok
	}
	anchor := livenessBaseline(c)
	reading := cellLivenessGone(now, anchor, c.cellInstance, row, exists, grace)
	if c.generationZero && c.status == StatusRunning {
		reading = controllerLiveness(now, anchor, row, exists)
	}
	switch reading {
	case livenessConfirmedGone:
		return ReasonCellLost, true
	case livenessMoved:
		// cell-rolled applies when the run's stored cell still holds a
		// member of the same slot, so the pod was replaced under the
		// tenant. tenant-moved applies otherwise.
		if home := memberForJob(row.members, c.jobPath); c.cell == home.Cell {
			return ReasonCellRolled, true
		}
		return ReasonTenantMoved, true
	case livenessSilent:
		return ReasonNoHeartbeat, true
	}
	if c.status == StatusRunning && anchor.Before(now.Add(-liveOwnerSilentWindow)) {
		return ReasonHeartbeatLost, true
	}
	return "", false
}

// controllerLiveness is the liveness reading of a run that a controller
// with no cell role owns. The controller holds such a run, not its pod:
// Jenkins on the next pod resumes the build from the controller's volume,
// and that pod takes the run over (adoptReplacementPod). A pod read that
// proves the old pod gone therefore proves no loss, so neither the
// confirmed test nor the moved test applies. The silent test applies to
// the controller's current liveness signal.
func controllerLiveness(now, anchor time.Time, row tenantPlacementLivenessRow, exists bool) livenessReading {
	if exists && anchor.Before(now.Add(-silentHeartbeatWindow)) && currentInstanceGone(row, now, silentHeartbeatWindow) {
		return livenessSilent
	}
	return livenessAlive
}

// provisionalMark reports whether reason names a mark the run's owner can
// still replace. Such a mark deletes no run-state objects, publishes no
// cancel, and publishes no automatic retry.
func provisionalMark(reason string) bool {
	return reason == ReasonHeartbeatLost
}

// endsOpenStages reports whether a mark with reason ends the open stages of
// run. A heartbeat-lost mark and a start-lost mark keep them, because the
// owner can still run them, and a later run.started supersedes a start-lost
// mark. A QUEUED run keeps them too: a later run.started supersedes the mark
// of an accepted row.
func endsOpenStages(run runInterruptCandidate, reason string) bool {
	return run.status == StatusRunning && !provisionalMark(reason) && reason != ReasonStartLost
}

// resumeDispatched reports whether the mesh moved this run's owner to a
// live target. Every reader tests the named instance, never the
// presence of the resume: an armed run holds a resume that names none.
func (c runInterruptCandidate) resumeDispatched() bool {
	return c.mesh != nil && c.mesh.Resume != nil && c.mesh.Resume.Instance != ""
}

// evaluateResumeGuard applies the pipeline mesh's three guard rows
// before every other test. It reports decided=false when the run holds
// no resume, which leaves the ordinary tests to answer.
//
// Row 0a shields an armed run inside its arm window. Row 0b shields a
// dispatched run inside its deadline. Row 0c marks a run whose
// applicable window has passed.
//
// Row 0c applies only while the run's heartbeat receive stamp is null
// or older than the resume's own request time. A resumed build proves
// itself by its ordinary heartbeat, which the plugin emits every 60
// seconds, so the 6-minute deadline gives the target six chances and
// one lost heartbeat never times out a live resume.
//
// The recorder stamps both values, so the test reads one clock. The
// pod's own occurred_at would answer on the skew between two machines.
func evaluateResumeGuard(now time.Time, c runInterruptCandidate) (reason string, ok, decided bool) {
	if c.mesh == nil || c.mesh.Resume == nil {
		return "", false, false
	}
	r := c.mesh.Resume

	if r.Instance == "" {
		if r.ArmExpires != nil && r.ArmExpires.After(now) {
			return "", false, true
		}
		if len(r.Failed) > 0 {
			return ReasonResumeFailed, true, true
		}
		return ReasonResumeTimeout, true, true
	}

	// A heartbeat newer than the request proves the target took the
	// run over, so this resume is complete. The guard then answers
	// nothing and the ordinary liveness tests decide, which is what
	// marks the run if the target itself dies later. The heartbeat
	// ingest clears the resume record for the same reason; this
	// fall-through covers a record the clear has not reached yet.
	if c.heartbeatReceivedAt != nil && r.RequestedAt != nil && c.heartbeatReceivedAt.After(*r.RequestedAt) {
		return "", false, false
	}
	if r.Deadline != nil && r.Deadline.After(now) {
		return "", false, true
	}
	return ReasonResumeTimeout, true, true
}

// scanCandidates reads the QUEUED/RUNNING candidate columns
// evaluateInterruption and markInterrupted need, shared by the periodic
// sweep's query and the announce-triggered query.
func scanCandidates(rows pgx.Rows) ([]runInterruptCandidate, error) {
	defer rows.Close()

	var out []runInterruptCandidate
	for rows.Next() {
		var (
			c              runInterruptCandidate
			owningInstance *string
			actor          *string
			meshRaw        json.RawMessage
		)
		if err := rows.Scan(&c.runID, &c.createdAt, &c.cluster, &c.namespace, &c.cell, &c.tenant, &c.jobPath,
			&c.status, &c.buildNumber, &actor, &c.params, &c.heartbeatReceivedAt, &c.acceptedAt, &owningInstance,
			&meshRaw, &c.scmRevision, &c.scmURL, &c.generationZero); err != nil {
			return nil, fmt.Errorf("scan interrupt candidate: %w", err)
		}
		c.mesh = decodeMeshRecord(meshRaw)
		c.cellInstance = derefString(owningInstance)
		c.actor = derefString(actor)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select interrupt candidates: %w", err)
	}
	return out, nil
}

// candidatesQuery is the column list scanCandidates expects, shared by
// every caller so the two never drift apart. The owning instance is the
// ledger's cell_instance when an intent exists, else the accepted
// trigger's own accepted_instance payload value: a QUEUED run can hold
// no intent yet.
const candidatesQuery = `
	SELECT r.run_id, r.created_at, r.cluster, r.namespace, r.cell, r.tenant, r.job_path,
	       r.status, r.build_number, r.actor, r.params, r.heartbeat_received_at,
	       (r.payload->>'accepted_at')::timestamptz,
	       COALESCE(NULLIF(ri.cell_instance, ''), r.payload->>'accepted_instance'),
	       r.payload->'mesh',
	       COALESCE(r.scm_revision, ''),
	       COALESCE(NULLIF(r.payload->'run.source'->>'scm_url', ''), r.payload->'run.started'->'payload'->>'scm_url', ''),
	       COALESCE(ri.placement_generation = 0, false)
	FROM varroa.runs r
	LEFT JOIN varroa.run_intents ri ON ri.run_id = r.run_id
	WHERE r.status IN ('QUEUED', 'RUNNING')`

// selectCandidates reads every QUEUED or RUNNING run fleet-wide, for the
// periodic sweep.
func selectCandidates(ctx context.Context, pool *pgxpool.Pool) ([]runInterruptCandidate, error) {
	rows, err := pool.Query(ctx, candidatesQuery)
	if err != nil {
		return nil, fmt.Errorf("select interrupt candidates: %w", err)
	}
	return scanCandidates(rows)
}

// selectCandidatesForTenant reads every QUEUED or RUNNING run of one
// cluster and tenant, for the announce-triggered marking.
func selectCandidatesForTenant(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string) ([]runInterruptCandidate, error) {
	rows, err := pool.Query(ctx, candidatesQuery+" AND r.cluster = $1 AND r.tenant = $2", cluster, tenant)
	if err != nil {
		return nil, fmt.Errorf("select interrupt candidates for tenant: %w", err)
	}
	return scanCandidates(rows)
}

// markInterrupted performs the whole marking transaction for one run:
// the terminal write conditioned on status = the status the round
// observed, the log-summary finalize marker, and the build.interrupted
// activity row, all in one transaction. It reports whether the
// conditional update changed the row; a miss means a real terminal
// event already won the race, and the caller counts it in
// recorder_interrupted_skipped_total. It leaves a QUEUED run's accepted
// keys in place: a query that asks whether a real build was ever
// recorded must still answer no for a marked queued run.
//
// The mesh window lives in this write, not in the caller's test: the
// sweep reads its candidates in one query and marks each one later, so
// an arm that commits between the two would otherwise be ignored. The
// clause re-reads the committed record, so a dispatched run answers on
// its deadline and never on its arm window. A jsonb_typeof test covers
// a JSON null; a bare IS NULL test would leave a JSON null matching no
// branch, and the run unmarkable. The test says IS DISTINCT FROM, never
// <>: a null payload and a missing mesh key both make jsonb_typeof
// answer SQL NULL, and <> would then answer NULL for every branch.
//
// The dispatched branch also compares the heartbeat receive stamp
// against the resume's own request time. A target that proved itself
// between the candidate read and this write must not be marked. Both
// values carry the recorder's own clock.
//
// A heartbeat-lost mark re-reads the heartbeat receive stamp the same
// way. That mark rests on the gap alone, so a heartbeat that arrives
// between the candidate read and this write must stop it.
//
// A heartbeat-lost mark also keeps the run's pending input set. Its owner
// can still wait on that input, and no later event restores a set on a
// completed row. The owner's run.completed clears it.
// markInterrupted marks one run without publishing. Production paths call
// markInterruptedAndPublish; this form serves the tests.
//
//nolint:unparam // the tests pass no dashboard URL; production passes one through markInterruptedAndPublish
func markInterrupted(ctx context.Context, pool *pgxpool.Pool, run runInterruptCandidate, reason string, now time.Time,
	dashboardURL string) (bool, error) {
	return markInterruptedAndPublish(ctx, pool, nil, run, reason, now, dashboardURL)
}

// markInterruptedAndPublish marks one run and sends its activity row to
// activityPub after the commit. A nil activityPub writes the row and publishes nothing.
func markInterruptedAndPublish(ctx context.Context, pool *pgxpool.Pool, activityPub *activity.Publisher, run runInterruptCandidate,
	reason string, now time.Time, dashboardURL string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin interrupted-run marking: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out activityOutbox
	changed, err := markInterruptedTx(ctx, tx, run, reason, now, dashboardURL, &out)
	if err != nil || !changed {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit interrupted-run marking: %w", err)
	}
	publishActivityAfterCommit(activityPub, &out)
	return true, nil
}

// markInterruptedTx is markInterrupted's body inside the caller's
// transaction: the conditional terminal write, the end of the open stages,
// and the build.interrupted activity row. The caller commits and then
// publishes out.
//
// A mark that endsOpenStages admits sets each IN_PROGRESS stage of the run
// to ABORTED, with the mark time as its end. The write sets no duration,
// so no stage median counts the row. The stage payload takes the key
// interrupted_reason, which names a stage that the recorder ended. A later
// terminal run.stage fact of the owner replaces the whole row and that key
// (applyRunStage).
//
// A cell-rolled mark also writes the payload key interrupted_detail when
// readCellRollCause finds the roll. Every mark removes a stored detail
// first, so the key always belongs to the stored reason. A failed or slow
// read stores no detail and never stops the mark.
func markInterruptedTx(ctx context.Context, tx pgx.Tx, run runInterruptCandidate, reason string, now time.Time,
	dashboardURL string, out *activityOutbox) (bool, error) {
	detail := ""
	if reason == ReasonCellRolled {
		var err error
		if detail, err = readCellRollCause(ctx, tx, run, now, cellRollCauseReadTimeout, slog.Default()); err != nil {
			return false, err
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE varroa.runs
		SET status = 'COMPLETED',
		    result = 'INTERRUPTED',
		    ended_at = $2,
		    inputs = CASE WHEN $3::text = '`+ReasonHeartbeatLost+`' THEN inputs ELSE NULL END,
		    payload = ((COALESCE(payload, '{}'::jsonb) - 'interrupted_detail')
		               || jsonb_build_object('interrupted_reason', $3::text)
		               || CASE WHEN $6::text = '' THEN '{}'::jsonb
		                       ELSE jsonb_build_object('interrupted_detail', $6::text) END)
		        || jsonb_build_object('log_summary',
		             CASE
		                 WHEN payload -> 'log_summary' IS NOT NULL
		                 THEN COALESCE(payload -> 'log_summary', '{}'::jsonb) || '{"finalized": true, "incomplete": true, "incomplete_reason": "`+logReasonNotFinalized+`"}'::jsonb
		                      || `+logReasonKeepSQL(`payload->'log_summary'->>'incomplete_reason'`, `'`+logReasonNotFinalized+`'`)+`
		                 ELSE '{"finalized": true, "incomplete": true, "incomplete_reason": "no-summary"}'::jsonb
		             END)
		WHERE run_id = $1 AND status = $4
		  AND (
		    jsonb_typeof(payload->'mesh'->'resume') IS DISTINCT FROM 'object'
		    OR (payload->'mesh'->'resume'->>'instance' IS NULL
		        AND (payload->'mesh'->'resume'->>'arm_expires')::timestamptz <= $2)
		    OR (payload->'mesh'->'resume'->>'instance' IS NOT NULL
		        AND (payload->'mesh'->'resume'->>'deadline')::timestamptz <= $2
		        AND (heartbeat_received_at IS NULL
		             OR heartbeat_received_at <= (payload->'mesh'->'resume'->>'requested_at')::timestamptz))
		  )
		  AND ($3::text <> '`+ReasonHeartbeatLost+`'
		       OR heartbeat_received_at IS NULL
		       OR heartbeat_received_at <= $5)
	`, run.runID, now, reason, run.status, now.Add(-liveOwnerSilentWindow), detail)
	if err != nil {
		return false, fmt.Errorf("mark run interrupted: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	if endsOpenStages(run, reason) {
		if _, err := tx.Exec(ctx, `
			UPDATE varroa.run_stages
			SET status = 'ABORTED',
			    ended_at = $2,
			    duration_ms = NULL,
			    payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('interrupted_reason', $3::text)
			WHERE run_id = $1 AND status = 'IN_PROGRESS'
		`, run.runID, now, reason); err != nil {
			return false, fmt.Errorf("end the open stages of the interrupted run: %w", err)
		}
	}

	event := activity.Event{
		Timestamp:   now,
		Type:        "build.interrupted",
		Source:      "recorder",
		Severity:    "warning",
		Cluster:     run.cluster,
		Namespace:   run.namespace,
		Controller:  run.cell,
		Actor:       run.actor,
		Message:     interruptedActivityMessage(run, reason),
		Reason:      reason,
		ItemPath:    run.jobPath,
		BuildNumber: derefInt64(run.buildNumber),
		Result:      "INTERRUPTED",
	}
	if err := insertActivityForPublish(ctx, tx, event, out); err != nil {
		return false, fmt.Errorf("insert interrupted activity row: %w", err)
	}
	// The synthetic delivery shares this transaction, so a rolled-back
	// mark enqueues nothing.
	if _, err := enqueueInterruptedWebhookDelivery(ctx, tx, run, reason, now, dashboardURL); err != nil {
		return false, err
	}
	return true, nil
}

// interruptedActivityMessage renders the build.interrupted activity
// row's message.
func interruptedActivityMessage(run runInterruptCandidate, reason string) string {
	if run.buildNumber != nil {
		return fmt.Sprintf("%s #%d was interrupted (%s)", run.jobPath, *run.buildNumber, reason)
	}
	return fmt.Sprintf("%s was interrupted (%s)", run.jobPath, reason)
}

// cellRolledActivityType is the activity type that the operator writes when
// its cell readiness rule deletes a cell pod.
const cellRolledActivityType = "cell.rolled"

// cellRollCauseShape admits one condition reason word, such as
// MiteUnreachable. A tenant reader reads the stored detail, so the mark
// never copies the free-text message of the row. That message can hold a
// raw presence read error, and the operator can change its text.
var cellRollCauseShape = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,63}$`)

// cellRollCauseReadTimeout bounds the cell.rolled read of a cell-rolled
// mark. The detail is not needed to end the run, so a slow activity table
// must not hold the mark or the locks that its transaction already holds.
const cellRollCauseReadTimeout = 2 * time.Second

// readCellRollCause answers the reason word of the oldest cell.rolled
// activity row of the run's cell, from after the run's liveness baseline to
// the mark time. The first roll after the run's last sign of life removed
// the pod of the run. A later roll in the window removed a replacement pod.
// It answers "" when no row matches or the reason word has another shape.
//
// The row comes from the operator through the activity stream, so it can
// arrive after the mark. A later row fills nothing: the detail is a record
// of what the recorder knew when it ended the run.
//
// The read runs in a savepoint with a local statement timeout. A failed
// statement aborts the whole Postgres transaction, so the read always rolls
// back to the savepoint, and that rollback also removes the local timeout.
// A read that fails or passes the timeout logs one Warn line and answers
// "", so the mark goes on. The bound is on the server, not on ctx: pgx
// closes the connection when a context deadline passes, and that ends the
// mark's transaction. The function answers an error only when the
// savepoint itself fails, because the transaction is then unusable.
func readCellRollCause(ctx context.Context, tx pgx.Tx, run runInterruptCandidate, now time.Time,
	timeout time.Duration, logger *slog.Logger) (string, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("open the cell roll cause savepoint: %w", err)
	}
	cause, readErr := queryCellRollCause(ctx, sp, run, now, timeout)
	if err := sp.Rollback(ctx); err != nil {
		return "", fmt.Errorf("roll back the cell roll cause savepoint: %w", err)
	}
	if readErr != nil {
		logger.Warn("read cell roll cause failed; the mark stores no detail", "run_id", run.runID, "error", readErr)
		return "", nil
	}
	if !cellRollCauseShape.MatchString(cause) {
		return "", nil
	}
	return cause, nil
}

// queryCellRollCause sets the local statement timeout and reads the reason
// word in the caller's savepoint. No row answers "" and no error.
func queryCellRollCause(ctx context.Context, sp pgx.Tx, run runInterruptCandidate, now time.Time,
	timeout time.Duration) (string, error) {
	if _, err := sp.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())); err != nil {
		return "", fmt.Errorf("set the cell roll cause read timeout: %w", err)
	}
	var cause string
	err := sp.QueryRow(ctx, `
		SELECT reason FROM varroa.activity
		WHERE type = $1 AND cluster = $2 AND namespace = $3 AND controller = $4
		  AND ts > $5 AND ts <= $6
		ORDER BY ts ASC
		LIMIT 1
	`, cellRolledActivityType, run.cluster, run.namespace, run.cell, livenessBaseline(run), now).Scan(&cause)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read cell roll cause: %w", err)
	}
	return cause, nil
}

// policyInterruptedRetry decodes the one field of a tenant_placements
// policy column interrupted-run marking needs. An empty or malformed
// policy answers the empty string, which never equals
// interruptedRetryAutomatic.
type policyInterruptedRetry struct {
	InterruptedRetry string `json:"interruptedRetry"`
}

func decodeInterruptedRetry(policy json.RawMessage) string {
	if len(policy) == 0 {
		return ""
	}
	var p policyInterruptedRetry
	if err := json.Unmarshal(policy, &p); err != nil {
		return ""
	}
	return p.InterruptedRetry
}

// afterMark publishes the actions a successful mark triggers: one
// end_commit_status for a run with a recorded commit, one cancel_run for
// the silent reason alone, and one automatic retry_run when the tenant's
// policy calls for it. The retry names a new build number, so its own
// status never overlaps the ended one. None of
// these publishes is
// transactional with markInterrupted's write: a lost action produces no
// retry and no cancel, per design D14, and the recorder never
// republishes.
//
// The cancel belongs to ReasonNoHeartbeat and ReasonResumeTimeout. A
// ReasonCellLost mark proves the pod gone, so no process remains to
// stop and the cell that would receive the command no longer runs. A
// moved reason names an owning instance that already changed, so its
// process died with its pod.
//
// A dispatched resume moved the owner to a live target, so a process
// remains to stop. An undispatched arm still names the dead instance,
// so it takes the existing "owner already changed" arm and publishes
// no cancel.
//
// A marked QUEUED run publishes neither action: no build ever started,
// so there is nothing to cancel, and the store holds no parameters to
// retry with.
//
// A provisional mark publishes neither action either. Its owner can
// still hold the build, so a cancel would stop a live build and a retry
// would run the job twice. An automatic retry it skips is counted.
//
// The cancel names the run's own cell, which holds the process to stop.
// The retry names home, the job's home member (retryAddress). The caller
// answers home through jobHomeOf over the placement its mark decision
// read, so the mark and the retry read one placement.
func afterMark(ctx context.Context, conn *bus.Conn, home JobHome,
	metrics *InterruptedMetrics, actionMetrics *ActionMetrics,
	run runInterruptCandidate, reason, interruptedRetry string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if run.status == StatusQueued {
		return
	}

	if reason == ReasonResumeTimeout || reason == ReasonResumeFailed {
		metrics.addResumeTimeout(ctx)
	}

	if provisionalMark(reason) {
		if interruptedRetry == interruptedRetryAutomatic {
			metrics.addRetrySkipped(ctx, retrySkipHeartbeatLost)
		}
		return
	}

	publishEndCommitStatus(ctx, conn, actionMetrics, run, reason, logger)

	if reason == ReasonNoHeartbeat || (reason == ReasonResumeTimeout && run.resumeDispatched()) {
		action := LedgerAction{
			Kind:        LedgerActionCancelRun,
			Cluster:     run.cluster,
			Namespace:   run.namespace,
			Controller:  run.cell,
			Actor:       LedgerActionRecorder,
			RunID:       run.runID.String(),
			JobPath:     run.jobPath,
			BuildNumber: run.buildNumber,
		}
		if err := PublishLedgerAction(conn, action); err != nil {
			logger.Warn("publish cancel_run action failed", "run_id", run.runID, "error", err)
		} else {
			metrics.addCancel(ctx)
			actionMetrics.addPublished(ctx, action.Kind)
		}
	}

	if interruptedRetry == interruptedRetryAutomatic {
		retryID, err := uuid.NewV7()
		if err != nil {
			logger.Warn("generate automatic retry run id failed", "run_id", run.runID, "error", err)
			return
		}
		namespace, cell, ok := retryAddress(ctx, home, metrics, run, logger)
		if !ok {
			return
		}
		action := LedgerAction{
			Kind:       LedgerActionRetryRun,
			Cluster:    run.cluster,
			Namespace:  namespace,
			Controller: cell,
			Actor:      run.actor,
			RunID:      retryID.String(),
			IntentKey:  "retry:" + run.runID.String(),
			JobPath:    run.jobPath,
			Parameters: run.params,
		}
		if err := PublishLedgerAction(conn, action); err != nil {
			logger.Warn("publish retry_run action failed", "run_id", run.runID, "error", err)
			return
		}
		metrics.addRetryPublished(ctx)
		actionMetrics.addPublished(ctx, action.Kind)
	}
}

// publishEndCommitStatus asks the operator to end the pending commit
// statuses of the build. It publishes nothing when the run recorded no
// commit or no repository, or when it has no build number, because the
// operator cannot find the status without them. The caller runs it only
// after a mark that is not provisional, so a live owner's status is never
// ended early.
func publishEndCommitStatus(ctx context.Context, conn *bus.Conn, actionMetrics *ActionMetrics,
	run runInterruptCandidate, reason string, logger *slog.Logger) {
	if run.scmRevision == "" || run.scmURL == "" || run.buildNumber == nil {
		return
	}
	action := LedgerAction{
		Kind:        LedgerActionEndCommitStatus,
		Cluster:     run.cluster,
		Namespace:   run.namespace,
		Controller:  run.cell,
		Actor:       LedgerActionRecorder,
		RunID:       run.runID.String(),
		JobPath:     run.jobPath,
		BuildNumber: run.buildNumber,
		Tenant:      run.tenant,
		Repo:        run.scmURL,
		Revision:    run.scmRevision,
		Reason:      reason,
	}
	if err := PublishLedgerAction(conn, action); err != nil {
		logger.Warn("publish end_commit_status action failed", "run_id", run.runID, "error", err)
		return
	}
	actionMetrics.addPublished(ctx, action.Kind)
}

// retryAddress answers the namespace and cell an automatic retry of run
// names. For a placed tenant, they are home, the member of the job's home
// slot, because the claim fence refuses a claim from any other cell. A
// tenant with no placement row keeps the run's own coordinate. A home
// slot with no cell answers ok false and counts one unplaced skip: the
// recorder never republishes a retry.
func retryAddress(ctx context.Context, home JobHome, metrics *InterruptedMetrics,
	run runInterruptCandidate, logger *slog.Logger) (namespace, cell string, ok bool) {
	if !home.Placed {
		return run.namespace, run.cell, true
	}
	if home.Cell == "" {
		logger.Warn("automatic retry skipped: the job's home slot holds no cell",
			"run_id", run.runID, "job_path", run.jobPath)
		metrics.addRetrySkipped(ctx, retrySkipUnplaced)
		return "", "", false
	}
	return home.Namespace, home.Cell, true
}

// deleteRunState removes a marked run's replicated state, after the
// marking transaction commits and never inside it: a lost object delete
// must never roll the record back. The run keeps its build.xml, because a
// marked run is terminal and a replay reads that file. It reads the run's
// committed mesh file list, never the candidate's earlier copy, so a push
// that landed before the mark is deleted too. A nil deleter skips the
// delete, the same rule the log store follows.
func deleteRunState(ctx context.Context, pool *pgxpool.Pool, store ObjectDeleter, run runInterruptCandidate, logger *slog.Logger) {
	if store == nil {
		return
	}
	runID := run.runID.String()
	files := []runStateFile{}
	var raw []byte
	err := pool.QueryRow(ctx, `
		SELECT payload->'mesh'->'files' FROM varroa.runs WHERE run_id = $1 AND created_at = $2
	`, run.runID, run.createdAt).Scan(&raw)
	switch {
	case err == nil:
		files = runStateFilesOrEmpty(raw)
	case !errors.Is(err, pgx.ErrNoRows):
		logger.Warn("read run-state file list after marking failed", "run_id", runID, "error", err)
		return
	}
	if err := deleteRunStateExceptScript(ctx, store, runID, files); err != nil {
		logger.Warn("delete run state after marking failed", "run_id", runID, "error", err)
	}
}

// markedStatusLabel maps a candidate's status to the "status" attribute
// recorder_interrupted_marked_total carries: "queued" or "running".
func markedStatusLabel(status string) string {
	if status == StatusQueued {
		return "queued"
	}
	return "running"
}

// MarkMovedRunsForTenant runs the confirmed and moved tests, for every
// QUEUED or RUNNING run of cluster/tenant, immediately after a
// tenant.sync announce changes cell or cell_instance. row is the
// placement row the announce just wrote. It answers how many runs it
// marked, for the TenantSyncResponse InterruptedMarked field. The sweep
// marks the rest as their heartbeats age: this is the fast path design
// D9 gives a cell roll, not a separate rule, so the silent test never
// fires here, and neither does the heartbeat-lost test. The confirmed
// test does fire here, because a pod read proves the loss and needs no
// heartbeat window.
//
// Each mark sends one run notification through notifier after its
// commit. The notification names the runs row that the marking read. A
// nil notifier publishes nothing.
func MarkMovedRunsForTenant(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, activityPub *activity.Publisher, notifier *RunNotifier,
	metrics *InterruptedMetrics, actionMetrics *ActionMetrics, stateDeleter ObjectDeleter, cluster, tenant string,
	row tenantPlacementLivenessRow, now time.Time, grace time.Duration, dashboardURL string, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	candidates, err := selectCandidatesForTenant(ctx, pool, cluster, tenant)
	if err != nil {
		return 0, err
	}

	interruptedRetry := decodeInterruptedRetry(row.policy)
	marked := 0
	for _, c := range candidates {
		if c.cellInstance == "" {
			metrics.addSweepSkipped(ctx, "no-owning-instance")
			continue
		}
		reason, ok := evaluateInterruption(now, c, row, true, grace)
		if !ok || reason == ReasonNoHeartbeat || reason == ReasonHeartbeatLost {
			continue
		}
		changed, err := markInterruptedAndPublish(ctx, pool, activityPub, c, reason, now, dashboardURL)
		if err != nil {
			logger.Warn("announce-triggered interrupted marking failed", "run_id", c.runID, "error", err)
			continue
		}
		if !changed {
			metrics.addSkipped(ctx)
			continue
		}
		metrics.addMarked(ctx, "announce", markedStatusLabel(c.status))
		notifier.Notify(RunNotification{
			Kind:    "run",
			Cluster: c.cluster,
			Tenant:  c.tenant,
			JobPath: c.jobPath,
			RunID:   c.runID.String(),
			Status:  StatusCompleted,
			Result:  "INTERRUPTED",
		})
		if reason == ReasonCellLost {
			metrics.addCellLoss(ctx, "announce")
		}
		deleteRunState(ctx, pool, stateDeleter, c, logger)
		afterMark(ctx, conn, jobHomeOf(row.members, true, c.jobPath), metrics, actionMetrics, c, reason,
			interruptedRetry, logger)
		releaseRunAgentsAfterMark(ctx, pool, conn, actionMetrics, c, reason, logger)
		marked++
	}
	return marked, nil
}

// sweepConsumerSkip answers whether the recorder's own varroa_builds
// consumer is too far behind for the sweep to trust a run's heartbeat
// age, and names the reason. A heartbeat reaches runs.heartbeat_received_at
// through that consumer, so a backlog makes a live run look silent.
//
// The gate tests a BACKLOG, never quiet. A consumer with no pending
// message has processed everything the stream holds, so an old or
// absent AckFloor.Last means only that no build event arrived lately.
// A quiet tenant is exactly the case the sweep must still serve: with
// no other build running, nothing would ever refresh the floor, and a
// lost cell's runs would wait for a build that never comes.
//
// The floor age therefore applies only while the consumer holds work.
func sweepConsumerSkip(info *nats.ConsumerInfo, maxPending uint64, now time.Time) (reason string, skip bool) {
	if info.NumPending >= maxPending {
		return "pending", true
	}
	if info.NumPending == 0 {
		return "", false
	}
	if info.AckFloor.Last == nil || now.Sub(*info.AckFloor.Last) >= pluginHeartbeatInterval {
		return "stale", true
	}
	return "", false
}

// SweepInterruptedRuns runs one round of the interrupted-run sweep
// (design D9, task 9.6a's gate): it checks the recorder's own
// varroa_builds consumer for a backlog (sweepConsumerSkip) before it
// reads any run, then applies the confirmed, moved, and silent tests to
// every QUEUED or RUNNING run fleet-wide, marking and publishing for
// each one that qualifies. It answers how many runs it marked.
//
// Each mark sends one run notification through notifier after its
// commit. The notification names the runs row that the sweep read. A nil
// notifier publishes nothing.
func SweepInterruptedRuns(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, activityPub *activity.Publisher, notifier *RunNotifier,
	metrics *InterruptedMetrics, actionMetrics *ActionMetrics, stateDeleter ObjectDeleter, maxPending uint64, now time.Time,
	grace time.Duration, dashboardURL string, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	info, err := conn.ConsumerInfo(bus.BuildsStreamName, RecorderConsumerName)
	if err != nil {
		return 0, fmt.Errorf("read varroa_builds consumer info: %w", err)
	}
	if reason, skip := sweepConsumerSkip(info, maxPending, now); skip {
		metrics.addSweepSkipped(ctx, reason)
		return 0, nil
	}

	candidates, err := selectCandidates(ctx, pool)
	if err != nil {
		return 0, err
	}

	marked := 0
	for _, c := range candidates {
		if c.cellInstance == "" {
			metrics.addSweepSkipped(ctx, "no-owning-instance")
			continue
		}
		row, exists, err := readTenantPlacementLivenessRow(ctx, pool, c.cluster, c.tenant)
		if err != nil {
			logger.Warn("interrupted sweep: read tenant placement failed", "run_id", c.runID, "error", err)
			continue
		}
		reason, ok := evaluateInterruption(now, c, row, exists, grace)
		if !ok {
			continue
		}
		changed, err := markInterruptedAndPublish(ctx, pool, activityPub, c, reason, now, dashboardURL)
		if err != nil {
			logger.Warn("interrupted sweep: mark failed", "run_id", c.runID, "error", err)
			continue
		}
		if !changed {
			metrics.addSkipped(ctx)
			continue
		}
		metrics.addMarked(ctx, "sweep", markedStatusLabel(c.status))
		notifier.Notify(RunNotification{
			Kind:    "run",
			Cluster: c.cluster,
			Tenant:  c.tenant,
			JobPath: c.jobPath,
			RunID:   c.runID.String(),
			Status:  StatusCompleted,
			Result:  "INTERRUPTED",
		})
		if reason == ReasonCellLost {
			metrics.addCellLoss(ctx, "sweep")
		}
		if !provisionalMark(reason) {
			deleteRunState(ctx, pool, stateDeleter, c, logger)
		}
		afterMark(ctx, conn, jobHomeOf(row.members, exists, c.jobPath), metrics, actionMetrics, c, reason,
			decodeInterruptedRetry(row.policy), logger)
		releaseRunAgentsAfterMark(ctx, pool, conn, actionMetrics, c, reason, logger)
		marked++
	}
	return marked, nil
}
