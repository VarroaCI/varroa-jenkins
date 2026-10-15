package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// handleCrossTenantLockRenew implements the lock_renew kind: the same
// CheckPlacement call, name check, and cross-tenant grant an acquire
// runs, then the refresh.
//
// The run is the renew's identity. A fleet_locks row is keyed by the
// lock name and the run id, and a renew carries no instance of its own,
// so the row a caller reaches is the row of the run it names. The
// ownership move rewrites that row's cell_instance inside its own
// transaction, which is why a run resumed on another cell still renews
// its own lock.
//
// checkCrossTenantLockPlacement is the authorization gate. It refuses a
// caller that holds no member of the tenant, so a caller outside the
// placement never reaches the refresh below.
//
// The renew applies no generation test in either direction. The stored
// generation freezes at the acquire, and a tenant's generation rises
// whenever its bound cell set changes, so a generation test would refuse
// a live holder after a takeover or a scale. A lower caller generation
// only means that the caller's resolver cache is behind. The placement
// orders run first, so a caller on no member still gets FENCED.
func handleCrossTenantLockRenew(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	if veto, err := checkCrossTenantLockPlacement(ctx, deps, req); err != nil || veto != nil {
		return derefCrossTenantResponse(veto), err
	}

	ownerTenant, bareName, crossesTenant, veto := resolveLockTarget(req)
	if veto != nil {
		return *veto, nil
	}
	if crossesTenant {
		grant, err := checkCrossTenantLockGrant(ctx, deps, req, ownerTenant, bareName)
		if err != nil {
			return CrossTenantResponse{}, err
		}
		if !grant.Allow {
			return CrossTenantResponse{ReasonCode: grant.ReasonCode, Message: grant.Message}, nil
		}
	}

	name := qualifiedLockName(ownerTenant, bareName)
	lease := req.LeaseSeconds
	if lease <= 0 {
		lease = fleetLockDefaultLeaseSeconds
	}
	expiresAt := time.Now().UTC().Add(time.Duration(lease) * time.Second)
	row, found, err := renewFleetLockRow(ctx, deps.Pool, name, req.RunID, req.CallerTenant, expiresAt, req.PlacementGeneration)
	if err != nil {
		return CrossTenantResponse{}, err
	}
	if !found {
		// A sweep or another acquire already deleted the expired row, or
		// the row belongs to another tenant's run of the same run id.
		return CrossTenantResponse{State: FleetLockStateLost}, nil
	}

	return CrossTenantResponse{State: row.state, FenceToken: row.fenceToken, ExpiresAt: &expiresAt}, nil
}

// handleCrossTenantLockRelease implements the lock_release kind. It
// deletes the caller's row, whatever its state, and answers "released"
// even when no row remains. Release carries no fence and no grant check.
// The delete matches the row's tenant column to the gateway-stamped
// caller tenant instead, so a caller that learns another tenant's run id
// still cannot release that tenant's row.
//
// A name the acquire refuses answers the same INVALID_NAME veto, because
// no row can exist under it.
//
// The delete reads the deleted row's own tenant and job path. The
// request's owner tenant is the lock's owner, not the holder, and a
// grant can let the caller hold a lock another tenant owns. The row's
// own cluster and run id need no second read: name and run_id are both
// the caller's own values, so the deleted row is the caller's own row by
// primary key. A delete that affects no row publishes nothing.
func handleCrossTenantLockRelease(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	ownerTenant, bareName, _, veto := resolveLockTarget(req)
	if veto != nil {
		return *veto, nil
	}
	name := qualifiedLockName(ownerTenant, bareName)

	var tenant, jobPath string
	err := deps.Pool.QueryRow(ctx, `
		DELETE FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2 AND tenant = $3
		RETURNING tenant, job_path
	`, name, req.RunID, req.CallerTenant).Scan(&tenant, &jobPath)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return CrossTenantResponse{State: FleetLockStateReleased}, nil
	case err != nil:
		return CrossTenantResponse{}, fmt.Errorf("release fleet lock: %w", err)
	}

	deps.LockNotifier.Notify(lockNotification(req.Cluster, tenant, jobPath, name, req.RunID, FleetLockStateReleased))
	return CrossTenantResponse{State: FleetLockStateReleased}, nil
}

// fleetLockRow is the subset of one fleet_locks row a renew answers. It
// holds no cell_instance: a renew carries none to compare it against.
type fleetLockRow struct {
	state      string
	fenceToken int64
}

// renewFleetLockRow refreshes name/runID's expires_at and raises its
// placement_generation to generation, when the row's tenant column is
// tenant. It answers the row's state and fence token as the same
// statement leaves them, and found=false, with no error, when no such
// row exists. One statement reads and writes the row, so a row a sweep
// deletes between two statements never answers a stale held token.
//
// The generation only rises: it follows its holder and never falls
// behind. The tenant match keeps a caller that learns another tenant's
// run id from renewing that row or reading its fence token. It writes
// no other column: mutual exclusion rests on the acquire path and on the
// fencing token.
func renewFleetLockRow(ctx context.Context, pool *pgxpool.Pool, name, runID, tenant string,
	expiresAt time.Time, generation int64) (fleetLockRow, bool, error) {
	var row fleetLockRow
	err := pool.QueryRow(ctx, `
		UPDATE varroa.fleet_locks
		SET expires_at = $3, placement_generation = GREATEST(placement_generation, $4)
		WHERE name = $1 AND run_id = $2 AND tenant = $5
		RETURNING state, fence_token
	`, name, runID, expiresAt, generation, tenant).Scan(&row.state, &row.fenceToken)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fleetLockRow{}, false, nil
	case err != nil:
		return fleetLockRow{}, false, fmt.Errorf("renew fleet lock: %w", err)
	}
	return row, true, nil
}
