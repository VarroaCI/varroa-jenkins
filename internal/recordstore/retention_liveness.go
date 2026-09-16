package recordstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// heartbeatGoneAfter is the age a run's last_heartbeat_at, or its
// started_at when no heartbeat ever arrived, must reach before
// HeartbeatLiveness reports the owning cell gone. It matches the
// heartbeat rule of the run-ledger ingest kind.
const heartbeatGoneAfter = 15 * time.Minute

// RunLiveness carries the fields a Liveness implementation needs to
// judge whether a run's owning cell is still alive. Tenant and
// CellInstance feed the placement-aware implementation below; the
// heartbeat-only implementation ignores them.
type RunLiveness struct {
	RunID           string
	Cluster         string
	Namespace       string
	Cell            string
	Tenant          string
	CellInstance    string
	LastHeartbeatAt *time.Time
	StartedAt       *time.Time
}

// Liveness answers whether the cell that owns a run is gone. This
// package ships the heartbeat-only HeartbeatLiveness, because C1 applies
// before the tenant_placements table exists. C6 supplies a
// placement-aware implementation that needs no migration from this
// change.
type Liveness interface {
	CellGone(ctx context.Context, run RunLiveness) (bool, error)
}

// HeartbeatLiveness is the heartbeat-only Liveness this change ships.
// The cell is gone when LastHeartbeatAt is older than
// heartbeatGoneAfter, or when it is null and StartedAt is older than
// heartbeatGoneAfter.
type HeartbeatLiveness struct {
	// Now returns the reference time. A nil Now defaults to time.Now, so
	// a test can pin it to a fixed instant.
	Now func() time.Time
}

// CellGone reports whether run's owning cell is gone, by heartbeat age
// alone.
func (h HeartbeatLiveness) CellGone(_ context.Context, run RunLiveness) (bool, error) {
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	cutoff := now().Add(-heartbeatGoneAfter)

	if run.LastHeartbeatAt != nil {
		return run.LastHeartbeatAt.Before(cutoff), nil
	}
	if run.StartedAt != nil {
		return run.StartedAt.Before(cutoff), nil
	}
	return false, nil
}

// PlacementLiveness is the placement-aware Liveness the interrupted-runs
// capability supplies. It reads varroa.tenant_placements and applies
// cellLivenessGone, the same reading the interrupted-run sweep uses, so
// the log-finalization sweep and the interrupted-run sweep never
// disagree about whether a cell is gone.
type PlacementLiveness struct {
	pool *pgxpool.Pool
	// grace is the confirmed-gone grace the confirmed reading applies.
	grace time.Duration
	// Now returns the reference time. A nil Now defaults to time.Now, so
	// a test can pin it to a fixed instant.
	Now func() time.Time
}

// NewPlacementLiveness returns a PlacementLiveness over pool, with grace
// as the confirmed-gone grace.
func NewPlacementLiveness(pool *pgxpool.Pool, grace time.Duration) *PlacementLiveness {
	return &PlacementLiveness{pool: pool, grace: grace}
}

// CellGone implements Liveness. It answers gone only when a
// tenant_placements row exists for run's cluster and tenant, and a
// cell-liveness reading confirms run's owning cell instance is absent
// for that reading's own window.
func (p *PlacementLiveness) CellGone(ctx context.Context, run RunLiveness) (bool, error) {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	t := now()

	var anchor *time.Time
	switch {
	case run.LastHeartbeatAt != nil:
		anchor = run.LastHeartbeatAt
	case run.StartedAt != nil:
		anchor = run.StartedAt
	default:
		return false, nil
	}

	row, exists, err := readTenantPlacementLivenessRow(ctx, p.pool, run.Cluster, run.Tenant)
	if err != nil {
		return false, err
	}
	// Any reading but livenessAlive answers gone, so this seam keeps the
	// behavior it had before the reading carried its own name.
	return cellLivenessGone(t, *anchor, run.CellInstance, row, exists, p.grace) != livenessAlive, nil
}
