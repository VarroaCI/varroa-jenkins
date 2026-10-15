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

// approveDeleter is the slice of *controller.Reconciler that the approve
// deletion handler needs, kept as an interface so the handler is testable
// without a full Reconciler.
type approveDeleter interface {
	ApproveDeletion(ctx context.Context, cluster, namespace, name, path string) error
}

// handleApproveDeletion implements the operator.<cluster>.approvedeletion
// request-reply handler. Every replica receives each request. A replica that
// does not own the controller returns nil, so it sends no reply and the
// owner's reply is the only one. With no owner, the requester times out.
func handleApproveDeletion(ctx context.Context, logger *slog.Logger, rec approveDeleter, cluster string, data []byte) []byte {
	var req bus.ApproveDeletionRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("approve deletion request decode failed", "error", err)
		resp, _ := json.Marshal(bus.ApproveDeletionResponse{Error: "invalid request"})
		return resp
	}
	ctx, cancel := context.WithTimeout(ctx, approveRequestTimeout)
	defer cancel()
	if err := rec.ApproveDeletion(ctx, cluster, req.Namespace, req.Name, req.Path); err != nil {
		if errors.Is(err, controller.ErrNotShardOwner) {
			return nil
		}
		logger.Warn("approve deletion failed",
			"controller", req.Namespace+"/"+req.Name,
			"path", req.Path, "error", err)
		resp, _ := json.Marshal(bus.ApproveDeletionResponse{Error: err.Error()})
		return resp
	}
	resp, _ := json.Marshal(bus.ApproveDeletionResponse{})
	return resp
}

// approveDeletionRunner serves approve deletion requests on every replica.
// It subscribes with no queue group, because only the shard owner of a
// controller may act on its approve, and a queue group would hand the
// request to one replica that is not always the owner.
type approveDeletionRunner struct {
	busConn    *bus.Conn
	cluster    string
	reconciler approveDeleter
	logger     *slog.Logger
}

func (r *approveDeletionRunner) NeedLeaderElection() bool { return false }

func (r *approveDeletionRunner) Start(ctx context.Context) error {
	sub, err := r.busConn.ServeRequestsConcurrently(bus.OperatorApproveDeletionSubject(r.cluster), "", approveConcurrency, func(data []byte) []byte {
		return handleApproveDeletion(ctx, r.logger, r.reconciler, r.cluster, data)
	})
	if err != nil {
		return fmt.Errorf("subscribe approve deletion: %w", err)
	}
	<-ctx.Done()
	if err := sub.Unsubscribe(); err != nil {
		r.logger.Warn("unsubscribe approve deletion failed", "error", err)
	}
	return nil
}
