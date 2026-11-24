package mite

import (
	"context"
	"io"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// hangingStateStore answers no blob call until the call's context ends,
// as a store with a dead connection does.
type hangingStateStore struct {
	blob.Store
}

func (hangingStateStore) Replace(ctx context.Context, _ string, _ io.Reader, _ blob.ObjectOptions) error {
	<-ctx.Done()
	return ctx.Err()
}

func (hangingStateStore) NewRangeReader(ctx context.Context, _ string, _, _ int64) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestOnRunStateChunk_HangingStoreAnswersBusy proves that a run-state write
// to a store that never answers ends at the bound and answers BUSY. The
// write runs on the stream read loop, so a write with no bound stops every
// message of the stream.
func TestOnRunStateChunk_HangingStoreAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	h.logStore = hangingStateStore{Store: h.logStore}
	h.runStateStoreWait = 20 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, "jobs/demo/builds/1/program.dat", 1, []byte("hello")))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("OnRunStateChunk did not return; the store call has no bound")
	}
	if ack := lastRunStateAck(t, sender); ack.Status != stateAckStatusBusy {
		t.Fatalf("ack = %+v, want BUSY after the store bound", ack)
	}
}

// TestOnRunStateRequest_HangingStoreAnswersAnError proves that a fetch from
// a store that never answers ends at the bound with an error answer.
func TestOnRunStateRequest_HangingStoreAnswersAnError(t *testing.T) {
	sender := &recordingSender{}
	h := newRunStateTestHandler(t, newFakeOwnerRequester(), sender)
	h.logStore = hangingStateStore{Store: h.logStore}
	h.runStateStoreWait = 20 * time.Millisecond

	requestID := uint64(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.OnRunStateRequest("cell1", "ns1", &mitev1.RunStateRequest{
			RequestId: &requestID, RunId: newTestRunID(), Relpath: "jobs/demo/builds/1/program.dat",
			PusherEpoch: oneChunkEpoch, StateGen: 1,
		}, sender.send)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("OnRunStateRequest did not return; the store read has no bound")
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.sent) != 1 || sender.sent[0].GetRunStateResponse() == nil || sender.sent[0].GetRunStateResponse().Error == "" {
		t.Fatalf("sent = %v, want one error response", sender.sent)
	}
}
