package recordstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// lockWaitExceededAfter is the wait that makes a waiter an event.
const lockWaitExceededAfter = 15 * time.Minute

// lockWaitWindow bounds the waits that one round may catch. A round runs once
// a minute, so a two-minute window catches each wait once. The event time is
// the wait start plus lockWaitExceededAfter, which is the same for every
// round, so the event key dedupes a wait that a later round sees again.
const lockWaitWindow = 2 * time.Minute

// publishLockWaitsExceeded writes one fleetlock.wait_exceeded row for each
// waiter that has waited for the threshold. It publishes the rows after their
// commit. It returns the number of rows the round wrote.
func publishLockWaitsExceeded(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, now time.Time, logger *slog.Logger) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT fl.name, fl.cluster, fl.job_path, fl.waiting_since,
		       ri.build_number, ri.cell, ri.namespace
		FROM varroa.fleet_locks fl
		LEFT JOIN varroa.run_intents ri ON ri.run_id::text = fl.run_id
		WHERE fl.state = 'waiting'
		  AND fl.waiting_since <= $1::timestamptz - $2::interval
		  AND fl.waiting_since > $1::timestamptz - $3::interval
	`, now, durationInterval(lockWaitExceededAfter), durationInterval(lockWaitExceededAfter+lockWaitWindow))
	if err != nil {
		return 0, fmt.Errorf("select lock waits: %w", err)
	}
	type waiter struct {
		name, cluster, jobPath string
		since                  time.Time
		build                  *int64
		cell, namespace        *string
	}
	var waiters []waiter
	for rows.Next() {
		var w waiter
		if err := rows.Scan(&w.name, &w.cluster, &w.jobPath, &w.since, &w.build, &w.cell, &w.namespace); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan lock wait: %w", err)
		}
		waiters = append(waiters, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read lock waits: %w", err)
	}
	if len(waiters) == 0 {
		return 0, nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin lock wait round: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out activityOutbox
	for _, w := range waiters {
		ev := activity.Event{
			Timestamp: w.since.Add(lockWaitExceededAfter),
			Type:      "fleetlock.wait_exceeded",
			Source:    "recorder",
			Severity:  "warning",
			Cluster:   w.cluster,
			ItemPath:  w.jobPath,
			Reason:    w.name,
			Message:   fmt.Sprintf("%s waited more than %d minutes for lock %s", w.jobPath, int(lockWaitExceededAfter/time.Minute), w.name),
		}
		if w.namespace != nil {
			ev.Namespace = *w.namespace
		}
		if w.cell != nil {
			ev.Controller = *w.cell
		}
		if w.build != nil {
			ev.BuildNumber = *w.build
		}
		if err := insertActivityForPublish(ctx, tx, ev, &out); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit lock wait round: %w", err)
	}
	written := len(out.events)
	publishActivityAfterCommit(conn, &out)
	if written > 0 && logger != nil {
		logger.Info("fleet lock waits exceeded", "events", written)
	}
	return written, nil
}

// durationInterval renders a duration as a Postgres interval string.
func durationInterval(d time.Duration) string {
	return fmt.Sprintf("%d microseconds", d.Microseconds())
}
