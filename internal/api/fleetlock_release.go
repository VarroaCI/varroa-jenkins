package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fleetLockReleaseTimeout bounds the wait for the recorder's reply. It is
// longer than the recorder's own database bound, so a recorder that
// commits a release answers before the wait ends.
const fleetLockReleaseTimeout = 15 * time.Second

// ErrFleetLockReleaseNotDelivered reports that no recorder received the
// request, so no row changed.
var ErrFleetLockReleaseNotDelivered = errors.New("no record store received the fleet lock release")

// ErrFleetLockReleaseFailed reports that the recorder answered an error
// reply that it raised before its commit, so no row changed. An error
// reply from the commit itself is an unknown outcome instead.
var ErrFleetLockReleaseFailed = errors.New("the record store refused the fleet lock release")

// FleetLockReleaser sends one force-release request to the recorder and
// answers its reply. The recorder owns every write to varroa.fleet_locks.
// An error wraps ErrFleetLockReleaseNotDelivered or ErrFleetLockReleaseFailed
// when no row changed. Every other error leaves the outcome unknown: the
// recorder can have released the row.
type FleetLockReleaser interface {
	ForceRelease(ctx context.Context, req recordstore.FleetLockForceReleaseRequest) (recordstore.FleetLockForceReleaseResponse, error)
}

// busFleetLockReleaser is the FleetLockReleaser over the core NATS
// request-reply subject bus.FleetLockReleaseSubject.
type busFleetLockReleaser struct {
	conn *bus.Conn
}

// NewBusFleetLockReleaser answers a FleetLockReleaser over conn.
func NewBusFleetLockReleaser(conn *bus.Conn) FleetLockReleaser {
	return &busFleetLockReleaser{conn: conn}
}

func (b *busFleetLockReleaser) ForceRelease(ctx context.Context,
	req recordstore.FleetLockForceReleaseRequest) (recordstore.FleetLockForceReleaseResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return recordstore.FleetLockForceReleaseResponse{}, fmt.Errorf("%w: encode: %v", ErrFleetLockReleaseNotDelivered, err)
	}
	msg, err := b.conn.RequestWithContext(ctx, bus.FleetLockReleaseSubject, data, fleetLockReleaseTimeout)
	if err != nil {
		if bus.IsNoResponder(err) {
			return recordstore.FleetLockForceReleaseResponse{}, fmt.Errorf("%w: %v", ErrFleetLockReleaseNotDelivered, err)
		}
		return recordstore.FleetLockForceReleaseResponse{}, fmt.Errorf("request fleet lock release: %w", err)
	}
	var resp recordstore.FleetLockForceReleaseResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return recordstore.FleetLockForceReleaseResponse{}, fmt.Errorf("decode fleet lock release reply: %w", err)
	}
	if err := releaseReplyError(resp); err != nil {
		return recordstore.FleetLockForceReleaseResponse{}, err
	}
	return resp, nil
}

// releaseReplyError answers the error an error reply carries. A fault at the
// commit is an unknown outcome, so it does not wrap ErrFleetLockReleaseFailed.
func releaseReplyError(resp recordstore.FleetLockForceReleaseResponse) error {
	switch {
	case resp.Error == "":
		return nil
	case resp.Unknown:
		return fmt.Errorf("fleet lock release outcome unknown: %s", resp.Error)
	default:
		return fmt.Errorf("%w: %s", ErrFleetLockReleaseFailed, resp.Error)
	}
}

// FleetLockReleaseResult is the released row, the shape POST
// /locks/release answers (components/schemas.yaml#FleetLockReleaseResponse).
type FleetLockReleaseResult struct {
	Name         string `json:"name"`
	RunID        string `json:"runId"`
	Cluster      string `json:"cluster"`
	Tenant       string `json:"tenant"`
	JobPath      string `json:"jobPath"`
	CellInstance string `json:"cellInstance,omitempty"`
	State        string `json:"state"`
}

// FleetLockHolderCell is the cell that still runs a releasing lock's
// holder, the holderCell a holder_live refusal of POST /locks/release
// carries (components/schemas.yaml#FleetLockHolderCell). A restart of
// that cell ends the holder, and the confirmed-gone path then releases
// the lock.
type FleetLockHolderCell struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// fleetLockReleaseBody is the request body of POST /locks/release.
type fleetLockReleaseBody struct {
	Name   string `json:"name"`
	RunID  string `json:"runId"`
	Reason string `json:"reason"`
}

