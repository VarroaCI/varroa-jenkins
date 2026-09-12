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
// row's own stale-generation refusal, then the refresh.
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
	if row.placementGeneration < req.PlacementGeneration {
		// A cell cannot renew a lease it took under an older generation.
		return CrossTenantResponse{ReasonCode: ReasonFenced,
			Message: fmt.Sprintf("placement generation %d is stale for lock %s", row.placementGeneration, name)}, nil
	}

	lease := req.LeaseSeconds
	if lease <= 0 {
		lease = fleetLockDefaultLeaseSeconds
	}
	expiresAt := time.Now().UTC().Add(time.Duration(lease) * time.Second)
	if err := renewFleetLockExpiry(ctx, deps.Pool, name, req.RunID, expiresAt); err != nil {
		return CrossTenantResponse{}, err
	}

	return CrossTenantResponse{State: row.state, FenceToken: row.fenceToken, ExpiresAt: &expiresAt}, nil
}

// handleCrossTenantLockRelease implements the lock_release kind. It
// deletes the caller's row, whatever its state, and answers "released"
// even when no row remains. Release carries no fence: a caller
// releasing its own row needs no authorization to do so.
func handleCrossTenantLockRelease(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	ownerTenant, bareName, _ := splitLockName(req.Name, req.CallerTenant)
	name := ownerTenant + "/" + bareName
	if _, err := deps.Pool.Exec(ctx, `DELETE FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`, name, req.RunID); err != nil {
		return CrossTenantResponse{}, fmt.Errorf("release fleet lock: %w", err)
	}
	return CrossTenantResponse{State: FleetLockStateReleased}, nil
}

// fleetLockRow is the subset of one fleet_locks row a renew reads.
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
		SELECT state, fence_token, placement_generation FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
	`, name, runID).Scan(&row.state, &row.fenceToken, &row.placementGeneration)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fleetLockRow{}, false, nil
	case err != nil:
		return fleetLockRow{}, false, fmt.Errorf("read fleet lock row: %w", err)
	}
	return row, true, nil
}

// renewFleetLockExpiry refreshes name/runID's expires_at.
func renewFleetLockExpiry(ctx context.Context, pool *pgxpool.Pool, name, runID string, expiresAt time.Time) error {
	if _, err := pool.Exec(ctx, `
		UPDATE varroa.fleet_locks SET expires_at = $3 WHERE name = $1 AND run_id = $2
	`, name, runID, expiresAt); err != nil {
		return fmt.Errorf("renew fleet lock expiry: %w", err)
	}
	return nil
}
