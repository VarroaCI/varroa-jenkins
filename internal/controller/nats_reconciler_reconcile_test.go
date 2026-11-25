package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// proxyReconcileCall is one ReconcileRequest action of the proxy and the
// subject that the action sends on.
type proxyReconcileCall struct {
	subject string
	call    func(context.Context) error
}

// proxyReconcileCalls lists the three ReconcileRequest actions of p against
// the controller ns/ctl on the core cluster.
func proxyReconcileCalls(p *NATSReconcilerProxy) map[string]proxyReconcileCall {
	return map[string]proxyReconcileCall{
		"reconcile": {bus.OperatorReconcileSubject("core"), func(ctx context.Context) error {
			return p.TriggerReconcile(ctx, "core", "ctl", "ns")
		}},
		"nudge": {bus.OperatorNudgeSubject("core"), func(ctx context.Context) error {
			return p.WakeController(ctx, "core", "ns", "ctl")
		}},
		"reprovision": {bus.OperatorReprovisionSubject("core"), func(ctx context.Context) error {
			return p.Reprovision(ctx, "core", "ns", "ctl")
		}},
	}
}

// serveReconcileReply answers every request on subject with resp, and
// records the last request.
func serveReconcileReply(t *testing.T, conn *bus.Conn, subject string, resp bus.ReconcileResponse) *bus.ReconcileRequest {
	t.Helper()
	var got bus.ReconcileRequest
	r, err := conn.ServeRequests(subject, "", func(data []byte) []byte {
		_ = json.Unmarshal(data, &got)
		out, _ := json.Marshal(resp)
		return out
	})
	if err != nil {
		t.Fatalf("serve %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = r.Unsubscribe() })
	return &got
}

// TestNATSReconcilerProxy_ReconcileActionsReturnNoResponder pins that each
// action returns the no-responder error when no operator subscribes, so the
// BFF can answer 503.
func TestNATSReconcilerProxy_ReconcileActionsReturnNoResponder(t *testing.T) {
	p := NewNATSReconcilerProxy(startTestSyncBus(t))
	for name, a := range proxyReconcileCalls(p) {
		t.Run(name, func(t *testing.T) {
			if err := a.call(context.Background()); !bus.IsNoResponder(err) {
				t.Fatalf("err = %v, want a no-responder error", err)
			}
		})
	}
}

// TestNATSReconcilerProxy_ReconcileActionsReadTheReply pins that each action
// sends the controller, returns nil for an empty reply, and returns the
// operator's refusal as an *ActionError with the operator's code and text.
func TestNATSReconcilerProxy_ReconcileActionsReadTheReply(t *testing.T) {
	conn := startTestSyncBus(t)
	p := NewNATSReconcilerProxy(conn)
	for name, a := range proxyReconcileCalls(p) {
		t.Run(name+"/accepted", func(t *testing.T) {
			got := serveReconcileReply(t, conn, a.subject, bus.ReconcileResponse{})
			if err := a.call(context.Background()); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if *got != (bus.ReconcileRequest{Name: "ctl", Namespace: "ns"}) {
				t.Errorf("request = %+v, want ns/ctl", *got)
			}
		})
		t.Run(name+"/refused", func(t *testing.T) {
			serveReconcileReply(t, conn, a.subject, bus.ReconcileResponse{Error: "bad request"})
			err := a.call(context.Background())
			if err == nil || bus.IsNoReply(err) || !strings.Contains(err.Error(), "bad request") {
				t.Fatalf("err = %v, want the operator's refusal", err)
			}
		})
		t.Run(name+"/not_found", func(t *testing.T) {
			serveReconcileReply(t, conn, a.subject, bus.ReconcileResponse{Error: `controllers "ctl" not found`, Code: bus.CodeNotFound})
			err := a.call(context.Background())
			var ae *ActionError
			if !errors.As(err, &ae) || ae.Code != bus.CodeNotFound || ae.Msg != `controllers "ctl" not found` {
				t.Fatalf("err = %#v, want an *ActionError with code %q and the operator's text", err, bus.CodeNotFound)
			}
		})
	}
}
