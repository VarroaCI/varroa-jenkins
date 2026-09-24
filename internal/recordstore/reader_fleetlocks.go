// Package recordstore: the fleet lock reads of the fleet-locks
// capability. The tenant read answers the locks a tenant owns and the
// locks its own runs hold or wait for. The cluster-wide read answers
// every lock of one cluster. Neither read writes, and neither read
// answers a queue position.
package recordstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// fleetLockColumns lists varroa.fleet_locks' columns in the order
// scanFleetLocks expects.
const fleetLockColumns = `name, run_id, cluster, tenant, job_path, state,
	fence_token, waiting_since, acquired_at, expires_at`

// ListFleetLocks answers every fleet_locks row of cluster whose
// owner_tenant column is tenant, plus every row of cluster whose own
// tenant column is tenant. A cross-tenant acquire writes the caller's
// own tenant into the row's tenant column, so the second part answers a
// lock that tenant's own run holds under another tenant's ownership.
// The primary key (name, run_id) holds one row once, so a row both parts
// select appears once.
//
// Both parts test equality, never a name prefix: a controller tenant
// holds a "/" of its own, so tenant "a" must not read the locks of
// tenant "a/b", and a "_" or a "%" in a tenant is no wildcard.
//
// The order is (name, waiting_since, run_id). A row's waiting_since is
// stamped on its first insert and never updated, so the holder of a name
// sorts before its waiters, and the run_id term orders two rows of one
// timestamp.
func (r *pgReader) ListFleetLocks(ctx context.Context, cluster, tenant string) ([]FleetLock, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+fleetLockColumns+`
		FROM varroa.fleet_locks
		WHERE cluster = $1 AND (owner_tenant = $2 OR tenant = $2)
		ORDER BY name, waiting_since, run_id
	`, cluster, tenant)
	if err != nil {
		return nil, fmt.Errorf("list fleet locks: %w", err)
	}
	locks, err := scanFleetLocks(rows)
	if err != nil {
		return nil, fmt.Errorf("list fleet locks: %w", err)
	}
	return locks, nil
}

// ListAllFleetLocks answers every fleet_locks row of cluster, in the
// same order ListFleetLocks uses.
func (r *pgReader) ListAllFleetLocks(ctx context.Context, cluster string) ([]FleetLock, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+fleetLockColumns+`
		FROM varroa.fleet_locks
		WHERE cluster = $1
		ORDER BY name, waiting_since, run_id
	`, cluster)
	if err != nil {
		return nil, fmt.Errorf("list all fleet locks: %w", err)
	}
	locks, err := scanFleetLocks(rows)
	if err != nil {
		return nil, fmt.Errorf("list all fleet locks: %w", err)
	}
	return locks, nil
}

// scanFleetLocks reads either lock read's own rows. It closes rows, so
// the caller holds no connection open after the read.
func scanFleetLocks(rows pgx.Rows) ([]FleetLock, error) {
	defer rows.Close()
	var locks []FleetLock
	for rows.Next() {
		var lock FleetLock
		if err := rows.Scan(&lock.Name, &lock.RunID, &lock.Cluster, &lock.Tenant, &lock.JobPath,
			&lock.State, &lock.FenceToken, &lock.WaitingSince, &lock.AcquiredAt, &lock.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan fleet lock: %w", err)
		}
		locks = append(locks, lock)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return locks, nil
}
