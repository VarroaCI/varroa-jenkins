package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
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
)

// interruptedRetryAutomatic is the Team.spec.pipelines.interruptedRetry
// value that makes a mark publish one automatic retry_run action.
const interruptedRetryAutomatic = "automatic"

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
	runID           uuid.UUID
	createdAt       time.Time
	cluster         string
	namespace       string
	cell            string
	tenant          string
	jobPath         string
	status          string
	buildNumber     *int64
	actor           string
	params          json.RawMessage
	cellInstance    string
	lastHeartbeatAt *time.Time
	acceptedAt      *time.Time
	mesh            *runMeshRecord
}

// livenessBaseline is the moved and silent tests' anchor: the run's own
// last heartbeat when it has sent one, else its accepted_at payload
// value when it has one, else its creation time. A cell that died
// before a run's first heartbeat is still measured, and a queued run
// that never started is measured from the moment Jenkins accepted it.
func livenessBaseline(c runInterruptCandidate) time.Time {
	if c.lastHeartbeatAt != nil {
		return *c.lastHeartbeatAt
	}
	if c.acceptedAt != nil {
		return *c.acceptedAt
	}
	return c.createdAt
}

// evaluateInterruption applies the mesh guard, then the confirmed test,
// then the moved test, then the silent test, to one QUEUED or RUNNING
// run. It answers the reason the first test that holds names. No test
// fires without a cell-liveness reading naming the run's own owning cell
// instance.
func evaluateInterruption(now time.Time, c runInterruptCandidate, row tenantPlacementLivenessRow,
	exists bool, grace time.Duration) (reason string, ok bool) {
	if reason, ok, decided := evaluateResumeGuard(now, c); decided {
		return reason, ok
	}
	anchor := livenessBaseline(c)
	switch cellLivenessGone(now, anchor, c.cellInstance, row, exists, grace) {
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
	default:
		return "", false
	}
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
// Row 0c applies only while the run's last heartbeat is null or older
// than the resume's own request time. A resumed build proves itself by
// its ordinary heartbeat, which the plugin emits every 60 seconds, so
// the 6-minute deadline gives the target six chances and one lost
// heartbeat never times out a live resume.
func evaluateResumeGuard(now time.Time, c runInterruptCandidate) (reason string, ok, decided bool) {
	if c.mesh == nil || c.mesh.Resume == nil {
		return "", false, false
	}
	r := c.mesh.Resume

	if r.Instance == "" {
		if r.ArmExpires != nil && r.ArmExpires.After(now) {
			return "", false, true
		}
		return ReasonResumeTimeout, true, true
	}

	// A heartbeat newer than the request proves the target took the
	// run over, so this resume is complete. The guard then answers
	// nothing and the ordinary liveness tests decide, which is what
	// marks the run if the target itself dies later. The heartbeat
	// ingest clears the resume record for the same reason; this
	// fall-through covers a record the clear has not reached yet.
	if c.lastHeartbeatAt != nil && r.RequestedAt != nil && c.lastHeartbeatAt.After(*r.RequestedAt) {
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
			&c.status, &c.buildNumber, &actor, &c.params, &c.lastHeartbeatAt, &c.acceptedAt, &owningInstance,
			&meshRaw); err != nil {
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
	       r.status, r.build_number, r.actor, r.params, r.last_heartbeat_at,
	       (r.payload->>'accepted_at')::timestamptz,
	       COALESCE(NULLIF(ri.cell_instance, ''), r.payload->>'accepted_instance'),
	       r.payload->'mesh'
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
// The dispatched branch also compares the heartbeat against the
// resume's own request time. A target that proved itself between the
// candidate read and this write must not be marked.
func markInterrupted(ctx context.Context, pool *pgxpool.Pool, run runInterruptCandidate, reason string, now time.Time) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin interrupted-run marking: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE varroa.runs
		SET status = 'COMPLETED',
		    result = 'INTERRUPTED',
		    ended_at = $2,
		    payload = (COALESCE(payload, '{}'::jsonb) || jsonb_build_object('interrupted_reason', $3::text))
		        || jsonb_build_object('log_summary',
		             CASE
		                 WHEN payload -> 'log_summary' IS NOT NULL
		                 THEN COALESCE(payload -> 'log_summary', '{}'::jsonb) || '{"finalized": true, "incomplete": true, "incomplete_reason": "not-finalized"}'::jsonb
		                 ELSE '{"finalized": true, "incomplete": true, "incomplete_reason": "no-summary"}'::jsonb
		             END)
		WHERE run_id = $1 AND status = $4
		  AND (
		    jsonb_typeof(payload->'mesh'->'resume') IS DISTINCT FROM 'object'
		    OR (payload->'mesh'->'resume'->>'instance' IS NULL
		        AND (payload->'mesh'->'resume'->>'arm_expires')::timestamptz <= $2)
		    OR (payload->'mesh'->'resume'->>'instance' IS NOT NULL
		        AND (payload->'mesh'->'resume'->>'deadline')::timestamptz <= $2
		        AND (last_heartbeat_at IS NULL
		             OR last_heartbeat_at <= (payload->'mesh'->'resume'->>'requested_at')::timestamptz))
		  )
	`, run.runID, now, reason, run.status)
	if err != nil {
		return false, fmt.Errorf("mark run interrupted: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
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
	event.Normalize()
	payloadJSON, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("marshal interrupted activity payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.activity
			(event_key, ts, type, source, severity, cluster, namespace, controller,
			 actor, message, phase, reason, item_path, build_number, result, url, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (ts, event_key) DO NOTHING
	`,
		EventKey(event), event.Timestamp, event.Type, event.Source, event.Severity, event.Cluster, event.Namespace,
		event.Controller, event.Actor, event.Message, event.Phase, event.Reason, event.ItemPath,
		nullableBuildNumber(event.BuildNumber), event.Result, event.URL, string(payloadJSON),
	); err != nil {
		return false, fmt.Errorf("insert interrupted activity row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit interrupted-run marking: %w", err)
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
// cancel_run for the silent reason alone, and one automatic retry_run
// when the tenant's policy calls for it. Neither publish is
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
func afterMark(ctx context.Context, conn *bus.Conn, metrics *InterruptedMetrics, actionMetrics *ActionMetrics,
	run runInterruptCandidate, reason, interruptedRetry string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if run.status == StatusQueued {
		return
	}

	if reason == ReasonResumeTimeout {
		metrics.addResumeTimeout(ctx)
	}

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
		action := LedgerAction{
			Kind:       LedgerActionRetryRun,
			Cluster:    run.cluster,
			Namespace:  run.namespace,
			Controller: run.cell,
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

// deleteRunState removes a marked run's replicated state, after the
// marking transaction commits and never inside it: a lost object delete
// must never roll the record back. A nil deleter skips the delete, the
// same rule the log store follows.
func deleteRunState(ctx context.Context, store ObjectDeleter, runID string, logger *slog.Logger) {
	if store == nil {
		return
	}
	if err := store.DeletePrefix(ctx, blob.RunStatePrefix(runID)); err != nil {
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
// fires here. The confirmed test does fire here, because a pod read
// proves the loss and needs no heartbeat window.
func MarkMovedRunsForTenant(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, stateDeleter ObjectDeleter, cluster, tenant string,
	row tenantPlacementLivenessRow, now time.Time, grace time.Duration, logger *slog.Logger) (int, error) {
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
		if !ok || reason == ReasonNoHeartbeat {
			continue
		}
		changed, err := markInterrupted(ctx, pool, c, reason, now)
		if err != nil {
			logger.Warn("announce-triggered interrupted marking failed", "run_id", c.runID, "error", err)
			continue
		}
		if !changed {
			metrics.addSkipped(ctx)
			continue
		}
		metrics.addMarked(ctx, "announce", markedStatusLabel(c.status))
		if reason == ReasonCellLost {
			metrics.addCellLoss(ctx, "announce")
		}
		deleteRunState(ctx, stateDeleter, c.runID.String(), logger)
		afterMark(ctx, conn, metrics, actionMetrics, c, reason, interruptedRetry, logger)
		marked++
	}
	return marked, nil
}

// sweepConsumerSkip answers whether the recorder's own varroa_builds
// consumer is too far behind for the sweep to trust a run's heartbeat
// age, and names the reason. A heartbeat reaches runs.last_heartbeat_at
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
func SweepInterruptedRuns(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, stateDeleter ObjectDeleter, maxPending uint64, now time.Time,
	grace time.Duration, logger *slog.Logger) (int, error) {
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
		changed, err := markInterrupted(ctx, pool, c, reason, now)
		if err != nil {
			logger.Warn("interrupted sweep: mark failed", "run_id", c.runID, "error", err)
			continue
		}
		if !changed {
			metrics.addSkipped(ctx)
			continue
		}
		metrics.addMarked(ctx, "sweep", markedStatusLabel(c.status))
		if reason == ReasonCellLost {
			metrics.addCellLoss(ctx, "sweep")
		}
		deleteRunState(ctx, stateDeleter, c.runID.String(), logger)
		afterMark(ctx, conn, metrics, actionMetrics, c, reason, decodeInterruptedRetry(row.policy), logger)
		marked++
	}
	return marked, nil
}
