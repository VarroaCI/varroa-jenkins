package mite

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// ownerAnswer is the ledger answer that names ns1/cell1/inst1 as the
// owner of a live run.
func ownerAnswer() ownerLookupResponse {
	return ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"}
}

// completedOwnerAnswer is the ledger answer for the same owner after the
// run completed at anchor.
func completedOwnerAnswer(anchor time.Time) ownerLookupResponse {
	a := ownerAnswer()
	a.Status, a.AnchorUnixMs = "COMPLETED", anchor.UnixMilli()
	return a
}

// waitForEviction waits until the run has no state and has a tombstone.
func waitForEviction(t *testing.T, h *BusHandler, runID string) {
	t.Helper()
	waitFor(t, func() bool {
		_, live := h.lookupRunState(runID)
		return !live && h.hasRunLogRecord(runID)
	}, 2*time.Second, "the run state was not replaced with a tombstone")
}

// storeFinalizedRun stores seq 0 and the final seq 1 of runID, and waits
// until the gateway evicts the run's state.
func storeFinalizedRun(t *testing.T, h *BusHandler, sender *recordingSender, runID string) {
	t.Helper()
	before := len(sender.logAcks())
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1", JobPath: "tenant-a/build"})
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Final: true, CellInstance: "inst1", JobPath: "tenant-a/build"})
	acks := waitForLogAcks(t, sender, before+2)
	if last := acks[len(acks)-1]; last.Status != logAckStatusStored || last.AckSeq != 1 {
		t.Fatalf("expected the final chunk STORED at ack_seq 1, got %+v", last)
	}
	waitForEviction(t, h, runID)
}

// lastLogAck returns the last log ack the sender received.
func lastLogAck(t *testing.T, sender *recordingSender) *mitev1.LogAck {
	t.Helper()
	acks := sender.logAcks()
	if len(acks) == 0 {
		t.Fatal("expected a log ack, got none")
	}
	return acks[len(acks)-1]
}

// pendingLogBytes returns the bytes charged to the gateway-wide budget.
func pendingLogBytes(h *BusHandler) int64 {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.logTotalPendingBytes
}

// TestOnLogChunkFinalizedRunStateIsEvicted proves that the gateway drops
// a run's state once the final entry is durable and its ack is sent. A
// later duplicate, such as a re-send after a lost ack, gets the STORED
// answer the state gave, and it creates no state.
func TestOnLogChunkFinalizedRunStateIsEvicted(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)

	storeFinalizedRun(t, h, sender, runID)
	if !h.runFinalized(runID) {
		t.Fatal("expected runFinalized true from the tombstone")
	}
	if incomplete, reason := h.runIncompleteness(runID); incomplete {
		t.Fatalf("expected a complete log, got incomplete reason %q", reason)
	}

	for _, seq := range []int64{0, 1} {
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: seq, CellInstance: "inst1", JobPath: "tenant-a/build"})
		if got := lastLogAck(t, sender); got.Status != logAckStatusStored || got.AckSeq != 1 {
			t.Fatalf("duplicate seq %d: expected STORED ack_seq 1 from the tombstone, got %+v", seq, got)
		}
	}
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected no new run state for a duplicate of a finished log")
	}
	if pending := pendingLogBytes(h); pending != 0 {
		t.Fatalf("pending bytes = %d, want 0", pending)
	}
}

// TestOnLogChunkAboveAFinalizedLogIsRejected proves that a chunk above a
// finished log's final entry does not open the log again. It is refused
// with finalize-deadline, the run is marked short once, and the run then
// answers every chunk as an abandoned run does.
func TestOnLogChunkAboveAFinalizedLogIsRejected(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub

	storeFinalizedRun(t, h, sender, runID)
	published := pub.publishedCount()

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 2, Data: []byte("late"), CellInstance: "inst1", JobPath: "tenant-a/build"})
	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 2 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 2, got %+v", got)
	}
	if !h.runRejectedForever(runID) {
		t.Fatal("expected the tombstone to reject every later chunk")
	}
	if incomplete, reason := h.runIncompleteness(runID); !incomplete || reason != logReasonFinalizeDeadline {
		t.Fatalf("incomplete=%v reason=%q, want true/finalize-deadline", incomplete, reason)
	}
	if !h.runFinalized(runID) {
		t.Fatal("expected the final marker kept")
	}
	if got := pub.publishedCount(); got != published+1 {
		t.Fatalf("published %d envelopes after the late chunk, want 1", got-published)
	}
	if env := pub.envelopeAt(published); env.Payload["incomplete"] != true || env.JobPath != "tenant-a/build" {
		t.Fatalf("late-chunk envelope = %+v, want incomplete true for job tenant-a/build", env)
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, CellInstance: "inst1"})
	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected REJECTED finalize-deadline after the run turned rejected, got %+v", got)
	}
	if got := pub.publishedCount(); got != published+1 {
		t.Fatalf("published %d envelopes in total after the late chunks, want 1", got-published)
	}
}

