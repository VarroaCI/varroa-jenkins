package recordstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// markedLostBatch bounds the intents one round ends. Each end commits its
// own transaction under the backstop lock.
const markedLostBatch = 500

// retrySkipParameterized is recorder_retry_skipped_total's reason for a
// retry of a run whose parameters the store does not hold.
const retrySkipParameterized = "parameterized"

// markedLostIntentGuardSQL admits an intent "ri" whose owner marked the
// start and whose run holds no record of the build: no runs row, or only
// a QUEUED row that records no build. The candidate read and the locked
// re-read both apply it, so an event that commits first keeps the run.
//
// A row in any other status is excluded even when it records no build. An
// earlier round's own write, a refused trigger, and a sweep mark of an
// accepted row are all terminal.
const markedLostIntentGuardSQL = `
	ri.state = 'GRANTED'
	AND ri.start_marked_at IS NOT NULL
	AND ri.cell_instance IS NOT NULL AND ri.cell_instance <> ''
	AND NOT EXISTS (
	    SELECT 1 FROM varroa.runs r
	    WHERE r.run_id = ri.run_id AND (` + recordsBuildRunSQL + ` OR r.status <> 'QUEUED')
	)`

// markedLostIntent is one marked intent the round can end.
type markedLostIntent struct {
	runID         uuid.UUID
	createdAt     time.Time
	intentKey     string
	cluster       string
	namespace     string
	cell          string
	tenant        string
	jobPath       string
	cellInstance  string
	actor         string
	paramsDigest  string
	buildNumber   *int64
	startMarkedAt time.Time
}

