package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

type fakeApproveRestarter struct {
	err error
	// left is the time to the context deadline at the call; zero means
	// the context had no deadline.
	left     time.Duration
	calls    []string // "ns/name/action"
	expected string
}

func (f *fakeApproveRestarter) ApproveRestart(ctx context.Context, _, namespace, name, action, expectedContentHash string) error {
	if d, ok := ctx.Deadline(); ok {
		f.left = time.Until(d)
	}
	f.calls = append(f.calls, namespace+"/"+name+"/"+action)
	f.expected = expectedContentHash
	return f.err
}

// TestHandleApproveRestart_NonOwnerSendsNoReply pins that a replica that
// does not own the controller sends no reply, so only the owner answers.
func TestHandleApproveRestart_NonOwnerSendsNoReply(t *testing.T) {
	rec := &fakeApproveRestarter{err: controller.ErrNotShardOwner}
	resp := handleApproveRestart(context.Background(), slog.Default(), rec, "core",
		mustJSON(t, bus.ApproveRestartRequest{Namespace: "team-a", Name: "ctrl", Action: "approve"}))
	if resp != nil {
		t.Fatalf("reply = %s, want none", resp)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("ApproveRestart calls = %v, want one", rec.calls)
	}
}

// TestHandleApproveRestart_OwnerReplies pins that the owner replies with
// an empty error on success and with the error text on a failure.
func TestHandleApproveRestart_OwnerReplies(t *testing.T) {
	req := mustJSON(t, bus.ApproveRestartRequest{Namespace: "team-a", Name: "ctrl", Action: "approve"})

	rec := &fakeApproveRestarter{}
	var ok bus.ApproveRestartResponse
	if err := json.Unmarshal(handleApproveRestart(context.Background(), slog.Default(), rec, "core", req), &ok); err != nil {
		t.Fatalf("decode success reply: %v", err)
	}
	if ok.Error != "" {
		t.Fatalf("success reply error = %q, want empty", ok.Error)
	}
	if len(rec.calls) != 1 || rec.calls[0] != "team-a/ctrl/approve" {
		t.Fatalf("ApproveRestart calls = %v, want [team-a/ctrl/approve]", rec.calls)
	}

	rec = &fakeApproveRestarter{err: errors.New("send desired state: boom")}
	var failed bus.ApproveRestartResponse
	if err := json.Unmarshal(handleApproveRestart(context.Background(), slog.Default(), rec, "core", req), &failed); err != nil {
		t.Fatalf("decode failure reply: %v", err)
	}
	if failed.Error != "send desired state: boom" || failed.Code != bus.CodeInternal {
		t.Fatalf("failure reply = %+v, want the error text with code internal", failed)
	}
}

// TestHandleApproveRestart_NoPendingRestartRepliesConflict pins that the
// refusal for a controller with no pending restart is a conflict that keeps
// its message.
func TestHandleApproveRestart_NoPendingRestartRepliesConflict(t *testing.T) {
	rec := &fakeApproveRestarter{err: fmt.Errorf("%w for team-a/ctrl", controller.ErrNoPendingRestart)}
	var resp bus.ApproveRestartResponse
	req := mustJSON(t, bus.ApproveRestartRequest{Namespace: "team-a", Name: "ctrl", Action: "approve"})
	if err := json.Unmarshal(handleApproveRestart(context.Background(), slog.Default(), rec, "core", req), &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if resp.Code != bus.CodeConflict || resp.Error != "no pending restart for team-a/ctrl" || len(resp.Changes) != 0 {
		t.Fatalf("reply = %+v, want a conflict with the message and no changes", resp)
	}
}

// TestHandleApproveRestart_ContentChangedRepliesConflictWithChanges pins
// that a refusal for a changed pending change is a conflict that names the
// new sections, and that the handler passes the reviewed hash through.
func TestHandleApproveRestart_ContentChangedRepliesConflictWithChanges(t *testing.T) {
	rec := &fakeApproveRestarter{err: &controller.PendingRestartChangedError{Changes: []string{"config", "rbac"}}}
	var resp bus.ApproveRestartResponse
	req := mustJSON(t, bus.ApproveRestartRequest{Namespace: "team-a", Name: "ctrl", Action: "approve", ExpectedContentHash: "seen"})
	if err := json.Unmarshal(handleApproveRestart(context.Background(), slog.Default(), rec, "core", req), &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if resp.Code != bus.CodeConflict || len(resp.Changes) != 2 || resp.Changes[0] != "config" || resp.Changes[1] != "rbac" {
		t.Fatalf("reply = %+v, want a conflict that names config and rbac", resp)
	}
	if resp.Error == "" {
		t.Fatal("reply carries no error text")
	}
	if rec.expected != "seen" {
		t.Fatalf("expected content hash = %q, want seen", rec.expected)
	}
}

// TestHandleApproveRestart_SetsARequestDeadline pins that the handler gives the
// approve a deadline under the requester's timeout, so the approve cannot
// hold the controller lock without a limit.
func TestHandleApproveRestart_SetsARequestDeadline(t *testing.T) {
	rec := &fakeApproveRestarter{}
	handleApproveRestart(context.Background(), slog.Default(), rec, "core", mustJSON(t, bus.ApproveRestartRequest{Namespace: "team-a", Name: "ctrl", Action: "approve"}))
	if rec.left <= 0 || rec.left > approveRequestTimeout {
		t.Fatalf("deadline left = %v, want a deadline of at most %v", rec.left, approveRequestTimeout)
	}
}
