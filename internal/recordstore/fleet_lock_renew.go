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
// CheckPlacement call and cross-prefix grant an acquire runs, then the
// refresh.
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

	ownerTenant, bareName, crossesPrefix := splitLockName(req.Name, req.CallerTenant)
	if crossesPrefix {
		grant, err := checkCrossTenantLockGrant(ctx, deps, req, ownerTenant, bareName)
		if err != nil {
			return CrossTenantResponse{}, err
		}
		if !grant.Allow {
			return CrossTenantResponse{ReasonCode: grant.ReasonCode, Message: grant.Message}, nil
		}
	}

	name := ownerTenant + "/" + bareName
	row, found, err := readFleetLockRow(ctx, deps.Pool, name, req.RunID)
	if err != nil {
		return CrossTenantResponse{}, err
	}
	if !found {
		// A sweep or another acquire already deleted the expired row.
		return CrossTenantResponse{State: FleetLockStateLost}, nil
	}
	lease := req.LeaseSeconds
	if lease <= 0 {
		lease = fleetLockDefaultLeaseSeconds
	}
	expiresAt := time.Now().UTC().Add(time.Duration(lease) * time.Second)
	// A successful renew raises the stored generation to the caller's,
	// so the epoch follows its holder and never falls behind. It never
	// lowers the stored value.
	generation := row.placementGeneration
	if req.PlacementGeneration > generation {
		generation = req.PlacementGeneration
	}
	if err := renewFleetLockExpiry(ctx, deps.Pool, name, req.RunID, expiresAt, generation); err != nil {
		return CrossTenantResponse{}, err
	}

	return CrossTenantResponse{State: row.state, FenceToken: row.fenceToken, ExpiresAt: &expiresAt}, nil
}

// handleCrossTenantLockRelease implements the lock_release kind. It
// deletes the caller's row, whatever its state, and answers "released"
// even when no row remains. Release carries no fence: a caller
// releasing its own row needs no authorization to do so.
//
// The delete reads the deleted row's own tenant and job path, because a
// cross-tenant release can carry a tenant the request does not: a grant
// let the caller hold that row under another tenant's prefix. The row's
// own cluster and run id need no second read: name and run_id are both
// the caller's own values, so the deleted row is the caller's own row by
// primary key. A delete that affects no row publishes nothing.
func handleCrossTenantLockRelease(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	ownerTenant, bareName, _ := splitLockName(req.Name, req.CallerTenant)
	name := ownerTenant + "/" + bareName

	var tenant, jobPath string
	err := deps.Pool.QueryRow(ctx, `
		DELETE FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
		RETURNING tenant, job_path
	`, name, req.RunID).Scan(&tenant, &jobPath)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return CrossTenantResponse{State: FleetLockStateReleased}, nil
	case err != nil:
		return CrossTenantResponse{}, fmt.Errorf("release fleet lock: %w", err)
	}

	deps.LockNotifier.Notify(lockNotification(req.Cluster, tenant, jobPath, name, req.RunID, FleetLockStateReleased))
	return CrossTenantResponse{State: FleetLockStateReleased}, nil
}

// fleetLockRow is the subset of one fleet_locks row a renew reads. It
// holds no cell_instance: a renew carries none to compare it against.
type fleetLockRow struct {
	state               string
	fenceToken          int64
	placementGeneration int64
}

// readFleetLockRow reads name/runID's fleet_locks row. It answers
// found=false, with no error, when no such row exists.
func readFleetLockRow(ctx context.Context, pool *pgxpool.Pool, name, runID string) (fleetLockRow, bool, error) {
	var row fleetLockRow
	err := pool.QueryRow(ctx, `
		SELECT state, fence_token, placement_generation
		FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
	`, name, runID).Scan(&row.state, &row.fenceToken, &row.placementGeneration)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fleetLockRow{}, false, nil
	case err != nil:
		return fleetLockRow{}, false, fmt.Errorf("read fleet lock row: %w", err)
	}
	return row, true, nil
}

// renewFleetLockExpiry refreshes name/runID's expires_at and raises its
// placement_generation. It writes no other column: mutual exclusion
// rests on the acquire path and on the fencing token.
func renewFleetLockExpiry(ctx context.Context, pool *pgxpool.Pool, name, runID string,
	expiresAt time.Time, generation int64) error {
	if _, err := pool.Exec(ctx, `
		UPDATE varroa.fleet_locks SET expires_at = $3, placement_generation = $4
		WHERE name = $1 AND run_id = $2
	`, name, runID, expiresAt, generation); err != nil {
		return fmt.Errorf("renew fleet lock expiry: %w", err)
	}
	return nil
}
