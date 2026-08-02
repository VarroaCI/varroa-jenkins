package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

const (
	natsReconcileTimeout = 5 * time.Second  // fast operation (channel send + wake)
	natsApproveTimeout   = 60 * time.Second // real work (bundle resolve, CRD patch, mite send)
)

// NATSReconcilerProxy implements ReconcilerAPI by forwarding requests over NATS
// to the operator binary. Used in split-mode deployments where the BFF does not
// have a local Reconciler.
// The target cluster is always the per-call argument — the proxy itself is
// cluster-agnostic so one instance serves the whole brood.
type NATSReconcilerProxy struct {
	conn   *bus.Conn
	logger *slog.Logger
}

// NewNATSReconcilerProxy creates a new proxy that relays reconciler commands
// to the operator over the NATS bus.
func NewNATSReconcilerProxy(conn *bus.Conn) *NATSReconcilerProxy {
	return &NATSReconcilerProxy{
		conn:   conn,
		logger: slog.Default().With("component", "nats_reconciler_proxy"),
	}
}

// SetLogger sets the logger for the proxy.
func (p *NATSReconcilerProxy) SetLogger(l *slog.Logger) {
	p.logger = l.With("component", "nats_reconciler_proxy")
}

// TriggerReconcile relays an on-demand reconcile to the operator. Errors are
// logged but not returned — this is a best-effort fire-and-forget signal.
func (p *NATSReconcilerProxy) TriggerReconcile(cluster, name, namespace string) {
	req := bus.ReconcileRequest{Name: name, Namespace: namespace}
	data, _ := json.Marshal(req)
	_, err := p.conn.Request(bus.OperatorReconcileSubject(cluster), data, natsReconcileTimeout)
	if err != nil {
		p.logger.Warn("trigger reconcile relay failed",
			"controller", namespace+"/"+name,
			"error", err)
	}
}

// WakeController relays a per-controller goroutine wake-up to the operator.
// Errors are logged but not returned — this is a best-effort signal.
func (p *NATSReconcilerProxy) WakeController(cluster, namespace, name string) {
	req := bus.ReconcileRequest{Name: name, Namespace: namespace}
	data, _ := json.Marshal(req)
	_, err := p.conn.Request(bus.OperatorWakeSubject(cluster), data, natsReconcileTimeout)
	if err != nil {
		p.logger.Warn("wake controller relay failed",
			"controller", namespace+"/"+name,
			"error", err)
	}
}

// Reprovision relays a force-reprovision (desired-state re-push) to the operator.
// Errors are logged but not returned — this is a best-effort signal.
func (p *NATSReconcilerProxy) Reprovision(cluster, namespace, name string) {
	req := bus.ReconcileRequest{Name: name, Namespace: namespace}
	data, _ := json.Marshal(req)
	_, err := p.conn.Request(bus.OperatorReprovisionSubject(cluster), data, natsReconcileTimeout)
	if err != nil {
		p.logger.Warn("reprovision relay failed",
			"controller", namespace+"/"+name,
			"error", err)
	}
}

// ApproveRestart relays an approve-restart command to the operator and returns
// the result. It respects the caller's context deadline if one is set, and
// cancels the in-flight NATS request if the context is cancelled.
func (p *NATSReconcilerProxy) ApproveRestart(ctx context.Context, cluster, namespace, name, action string) error {
	// If the context is already done, fail fast.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("approve restart: %w", err)
	}

	req := bus.ApproveRestartRequest{
		Name:      name,
		Namespace: namespace,
		Action:    action,
	}
	data, _ := json.Marshal(req)

	timeout := natsApproveTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d <= 0 {
			return fmt.Errorf("approve restart: context deadline already passed")
		} else if d < timeout {
			timeout = d
		}
	}

	respMsg, err := p.conn.RequestWithContext(ctx, bus.OperatorApproveSubject(cluster), data, timeout)
	if err != nil {
		return fmt.Errorf("approve restart relay: %w", err)
	}

	var resp bus.ApproveRestartResponse
	if err := json.Unmarshal(respMsg.Data, &resp); err != nil {
		return fmt.Errorf("approve restart decode response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// ApproveDeletion relays an approve-deletion command to the operator and returns
// the result.
func (p *NATSReconcilerProxy) ApproveDeletion(ctx context.Context, cluster, namespace, name, path string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("approve deletion: %w", err)
	}

	req := bus.ApproveDeletionRequest{
		Name:      name,
		Namespace: namespace,
		Path:      path,
	}
	data, _ := json.Marshal(req)

	timeout := natsApproveTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d <= 0 {
			return fmt.Errorf("approve deletion: context deadline already passed")
		} else if d < timeout {
			timeout = d
		}
	}

	respMsg, err := p.conn.RequestWithContext(ctx, bus.OperatorApproveDeletionSubject(cluster), data, timeout)
	if err != nil {
		return fmt.Errorf("approve deletion relay: %w", err)
	}

	var resp bus.ApproveDeletionResponse
	if err := json.Unmarshal(respMsg.Data, &resp); err != nil {
		return fmt.Errorf("approve deletion decode response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}
