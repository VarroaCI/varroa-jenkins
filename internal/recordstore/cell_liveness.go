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

// DefaultConfirmedGoneGrace is the wait an entry with a pod-read
// confirmation serves before it counts as gone. The operator can clear
// its stored cell instance on the same tick a replacement pod appears,
// and a zero grace would then mark a run the new pod is about to adopt.
// The chart value recordStore.cellLoss.confirmedGoneGrace sets it.
const DefaultConfirmedGoneGrace = 30 * time.Second

// livenessReading names which cell-liveness test fired. The interrupted
// reason depends on the test, so the reading carries more than a bool.
type livenessReading int

const (
	livenessAlive livenessReading = iota
	livenessConfirmedGone
	livenessMoved
	livenessSilent
)

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
	// members resolves the run's own owning instance. A tenant holds
	// several members, so the scalar cellInstance names slot 0 alone.
	members placementMembers
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
		row         tenantPlacementLivenessRow
		namespace   string
		rehydrated  string
		deadRaw     json.RawMessage
		pendingRaw  json.RawMessage
		membersRaw  json.RawMessage
		jobHomesRaw json.RawMessage
	)
	err := db.QueryRow(ctx, `
		SELECT cell, namespace, cell_instance, rehydrated_instance, cell_alive, cell_alive_at,
		       cell_last_seen_at, dead_instances, policy, pending_jobs, members, job_homes
		FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2
	`, cluster, tenant).Scan(&row.cell, &namespace, &row.cellInstance, &rehydrated, &row.cellAlive,
		&row.cellAliveAt, &row.cellLastSeenAt, &deadRaw, &row.policy, &pendingRaw, &membersRaw, &jobHomesRaw)
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
	var pending []string
	if len(pendingRaw) > 0 {
		if err := json.Unmarshal(pendingRaw, &pending); err != nil {
			return tenantPlacementLivenessRow{}, false, fmt.Errorf("decode pending jobs: %w", err)
		}
	}
	row.members, err = decodePlacementMembers(membersRaw, jobHomesRaw, placementScalars{
		cell: row.cell, namespace: namespace, cellInstance: row.cellInstance,
		rehydratedInstance: rehydrated, pendingJobs: pending,
	})
	if err != nil {
		return tenantPlacementLivenessRow{}, false, err
	}
	return row, true, nil
}

// deadInstanceGone reports whether instance appears in dead and counts
// as gone. The entry counts as gone on either of two arms: its
// ConfirmedGoneAt is set and older than grace, which rests on the
// operator's own pod read; or its LastSeenAt is older than window, the
// heartbeat-absence arm. An empty instance never matches.
func deadInstanceGone(dead []TenantSyncDeadInstance, instance string, now time.Time,
	window, grace time.Duration) bool {
	if instance == "" {
		return false
	}
	for _, d := range dead {
		if d.Instance != instance {
			continue
		}
		if confirmedGone(d, now, grace) {
			return true
		}
		return d.LastSeenAt.Before(now.Add(-window))
	}
	return false
}

// confirmedGone reports whether entry carries a pod-read confirmation
// older than grace. An entry with no confirmation never counts here.
func confirmedGone(entry TenantSyncDeadInstance, now time.Time, grace time.Duration) bool {
	return entry.ConfirmedGoneAt != nil && entry.ConfirmedGoneAt.Before(now.Add(-grace))
}

// currentInstanceGone reports whether row's own cell-liveness signal
// confirms its current instance is gone: cell_alive false, with
// cell_last_seen_at older than window. cell_alive_at older than twice
// the announce interval counts the whole signal as absent, so a stopped
// operator never interrupts a run.
//
// This reading tests no pod-read confirmation. It reads cell_alive,
// cell_alive_at, and cell_last_seen_at only, and never dead_instances,
// which is the one place a confirmation is stored.
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
// function: it answers which test proves cellInstance gone from row,
// given anchor (the caller's own liveness baseline) and now. Three tests
// apply in order, and the first match wins.
//
// The confirmed test holds when the entry for cellInstance carries a
// ConfirmedGoneAt older than grace. It applies no anchor and no window:
// a run's heartbeat age proves nothing once the operator has proved the
// pod gone.
//
// The moved test applies the dead_instances reading when no current
// member names cellInstance, and the silent test applies the cell_alive
// reading when one does. Neither fires unless anchor is already older
// than that test's own window.
//
// The member resolution is what keeps a live second slot out of the
// moved reading. A tenant holds several members, so the scalar
// cell_instance names slot 0 alone, and a run owned by slot 1 would
// otherwise read as moved merely because its instance differs.
//
// A missing row answers livenessAlive: no reading is possible. This is
// the "one piece of code" design D9 requires: both the placement-aware
// Liveness seam and the interrupted-run sweep call it.
func cellLivenessGone(now, anchor time.Time, cellInstance string, row tenantPlacementLivenessRow,
	exists bool, grace time.Duration) livenessReading {
	if !exists {
		return livenessAlive
	}
	if cellInstance != "" {
		for _, d := range row.deadInstances {
			if d.Instance == cellInstance && confirmedGone(d, now, grace) {
				return livenessConfirmedGone
			}
		}
	}
	if _, isMember := memberForInstance(row.members, cellInstance); !isMember {
		if anchor.Before(now.Add(-movedHeartbeatWindow)) &&
			deadInstanceGone(row.deadInstances, cellInstance, now, movedHeartbeatWindow, grace) {
			return livenessMoved
		}
		return livenessAlive
	}
	if anchor.Before(now.Add(-silentHeartbeatWindow)) && currentInstanceGone(row, now, silentHeartbeatWindow) {
		return livenessSilent
	}
	return livenessAlive
}