// TestOnLogChunkAbandonedRunStateIsEvicted proves that the gateway drops
// a run's state once abandonRun has sent its stranded acks. A later
// chunk still gets REJECTED{finalize-deadline}, and the run keeps its
// incompleteness for the COMPLETED envelope.
func TestOnLogChunkAbandonedRunStateIsEvicted(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)
	h.logFinalizeWait = time.Hour

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 5, Data: []byte("f"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 2)
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state while the hole is open")
	}
	waitForIdleRun(t, st)

	h.markRunTerminal("ns1", "cell1", runID, "", time.Now().Add(-2*time.Hour))
	waitForEviction(t, h, runID)

	if !h.runRejectedForever(runID) {
		t.Fatal("expected the tombstone to keep the rejection")
	}
	if incomplete, reason := h.runIncompleteness(runID); !incomplete || reason != logReasonGatewayGap {
		t.Fatalf("incomplete=%v reason=%q, want true/gateway-gap", incomplete, reason)
	}
	st.mu.Lock()
	hb, timer, queue := st.heartbeatCancel, st.deadlineTimer, st.queue
	appendErr := st.appendCtx.Err()
	st.mu.Unlock()
	if hb != nil || timer != nil || queue != nil || appendErr == nil {
		t.Fatalf("evicted state holds heartbeat=%v timer=%v queue=%v appendCtx=%v, want none and an ended context",
			hb != nil, timer != nil, queue != nil, appendErr)
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 6, Data: []byte("z"), CellInstance: "inst1"})
	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 6 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 6, got %+v", got)
	}
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected no new run state for a chunk of an abandoned run")
	}
}

// TestEvictRunStateStopsTheHeartbeatAndTheDeadlineTimer proves that a
// run marked terminal before its final chunk gives up its heartbeat, its
// deadline timer, and its append context when the gateway evicts it.
func TestEvictRunStateStopsTheHeartbeatAndTheDeadlineTimer(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)
	h.logFinalizeWait = time.Hour

	h.markRunTerminal("ns1", "cell1", runID, "tenant-a/build", time.Now())
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected markRunTerminal to create the run state")
	}
	st.mu.Lock()
	armed := st.heartbeatCancel != nil && st.deadlineTimer != nil
	st.mu.Unlock()
	if !armed {
		t.Fatal("expected a heartbeat and a deadline timer on the terminal run")
	}

	storeFinalizedRun(t, h, sender, runID)

	st.mu.Lock()
	hb, timer := st.heartbeatCancel, st.deadlineTimer
	appendErr := st.appendCtx.Err()
	st.mu.Unlock()
	if hb != nil || timer != nil || appendErr == nil {
		t.Fatalf("evicted state holds heartbeat=%v timer=%v appendCtx=%v, want none and an ended context", hb != nil, timer != nil, appendErr)
	}
}

// TestOnBuildEventCompletedAfterEvictionCreatesNoState proves that the
// owner's COMPLETED event after an evicted final chunk does not create a
// new state. Such a state would start a heartbeat and later abandon a
// finished log. The event's run.log payload still reports the log
// finalized, from the tombstone.
func TestOnBuildEventCompletedAfterEvictionCreatesNoState(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, _ := newCompletedOwnerTestHandler(t, owner, sender)

	storeFinalizedRun(t, h, sender, runID)

	evt := completedBuildEvent(runID, time.Now().Add(-2*time.Hour))
	if err := h.OnBuildEvent("cell1", "ns1", 0, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected no run state for the COMPLETED event of a finished log")
	}
	p := h.logSummaryPayload(evt)
	if p["finalized"] != true || p["incomplete"] != false {
		t.Fatalf("run.log payload = %+v, want finalized true and incomplete false", p)
	}
}

// TestOnLogChunkAbandonBeforeEnqueueRejectsTheChunk proves that a chunk
// whose own owner answer abandons the run is refused, and does not stay
// queued. The answer names the run terminal with a deadline in the past.
// The inline abandon then runs after the rejectedForever pre-check and
// before the enqueue.
func TestOnLogChunkAbandonBeforeEnqueueRejectsTheChunk(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 1)
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state after the first chunk")
	}
	waitForIdleRun(t, st)

	owner.setAnswer(runID, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	h.ownerCache.invalidate(runID)
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst1"})

	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 1 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 1, got %+v", got)
	}
	if pending := pendingLogBytes(h); pending != 0 {
		t.Fatalf("pending bytes = %d, want 0: the refused chunk must not hold the budget", pending)
	}
	st.mu.Lock()
	queued := len(st.queue)
	st.mu.Unlock()
	if queued != 0 {
		t.Fatalf("queued chunks = %d, want 0", queued)
	}
}

