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

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Interrupted-run reasons runs.payload.interrupted_reason holds. The
// moved test names cell-rolled when the run's stored cell equals the
// tenant's current cell (the same cell's pod rolled), and tenant-moved
// when they differ (the tenant now lives on a different cell).
const (
	ReasonCellRolled  = "cell-rolled"
	ReasonTenantMoved = "tenant-moved"
	ReasonNoHeartbeat = "no-heartbeat"
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

// runInterruptCandidate is one RUNNING run's data the interrupted-run
// tests and the marking write need.
type runInterruptCandidate struct {
	runID           uuid.UUID
	createdAt       time.Time
	cluster         string
	namespace       string
	cell            string
	tenant          string
	jobPath         string
	buildNumber     *int64
	actor           string
	params          json.RawMessage
	cellInstance    string
	lastHeartbeatAt *time.Time
}

// livenessBaseline is the moved and silent tests' anchor: the run's own
// last heartbeat when it has sent one, else its creation time. A cell
// that died before a run's first heartbeat is still measured.
func livenessBaseline(c runInterruptCandidate) time.Time {
	if c.lastHeartbeatAt != nil {
		return *c.lastHeartbeatAt
	}
	return c.createdAt
}

// evaluateInterruption applies the moved test, then the silent test, to
// one RUNNING run. It answers the reason the first test that holds
// names; the moved test wins when both would hold, because it runs
// first. Neither test fires without a cell-liveness reading naming the
// run's own owning cell instance.
func evaluateInterruption(now time.Time, c runInterruptCandidate, row tenantPlacementLivenessRow, exists bool) (reason string, ok bool) {
	anchor := livenessBaseline(c)
	if !cellLivenessGone(now, anchor, c.cellInstance, row, exists) {
		return "", false
	}
	if c.cellInstance != row.cellInstance {
		if c.cell == row.cell {
			return ReasonCellRolled, true
		}
		return ReasonTenantMoved, true
	}
	return ReasonNoHeartbeat, true
}

// scanRunningCandidates reads the running-run columns evaluateInterruption
// and markInterrupted need, shared by the periodic sweep's query and the
// announce-triggered query.
func scanRunningCandidates(rows pgx.Rows) ([]runInterruptCandidate, error) {
	defer rows.Close()

	var out []runInterruptCandidate
	for rows.Next() {
		var (
			c            runInterruptCandidate
			cellInstance *string
			actor        *string
		)
		if err := rows.Scan(&c.runID, &c.createdAt, &c.cluster, &c.namespace, &c.cell, &c.tenant, &c.jobPath,
			&c.buildNumber, &actor, &c.params, &c.lastHeartbeatAt, &cellInstance); err != nil {
			return nil, fmt.Errorf("scan running candidate: %w", err)
		}
		c.cellInstance = derefString(cellInstance)
		c.actor = derefString(actor)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select running candidates: %w", err)
	}
	return out, nil
}

// runningCandidatesQuery is the column list scanRunningCandidates
// expects, shared by every caller so the two never drift apart.
const runningCandidatesQuery = `
	SELECT r.run_id, r.created_at, r.cluster, r.namespace, r.cell, r.tenant, r.job_path,
	       r.build_number, r.actor, r.params, r.last_heartbeat_at, ri.cell_instance
	FROM varroa.runs r
	LEFT JOIN varroa.run_intents ri ON ri.run_id = r.run_id
	WHERE r.status = 'RUNNING'`

// selectRunningCandidates reads every RUNNING run fleet-wide, for the
// periodic sweep.
func selectRunningCandidates(ctx context.Context, pool *pgxpool.Pool) ([]runInterruptCandidate, error) {
	rows, err := pool.Query(ctx, runningCandidatesQuery)
	if err != nil {
		return nil, fmt.Errorf("select running candidates: %w", err)
	}
	return scanRunningCandidates(rows)
}

// selectRunningCandidatesForTenant reads every RUNNING run of one
// cluster and tenant, for the announce-triggered marking.
func selectRunningCandidatesForTenant(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string) ([]runInterruptCandidate, error) {
	rows, err := pool.Query(ctx, runningCandidatesQuery+" AND r.cluster = $1 AND r.tenant = $2", cluster, tenant)
	if err != nil {
		return nil, fmt.Errorf("select running candidates for tenant: %w", err)
	}
	return scanRunningCandidates(rows)
}

// markInterrupted performs the whole marking transaction for one run:
// the terminal write conditioned on status = 'RUNNING', the log-summary
// finalize marker, and the build.interrupted activity row, all in one
// transaction. It reports whether the conditional update changed the
// row; a miss means a real terminal event already won the race, and the
// caller counts it in recorder_interrupted_skipped_total.
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
		WHERE run_id = $1 AND status = 'RUNNING'
	`, run.runID, now, reason)
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
// cancel_run when the marked run's owning cell instance is still the
// tenant's stored one (the silent reason, ReasonNoHeartbeat, is the only
// case that holds), and one automatic retry_run when the tenant's policy
// calls for it. Neither publish is transactional with markInterrupted's
// write: a lost action produces no retry and no cancel, per design D14,
// and the recorder never republishes.
func afterMark(ctx context.Context, conn *bus.Conn, metrics *InterruptedMetrics, actionMetrics *ActionMetrics,
	run runInterruptCandidate, reason, interruptedRetry string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	if reason == ReasonNoHeartbeat {
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

// MarkMovedRunsForTenant runs the moved test only, for every RUNNING run
// of cluster/tenant, immediately after a tenant.sync announce changes
// cell or cell_instance. row is the placement row the announce just
// wrote. It answers how many runs it marked, for the TenantSyncResponse
// InterruptedMarked field. The sweep marks the rest as their heartbeats
// age: this is the fast path design D9 gives a cell roll, not a separate
// rule, so the silent test never fires here.
func MarkMovedRunsForTenant(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, cluster, tenant string, row tenantPlacementLivenessRow, now time.Time, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	candidates, err := selectRunningCandidatesForTenant(ctx, pool, cluster, tenant)
	if err != nil {
		return 0, err
	}

	interruptedRetry := decodeInterruptedRetry(row.policy)
	marked := 0
	for _, c := range candidates {
		reason, ok := evaluateInterruption(now, c, row, true)
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
		metrics.addMarked(ctx, "announce")
		afterMark(ctx, conn, metrics, actionMetrics, c, reason, interruptedRetry, logger)
		marked++
	}
	return marked, nil
}

// SweepInterruptedRuns runs one round of the interrupted-run sweep
// (design D9, task 9.6a's gate): it checks the recorder's own
// varroa_builds consumer for freshness before it reads any run, then
// applies the moved and silent tests to every RUNNING run fleet-wide,
// marking and publishing for each one that qualifies. It answers how
// many runs it marked.
func SweepInterruptedRuns(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, maxPending uint64, now time.Time, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	info, err := conn.ConsumerInfo(bus.BuildsStreamName, RecorderConsumerName)
	if err != nil {
		return 0, fmt.Errorf("read varroa_builds consumer info: %w", err)
	}
	if info.NumPending >= maxPending {
		metrics.addSweepSkipped(ctx, "pending")
		return 0, nil
	}
	if info.AckFloor.Last == nil || now.Sub(*info.AckFloor.Last) >= pluginHeartbeatInterval {
		metrics.addSweepSkipped(ctx, "stale")
		return 0, nil
	}

	candidates, err := selectRunningCandidates(ctx, pool)
	if err != nil {
		return 0, err
	}

	marked := 0
	for _, c := range candidates {
		row, exists, err := readTenantPlacementLivenessRow(ctx, pool, c.cluster, c.tenant)
		if err != nil {
			logger.Warn("interrupted sweep: read tenant placement failed", "run_id", c.runID, "error", err)
			continue
		}
		reason, ok := evaluateInterruption(now, c, row, exists)
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
		metrics.addMarked(ctx, "sweep")
		afterMark(ctx, conn, metrics, actionMetrics, c, reason, decodeInterruptedRetry(row.policy), logger)
		marked++
	}
	return marked, nil
}
