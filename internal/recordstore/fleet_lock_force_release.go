// Package recordstore: the operator force-release of a releasing fleet
// lock. It is the one release path no holder and no confirmation drives,
// so it accepts a releasing row only, and only when the stored placement
// names no live pod that can still run the holder.
package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// fleetLockReleaseQueueGroup is the queue group the force-release
// responder subscribes with, so one recorder replica answers each request.
const fleetLockReleaseQueueGroup = "recorder"

// fleetLockReleaseTimeout bounds one force-release call's database work.
const fleetLockReleaseTimeout = 10 * time.Second

// The outcomes a force-release answers.
const (
	// FleetLockForceReleased reports that the row was deleted.
	FleetLockForceReleased = "released"
	// FleetLockForceNotFound reports that no row has the name and run id.
	FleetLockForceNotFound = "not_found"
	// FleetLockForceNotReleasing reports a held or waiting row. The
	// holder's own release and the lease own those states.
	FleetLockForceNotReleasing = "not_releasing"
	// FleetLockForceHolderLive reports that the stored placement still
	// names the row's cell instance as a member. That pod can still run
	// the holder's build.
	FleetLockForceHolderLive = "holder_live"
)

// ErrFleetLockReleaseCommitUnknown reports a failed commit. Postgres can
// have committed the delete before the acknowledgment was lost, so the
// outcome is unknown.
var ErrFleetLockReleaseCommitUnknown = errors.New("fleet lock force release commit outcome unknown")

// FleetLockForceReleaseRequest is the request body on
// bus.FleetLockReleaseSubject. Actor and Reason are for the audit log.
type FleetLockForceReleaseRequest struct {
	Name   string `json:"name"`
	RunID  string `json:"run_id"`
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

// FleetLockForceReleaseResponse is the reply body. Outcome is one of the
// FleetLockForce values, and the row fields describe the row the request
// named. Error is set only for a malformed request or a server fault.
// Unknown is set beside Error when the fault came at the commit, so the
// row can be gone.
type FleetLockForceReleaseResponse struct {
	Outcome      string `json:"outcome,omitempty"`
	Cluster      string `json:"cluster,omitempty"`
	Tenant       string `json:"tenant,omitempty"`
	JobPath      string `json:"job_path,omitempty"`
	CellInstance string `json:"cell_instance,omitempty"`
	State        string `json:"state,omitempty"`
	Error        string `json:"error,omitempty"`
	Unknown      bool   `json:"unknown,omitempty"`
}

// ForceReleaseFleetLock deletes the releasing fleet_locks row that name
// and runID identify, in one transaction that holds the row FOR UPDATE.
// It answers FleetLockForceNotFound for no row, FleetLockForceNotReleasing
// for a row in another state, and FleetLockForceHolderLive when the stored
// placement row names the row's cell instance as a current member. A
// deleted row publishes one lock notification, after the commit, through
// notifier. A nil notifier publishes nothing.
func ForceReleaseFleetLock(ctx context.Context, pool *pgxpool.Pool, req FleetLockForceReleaseRequest,
	notifier *RunNotifier) (FleetLockForceReleaseResponse, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return FleetLockForceReleaseResponse{}, fmt.Errorf("begin fleet lock force release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var resp FleetLockForceReleaseResponse
	err = tx.QueryRow(ctx, `
		SELECT cluster, tenant, job_path, cell_instance, state FROM varroa.fleet_locks
		WHERE name = $1 AND run_id = $2
		FOR UPDATE
	`, req.Name, req.RunID).Scan(&resp.Cluster, &resp.Tenant, &resp.JobPath, &resp.CellInstance, &resp.State)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return FleetLockForceReleaseResponse{Outcome: FleetLockForceNotFound}, nil
	case err != nil:
		return FleetLockForceReleaseResponse{}, fmt.Errorf("read fleet lock %s/%s: %w", req.Name, req.RunID, err)
	}
	if resp.State != FleetLockStateReleasing {
		resp.Outcome = FleetLockForceNotReleasing
		return resp, nil
	}

	row, exists, err := readTenantPlacementLivenessRow(ctx, tx, resp.Cluster, resp.Tenant)
	if err != nil {
		return FleetLockForceReleaseResponse{}, err
	}
	if exists {
		if _, live := memberForInstance(row.members, resp.CellInstance); live {
			resp.Outcome = FleetLockForceHolderLive
			return resp, nil
		}
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2 AND state = 'releasing'
	`, req.Name, req.RunID); err != nil {
		return FleetLockForceReleaseResponse{}, fmt.Errorf("delete fleet lock %s/%s: %w", req.Name, req.RunID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return FleetLockForceReleaseResponse{}, fmt.Errorf("%w: %v", ErrFleetLockReleaseCommitUnknown, err)
	}
	notifier.Notify(lockNotification(resp.Cluster, resp.Tenant, resp.JobPath, req.Name, req.RunID, FleetLockStateReleased))
	resp.Outcome = FleetLockForceReleased
	resp.State = ""
	return resp, nil
}

// RegisterFleetLockReleaseResponder subscribes to
// bus.FleetLockReleaseSubject with queue group "recorder". notifier
// publishes the lock notification of each released row.
func RegisterFleetLockReleaseResponder(conn *bus.Conn, pool *pgxpool.Pool, notifier *RunNotifier,
	logger *slog.Logger) (*bus.Sub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := func(data []byte) []byte {
		return handleFleetLockRelease(pool, notifier, logger, data)
	}
	return conn.SubscribeRequest(bus.FleetLockReleaseSubject, fleetLockReleaseQueueGroup, handler)
}

// handleFleetLockRelease decodes one request and answers its reply body.
// A request with no name, no run id, no actor, or no reason writes nothing.
func handleFleetLockRelease(pool *pgxpool.Pool, notifier *RunNotifier, logger *slog.Logger, data []byte) []byte {
	var req FleetLockForceReleaseRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("fleet lock release request decode failed", "error", err)
		return marshalFleetLockRelease(logger, FleetLockForceReleaseResponse{Error: "malformed fleet lock release request"})
	}
	if req.Name == "" || req.RunID == "" || req.Actor == "" || req.Reason == "" {
		return marshalFleetLockRelease(logger, FleetLockForceReleaseResponse{
			Error: "a fleet lock release needs a name, a run id, an actor, and a reason"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), fleetLockReleaseTimeout)
	defer cancel()
	resp, err := ForceReleaseFleetLock(ctx, pool, req, notifier)
	if err != nil {
		logger.Error("fleet lock force release failed", "name", req.Name, "run_id", req.RunID, "error", err)
		return marshalFleetLockRelease(logger, FleetLockForceReleaseResponse{Error: "fleet lock force release failed",
			Unknown: errors.Is(err, ErrFleetLockReleaseCommitUnknown)})
	}
	logger.Info("fleet lock force release", "name", req.Name, "run_id", req.RunID, "outcome", resp.Outcome,
		"cluster", resp.Cluster, "tenant", resp.Tenant, "cell_instance", resp.CellInstance,
		"actor", req.Actor, "reason", req.Reason)
	return marshalFleetLockRelease(logger, resp)
}

// marshalFleetLockRelease encodes a reply body. An encode failure answers
// a fixed error body, so the requester never waits for a timeout.
func marshalFleetLockRelease(logger *slog.Logger, resp FleetLockForceReleaseResponse) []byte {
	data, err := json.Marshal(resp)
	if err != nil {
		logger.Error("fleet lock release reply encode failed", "error", err)
		return []byte(`{"error":"fleet lock release reply encode failed"}`)
	}
	return data
}
