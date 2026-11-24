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

// runTerminal reports whether runID has a run state with a terminal mark.
func runTerminal(h *BusHandler, runID string) bool {
	st, ok := h.lookupRunState(runID)
	if !ok {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.terminal
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
// pause answers as a failed request does: a cached match admits a chunk,
// every other chunk answers BUSY, and no COMPLETED event sets a deadline.
// After the pause, the next check reads the ledger again.
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
	if runTerminal(h, cachedRun) {
		t.Fatal("pause: expected no deadline from the cached match")
	}
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

// TestStaleCompletionInTheOwnerLookupPauseKeepsTheNewOwnersLog proves that
// a COMPLETED event of a stale cell in the pause sets no deadline from the
// cached match of that cell. The recorder moved the run from cell2 to
// cell1, and this replica missed the move signal. A deadline from the
// stale match would end the new owner's log on this replica: the end
// publishes a finalize-deadline mark, and the new owner's next chunk gets
// REJECTED. The finalize wait is one hour, so the test runs a deadline
// that the event set, as its timer would.
func TestStaleCompletionInTheOwnerLookupPauseKeepsTheNewOwnersLog(t *testing.T) {
	for _, tc := range []struct {
		name     string
		oldChunk bool
	}{
		{name: "old owner's chunk on this replica", oldChunk: true},
		{name: "no log state on this replica"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newOwnerSender := &recordingSender{}
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, cell2Owner())
			h, oldOwnerSender := newCompletedOwnerTestHandler(t, owner, newOwnerSender)
			pub := &fakePublisher{}
			h.publisher = pub
			h.logFinalizeWait = time.Hour
			next := int64(0)
			if tc.oldChunk {
				sendCell2Chunk(h, runID)
				if got := waitForLogAcks(t, oldOwnerSender, 1)[0]; got.Status != logAckStatusStored {
					t.Fatalf("expected the old owner's chunk STORED, got %+v", got)
				}
				st, _ := h.lookupRunState(runID)
				waitForIdleRun(t, st)
				next = 1
			} else {
				h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")
			}

			owner.setAnswer(runID, ownerAnswer())
			setOwnerLookupErr(owner, nats.ErrTimeout)
			if err := h.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(runID, time.Now())); err != nil {
				t.Fatalf("OnBuildEvent: %v", err)
			}
			setOwnerLookupErr(owner, nil)
			endOwnerLookupPause(h)

			sendInst1Chunk(h, runID, next, "b", false)
			if got := waitForLogAcks(t, newOwnerSender, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != next {
				t.Fatalf("expected the new owner's chunk STORED at ack_seq %d, got %+v", next, got)
			}
			if st, ok := h.lookupRunState(runID); ok && runTerminal(h, runID) {
				waitForIdleRun(t, st)
				fireDeadline(h, runID, st)
			}

			sendInst1Chunk(h, runID, next+1, "c", false)
			if got := waitForLogAcks(t, newOwnerSender, 2)[1]; got.Status != logAckStatusStored || got.AckSeq != next+1 {
				t.Fatalf("expected the new owner's next chunk STORED at ack_seq %d, got %+v", next+1, got)
			}
			if n := incompletePublishes(pub); n != 0 {
				t.Fatalf("published %d incomplete markers, want none: the new owner still writes the log", n)
			}
		})
	}
}

// TestConfirmOwnerInTheOwnerLookupPauseKeepsTheCachedMatch proves that the
// fresh owner read of the log writer drops no cached entry in the pause.
// The read sends no request in the pause, so the owner's chunks are still
// admitted from the cached match. The test holds a read record of the run
// open, so each lap that drops the entry counts one change.
func TestConfirmOwnerInTheOwnerLookupPauseKeepsTheCachedMatch(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst1")
	h.ownerCache.pauseLookups(time.Now())
	began := h.ownerCache.beginRead(runID)
	t.Cleanup(func() { h.ownerCache.endRead(runID) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, ok := h.confirmOwner(ctx, runID, "ns1", "cell1", "inst1", &runLogState{}); ok {
		t.Fatal("expected no owner answer in the pause")
	}
	h.ownerCache.mu.Lock()
	drops := h.ownerCache.reads[runID].changes - began
	h.ownerCache.mu.Unlock()
	if drops != 0 {
		t.Fatalf("confirmOwner dropped the cached match on %d laps, want none", drops)
	}
	if calls := owner.callCount(); calls != 0 {
		t.Fatalf("owner lookups = %d, want 0 in the pause", calls)
	}

	sendInst1Chunk(h, runID, 0, "a", false)
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored {
		t.Fatalf("expected the owner's chunk STORED from the cached match, got %+v", got)
	}
}

// TestConfirmOwnerLapsInTheOwnerLookupPause counts the laps of
// confirmOwner over one whole pause on a stepped clock. Each lap waits for
// the end of the pause, at most ownerLookupTimeout, so one pause costs six
// laps, not one lap for each logAppendBackoffFloor.
func TestConfirmOwnerLapsInTheOwnerLookupPause(t *testing.T) {
	h := newLogTestHandler(t, newFakeOwnerRequester(), &recordingSender{})
	start := time.Now()
	h.ownerCache.pauseLookups(start)

	laps := 0
	for now := start; h.ownerCache.lookupsPaused(now); now = now.Add(h.confirmOwnerRetryWait(now)) {
		laps++
	}
	if want := int(ownerLookupPause / ownerLookupTimeout); laps != want {
		t.Fatalf("laps in one pause = %d, want %d", laps, want)
	}
	for _, tc := range []struct {
		left, want time.Duration
	}{
		{left: time.Second, want: time.Second},
		{left: 100 * time.Millisecond, want: logAppendBackoffFloor},
		{left: 0, want: logAppendBackoffFloor},
	} {
		if got := h.confirmOwnerRetryWait(start.Add(ownerLookupPause - tc.left)); got != tc.want {
			t.Fatalf("wait with %v of the pause left = %v, want %v", tc.left, got, tc.want)
		}
	}
}