// gatedOwnerRequester holds one owner lookup until the test releases it.
// entered closes when that lookup starts.
type gatedOwnerRequester struct {
	inner   *fakeOwnerRequester
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedOwnerRequester) RequestWithContext(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.inner.RequestWithContext(ctx, subject, data, timeout)
}

// TestOnLogChunkDeadlineDuringOwnerLookupRejectsTheChunk proves that a
// chunk that passes the rejectedForever pre-check, and then waits on its
// owner lookup while the deadline abandons the run, is refused when the
// lookup answers. It never enters the cleared queue.
func TestOnLogChunkDeadlineDuringOwnerLookupRejectsTheChunk(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 1)
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state after the first chunk")
	}
	waitForIdleRun(t, st)

	gate := &gatedOwnerRequester{inner: owner, entered: make(chan struct{}), release: make(chan struct{})}
	h.ledger = gate
	h.ownerCache.invalidate(runID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst1"})
	}()
	<-gate.entered

	h.markRunTerminal("ns1", "cell1", runID, "", time.Now().Add(-2*time.Hour))
	if !h.runRejectedForever(runID) {
		t.Fatal("expected the deadline to abandon the run while the lookup waits")
	}
	close(gate.release)
	<-done

	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 1 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 1, got %+v", got)
	}
	if pending := pendingLogBytes(h); pending != 0 {
		t.Fatalf("pending bytes = %d, want 0: the refused chunk must not hold the budget", pending)
	}
	st.mu.Lock()
	queued, draining := len(st.queue), st.draining
	st.mu.Unlock()
	if queued != 0 || draining {
		t.Fatalf("queued=%d draining=%v, want an empty, idle queue", queued, draining)
	}
}

// TestOnLogChunkPastDeadlineWithNoStateCreatesNothing proves the release
// of a restarted gateway: the owner answer names the run terminal and its
// deadline passed, and the gateway holds no state for it. The chunk is
// refused with finalize-deadline, and the gateway creates no state, holds
// no budget, and publishes nothing.
func TestOnLogChunkPastDeadlineWithNoStateCreatesNothing(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 3, Data: []byte("x"), CellInstance: "inst1"})

	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 3 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 3, got %+v", got)
	}
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected no run state")
	}
	h.logMu.Lock()
	tombstones := h.tombstonesLocked().len()
	h.logMu.Unlock()
	if tombstones != 0 {
		t.Fatalf("tombstones = %d, want 0", tombstones)
	}
	if pending := pendingLogBytes(h); pending != 0 {
		t.Fatalf("pending bytes = %d, want 0", pending)
	}
	if n := pub.publishedCount(); n != 0 {
		t.Fatalf("published %d envelopes, want 0", n)
	}

	// The COMPLETED envelope of this gateway reports the log not
	// finalized. The recorder's finalizeStaleLogs sweep selects every
	// terminal run whose summary is not finalized, so a short log still
	// gets incomplete with not-finalized.
	evt := &mitev1.BuildEvent{Phase: phaseCompleted, RunId: runID, LogSummary: &mitev1.LogSummary{ChunkCount: 4}}
	if p := h.logSummaryPayload(evt); p["finalized"] != false || p["incomplete"] != false {
		t.Fatalf("run.log payload = %+v, want finalized false and incomplete false", p)
	}
}

// incompletePublishes counts the run.log envelopes that report the run
// incomplete.
func incompletePublishes(pub *fakePublisher) int {
	n := 0
	for i := 0; i < pub.publishedCount(); i++ {
		if pub.envelopeAt(i).Payload["incomplete"] == true {
			n++
		}
	}
	return n
}

// abandonRunningRun sends chunk 0 of runID over an index that conflicts
// with every entry, so the run is abandoned with gateway-gap while the
// ledger still names it live. It returns the run's state.
func abandonRunningRun(t *testing.T, h *BusHandler, sender *recordingSender, pub *fakePublisher, runID string) *runLogState {
	t.Helper()
	h.logStore = indexConflictStore{Store: h.logStore, runID: runID}
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1", JobPath: "tenant-a/build"})
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected the chunk to create the run state")
	}
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected the chunk REJECTED finalize-deadline by the abandon, got %+v", got)
	}
	waitFor(t, func() bool { return incompletePublishes(pub) == 1 }, 2*time.Second, "the abandon published no incomplete marker")
	return st
}