// ReleaseFleetLock force-releases one releasing fleet lock. The route and
// the release_fleet_lock MCP tool both call it, so they authorize, refuse,
// and audit identically. source names the surface in the activity row.
//
// The caller needs the cluster-wide fleetlocks:release verb. A release
// writes one fleetlock.force_released activity row on the global subject,
// after the recorder confirms the delete. A request the recorder can have
// acted on with no reply writes one fleetlock.force_release_unconfirmed row
// instead, so a committed release never lacks an audit record.
//
// The wait for the reply ignores the caller's cancellation. A caller that
// disconnects after the request goes out must not stop the audit.
func (s *Server) ReleaseFleetLock(ctx context.Context, claims *auth.Claims, name, runID, reason,
	source string) (*FleetLockReleaseResult, *ServiceError) {
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReleaseFleetLocks(claims) {
		return nil, &ServiceError{Status: http.StatusForbidden, Message: "forbidden"}
	}
	name, runID, reason = strings.TrimSpace(name), strings.TrimSpace(runID), strings.TrimSpace(reason)
	if name == "" || runID == "" || reason == "" {
		return nil, &ServiceError{Status: http.StatusBadRequest, Message: "name, runId, and reason are required"}
	}
	if s.deps.FleetLockReleaser == nil || s.deps.RecordStore == nil {
		return nil, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}

	actor := ActorFrom(claims)
	lockCluster, serr := s.fleetLockCluster(ctx, name, runID)
	if ctx.Err() != nil {
		// The caller left before the request went out, so nothing is sent
		// and nothing changes. After this point the wait is detached. The
		// check comes first, because a cancelled read fails as well.
		return nil, &ServiceError{Status: http.StatusServiceUnavailable, Message: "the request ended before the release was sent"}
	}
	if serr != nil {
		return nil, serr
	}
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fleetLockReleaseTimeout)
	defer cancel()
	resp, err := s.deps.FleetLockReleaser.ForceRelease(waitCtx, recordstore.FleetLockForceReleaseRequest{
		Name: name, RunID: runID, Actor: actor, Reason: reason,
	})
	if err != nil {
		s.deps.Logger.Error("fleet lock force release", "name", name, "run_id", runID, "error", err)
		if errors.Is(err, ErrFleetLockReleaseNotDelivered) || errors.Is(err, ErrFleetLockReleaseFailed) {
			return nil, &ServiceError{Status: http.StatusServiceUnavailable, Message: "the record store did not release the lock"}
		}
		s.notifyActivity(activity.Event{
			Type:     "fleetlock.force_release_unconfirmed",
			Source:   source,
			Severity: "warning",
			Actor:    actor,
			Cluster:  lockCluster,
			Reason:   reason,
			Message: fmt.Sprintf("fleet lock %s of run %s: force release by %s sent with no reply; the lock can be released: %s",
				name, runID, actor, reason),
		})
		return nil, &ServiceError{Status: http.StatusServiceUnavailable,
			Message: "the record store did not confirm the release; read the lock again before you retry"}
	}

	switch resp.Outcome {
	case recordstore.FleetLockForceReleased:
	case recordstore.FleetLockForceNotFound:
		return nil, &ServiceError{Status: http.StatusNotFound, Message: "fleet lock not found"}
	case recordstore.FleetLockForceNotReleasing:
		return nil, &ServiceError{Status: http.StatusConflict,
			Message: fmt.Sprintf("the lock is %s; only a releasing lock can be force-released", resp.State)}
	case recordstore.FleetLockForceHolderLive:
		return nil, &ServiceError{Status: http.StatusConflict,
			Message: fmt.Sprintf("cell %s/%s on cluster %s still runs the holder's pod %s. "+
				"Restart that cell when its Jenkins does not run. The lock then releases with no force release",
				resp.HolderNamespace, resp.HolderCell, resp.Cluster, resp.CellInstance),
			HolderCell: &FleetLockHolderCell{Cluster: resp.Cluster, Namespace: resp.HolderNamespace, Name: resp.HolderCell}}
	default:
		return nil, &ServiceError{Status: http.StatusInternalServerError,
			Message: fmt.Sprintf("unknown release outcome %q", resp.Outcome)}
	}

	s.notifyActivity(activity.Event{
		Type:     "fleetlock.force_released",
		Source:   source,
		Severity: "warning",
		Actor:    actor,
		Cluster:  resp.Cluster,
		Reason:   reason,
		Message: fmt.Sprintf("fleet lock %s of run %s (tenant %s) force-released by %s: %s",
			name, runID, resp.Tenant, actor, reason),
	})
	return &FleetLockReleaseResult{
		Name: name, RunID: runID, Cluster: resp.Cluster, Tenant: resp.Tenant, JobPath: resp.JobPath,
		CellInstance: resp.CellInstance, State: recordstore.FleetLockStateReleased,
	}, nil
}

// fleetLockCluster answers the cluster of the lock row that name and runID
// identify, read before the release. The unconfirmed audit row needs it,
// because no reply carries it. The read fails closed: a failed read answers
// 503 and a missing row answers 404, and no request goes out for either.
func (s *Server) fleetLockCluster(ctx context.Context, name, runID string) (string, *ServiceError) {
	locks, err := s.deps.RecordStore.ListAllFleetLocks(ctx)
	if err != nil {
		s.deps.Logger.Warn("read the fleet lock before its force release", "name", name, "run_id", runID, "error", err)
		return "", &ServiceError{Status: http.StatusServiceUnavailable, Message: "the record store could not read the lock"}
	}
	for _, l := range locks {
		if l.Name == name && l.RunID == runID {
			return l.Cluster, nil
		}
	}
	return "", &ServiceError{Status: http.StatusNotFound, Message: "fleet lock not found"}
}

// handleReleaseFleetLock serves POST /locks/release.
func (s *Server) handleReleaseFleetLock(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReleaseFleetLocks(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body fleetLockReleaseBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, serr := s.ReleaseFleetLock(r.Context(), claims, body.Name, body.RunID, body.Reason, "user")
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
