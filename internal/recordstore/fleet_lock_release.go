// Package recordstore: the fleet lock's second release confirmation,
// design.md D11. The first confirmation, a terminal run.completed for
// the holder, lives beside the runs write itself (ingest_completed.go).
// This file is the second: a releasing row's owning cell instance is
// confirmed gone.
package recordstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fleetLockOwnerConfirmedGone answers whether row, the tenant's own
// tenant_placements liveness row an announce just wrote, confirms that
// cellInstance, a releasing fleet lock's owning cell instance, is gone.
// It applies the fleet-locks capability's own two tests, reusing the
// interrupted-runs capability's two shared cell-liveness readings
// (cell_liveness.go) with no anchor test: a releasing row carries no
// run heartbeat to anchor against, only the placement's own state.
//
// grace is the confirmed-gone grace, so a pod-read confirmation releases
// a lock on the same schedule it interrupts a run.
func fleetLockOwnerConfirmedGone(row tenantPlacementLivenessRow, cellInstance string, now time.Time,
	grace time.Duration) bool {
	if cellInstance == "" {
		return false
	}
	if cellInstance == row.cellInstance {
		return currentInstanceGone(row, now, silentHeartbeatWindow)
	}
	return deadInstanceGone(row.deadInstances, cellInstance, now, movedHeartbeatWindow, grace)
}

// releasingFleetLock is one releasing fleet_locks row
// ReleaseConfirmedFleetLocks reads. jobPath travels with the row,
// because the row's own release notification carries it.
type releasingFleetLock struct {
	name, runID, cellInstance, jobPath string
}

// ReleaseConfirmedFleetLocks deletes every releasing fleet_locks row of
// cluster/tenant whose owning cell instance row, the tenant's own
// tenant_placements liveness row this same announce just wrote,
// confirms gone. It is the tenant-sync responder's own hook: the
// confirmation's evidence (dead_instances, cell_alive) can only become
// newly true at the moment of a fresh announce, so running this check
// once per announce, against the row that announce just wrote, is
// enough; the maintenance round adds nothing this test could not already
// see at the last announce. It answers how many rows it released.
//
// It publishes one lock notification per row it deletes, through
// notifier. A row whose delete affects nothing publishes nothing, which
// covers a repeat sweep of a row another path already removed. A nil
// notifier publishes nothing.
func ReleaseConfirmedFleetLocks(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string,
	row tenantPlacementLivenessRow, now time.Time, grace time.Duration, notifier *RunNotifier, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	rows, err := pool.Query(ctx, `
		SELECT name, run_id, cell_instance, job_path FROM varroa.fleet_locks
		WHERE cluster = $1 AND tenant = $2 AND state = 'releasing'
	`, cluster, tenant)
	if err != nil {
		return 0, fmt.Errorf("select releasing fleet locks for %s/%s: %w", cluster, tenant, err)
	}
	candidates, err := scanReleasingFleetLocks(rows)
	if err != nil {
		return 0, fmt.Errorf("select releasing fleet locks for %s/%s: %w", cluster, tenant, err)
	}

	released := 0
	for _, c := range candidates {
		if !fleetLockOwnerConfirmedGone(row, c.cellInstance, now, grace) {
			continue
		}
		tag, err := pool.Exec(ctx, `
			DELETE FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2 AND state = 'releasing'
		`, c.name, c.runID)
		if err != nil {
			logger.Warn("release confirmed fleet lock failed", "name", c.name, "run_id", c.runID, "error", err)
			continue
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		released += int(tag.RowsAffected())
		notifier.Notify(lockNotification(cluster, tenant, c.jobPath, c.name, c.runID, FleetLockStateReleased))
	}
	return released, nil
}

// scanReleasingFleetLocks reads ReleaseConfirmedFleetLocks's own query
// rows.
func scanReleasingFleetLocks(rows pgx.Rows) ([]releasingFleetLock, error) {
	defer rows.Close()
	var out []releasingFleetLock
	for rows.Next() {
		var c releasingFleetLock
		if err := rows.Scan(&c.name, &c.runID, &c.cellInstance, &c.jobPath); err != nil {
			return nil, fmt.Errorf("scan releasing fleet lock: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select releasing fleet locks: %w", err)
	}
	return out, nil
}