// TestOnLogChunkRunAbandonedWhileRunningStaysRejectedPastTheTombstoneLife
// proves that a run abandoned by a store fault, with no terminal mark,
// stays rejected for every later chunk after a tombstone lifetime. The
// ledger still names such a run live, so no owner answer could refuse its
// chunk. The gateway keeps the original state, opens no new one, and
// publishes the incomplete marker once only.
func TestOnLogChunkRunAbandonedWhileRunningStaysRejectedPastTheTombstoneLife(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub
	st := abandonRunningRun(t, h, sender, pub, runID)

	h.logMu.Lock()
	h.tombstonesLocked().expire(time.Now().Add(logTombstoneTTL + time.Minute))
	h.logMu.Unlock()
	h.ownerCache.invalidate(runID)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst1", JobPath: "tenant-a/build"})

	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 1 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 1, got %+v", got)
	}
	if cur, ok := h.lookupRunState(runID); !ok || cur != st {
		t.Fatalf("run state present=%v same=%v, want the abandoned state kept and no new state", ok, cur == st)
	}
	if incomplete, reason := h.runIncompleteness(runID); !incomplete || reason != logReasonGatewayGap {
		t.Fatalf("incomplete=%v reason=%q, want true/gateway-gap", incomplete, reason)
	}
	if n := incompletePublishes(pub); n != 1 {
		t.Fatalf("incomplete marker published %d times, want 1", n)
	}
	st.mu.Lock()
	queue, pending, data := st.queue, st.pendingEntries, st.pendingData
	st.mu.Unlock()
	if queue != nil || pending != nil || data != nil {
		t.Fatal("expected the kept state to hold no queue and no pending maps")
	}
}

// TestOnBuildEventCompletedEvictsARunAbandonedWhileRunning proves that
// the owner's COMPLETED event replaces the kept state of a run abandoned
// with no terminal mark by a tombstone. It starts no heartbeat and no
// deadline timer, and the event's run.log payload keeps the gap.
func TestOnBuildEventCompletedEvictsARunAbandonedWhileRunning(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	h, _ := newCompletedOwnerTestHandler(t, owner, sender)
	h.publisher = pub
	h.logFinalizeWait = time.Hour
	st := abandonRunningRun(t, h, sender, pub, runID)

	evt := completedBuildEvent(runID, time.Now())
	if err := h.OnBuildEvent("cell1", "ns1", 0, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected the COMPLETED event to evict the abandoned state")
	}
	if !h.runRejectedForever(runID) {
		t.Fatal("expected the tombstone to keep the rejection")
	}
	st.mu.Lock()
	hb, timer := st.heartbeatCancel, st.deadlineTimer
	st.mu.Unlock()
	if hb != nil || timer != nil {
		t.Fatalf("evicted state holds heartbeat=%v timer=%v, want neither", hb != nil, timer != nil)
	}
	p := h.logSummaryPayload(evt)
	if p["incomplete"] != true || p["incomplete_reason"] != logReasonGatewayGap || p["finalized"] != false {
		t.Fatalf("run.log payload = %+v, want incomplete gateway-gap and finalized false", p)
	}
}

// moveOwnerToInst2 makes the ledger name ns1/cell1/inst2 as the owner of
// runID, as a mesh takeover does, and drops the cached owner.
func moveOwnerToInst2(h *BusHandler, owner *fakeOwnerRequester, runID string) {
	moved := ownerAnswer()
	moved.Instance = "inst2"
	owner.setAnswer(runID, moved)
	h.ownerCache.invalidate(runID)
}

// countingReadStore counts every read of a stored part or listing.
type countingReadStore struct {
	blob.Store
	reads *atomic.Int32
}

func (s countingReadStore) PartStamp(ctx context.Context, key string, seq int64) (blob.Stamp, error) {
	s.reads.Add(1)
	return s.Store.PartStamp(ctx, key, seq)
}

func (s countingReadStore) Parts(ctx context.Context, key string, from int64) ([]blob.Part, error) {
	s.reads.Add(1)
	return s.Store.Parts(ctx, key, from)
}

func (s countingReadStore) NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	s.reads.Add(1)
	return s.Store.NewRangeReader(ctx, key, offset, length)
}

// assertFinishedLogKept asserts that runID has no state, and that its
// tombstone still reports a finalized, complete log that is not rejected.
func assertFinishedLogKept(t *testing.T, h *BusHandler, runID string) {
	t.Helper()
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected no run state")
	}
	if !h.hasRunLogRecord(runID) || h.runRejectedForever(runID) || !h.runFinalized(runID) {
		t.Fatalf("tombstone present=%v rejected=%v finalized=%v, want a finalized tombstone that is not rejected",
			h.hasRunLogRecord(runID), h.runRejectedForever(runID), h.runFinalized(runID))
	}
	if incomplete, reason := h.runIncompleteness(runID); incomplete {
		t.Fatalf("expected a complete log, got incomplete reason %q", reason)
	}
}

