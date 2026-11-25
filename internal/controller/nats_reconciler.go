package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

const (
	natsReconcileTimeout = 5 * time.Second  // fast operation (controller read, channel send, or annotation patch)
	natsApproveTimeout   = 60 * time.Second // real work (bundle resolve, CRD patch, mite send)
)

// NATSReconcilerProxy implements ReconcilerAPI by forwarding requests over NATS
// to the operator binary. The BFF has no local Reconciler, so it uses this
// proxy for every cluster, its own included.
// The target cluster is always the per-call argument — the proxy itself is
// cluster-agnostic so one instance serves the whole brood.
type NATSReconcilerProxy struct {
	conn *bus.Conn
}

// NewNATSReconcilerProxy creates a new proxy that relays reconciler commands
// to the operator over the NATS bus.
func NewNATSReconcilerProxy(conn *bus.Conn) *NATSReconcilerProxy {
	return &NATSReconcilerProxy{conn: conn}
}

// TriggerReconcile relays an on-demand reconcile to the operator of cluster
// and waits for its reply.
func (p *NATSReconcilerProxy) TriggerReconcile(ctx context.Context, cluster, name, namespace string) error {
	return p.requestReconcile(ctx, "reconcile", bus.OperatorReconcileSubject(cluster), namespace, name)
}

// WakeController relays a per-controller goroutine wake-up to the operator
// of cluster and waits for its reply.
func (p *NATSReconcilerProxy) WakeController(ctx context.Context, cluster, namespace, name string) error {
	return p.requestReconcile(ctx, "nudge", bus.OperatorNudgeSubject(cluster), namespace, name)
}

// Reprovision relays a force-reprovision (desired-state re-push) to the
// operator of cluster and waits for its reply.
func (p *NATSReconcilerProxy) Reprovision(ctx context.Context, cluster, namespace, name string) error {
	return p.requestReconcile(ctx, "reprovision", bus.OperatorReprovisionSubject(cluster), namespace, name)
}

// requestReconcile sends a ReconcileRequest on subject and decodes the
// ReconcileResponse. The operator replies after it records the request, not
// after the reconcile ends. A refusal returns an *ActionError with the
// operator's code and text. A request that gets no reply returns the bus
// error wrapped, so bus.IsNoResponder and bus.IsNoReply still match it.
func (p *NATSReconcilerProxy) requestReconcile(ctx context.Context, op, subject, namespace, name string) error {
	data, _ := json.Marshal(bus.ReconcileRequest{Name: name, Namespace: namespace})
	respMsg, err := p.conn.RequestWithContext(ctx, subject, data, natsReconcileTimeout)
	if err != nil {
		return fmt.Errorf("%s relay: %w", op, err)
	}
	var resp bus.ReconcileResponse
	if err := json.Unmarshal(respMsg.Data, &resp); err != nil {
		return fmt.Errorf("%s decode response: %w", op, err)
	}
	if resp.Error != "" {
		return &ActionError{Code: resp.Code, Msg: resp.Error}
	}
	return nil
}

// ApproveRestart relays an approve-restart command to the operator and returns
// the result. It respects the caller's context deadline if one is set, and
// cancels the in-flight NATS request if the context is cancelled.
func (p *NATSReconcilerProxy) ApproveRestart(ctx context.Context, cluster, namespace, name, action, expectedContentHash string) error {
	// If the context is already done, fail fast.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("approve restart: %w", err)
	}

	req := bus.ApproveRestartRequest{
		Name:                name,
		Namespace:           namespace,
		Action:              action,
		ExpectedContentHash: expectedContentHash,
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
	if resp.Error == "" {
		return nil
	}
	if resp.Code == bus.CodeConflict && len(resp.Changes) > 0 {
		return &PendingRestartChangedError{Changes: resp.Changes}
	}
	if resp.Code != "" {
		return &ActionError{Code: resp.Code, Msg: resp.Error}
	}
	return fmt.Errorf("%s", resp.Error)
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

// ActionError is a refusal the target operator replied with for a
// request-reply controller action. Code mirrors the bus error code the
// operator sent ("conflict", "not_found", "invalid", "internal"), so the BFF
// can map it to an HTTP status without string matching.
type ActionError struct {
	Code string
	Msg  string
}

func (e *ActionError) Error() string { return e.Msg }

// Hibernate relays an authenticated hibernate action to the target cluster's
// operator and returns the result.
func (p *NATSReconcilerProxy) Hibernate(ctx context.Context, cluster, namespace, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("hibernate: %w", err)
	}

	req := bus.HibernateRequest{Name: name, Namespace: namespace}
	data, _ := json.Marshal(req)

	timeout := natsApproveTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d <= 0 {
			return fmt.Errorf("hibernate: context deadline already passed")
		} else if d < timeout {
			timeout = d
		}
	}

	respMsg, err := p.conn.RequestWithContext(ctx, bus.OperatorHibernateSubject(cluster), data, timeout)
	if err != nil {
		return fmt.Errorf("hibernate relay: %w", err)
	}

	var resp bus.HibernateResponse
	if err := json.Unmarshal(respMsg.Data, &resp); err != nil {
		return fmt.Errorf("hibernate decode response: %w", err)
	}
	if resp.Error != "" {
		return &ActionError{Code: resp.Code, Msg: resp.Error}
	}
	return nil
}

// Wake relays an authenticated wake action to the target cluster's operator
// and returns the result.
func (p *NATSReconcilerProxy) Wake(ctx context.Context, cluster, namespace, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wake: %w", err)
	}

	req := bus.WakeRequest{Name: name, Namespace: namespace}
	data, _ := json.Marshal(req)

	timeout := natsApproveTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d <= 0 {
			return fmt.Errorf("wake: context deadline already passed")
		} else if d < timeout {
			timeout = d
		}
	}

	respMsg, err := p.conn.RequestWithContext(ctx, bus.OperatorWakeSubject(cluster), data, timeout)
	if err != nil {
		return fmt.Errorf("wake relay: %w", err)
	}

	var resp bus.WakeResponse
	if err := json.Unmarshal(respMsg.Data, &resp); err != nil {
		return fmt.Errorf("wake decode response: %w", err)
	}
	if resp.Error != "" {
		return &ActionError{Code: resp.Code, Msg: resp.Error}
	}
	return nil
}
