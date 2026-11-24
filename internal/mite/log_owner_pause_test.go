package mite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// setOwnerLookupErr sets the error of every later request to owner.
func setOwnerLookupErr(owner *fakeOwnerRequester, err error) {
	owner.mu.Lock()
	owner.err = err
	owner.mu.Unlock()
}

// endOwnerLookupPause ends the pause of h's owner cache, as the end of
// ownerLookupPause does.
func endOwnerLookupPause(h *BusHandler) {
	h.ownerCache.mu.Lock()
	h.ownerCache.pausedUntil = time.Now()
	h.ownerCache.mu.Unlock()
}

// sendCell2Chunk sends chunk 0 of runID from cell2, instance inst2.
func sendCell2Chunk(h *BusHandler, runID string) {
	h.OnLogChunk("cell2", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst2", JobPath: "tenant-a/build",
	})
}

// TestOwnerCheckWithAnExpiredMatchAndAFailedReadAdmitsNothing proves that
// a match past ownerMatchTTL never decides a check whose ledger read
// fails. The COMPLETED event sets no deadline and keeps its whole group.
// The chunk gets BUSY owner-lookup-failed. Neither creates a run state.
func TestOwnerCheckWithAnExpiredMatchAndAFailedReadAdmitsNothing(t *testing.T) {
	cases := []struct {
		lookupErr error
		wantCalls int32
	}{
		// A timed-out read pauses the requests, so the chunk sends none.
		{lookupErr: nats.ErrTimeout, wantCalls: 1},
		{lookupErr: nats.ErrNoResponders, wantCalls: 2},
	}
	for _, tc := range cases {
		lookupErr := tc.lookupErr
		t.Run(lookupErr.Error(), func(t *testing.T) {
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, cell2Owner())
			setOwnerLookupErr(owner, lookupErr)
			h, cell2 := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
			h.logFinalizeWait = time.Hour
			h.ownerCache.set(runID, ownerCacheEntry{
				found:   true,
				tuple:   ownerTuple{cluster: "core", namespace: "ns1", cell: "cell2", instance: "inst2"},
				expires: time.Now().Add(-time.Millisecond),
			})

			if err := h.OnBuildEvent("cell2", "ns1", 0, fullCompletedBuildEvent(runID)); err != nil {
				t.Fatalf("OnBuildEvent: %v", err)
			}
			if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(fullCompletedGroup) {
				t.Fatalf("group = %v, want the whole group %v", got, fullCompletedGroup)
			}
			if _, exists := h.lookupRunState(runID); exists {
				t.Fatal("expected no run state from an expired match")
			}

			sendCell2Chunk(h, runID)
			if got := waitForLogAcks(t, cell2, 1)[0]; got.Status != logAckStatusBusy || got.Reason != logReasonOwnerLookupFail {
				t.Fatalf("expected BUSY owner-lookup-failed, got %+v", got)
			}
			if _, exists := h.lookupRunState(runID); exists {
				t.Fatal("expected no run state from an expired match")
			}
			if _, ok := h.ownerCache.peek(runID); ok {
				t.Fatal("expected no cached entry after a failed read")
			}
			if calls := owner.callCount(); calls != tc.wantCalls {
				t.Fatalf("owner lookups = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// TestOwnerLookupPauseAfterATimeout proves the pause. A request that
// times out holds its caller for ownerLookupTimeout, so in an outage the
// checks that follow send no request for ownerLookupPause. A check in the
// pause answers as a failed request does: a cached match admits a chunk
// and sets a COMPLETED event's deadline, and every other check answers
// BUSY or sets no deadline. After the pause, the next check reads the
// ledger again.
func TestOwnerLookupPauseAfterATimeout(t *testing.T) {
	owner := newFakeOwnerRequester()
	cachedRun, unknownRun := newTestRunID(), newTestRunID()
	owner.setAnswer(cachedRun, cell2Owner())
	owner.setAnswer(unknownRun, cell2Owner())
	h, cell2 := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	h.logFinalizeWait = time.Hour
	h.ownerCache.observeGrant(cachedRun, "core", "ns1", "cell2", "inst2")
	setOwnerLookupErr(owner, nats.ErrTimeout)

	sendCell2Chunk(h, unknownRun)
	if got := waitForLogAcks(t, cell2, 1)[0]; got.Status != logAckStatusBusy || got.Reason != logReasonOwnerLookupFail {
		t.Fatalf("timed-out read: expected BUSY owner-lookup-failed, got %+v", got)
	}
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups = %d, want 1", calls)
	}
	setOwnerLookupErr(owner, nil)

	sendCell2Chunk(h, unknownRun)
	if got := waitForLogAcks(t, cell2, 2)[1]; got.Status != logAckStatusBusy || got.Reason != logReasonOwnerLookupFail {
		t.Fatalf("pause: expected BUSY owner-lookup-failed for a run with no entry, got %+v", got)
	}
	if err := h.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(unknownRun, time.Now())); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	if _, exists := h.lookupRunState(unknownRun); exists {
		t.Fatal("pause: expected no run state for a run with no entry")
	}
	sendCell2Chunk(h, cachedRun)
	if got := waitForLogAcks(t, cell2, 3)[2]; got.Status != logAckStatusStored {
		t.Fatalf("pause: expected the cached match's chunk STORED, got %+v", got)
	}
	if err := h.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(cachedRun, time.Now())); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	assertRunTerminal(t, h, cachedRun)
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups = %d, want 1: no check in the pause sends a request", calls)
	}

	endOwnerLookupPause(h)
	sendCell2Chunk(h, unknownRun)
	if got := waitForLogAcks(t, cell2, 4)[3]; got.Status != logAckStatusStored {
		t.Fatalf("after the pause: expected the chunk STORED, got %+v", got)
	}
	if calls := owner.callCount(); calls != 2 {
		t.Fatalf("owner lookups = %d, want 2", calls)
	}
}

// TestOnRunStateChunkInTheOwnerLookupPauseSendsNoRequest proves that the
// run-state path takes the same pause. A push for a run with no entry
// gets BUSY at once, and a push for a run with a cached match is stored.
// Neither sends a request.
func TestOnRunStateChunkInTheOwnerLookupPauseSendsNoRequest(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	cachedRun, unknownRun := newTestRunID(), newTestRunID()
	owner.setAnswer(cachedRun, ownerAnswer())
	owner.setAnswer(unknownRun, ownerAnswer())
	h := newRunStateTestHandler(t, owner, sender)
	h.ownerCache.observeGrant(cachedRun, "core", "ns1", "cell1", "inst1")
	h.ownerCache.pauseLookups(time.Now())

	h.OnRunStateChunk("cell1", "ns1", oneChunk(unknownRun, "jobs/a/builds/1/program.dat", 1, []byte("hello")))
	h.OnRunStateChunk("cell1", "ns1", oneChunk(cachedRun, "jobs/a/builds/1/program.dat", 1, []byte("hello")))

	acks := runStateAcksFrom(sender)
	if len(acks) != 2 || acks[0].Status != stateAckStatusBusy || acks[1].Status != stateAckStatusStored {
		t.Fatalf("acks = %+v, want BUSY for the run with no entry and STORED for the cached match", acks)
	}
	if calls := owner.callCount(); calls != 0 {
		t.Fatalf("owner lookups = %d, want 0 in the pause", calls)
	}
}

// TestOwnerLookupFailureThatHoldsNoCallerStartsNoPause proves that only a
// request that waited its full time starts the pause. A request that no
// responder takes, an error reply from the recorder, and the end of the
// caller's own context fail at once, so the next check reads again.
func TestOwnerLookupFailureThatHoldsNoCallerStartsNoPause(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, owner *fakeOwnerRequester, runID string) context.Context
	}{
		{name: "no responder", prepare: func(_ *testing.T, owner *fakeOwnerRequester, _ string) context.Context {
			setOwnerLookupErr(owner, nats.ErrNoResponders)
			return context.Background()
		}},
		{name: "recorder error reply", prepare: func(_ *testing.T, owner *fakeOwnerRequester, runID string) context.Context {
			owner.setAnswer(runID, ownerLookupResponse{Error: "owner lookup failed"})
			return context.Background()
		}},
		{name: "caller context past its deadline", prepare: func(t *testing.T, _ *fakeOwnerRequester, _ string) context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			owner.delay = time.Millisecond
			runID := newTestRunID()
			h := newLogTestHandler(t, owner, &recordingSender{})
			ctx := tc.prepare(t, owner, runID)

			if decision, _ := h.checkOwner(ctx, "core", "ns1", "cell1", "inst1", runID); decision != ownerUnknown {
				t.Fatalf("failed check = %v, want ownerUnknown", decision)
			}
			if h.ownerCache.lookupsPaused(time.Now()) {
				t.Fatal("expected no pause after a failure that held no caller")
			}
			if _, err := h.ownerCache.doLookup(context.Background(), owner, runID, "core", "ns1", "cell1", "inst1"); err == errOwnerLookupPaused {
				t.Fatal("the next read was paused")
			}
			if calls := owner.callCount(); calls != 2 {
				t.Fatalf("owner lookups = %d, want 2", calls)
			}
		})
	}
}

// TestOwnerLookupReplyEndsThePause proves that a reply ends the pause. A
// request that began before another request timed out gets a reply, so
// the ledger answers again and the next check reads it at once.
func TestOwnerLookupReplyEndsThePause(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, &recordingSender{})
	gate := newStaleGatedOwnerRequester(owner)
	h.ledger = gate

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.checkOwner(context.Background(), "core", "ns1", "cell1", "inst1", runID)
	}()
	gate.waitEntered(t, done)
	h.ownerCache.pauseLookups(time.Now())
	close(gate.release)
	<-done

	if h.ownerCache.lookupsPaused(time.Now()) {
		t.Fatal("expected the reply to end the pause")
	}
	if entry, ok := h.ownerCache.peek(runID); !ok || decideOwner(entry, "core", "ns1", "cell1", "inst1") != ownerMatched {
		t.Fatalf("cached entry = %+v (ok=%v), want the match from the reply", entry, ok)
	}
}