// TestOnLogChunkMovedWriterRepeatPastTheDeadlineStaysStored proves that
// a moved writer's repeat with the stored bytes, after the run's finalize
// deadline, gets STORED. The finished log stays finalized and complete,
// and its tombstone does not turn rejected.
func TestOnLogChunkMovedWriterRepeatPastTheDeadlineStaysStored(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub
	h.logFinalizeWait = 400 * time.Millisecond

	h.markRunTerminal("ns1", "cell1", runID, "tenant-a/build", time.Now())
	deadline := time.Now().Add(h.logFinalizeWait)
	storeFinalizedRun(t, h, sender, runID)
	assertFinishedLogKept(t, h, runID)
	for time.Now().Before(deadline.Add(50 * time.Millisecond)) {
		time.Sleep(10 * time.Millisecond)
	}
	published := pub.publishedCount()

	moveOwnerToInst2(h, owner, runID)
	before := len(sender.logAcks())
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst2", JobPath: "tenant-a/build"})

	if got := waitForLogAcks(t, sender, before+1)[before]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("expected STORED ack_seq 1 for the moved writer's repeat, got %+v", got)
	}
	waitForCompareIdle(t, h)
	assertFinishedLogKept(t, h, runID)
	if got := pub.publishedCount(); got != published {
		t.Fatalf("published %d envelopes for the repeat, want 0", got-published)
	}
}

// TestOnLogChunkMovedWriterRepeatUnderAFullBudgetLeavesNoState proves
// that a moved writer's repeat never takes the pending budget. It gets
// STORED while the budget is full, the gateway creates no state, and the
// tombstone stays as it was.
func TestOnLogChunkMovedWriterRepeatUnderAFullBudgetLeavesNoState(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)
	storeFinalizedRun(t, h, sender, runID)

	h.logMu.Lock()
	h.logPendingMaxBytes = 10
	h.logTotalPendingBytes = 10
	h.logMu.Unlock()
	moveOwnerToInst2(h, owner, runID)
	before := len(sender.logAcks())
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst2", JobPath: "tenant-a/build"})

	if got := waitForLogAcks(t, sender, before+1)[before]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("expected STORED ack_seq 1 for the moved writer's repeat, got %+v", got)
	}
	waitForCompareIdle(t, h)
	assertFinishedLogKept(t, h, runID)
}

// TestOnLogChunkSameWriterRepeatReadsNoStoredPart proves that a repeat
// of the run's last writer gets STORED from the tombstone at once, with
// no store read and no new state.
func TestOnLogChunkSameWriterRepeatReadsNoStoredPart(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	base, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	reads := &atomic.Int32{}
	h := newLogTestHandlerOnStore(t, owner, sender, countingReadStore{Store: base, reads: reads})
	storeFinalizedRun(t, h, sender, runID)

	reads.Store(0)
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1", JobPath: "tenant-a/build"})

	if got := lastLogAck(t, sender); got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("expected STORED ack_seq 1 from the tombstone, got %+v", got)
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("store reads = %d, want 0", n)
	}
	assertFinishedLogKept(t, h, runID)
}

// TestOnLogChunkMovedWriterOnARejectedTombstoneIsRejected proves that a
// chunk of another writer never opens an abandoned run again. It gets
// REJECTED{finalize-deadline}, and the gateway creates no state.
func TestOnLogChunkMovedWriterOnARejectedTombstoneIsRejected(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)
	h.logFinalizeWait = time.Hour

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 5, Data: []byte("f"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 2)
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state while the hole is open")
	}
	waitForIdleRun(t, st)
	h.markRunTerminal("ns1", "cell1", runID, "", time.Now().Add(-2*time.Hour))
	waitForEviction(t, h, runID)

	moveOwnerToInst2(h, owner, runID)
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst2"})

	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 0 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 0, got %+v", got)
	}
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected no new run state for another writer of an abandoned run")
	}
	if !h.runRejectedForever(runID) {
		t.Fatal("expected the tombstone to keep the rejection")
	}
}

