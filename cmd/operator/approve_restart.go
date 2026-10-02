package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

// approveRestarter is the slice of *controller.Reconciler that the approve
// restart handler needs, kept as an interface so the handler is testable
// without a full Reconciler.
type approveRestarter interface {
	ApproveRestart(ctx context.Context, cluster, namespace, name, action string) error
}

// handleApproveRestart implements the operator.<cluster>.approverestart
// request-reply handler. Every replica receives each request. A replica that
// does not own the controller returns nil, so it sends no reply and the
// owner's reply is the only one. With no owner, the requester times out.
func handleApproveRestart(ctx context.Context, logger *slog.Logger, rec approveRestarter, cluster string, data []byte) []byte {
	var req bus.ApproveRestartRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("approve restart request decode failed", "error", err)
		resp, _ := json.Marshal(bus.ApproveRestartResponse{Error: "invalid request"})
		return resp
	}
	if err := rec.ApproveRestart(ctx, cluster, req.Namespace, req.Name, req.Action); err != nil {
		if errors.Is(err, controller.ErrNotShardOwner) {
			return nil
		}
		logger.Warn("approve restart failed",
			"controller", req.Namespace+"/"+req.Name,
			"action", req.Action, "error", err)
		resp, _ := json.Marshal(bus.ApproveRestartResponse{Error: err.Error()})
		return resp
	}
	resp, _ := json.Marshal(bus.ApproveRestartResponse{})
	return resp
}

// approveRestartRunner serves approve restart requests on every replica.
// It subscribes with no queue group, because only the shard owner of a
// controller may act on its approve, and a queue group would hand the
// request to one replica that is not always the owner.
type approveRestartRunner struct {
	busConn    *bus.Conn
	cluster    string
	reconciler approveRestarter
	logger     *slog.Logger
}

func (r *approveRestartRunner) NeedLeaderElection() bool { return false }

func (r *approveRestartRunner) Start(ctx context.Context) error {
	sub, err := r.busConn.ServeRequests(bus.OperatorApproveSubject(r.cluster), "", func(data []byte) []byte {
		return handleApproveRestart(ctx, r.logger, r.reconciler, r.cluster, data)
	})
	if err != nil {
		return fmt.Errorf("subscribe approve restart: %w", err)
	}
	<-ctx.Done()
	if err := sub.Unsubscribe(); err != nil {
		r.logger.Warn("unsubscribe approve restart failed", "error", err)
	}
	return nil
}
