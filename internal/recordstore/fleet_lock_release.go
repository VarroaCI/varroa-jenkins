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
//
// A dead_instances entry counts on two arms only. An entry with a
// pod-read confirmation counts once the grace elapses. An entry the
// recorder derived from a rolled member counts once its last_seen_at
// passes the moved window, because a cell never runs two pods of one
// name. Every other entry never counts: a changed placement alone
// releases no lock, because the cell the tenant left can still be alive
// and still deploying.
func fleetLockOwnerConfirmedGone(row tenantPlacementLivenessRow, cellInstance string, now time.Time,
	grace time.Duration) bool {
	if cellInstance == "" {
		return false
	}
	if cellInstance == row.cellInstance {
		return currentInstanceGone(row, now, silentHeartbeatWindow)
	}
	return deadInstanceGone(lockReleaseEntries(row.deadInstances, now, grace), cellInstance, now, movedHeartbeatWindow, grace)
}

// lockReleaseEntries answers the entries of dead that can release a
// fleet lock: an entry whose confirmation is older than grace, and an
// entry tagged DeadInstanceDerivedRolled. Every other entry with no
// confirmation is left out, because it does not prove the pod gone.
func lockReleaseEntries(dead []TenantSyncDeadInstance, now time.Time, grace time.Duration) []TenantSyncDeadInstance {
	out := make([]TenantSyncDeadInstance, 0, len(dead))
	for _, d := range dead {
		if confirmedGone(d, now, grace) || d.Derived == DeadInstanceDerivedRolled {
			out = append(out, d)
		}
	}
	return out
}

// releasingFleetLock is one releasing fleet_locks row
// ReleaseConfirmedFleetLocks reads. jobPath travels with the row,
// because the row's own release notification carries it.
type releasingFleetLock struct {
	name, runID, cellInstance, jobPath string
}

// ReleaseConfirmedFleetLocks deletes every releasing fleet_locks row of
// cluster/tenant whose owning cell instance row, the tenant's
// tenant_placements liveness row, confirms gone. Two callers run it: the
// tenant-sync responder, against the row the announce just wrote, and
// the backstop round, against the stored row. The backstop round is
// necessary because a confirmation counts only once the grace elapses.
// The announce that brings a confirmation therefore never releases the
// lock, and the next announce can come ten minutes later. It answers how
// many rows it released.
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

// releasingFleetLockTenant is one cluster and tenant that holds at least
// one releasing fleet_locks row.
type releasingFleetLockTenant struct {
	cluster, tenant string
}

// ReleaseConfirmedFleetLocksRound runs ReleaseConfirmedFleetLocks for
// every tenant that holds a releasing fleet lock, against that tenant's
// stored tenant_placements row. The backstop round calls it once a
// minute, so a confirmation releases its lock soon after the grace
// elapses. A tenant with no stored row gives no reading and releases
// nothing. The stored cell_alive_at keeps the staleness test, so a
// stopped operator releases no lock. It answers how many rows it released.
func ReleaseConfirmedFleetLocksRound(ctx context.Context, pool *pgxpool.Pool, now time.Time, grace time.Duration,
	notifier *RunNotifier, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT cluster, tenant FROM varroa.fleet_locks WHERE state = 'releasing'
	`)
	if err != nil {
		return 0, fmt.Errorf("select tenants with releasing fleet locks: %w", err)
	}
	tenants, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (releasingFleetLockTenant, error) {
		var t releasingFleetLockTenant
		err := r.Scan(&t.cluster, &t.tenant)
		return t, err
	})
	if err != nil {
		return 0, fmt.Errorf("select tenants with releasing fleet locks: %w", err)
	}

	released := 0
	for _, t := range tenants {
		row, exists, err := readTenantPlacementLivenessRow(ctx, pool, t.cluster, t.tenant)
		if err != nil {
			logger.Warn("fleet lock release round: read tenant placement failed",
				"cluster", t.cluster, "tenant", t.tenant, "error", err)
			continue
		}
		if !exists {
			continue
		}
		n, err := ReleaseConfirmedFleetLocks(ctx, pool, t.cluster, t.tenant, row, now, grace, notifier, logger)
		if err != nil {
			logger.Warn("fleet lock release round failed", "cluster", t.cluster, "tenant", t.tenant, "error", err)
			continue
		}
		released += n
	}
	return released, nil
}