// TestOnLogChunkMovedWriterDifferingRepeatMarksTheGapOnce proves that a
// moved writer's chunk with other bytes than the stored part gets STORED
// at the final entry and marks the run gateway-gap. A second differing
// chunk publishes no second marker and reads no stored part. The stored
// log does not change.
func TestOnLogChunkMovedWriterDifferingRepeatMarksTheGapOnce(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	base, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	reads := &atomic.Int32{}
	h := newLogTestHandlerOnStore(t, owner, sender, base)
	h.publisher = pub
	storeFinalizedRun(t, h, sender, runID)
	h.logStore = countingReadStore{Store: base, reads: reads}
	moveOwnerToInst2(h, owner, runID)

	for i, c := range []struct {
		seq  int64
		data string
	}{{0, "b0 "}, {1, "x"}} {
		before := len(sender.logAcks())
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: c.seq, Data: []byte(c.data), CellInstance: "inst2", JobPath: "tenant-a/build"})
		if got := waitForLogAcks(t, sender, before+1)[before]; got.Status != logAckStatusStored || got.AckSeq != 1 {
			t.Fatalf("chunk %d: expected STORED ack_seq 1, got %+v", i, got)
		}
		waitForCompareIdle(t, h)
		if n := reads.Load(); n != 1 {
			t.Fatalf("chunk %d: stored-part reads = %d, want 1", i, n)
		}
		assertRunGatewayGap(t, h, runID)
		if n := gatewayGapPublishes(pub); n != 1 {
			t.Fatalf("chunk %d: gateway-gap marker published %d times, want 1", i, n)
		}
	}
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected no run state")
	}
	if !h.runFinalized(runID) || h.runRejectedForever(runID) {
		t.Fatal("expected the final marker kept and no rejection")
	}
	if got, want := readStoredLog(t, h.logStore, runID), "a"; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
}

// TestOnLogChunkBusyFirstChunkLeavesNoState proves that a first chunk
// answered BUSY for the gateway budget leaves no run state behind.
func TestOnLogChunkBusyFirstChunkLeavesNoState(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)
	h.logPendingMaxBytes = 4

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("aaaaa"), CellInstance: "inst1"})

	if got := lastLogAck(t, sender); got.Status != logAckStatusBusy || got.Reason != logReasonGatewayBudgetFul {
		t.Fatalf("expected BUSY gateway-budget-full, got %+v", got)
	}
	if h.hasRunLogRecord(runID) {
		t.Fatal("expected no run state and no tombstone after a BUSY first chunk")
	}
}

// TestOnLogChunkAfterTombstoneExpiryAsksTheLedger proves what a late
// duplicate gets once its run's tombstone has expired: the owner answer
// names the run terminal past its deadline, so the chunk is refused with
// finalize-deadline and the gateway creates no state.
func TestOnLogChunkAfterTombstoneExpiryAsksTheLedger(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)

	storeFinalizedRun(t, h, sender, runID)
	h.logMu.Lock()
	h.tombstonesLocked().expire(time.Now().Add(logTombstoneTTL))
	h.logMu.Unlock()
	if h.hasRunLogRecord(runID) {
		t.Fatal("expected the tombstone expired")
	}

	owner.setAnswer(runID, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	h.ownerCache.invalidate(runID)
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Final: true, CellInstance: "inst1"})

	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 1 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 1, got %+v", got)
	}
	if h.hasRunLogRecord(runID) {
		t.Fatal("expected no new run state and no new tombstone")
	}
}

// TestLogTombstonesExpireAndStayBounded pins the tombstone set: a
// tombstone lives for its lifetime, a second put replaces the first, an
// update keeps the expiry, and the oldest tombstone goes first at the
// count bound.
func TestLogTombstonesExpireAndStayBounded(t *testing.T) {
	s := newLogTombstones()
	t0 := time.Now()
	const ttl = time.Minute

	s.put("a", logRunTombstone{ackSeq: 1}, t0, ttl, 2)
	s.put("b", logRunTombstone{ackSeq: 2}, t0.Add(time.Second), ttl, 2)
	s.put("c", logRunTombstone{ackSeq: 3}, t0.Add(2*time.Second), ttl, 2)
	if _, ok := s.get("a", t0.Add(2*time.Second)); ok {
		t.Fatal("expected the oldest tombstone dropped at the count bound")
	}
	if got, ok := s.get("b", t0.Add(2*time.Second)); !ok || got.ackSeq != 2 {
		t.Fatalf("tombstone b = %+v, %v, want ack_seq 2", got, ok)
	}

	s.update("b", logRunTombstone{rejected: true, ackSeq: 2})
	if got, ok := s.get("b", t0.Add(ttl)); !ok || !got.rejected {
		t.Fatalf("tombstone b after update = %+v, %v, want rejected", got, ok)
	}
	if _, ok := s.get("b", t0.Add(time.Second+ttl)); ok {
		t.Fatal("expected the update to keep b's expiry")
	}
	if got, ok := s.get("c", t0.Add(time.Second+ttl)); !ok || got.ackSeq != 3 {
		t.Fatalf("tombstone c = %+v, %v, want ack_seq 3", got, ok)
	}

	s.put("c", logRunTombstone{ackSeq: 4}, t0.Add(ttl), ttl, 2)
	if got, ok := s.get("c", t0.Add(2*ttl-time.Second)); !ok || got.ackSeq != 4 {
		t.Fatalf("tombstone c after a second put = %+v, %v, want ack_seq 4", got, ok)
	}
	if n := s.len(); n != 1 {
		t.Fatalf("tombstones = %d, want 1", n)
	}
	if _, ok := s.get("c", t0.Add(2*ttl+time.Second)); ok || s.len() != 0 {
		t.Fatal("expected every tombstone expired")
	}
}