// selectMarkedLostIntents reads the oldest markedLostBatch intents the
// guard admits, fleet-wide, whose owner holds a confirmation at or
// before confirmedBy.
//
// The confirmation test is in the query, not only in the caller. An
// intent whose owner is alive or unconfirmed would otherwise keep its
// place in the batch on every round, and enough of them would hold an
// older confirmed loss out of the batch for good. The caller still
// applies confirmedGoneInstance, the one reading of the grace.
func selectMarkedLostIntents(ctx context.Context, pool *pgxpool.Pool, confirmedBy time.Time) ([]markedLostIntent, error) {
	rows, err := pool.Query(ctx, `
		SELECT ri.run_id, ri.created_at, ri.intent_key, ri.cluster, ri.namespace, COALESCE(ri.cell, ''),
		       ri.tenant, ri.job_path, ri.cell_instance, COALESCE(ri.actor, ''), COALESCE(ri.params_digest, ''),
		       ri.build_number, ri.start_marked_at
		FROM varroa.run_intents ri
		WHERE `+markedLostIntentGuardSQL+`
		  AND EXISTS (
		    SELECT 1 FROM varroa.tenant_placements tp,
		         jsonb_array_elements(CASE WHEN jsonb_typeof(tp.dead_instances) = 'array'
		                                   THEN tp.dead_instances ELSE '[]'::jsonb END) d
		    WHERE tp.cluster = ri.cluster AND tp.tenant = ri.tenant
		      AND d->>'instance' = ri.cell_instance
		      AND (d->>'confirmed_gone_at')::timestamptz <= $2
		  )
		ORDER BY ri.start_marked_at
		LIMIT $1`, markedLostBatch, confirmedBy)
	if err != nil {
		return nil, fmt.Errorf("select marked lost intents: %w", err)
	}
	defer rows.Close()

	var out []markedLostIntent
	for rows.Next() {
		var c markedLostIntent
		if err := rows.Scan(&c.runID, &c.createdAt, &c.intentKey, &c.cluster, &c.namespace, &c.cell,
			&c.tenant, &c.jobPath, &c.cellInstance, &c.actor, &c.paramsDigest, &c.buildNumber,
			&c.startMarkedAt); err != nil {
			return nil, fmt.Errorf("scan marked lost intent: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select marked lost intents: %w", err)
	}
	return out, nil
}

// candidate answers the interrupt candidate the marking write and
// afterMark read. The start mark proves the build left the queue, so the
// run is RUNNING.
func (c markedLostIntent) candidate() runInterruptCandidate {
	return runInterruptCandidate{
		runID:        c.runID,
		createdAt:    c.createdAt,
		cluster:      c.cluster,
		namespace:    c.namespace,
		cell:         c.cell,
		tenant:       c.tenant,
		jobPath:      c.jobPath,
		status:       StatusRunning,
		buildNumber:  c.buildNumber,
		actor:        c.actor,
		cellInstance: c.cellInstance,
	}
}

// endMarkedLostIntent writes one run's RUNNING row and its INTERRUPTED
// mark in one transaction. It reports whether it wrote the mark.
//
// The intent row is locked first, in the ingest's lock order, and the
// guard is read again under the lock with the observed owner. The row
// write covers both shapes the guard admits: an insert when no row
// exists, and an update of a QUEUED row that records no build. The update
// removes the accepted keys, as run.started does, so the row records a
// build. The mark then takes the ordinary conditional write.
//
// The intent stays GRANTED. The marking writes no run_intents row.
func endMarkedLostIntent(ctx context.Context, pool *pgxpool.Pool, c markedLostIntent, now time.Time) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin marked lost intent end: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT ri.run_id FROM varroa.run_intents ri
		WHERE ri.run_id = $1 AND ri.cell_instance = $2 AND `+markedLostIntentGuardSQL+`
		FOR UPDATE
	`, c.runID, c.cellInstance).Scan(&locked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("lock marked lost intent: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			build_number, status, started_at, actor
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, 'RUNNING', $10, $11
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			status       = 'RUNNING',
			cell         = EXCLUDED.cell,
			namespace    = EXCLUDED.namespace,
			build_number = COALESCE(EXCLUDED.build_number, runs.build_number),
			started_at   = COALESCE(runs.started_at, EXCLUDED.started_at),
			actor        = COALESCE(runs.actor, EXCLUDED.actor),
			payload      = COALESCE(runs.payload, '{}'::jsonb) `+acceptedSupersedeKeysSQL+`
		WHERE runs.status = 'QUEUED' AND `+supersedeSQL("TRUE")+`
	`, c.runID, c.createdAt, c.intentKey, c.cluster, c.namespace, c.cell, c.tenant, c.jobPath,
		c.buildNumber, c.startMarkedAt, nullableString(c.actor))
	if err != nil {
		return false, fmt.Errorf("write marked lost run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	changed, err := markInterruptedTx(ctx, tx, c.candidate(), ReasonCellRolled, now)
	if err != nil || !changed {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit marked lost intent end: %w", err)
	}
	return true, nil
}

// EndMarkedLostIntents ends every run whose owner marked the start, whose
// start event never reached the store, and whose owner is confirmed gone.
// It writes the run RUNNING and marks it INTERRUPTED with the reason
// cell-rolled in one transaction. It answers how many runs it ended.
//
// No other path ends such a run. The interrupted sweep reads runs rows
// alone, the re-fire skips a marked intent, a redelivered claim answers
// duplicate, and the claim-abandoned sweep skips a marked intent.
//
// The round applies the interrupted sweep's consumer gate: a start event
// in the backlog would otherwise lose to a synthetic mark. Only a
// pod-read confirmation older than grace ends a run. An unconfirmed dead
// entry waits for the operator's pod read.
//
// An automatic retry applies only to an intent that carries no
// parameters. The store holds no parameters for a run whose start event
// was lost, and a retry would run with each declared default.
//
// A nil notifier and nil metrics are no-ops.
func EndMarkedLostIntents(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, notifier *RunNotifier,
	metrics *InterruptedMetrics, actionMetrics *ActionMetrics, stateDeleter ObjectDeleter, maxPending uint64,
	now time.Time, grace time.Duration, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	info, err := conn.ConsumerInfo(bus.BuildsStreamName, RecorderConsumerName)
	if err != nil {
		return 0, fmt.Errorf("read varroa_builds consumer info: %w", err)
	}
	if _, skip := sweepConsumerSkip(info, maxPending, now); skip {
		return 0, nil
	}

	candidates, err := selectMarkedLostIntents(ctx, pool, now.Add(-grace))
	if err != nil {
		return 0, err
	}

	ended := 0
	for _, c := range candidates {
		row, exists, err := readTenantPlacementLivenessRow(ctx, pool, c.cluster, c.tenant)
		if err != nil {
			logger.Warn("marked lost intent: read tenant placement failed", "run_id", c.runID, "error", err)
			continue
		}
		if !exists || !confirmedGoneInstance(row.deadInstances, c.cellInstance, now, grace) {
			continue
		}
		changed, err := endMarkedLostIntent(ctx, pool, c, now)
		if err != nil {
			logger.Warn("marked lost intent: end failed", "run_id", c.runID, "error", err)
			continue
		}
		if !changed {
			metrics.addSkipped(ctx)
			continue
		}
		metrics.addMarked(ctx, "sweep", markedStatusLabel(StatusRunning))
		notifier.Notify(RunNotification{
			Kind:    "run",
			Cluster: c.cluster,
			Tenant:  c.tenant,
			JobPath: c.jobPath,
			RunID:   c.runID.String(),
			Status:  StatusCompleted,
			Result:  "INTERRUPTED",
		})
		run := c.candidate()
		deleteRunState(ctx, pool, stateDeleter, run, logger)
		interruptedRetry := decodeInterruptedRetry(row.policy)
		if interruptedRetry == interruptedRetryAutomatic && !parameterlessIntent(c.paramsDigest) {
			metrics.addRetrySkipped(ctx, retrySkipParameterized)
			interruptedRetry = ""
		}
		afterMark(ctx, conn, jobHomeOf(row.members, exists, c.jobPath), metrics, actionMetrics, run,
			ReasonCellRolled, interruptedRetry, logger)
		ended++
	}
	if ended > 0 {
		logger.Info("ended marked runs whose start event was lost", "count", ended)
	}
	return ended, nil
}
