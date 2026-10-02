package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

type fakeApproveRestarter struct {
	err   error
	calls []string // "ns/name/action"
}

func (f *fakeApproveRestarter) ApproveRestart(_ context.Context, _, namespace, name, action string) error {
	f.calls = append(f.calls, namespace+"/"+name+"/"+action)
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

	rec = &fakeApproveRestarter{err: errors.New("no pending restart for team-a/ctrl")}
	var failed bus.ApproveRestartResponse
	if err := json.Unmarshal(handleApproveRestart(context.Background(), slog.Default(), rec, "core", req), &failed); err != nil {
		t.Fatalf("decode failure reply: %v", err)
	}
	if failed.Error != "no pending restart for team-a/ctrl" {
		t.Fatalf("failure reply error = %q, want the error text", failed.Error)
	}
}