// gatedStampStore holds each PartStamp read until release closes, and
// records how many reads ran at once. A nil release holds nothing. err,
// when set, fails every read after the hold.
type gatedStampStore struct {
	blob.Store
	release chan struct{}
	err     error

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	reads       int
}

func (g *gatedStampStore) PartStamp(ctx context.Context, key string, seq int64) (blob.Stamp, error) {
	g.mu.Lock()
	g.inFlight++
	g.reads++
	if g.inFlight > g.maxInFlight {
		g.maxInFlight = g.inFlight
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
	}()
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return blob.Stamp{}, ctx.Err()
		}
	}
	if g.err != nil {
		return blob.Stamp{}, g.err
	}
	return g.Store.PartStamp(ctx, key, seq)
}

// counts returns the reads in flight, the highest number in flight at
// once, and the reads in total.
func (g *gatedStampStore) counts() (inFlight, maxInFlight, reads int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight, g.maxInFlight, g.reads
}

// compareQueueState returns the repeats that wait for a compare and the
// compare workers that run.
func compareQueueState(h *BusHandler) (queued, running int) {
	h.logCompareMu.Lock()
	defer h.logCompareMu.Unlock()
	return len(h.logCompareQueue), h.logCompareRunning
}

// sendMovedRepeat sends a repeat of seq 0 of runID from the moved writer
// ns1/cell1/inst2.
func sendMovedRepeat(h *BusHandler, runID, data string) {
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte(data), CellInstance: "inst2", JobPath: "tenant-a/build"})
}

// assertStoredAtFinalEntry asserts that the sender received exactly want
// acks after the first before acks, each STORED at ack_seq 1, the final
// entry that storeFinalizedRun writes.
func assertStoredAtFinalEntry(t *testing.T, sender *recordingSender, before, want int) {
	t.Helper()
	acks := sender.logAcks()[before:]
	if len(acks) != want {
		t.Fatalf("acks = %d, want %d", len(acks), want)
	}
	for i, got := range acks {
		if got.Status != logAckStatusStored || got.AckSeq != 1 {
			t.Fatalf("ack %d = %+v, want STORED ack_seq 1", i, got)
		}
	}
}

// TestOnLogChunkMovedWriterCompareReadErrorMarksNothing proves the read
// error path of the compare. The repeat gets STORED, the failed read
// marks no gap and publishes nothing, and the finished log stays
// finalized and complete.
func TestOnLogChunkMovedWriterCompareReadErrorMarksNothing(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub
	storeFinalizedRun(t, h, sender, runID)
	published := pub.publishedCount()

	gated := &gatedStampStore{Store: h.logStore, err: errors.New("store unreachable")}
	h.logStore = gated
	moveOwnerToInst2(h, owner, runID)
	before := len(sender.logAcks())
	sendMovedRepeat(h, runID, "other bytes")

	waitForLogAcks(t, sender, before+1)
	waitForCompareIdle(t, h)
	assertStoredAtFinalEntry(t, sender, before, 1)
	if _, _, reads := gated.counts(); reads != 1 {
		t.Fatalf("stored-part reads = %d, want 1", reads)
	}
	if got := pub.publishedCount(); got != published {
		t.Fatalf("published %d envelopes after a failed read, want 0", got-published)
	}
	assertFinishedLogKept(t, h, runID)
}

// TestOnLogChunkMovedWriterRepeatsHoldTheCompareLimit proves the
// gateway-wide compare limit. A burst of repeats, as a replay after a
// move sends, gets one STORED ack per repeat at once. Only the worker
// limit of reads runs at a time while the store holds them, the rest
// wait in the queue, and every repeat is compared once the store answers.
func TestOnLogChunkMovedWriterRepeatsHoldTheCompareLimit(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub
	h.logCompareWorkersMax = 2
	h.logCompareQueueCap = 16
	storeFinalizedRun(t, h, sender, runID)

	gated := &gatedStampStore{Store: h.logStore, release: make(chan struct{})}
	h.logStore = gated
	moveOwnerToInst2(h, owner, runID)
	const repeats = 10
	before := len(sender.logAcks())
	for i := 0; i < repeats; i++ {
		sendMovedRepeat(h, runID, "a")
	}

	assertStoredAtFinalEntry(t, sender, before, repeats)
	waitFor(t, func() bool {
		inFlight, _, _ := gated.counts()
		return inFlight == 2
	}, 2*time.Second, "the compare workers did not start their reads")
	if queued, running := compareQueueState(h); queued != repeats-2 || running != 2 {
		t.Fatalf("queued=%d running=%d while the store holds the reads, want %d and 2", queued, running, repeats-2)
	}

	close(gated.release)
	waitForCompareIdle(t, h)
	if _, maxInFlight, reads := gated.counts(); maxInFlight != 2 || reads != repeats {
		t.Fatalf("max reads in flight = %d, reads = %d, want 2 and %d", maxInFlight, reads, repeats)
	}
	assertStoredAtFinalEntry(t, sender, before, repeats)
	if n := gatewayGapPublishes(pub); n != 0 {
		t.Fatalf("gateway-gap marker published %d times for repeats of the stored bytes, want 0", n)
	}
	assertFinishedLogKept(t, h, runID)
}

