package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// movedHeartbeatWindow and silentHeartbeatWindow are the interrupted-run
// spec's two fixed thresholds. Neither is a tenant setting: a heartbeat
// arrives every 60 seconds, so no install needs a different number.
const (
	movedHeartbeatWindow  = 5 * time.Minute
	silentHeartbeatWindow = 15 * time.Minute
)

// cellAliveStaleAfter is twice the ten-minute announce resync interval
// (internal/controller/cells.syncResyncInterval), design D9's "twice the
// announce interval" rule. A cell-liveness reading older than this
// counts as absent, so a stopped operator never interrupts a run.
const cellAliveStaleAfter = 20 * time.Minute

// tenantPlacementLivenessRow is the subset of one tenant_placements row
// the cell-liveness readings need. It is read fresh for the periodic
// sweep, and built directly from an announce request's own fields for
// the announce-triggered marking, which runs before the announce's
// write is necessarily visible to a second read.
type tenantPlacementLivenessRow struct {
	cell           string
	cellInstance   string
	cellAlive      bool
	cellAliveAt    *time.Time
	cellLastSeenAt *time.Time
	deadInstances  []TenantSyncDeadInstance
	policy         json.RawMessage
}

// livenessQuerier is the subset of *pgxpool.Pool and pgx.Tx
// readTenantPlacementLivenessRow needs.
type livenessQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readTenantPlacementLivenessRow reads cluster/tenant's tenant_placements
// row for a cell-liveness reading. It answers exists=false, with no
// error, when no row is stored yet: a missing row gives no reading.
func readTenantPlacementLivenessRow(ctx context.Context, db livenessQuerier, cluster, tenant string) (tenantPlacementLivenessRow, bool, error) {
	var (
		row     tenantPlacementLivenessRow
		deadRaw json.RawMessage
	)
	err := db.QueryRow(ctx, `
		SELECT cell, cell_instance, cell_alive, cell_alive_at, cell_last_seen_at, dead_instances, policy
		FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2
	`, cluster, tenant).Scan(&row.cell, &row.cellInstance, &row.cellAlive, &row.cellAliveAt,
		&row.cellLastSeenAt, &deadRaw, &row.policy)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return tenantPlacementLivenessRow{}, false, nil
	case err != nil:
		return tenantPlacementLivenessRow{}, false, fmt.Errorf("read tenant placement liveness: %w", err)
	}
	if len(deadRaw) > 0 {
		if err := json.Unmarshal(deadRaw, &row.deadInstances); err != nil {
			return tenantPlacementLivenessRow{}, false, fmt.Errorf("decode dead instances: %w", err)
		}
	}
	return row, true, nil
}

// deadInstanceGone reports whether instance appears in dead, with a
// LastSeenAt older than window. An empty instance never matches.
func deadInstanceGone(dead []TenantSyncDeadInstance, instance string, now time.Time, window time.Duration) bool {
	if instance == "" {
		return false
	}
	for _, d := range dead {
		if d.Instance == instance {
			return d.LastSeenAt.Before(now.Add(-window))
		}
	}
	return false
}

// currentInstanceGone reports whether row's own cell-liveness signal
// confirms its current instance is gone: cell_alive false, with
// cell_last_seen_at older than window. cell_alive_at older than twice
// the announce interval counts the whole signal as absent, so a stopped
// operator never interrupts a run.
func currentInstanceGone(row tenantPlacementLivenessRow, now time.Time, window time.Duration) bool {
	if row.cellAlive {
		return false
	}
	if row.cellAliveAt == nil || row.cellAliveAt.Before(now.Add(-cellAliveStaleAfter)) {
		return false
	}
	if row.cellLastSeenAt == nil || !row.cellLastSeenAt.Before(now.Add(-window)) {
		return false
	}
	return true
}

// cellLivenessGone is the interrupted-runs spec's one shared reading
// function: it answers whether cellInstance counts as gone from row,
// given anchor (the caller's own liveness baseline) and now. It applies
// the moved reading (dead_instances) when cellInstance differs from
// row's own stored instance, and the silent reading (cell_alive) when it
// matches. Neither reading fires unless anchor is already older than
// that reading's own window. A missing row answers false: no reading is
// possible. This is the "one piece of code" design D9 requires: both the
// placement-aware Liveness seam and the interrupted-run sweep call it.
func cellLivenessGone(now, anchor time.Time, cellInstance string, row tenantPlacementLivenessRow, exists bool) bool {
	if !exists {
		return false
	}
	if cellInstance != row.cellInstance {
		return anchor.Before(now.Add(-movedHeartbeatWindow)) &&
			deadInstanceGone(row.deadInstances, cellInstance, now, movedHeartbeatWindow)
	}
	return anchor.Before(now.Add(-silentHeartbeatWindow)) && currentInstanceGone(row, now, silentHeartbeatWindow)
}
