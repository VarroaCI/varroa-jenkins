package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// serveApproveReply answers every approve request on the core cluster's
// subject with resp, and records the last request.
func serveApproveReply(t *testing.T, conn *bus.Conn, resp bus.ApproveRestartResponse) *bus.ApproveRestartRequest {
	t.Helper()
	var got bus.ApproveRestartRequest
	r, err := conn.ServeRequests(bus.OperatorApproveSubject("core"), "", func(data []byte) []byte {
		_ = json.Unmarshal(data, &got)
		out, _ := json.Marshal(resp)
		return out
	})
	if err != nil {
		t.Fatalf("serve approve: %v", err)
	}
	t.Cleanup(func() { _ = r.Unsubscribe() })
	return &got
}

// TestNATSReconcilerProxy_ApproveRestartDecodesContentChanged pins that the
// proxy sends the reviewed hash and turns a conflict that names sections
// into a PendingRestartChangedError.
func TestNATSReconcilerProxy_ApproveRestartDecodesContentChanged(t *testing.T) {
	conn := startTestSyncBus(t)
	got := serveApproveReply(t, conn, bus.ApproveRestartResponse{
		Error: "changed", Code: bus.CodeConflict, Changes: []string{"items"},
	})

	err := NewNATSReconcilerProxy(conn).ApproveRestart(context.Background(), "core", "ns", "ctl", "approve", "seen")
	var changed *PendingRestartChangedError
	if !errors.As(err, &changed) || len(changed.Changes) != 1 || changed.Changes[0] != "items" {
		t.Fatalf("err = %v, want a PendingRestartChangedError for [items]", err)
	}
	if got.ExpectedContentHash != "seen" || got.Action != "approve" {
		t.Fatalf("request = %+v, want action approve with the reviewed hash", got)
	}
}

// TestNATSReconcilerProxy_ApproveRestartDecodesACodedRefusal pins that a
// coded refusal with no sections is an ActionError.
func TestNATSReconcilerProxy_ApproveRestartDecodesACodedRefusal(t *testing.T) {
	conn := startTestSyncBus(t)
	serveApproveReply(t, conn, bus.ApproveRestartResponse{Error: "no pending restart for ns/ctl", Code: bus.CodeConflict})

	err := NewNATSReconcilerProxy(conn).ApproveRestart(context.Background(), "core", "ns", "ctl", "approve", "")
	var ae *ActionError
	if !errors.As(err, &ae) || ae.Code != bus.CodeConflict || ae.Msg != "no pending restart for ns/ctl" {
		t.Fatalf("err = %v, want a conflict ActionError with the message", err)
	}
}