// TestOnLogChunkMovedWriterRepeatOverAFullCompareQueueIsNotCompared
// proves what a full compare queue loses. The repeat over the bound gets
// STORED like every other repeat, reads no stored part, and counts one
// skip. Its bytes differ from the stored part, so the run misses the
// gateway-gap marker of that repeat.
func TestOnLogChunkMovedWriterRepeatOverAFullCompareQueueIsNotCompared(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	mp, reader := newTestMeterProvider(t)
	metrics, err := newGatewayMetrics(mp)
	if err != nil {
		t.Fatalf("newGatewayMetrics: %v", err)
	}
	h := newLogTestHandler(t, owner, sender)
	h.metrics = metrics
	h.publisher = pub
	h.logCompareWorkersMax = 1
	h.logCompareQueueCap = 1
	storeFinalizedRun(t, h, sender, runID)

	gated := &gatedStampStore{Store: h.logStore, release: make(chan struct{})}
	h.logStore = gated
	moveOwnerToInst2(h, owner, runID)
	before := len(sender.logAcks())
	sendMovedRepeat(h, runID, "a")
	waitFor(t, func() bool {
		inFlight, _, _ := gated.counts()
		return inFlight == 1
	}, 2*time.Second, "the compare worker did not start its read")
	sendMovedRepeat(h, runID, "a")
	sendMovedRepeat(h, runID, "lost bytes")

	assertStoredAtFinalEntry(t, sender, before, 3)
	if queued, running := compareQueueState(h); queued != 1 || running != 1 {
		t.Fatalf("queued=%d running=%d, want 1 and 1", queued, running)
	}
	if got := collectGatewaySums(t, reader)["varroa.gateway.log.compare.skipped"]; got != 1 {
		t.Fatalf("varroa.gateway.log.compare.skipped = %d, want 1", got)
	}

	close(gated.release)
	waitForCompareIdle(t, h)
	if _, _, reads := gated.counts(); reads != 2 {
		t.Fatalf("stored-part reads = %d, want 2", reads)
	}
	if n := gatewayGapPublishes(pub); n != 0 {
		t.Fatalf("gateway-gap marker published %d times, want 0: the differing repeat was not compared", n)
	}
	assertFinishedLogKept(t, h, runID)
}

// TestOnLogChunkMovedWriterRepeatOnAnExpiredTombstoneReadsNothing proves
// that a repeat whose tombstone expires while it waits in the compare
// queue reads no stored part and marks nothing. Its bytes differ, so this
// is one of the cases in which a differing repeat marks no gap.
func TestOnLogChunkMovedWriterRepeatOnAnExpiredTombstoneReadsNothing(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub
	h.logCompareWorkersMax = 1
	h.logCompareQueueCap = 4
	storeFinalizedRun(t, h, sender, runID)

	gated := &gatedStampStore{Store: h.logStore, release: make(chan struct{})}
	h.logStore = gated
	moveOwnerToInst2(h, owner, runID)
	before := len(sender.logAcks())
	sendMovedRepeat(h, runID, "a")
	waitFor(t, func() bool {
		inFlight, _, _ := gated.counts()
		return inFlight == 1
	}, 2*time.Second, "the compare worker did not start its read")
	sendMovedRepeat(h, runID, "lost bytes")
	assertStoredAtFinalEntry(t, sender, before, 2)
	if queued, _ := compareQueueState(h); queued != 1 {
		t.Fatalf("queued = %d, want 1", queued)
	}

	h.logMu.Lock()
	h.tombstonesLocked().expire(time.Now().Add(logTombstoneTTL + time.Minute))
	h.logMu.Unlock()
	close(gated.release)
	waitForCompareIdle(t, h)

	if _, _, reads := gated.counts(); reads != 1 {
		t.Fatalf("stored-part reads = %d, want 1: the repeat on the expired tombstone must read nothing", reads)
	}
	if n := gatewayGapPublishes(pub); n != 0 {
		t.Fatalf("gateway-gap marker published %d times, want 0", n)
	}
}
