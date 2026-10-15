package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

type fakeApproveDeleter struct {
	err error
	// left is the time to the context deadline at the call; zero means
	// the context had no deadline.
	left  time.Duration
	calls []string // "ns/name/path"
}

func (f *fakeApproveDeleter) ApproveDeletion(ctx context.Context, _, namespace, name, path string) error {
	if d, ok := ctx.Deadline(); ok {
		f.left = time.Until(d)
	}
	f.calls = append(f.calls, namespace+"/"+name+"/"+path)
	return f.err
}

// TestHandleApproveDeletion_NonOwnerSendsNoReply pins that a replica that
// does not own the controller sends no reply, so only the owner answers.
func TestHandleApproveDeletion_NonOwnerSendsNoReply(t *testing.T) {
	rec := &fakeApproveDeleter{err: controller.ErrNotShardOwner}
	resp := handleApproveDeletion(context.Background(), slog.Default(), rec, "core",
		mustJSON(t, bus.ApproveDeletionRequest{Namespace: "team-a", Name: "ctrl", Path: "folder/job"}))
	if resp != nil {
		t.Fatalf("reply = %s, want none", resp)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("ApproveDeletion calls = %v, want one", rec.calls)
	}
}

// TestHandleApproveDeletion_OwnerReplies pins that the owner replies with
// an empty error on success and with the error text on a failure.
func TestHandleApproveDeletion_OwnerReplies(t *testing.T) {
	req := mustJSON(t, bus.ApproveDeletionRequest{Namespace: "team-a", Name: "ctrl", Path: "folder/job"})

	rec := &fakeApproveDeleter{}
	var ok bus.ApproveDeletionResponse
	if err := json.Unmarshal(handleApproveDeletion(context.Background(), slog.Default(), rec, "core", req), &ok); err != nil {
		t.Fatalf("decode success reply: %v", err)
	}
	if ok.Error != "" {
		t.Fatalf("success reply error = %q, want empty", ok.Error)
	}
	if len(rec.calls) != 1 || rec.calls[0] != "team-a/ctrl/folder/job" {
		t.Fatalf("ApproveDeletion calls = %v, want [team-a/ctrl/folder/job]", rec.calls)
	}

	rec = &fakeApproveDeleter{err: errors.New(`no pending deletion for "folder/job" on team-a/ctrl`)}
	var failed bus.ApproveDeletionResponse
	if err := json.Unmarshal(handleApproveDeletion(context.Background(), slog.Default(), rec, "core", req), &failed); err != nil {
		t.Fatalf("decode failure reply: %v", err)
	}
	if failed.Error != `no pending deletion for "folder/job" on team-a/ctrl` {
		t.Fatalf("failure reply error = %q, want the error text", failed.Error)
	}
}

// TestHandleApproveDeletion_InvalidRequest pins that an undecodable request
// gets an error reply and never reaches the reconciler.
func TestHandleApproveDeletion_InvalidRequest(t *testing.T) {
	rec := &fakeApproveDeleter{}
	var resp bus.ApproveDeletionResponse
	if err := json.Unmarshal(handleApproveDeletion(context.Background(), slog.Default(), rec, "core", []byte("{")), &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if resp.Error != "invalid request" {
		t.Fatalf("reply error = %q, want %q", resp.Error, "invalid request")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("ApproveDeletion calls = %v, want none", rec.calls)
	}
}

// TestHandleApproveDeletion_SetsARequestDeadline pins that the handler gives the
// approve a deadline under the requester's timeout, so the approve cannot
// hold the controller lock without a limit.
func TestHandleApproveDeletion_SetsARequestDeadline(t *testing.T) {
	rec := &fakeApproveDeleter{}
	handleApproveDeletion(context.Background(), slog.Default(), rec, "core", mustJSON(t, bus.ApproveDeletionRequest{Namespace: "team-a", Name: "ctrl", Path: "folder/job"}))
	if rec.left <= 0 || rec.left > approveRequestTimeout {
		t.Fatalf("deadline left = %v, want a deadline of at most %v", rec.left, approveRequestTimeout)
	}
}
