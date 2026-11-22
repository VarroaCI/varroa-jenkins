package mite

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// stepClock is a log clock that a test moves forward by hand.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStepClock() *stepClock { return &stepClock{now: time.Now()} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// ctxGatedOwnerRequester holds the first owner lookup until release
// closes or the lookup's context ends. entered closes when that lookup
// starts. Every later lookup goes straight to inner.
type ctxGatedOwnerRequester struct {
	inner   *fakeOwnerRequester
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newCtxGatedOwnerRequester(inner *fakeOwnerRequester) *ctxGatedOwnerRequester {
	return &ctxGatedOwnerRequester{inner: inner, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *ctxGatedOwnerRequester) RequestWithContext(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	held := false
	g.once.Do(func() {
		held = true
		close(g.entered)
	})
	if held {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.inner.RequestWithContext(ctx, subject, data, timeout)
}

// newIdleSweepTestHandler builds a log test handler on a step clock with
// a recording publisher.
func newIdleSweepTestHandler(t *testing.T, owner *fakeOwnerRequester, sender *recordingSender, store blob.Store) (*BusHandler, *stepClock, *fakePublisher) {
	t.Helper()
	h := newLogTestHandlerOnStore(t, owner, sender, store)
	clock := newStepClock()
	h.logNow = clock.Now
	pub := &fakePublisher{}
	h.publisher = pub
	return h, clock, pub
}

// newTestLogStore opens an empty file:// blob store.
func newTestLogStore(t *testing.T) blob.Store {
	t.Helper()
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	return store
}

// sendInst1Chunk sends chunk seq of runID from ns1/cell1/inst1.
func sendInst1Chunk(h *BusHandler, runID string, seq int64, data string, final bool) {
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: seq, Data: []byte(data), Final: final, CellInstance: "inst1", JobPath: "tenant-a/build",
	})
}

// openHoleState stores chunk 0 of runID and holds chunk 2 pending above
// the hole at 1, as a stream that broke after chunk 2 leaves it. It
// returns the idle state.
func openHoleState(t *testing.T, h *BusHandler, sender *recordingSender, runID string) *runLogState {
	t.Helper()
	before := len(sender.logAcks())
	sendInst1Chunk(h, runID, 0, "a", false)
	sendInst1Chunk(h, runID, 2, "c", false)
	waitForLogAcks(t, sender, before+2)
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state while the hole is open")
	}
	waitForIdleRun(t, st)
	st.mu.Lock()
	_, pending := st.pendingEntries[2]
	st.mu.Unlock()
	if !pending {
		t.Fatal("expected chunk 2 pending above the hole")
	}
	return st
}

// endRunInLedger makes the ledger name the run COMPLETED by its owner
// two hours ago, far past the finalize deadline, and drops the cached
// owner.
func endRunInLedger(h *BusHandler, owner *fakeOwnerRequester, runID string) {
	owner.setAnswer(runID, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	h.ownerCache.invalidate(runID)
}

// assertStateKept asserts that st is still the run's state.
func assertStateKept(t *testing.T, h *BusHandler, runID string, st *runLogState) {
	t.Helper()
	cur, ok := h.lookupRunState(runID)
	if !ok || cur != st {
		t.Fatalf("run state present=%v same=%v, want the state kept", ok, cur == st)
	}
	st.mu.Lock()
	evicted := st.evicted
	st.mu.Unlock()
	if evicted {
		t.Fatal("expected the kept state not evicted")
	}
}

// tombstoneCount returns the number of tombstones the gateway holds.
func tombstoneCount(h *BusHandler) int {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.tombstonesLocked().len()
}

// TestIdleSweepReleasesARunThatEndedWithNoTerminalMark proves the sweep
// for a run whose COMPLETED event never reached this gateway. The ledger
// names the run terminal past its deadline, so the sweep replaces the
// state with a rejected tombstone. The stranded chunk gets
// REJECTED{finalize-deadline}, and the sweep publishes no marker. A
// later chunk is refused, and a late COMPLETED creates no state.
func TestIdleSweepReleasesARunThatEndedWithNoTerminalMark(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, pub := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	st := openHoleState(t, h, sender, runID)
	published := pub.publishedCount()

	endRunInLedger(h, owner, runID)
	clock.advance(logIdleSweepAfter)
	calls := owner.callCount()
	before := len(sender.logAcks())
	h.sweepIdleLogRuns(context.Background())

	if got := owner.callCount(); got != calls+1 {
		t.Fatalf("owner lookups = %d, want 1", got-calls)
	}
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected the sweep to drop the state")
	}
	if !h.runRejectedForever(runID) {
		t.Fatal("expected a rejected tombstone")
	}
	acks := sender.logAcks()[before:]
	if len(acks) != 1 || acks[0].Status != logAckStatusRejected || acks[0].Reason != logReasonFinalizeDeadline || acks[0].AckSeq != 2 {
		t.Fatalf("acks = %+v, want one REJECTED finalize-deadline for the stranded seq 2", acks)
	}
	if got := pub.publishedCount(); got != published {
		t.Fatalf("published %d envelopes, want 0", got-published)
	}
	st.mu.Lock()
	pending, data, appendErr := st.pendingEntries, st.pendingData, st.appendCtx.Err()
	st.mu.Unlock()
	if pending != nil || data != nil || appendErr == nil {
		t.Fatal("expected the dropped state to hold no pending maps and an ended append context")
	}
	if p := h.logSummaryPayload(completedBuildEvent(runID, time.Now())); p["incomplete"] != false || p["finalized"] != false {
		t.Fatalf("run.log payload = %+v, want incomplete false and finalized false", p)
	}

	sendInst1Chunk(h, runID, 1, "b", false)
	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 1 {
		t.Fatalf("expected REJECTED finalize-deadline for seq 1, got %+v", got)
	}
	h.markRunTerminal("ns1", "cell1", runID, "tenant-a/build", time.Now().Add(-2*time.Hour))
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected no new state after the release")
	}
	if got := pub.publishedCount(); got != published {
		t.Fatalf("published %d envelopes after the late chunk and mark, want 0", got-published)
	}
}

// TestIdleSweepOnAStaleReplicaPublishesNoMarkerForAFinishedLog proves
// that the sweep never marks a complete log incomplete. Replica A holds a
// chunk above a hole when the mite moves to replica B. B stores the
// replay and the final chunk, and gets the COMPLETED event. A's sweep then
// drops its state and publishes nothing.
func TestIdleSweepOnAStaleReplicaPublishesNoMarkerForAFinishedLog(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	store := newTestLogStore(t)
	senderA := &recordingSender{}
	a, clockA, pubA := newIdleSweepTestHandler(t, owner, senderA, store)
	openHoleState(t, a, senderA, runID)

	senderB := &recordingSender{}
	b := newLogTestHandlerOnStore(t, owner, senderB, store)
	sendInst1Chunk(b, runID, 1, "b", false)
	sendInst1Chunk(b, runID, 2, "c", false)
	sendInst1Chunk(b, runID, 3, "", true)
	if got := waitForLogAcks(t, senderB, 3)[2]; got.Status != logAckStatusStored || got.AckSeq != 3 {
		t.Fatalf("replica B: expected the final chunk STORED at ack_seq 3, got %+v", got)
	}
	waitForEviction(t, b, runID)
	if !b.runFinalized(runID) {
		t.Fatal("replica B: expected the log finalized")
	}

	endRunInLedger(a, owner, runID)
	clockA.advance(logIdleSweepAfter)
	a.sweepIdleLogRuns(context.Background())

	if _, live := a.lookupRunState(runID); live {
		t.Fatal("replica A: expected the sweep to drop the stale state")
	}
	if n := incompletePublishes(pubA); n != 0 {
		t.Fatalf("replica A published the incomplete marker %d times, want 0", n)
	}
	if got := readStoredLog(t, store, runID); got != "abc" {
		t.Fatalf("stored log = %q, want %q", got, "abc")
	}
}

// TestIdleSweepKeepsALiveRun proves that the sweep keeps the state of a
// run that the ledger names live for this writer, and of a terminal run
// inside its finalize deadline. A fresh owner-cache entry answers with
// no ledger request.
func TestIdleSweepKeepsALiveRun(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, pub := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	st := openHoleState(t, h, sender, runID)
	before := len(sender.logAcks())

	clock.advance(logIdleSweepAfter)
	calls := owner.callCount()
	h.sweepIdleLogRuns(context.Background())
	if got := owner.callCount(); got != calls {
		t.Fatalf("owner lookups = %d, want 0: the cached match answers", got-calls)
	}
	assertStateKept(t, h, runID, st)

	owner.setAnswer(runID, completedOwnerAnswer(clock.Now()))
	h.ownerCache.invalidate(runID)
	h.sweepIdleLogRuns(context.Background())
	if got := owner.callCount(); got != calls+1 {
		t.Fatalf("owner lookups = %d, want 1", got-calls)
	}
	assertStateKept(t, h, runID, st)
	if got := len(sender.logAcks()); got != before {
		t.Fatalf("acks = %d, want none", got-before)
	}
	if n := pub.publishedCount(); n != 0 {
		t.Fatalf("published %d envelopes, want 0", n)
	}
}

// TestIdleSweepSkipsAStateWithRecentActivity proves that the sweep reads
// no owner for a state that a chunk touched inside logIdleSweepAfter.
func TestIdleSweepSkipsAStateWithRecentActivity(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	st := openHoleState(t, h, sender, runID)

	endRunInLedger(h, owner, runID)
	clock.advance(logIdleSweepAfter - time.Second)
	calls := owner.callCount()
	h.sweepIdleLogRuns(context.Background())

	if got := owner.callCount(); got != calls {
		t.Fatalf("owner lookups = %d, want 0", got-calls)
	}
	assertStateKept(t, h, runID, st)
}

// TestIdleSweepForgetsARunThatAnotherWriterOwns proves the sweep for a
// moved run. The ledger names another cell instance, so the sweep drops
// the old writer's state with no tombstone, no ack, and no marker. The
// new writer then continues on this gateway from the stored log, and
// the old writer's chunk is refused.
func TestIdleSweepForgetsARunThatAnotherWriterOwns(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, pub := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	openHoleState(t, h, sender, runID)
	before := len(sender.logAcks())

	moveOwnerToInst2(h, owner, runID)
	clock.advance(logIdleSweepAfter)
	h.sweepIdleLogRuns(context.Background())

	if h.hasRunLogRecord(runID) {
		t.Fatal("expected no state and no tombstone after the sweep")
	}
	if got := len(sender.logAcks()); got != before {
		t.Fatalf("acks = %d, want none", got-before)
	}
	if n := pub.publishedCount(); n != 0 {
		t.Fatalf("published %d envelopes, want 0", n)
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("B"), CellInstance: "inst2", JobPath: "tenant-a/build"})
	if got := waitForLogAcks(t, sender, before+1)[before]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("expected the new writer's chunk STORED at ack_seq 1, got %+v", got)
	}
	sendInst1Chunk(h, runID, 2, "c", false)
	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun {
		t.Fatalf("expected the old writer's chunk REJECTED foreign-run, got %+v", got)
	}
}

// TestIdleSweepForgetsARunWithNoLedgerRow proves that the sweep drops the
// state of a run the ledger no longer holds, and keeps no tombstone. A
// later chunk is refused on the owner check.
func TestIdleSweepForgetsARunWithNoLedgerRow(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	openHoleState(t, h, sender, runID)

	owner.setAnswer(runID, ownerLookupResponse{Found: false})
	h.ownerCache.invalidate(runID)
	clock.advance(logIdleSweepAfter)
	h.sweepIdleLogRuns(context.Background())

	if h.hasRunLogRecord(runID) {
		t.Fatal("expected no state and no tombstone after the sweep")
	}
	sendInst1Chunk(h, runID, 1, "b", false)
	if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun {
		t.Fatalf("expected REJECTED foreign-run, got %+v", got)
	}
}

// TestIdleSweepEndsARunAbandonedWhileRunningAtItsTerminalAnswer proves
// the sweep for the bare state of a run that abandonRun ended before its
// terminal mark. While the ledger names the run live, for this writer or
// for another one, the state keeps the rejection. A terminal answer, even
// inside the deadline, replaces it with a rejected tombstone, and the
// marker of the abandon is not published again.
func TestIdleSweepEndsARunAbandonedWhileRunningAtItsTerminalAnswer(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, pub := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	st := abandonRunningRun(t, h, sender, pub, runID)
	clock.advance(logIdleSweepAfter)

	h.ownerCache.invalidate(runID)
	h.sweepIdleLogRuns(context.Background())
	assertStateKept(t, h, runID, st)

	moveOwnerToInst2(h, owner, runID)
	h.sweepIdleLogRuns(context.Background())
	assertStateKept(t, h, runID, st)

	owner.setAnswer(runID, completedOwnerAnswer(clock.Now()))
	h.ownerCache.invalidate(runID)
	h.sweepIdleLogRuns(context.Background())
	if _, live := h.lookupRunState(runID); live {
		t.Fatal("expected the terminal answer to drop the abandoned state")
	}
	if !h.runRejectedForever(runID) {
		t.Fatal("expected the tombstone to keep the rejection")
	}
	if incomplete, reason := h.runIncompleteness(runID); !incomplete || reason != logReasonGatewayGap {
		t.Fatalf("incomplete=%v reason=%q, want true/gateway-gap", incomplete, reason)
	}
	if n := incompletePublishes(pub); n != 1 {
		t.Fatalf("incomplete marker published %d times, want 1", n)
	}
}

// TestIdleSweepFailedLookupEndsTheRound proves that a failed owner lookup
// is never a terminal answer. The round ends at the first failure, and
// every state stays. The next round reads the owners again.
func TestIdleSweepFailedLookupEndsTheRound(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runA, runB := newTestRunID(), newTestRunID()
	owner.setAnswer(runA, ownerAnswer())
	owner.setAnswer(runB, ownerAnswer())
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	stA := openHoleState(t, h, sender, runA)
	stB := openHoleState(t, h, sender, runB)
	endRunInLedger(h, owner, runA)
	endRunInLedger(h, owner, runB)
	clock.advance(logIdleSweepAfter)

	owner.mu.Lock()
	owner.err = errors.New("recorder unreachable")
	owner.mu.Unlock()
	calls := owner.callCount()
	h.sweepIdleLogRuns(context.Background())
	if got := owner.callCount(); got != calls+1 {
		t.Fatalf("owner lookups = %d, want 1: the round ends at the first failure", got-calls)
	}
	assertStateKept(t, h, runA, stA)
	assertStateKept(t, h, runB, stB)

	owner.mu.Lock()
	owner.err = nil
	owner.mu.Unlock()
	h.sweepIdleLogRuns(context.Background())
	if h.hasRunLogRecord(runA) && !h.runRejectedForever(runA) || h.hasRunLogRecord(runB) && !h.runRejectedForever(runB) {
		t.Fatal("expected both runs released in the next round")
	}
	if _, live := h.lookupRunState(runA); live {
		t.Fatal("expected run A released")
	}
	if _, live := h.lookupRunState(runB); live {
		t.Fatal("expected run B released")
	}
}

// TestIdleSweepLookupBudgetReadsTheOldestStateFirst proves the cost bound
// of one round. With a budget of two lookups and three states with no
// cached owner, a round reads two. The next round reads the third state
// first and answers the other two from the owner cache.
func TestIdleSweepLookupBudgetReadsTheOldestStateFirst(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	h.logIdleSweepLookupsMax = 2
	runs := []string{newTestRunID(), newTestRunID(), newTestRunID()}
	states := make([]*runLogState, len(runs))
	for i, runID := range runs {
		owner.setAnswer(runID, ownerAnswer())
		states[i] = openHoleState(t, h, sender, runID)
		h.ownerCache.invalidate(runID)
	}
	clock.advance(logIdleSweepAfter)
	swept := func() int {
		n := 0
		for _, st := range states {
			st.mu.Lock()
			if !st.lastSwept.IsZero() {
				n++
			}
			st.mu.Unlock()
		}
		return n
	}

	calls := owner.callCount()
	h.sweepIdleLogRuns(context.Background())
	if got := owner.callCount(); got != calls+2 {
		t.Fatalf("first round: owner lookups = %d, want 2", got-calls)
	}
	if n := swept(); n != 2 {
		t.Fatalf("first round: states read = %d, want 2", n)
	}

	clock.advance(logIdleSweepInterval)
	h.sweepIdleLogRuns(context.Background())
	if got := owner.callCount(); got != calls+3 {
		t.Fatalf("second round: owner lookups = %d, want 1", got-calls-2)
	}
	if n := swept(); n != 3 {
		t.Fatalf("second round: states read = %d, want 3", n)
	}
	for i, runID := range runs {
		assertStateKept(t, h, runID, states[i])
	}
}

// startGatedSweep starts one round on h with gate as the ledger. It
// returns after the round's owner lookup has started, and the returned
// channel closes when the round ends.
func startGatedSweep(ctx context.Context, h *BusHandler, gate *ctxGatedOwnerRequester) <-chan struct{} {
	h.ledger = gate
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.sweepIdleLogRuns(ctx)
	}()
	<-gate.entered
	return done
}

// TestIdleSweepChunkDuringTheOwnerReadKeepsTheState proves that a chunk
// that enters the queue while the round waits for the owner answer keeps
// the state, although the answer names the run terminal past its
// deadline. The chunk is stored and acknowledged as usual.
func TestIdleSweepChunkDuringTheOwnerReadKeepsTheState(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	st := openHoleState(t, h, sender, runID)
	endRunInLedger(h, owner, runID)
	clock.advance(logIdleSweepAfter)

	gate := newCtxGatedOwnerRequester(owner)
	done := startGatedSweep(context.Background(), h, gate)
	before := len(sender.logAcks())
	h.enqueueLogChunk("ns1", "cell1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst1", JobPath: "tenant-a/build"})
	if got := waitForLogAcks(t, sender, before+1)[before]; got.Status != logAckStatusStored || got.AckSeq != 2 {
		t.Fatalf("expected the chunk STORED at ack_seq 2, got %+v", got)
	}
	close(gate.release)
	<-done

	assertStateKept(t, h, runID, st)
	if n := tombstoneCount(h); n != 0 {
		t.Fatalf("tombstones = %d, want 0", n)
	}
	if got := len(sender.logAcks()); got != before+1 {
		t.Fatalf("acks = %d, want 1", got-before)
	}
}

// TestIdleSweepStateFinalizedDuringTheOwnerReadKeepsItsTombstone proves
// that a round never acts on a state that the drain evicted during the
// owner read. The finalized tombstone stays, and the round creates no
// state and sends no ack.
func TestIdleSweepStateFinalizedDuringTheOwnerReadKeepsItsTombstone(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	openHoleState(t, h, sender, runID)
	endRunInLedger(h, owner, runID)
	clock.advance(logIdleSweepAfter)

	gate := newCtxGatedOwnerRequester(owner)
	done := startGatedSweep(context.Background(), h, gate)
	h.enqueueLogChunk("ns1", "cell1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst1", JobPath: "tenant-a/build"})
	h.enqueueLogChunk("ns1", "cell1", &mitev1.LogChunk{RunId: runID, Seq: 3, Final: true, CellInstance: "inst1", JobPath: "tenant-a/build"})
	waitForEviction(t, h, runID)
	acks := len(sender.logAcks())
	close(gate.release)
	<-done

	assertFinishedLogKept(t, h, runID)
	if got := len(sender.logAcks()); got != acks {
		t.Fatalf("the round sent %d acks, want 0", got-acks)
	}
}

// TestIdleSweepStateReplacedDuringTheOwnerReadKeepsTheNewState proves
// that a round acts only on the state it read. During the owner read the
// deadline abandons and evicts that state, the tombstone expires, and a
// new chunk opens a new state. The round leaves the new state alone.
func TestIdleSweepStateReplacedDuringTheOwnerReadKeepsTheNewState(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	old := openHoleState(t, h, sender, runID)
	endRunInLedger(h, owner, runID)
	clock.advance(logIdleSweepAfter)

	gate := newCtxGatedOwnerRequester(owner)
	done := startGatedSweep(context.Background(), h, gate)
	h.markRunTerminal("ns1", "cell1", runID, "tenant-a/build", time.Now().Add(-2*time.Hour))
	waitForEviction(t, h, runID)
	h.logMu.Lock()
	h.tombstonesLocked().expire(time.Now().Add(logTombstoneTTL + time.Minute))
	h.logMu.Unlock()
	before := len(sender.logAcks())
	h.enqueueLogChunk("ns1", "cell1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst1", JobPath: "tenant-a/build"})
	waitForLogAcks(t, sender, before+1)
	cur, ok := h.lookupRunState(runID)
	if !ok || cur == old {
		t.Fatal("expected a new run state")
	}
	waitForIdleRun(t, cur)
	close(gate.release)
	<-done

	assertStateKept(t, h, runID, cur)
}

// TestIdleSweepWriterAdoptedDuringTheOwnerReadKeepsTheState proves that a
// round does not drop a state that the new writer adopts during the owner
// read. The answer names the new writer, so the round would forget the
// old writer's state. The new writer's chunk entered the queue first, so
// the state stays and the adoption completes.
func TestIdleSweepWriterAdoptedDuringTheOwnerReadKeepsTheState(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	st := openHoleState(t, h, sender, runID)
	moveOwnerToInst2(h, owner, runID)
	clock.advance(logIdleSweepAfter)

	gate := newCtxGatedOwnerRequester(owner)
	done := startGatedSweep(context.Background(), h, gate)
	before := len(sender.logAcks())
	h.enqueueLogChunk("ns1", "cell1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("B"), CellInstance: "inst2", JobPath: "tenant-a/build"})
	close(gate.release)
	<-done

	if got := waitForLogAcks(t, sender, before+1)[before]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("expected the new writer's chunk STORED at ack_seq 1, got %+v", got)
	}
	assertStateKept(t, h, runID, st)
	waitForIdleRun(t, st)
	st.mu.Lock()
	writer := st.writer
	st.mu.Unlock()
	if writer != "ns1/cell1/inst2" {
		t.Fatalf("writer = %q, want ns1/cell1/inst2", writer)
	}
}

// TestIdleSweepShutdownDuringTheOwnerReadActsOnNothing proves that a
// round that the gateway stops during an owner read ends at once and
// changes no state.
func TestIdleSweepShutdownDuringTheOwnerReadActsOnNothing(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, pub := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	st := openHoleState(t, h, sender, runID)
	endRunInLedger(h, owner, runID)
	clock.advance(logIdleSweepAfter)
	before := len(sender.logAcks())

	ctx, cancel := context.WithCancel(context.Background())
	gate := newCtxGatedOwnerRequester(owner)
	done := startGatedSweep(ctx, h, gate)
	cancel()
	<-done

	assertStateKept(t, h, runID, st)
	if got := len(sender.logAcks()); got != before {
		t.Fatalf("acks = %d, want none", got-before)
	}
	if n := pub.publishedCount(); n != 0 {
		t.Fatalf("published %d envelopes, want 0", n)
	}
}

// TestLogIdleSweepRunsRoundsUntilClose proves the sweep goroutine. It
// runs a round each interval, and Close stops it and waits for it.
func TestLogIdleSweepRunsRoundsUntilClose(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	h.contentDone = make(chan struct{})
	openHoleState(t, h, sender, runID)
	endRunInLedger(h, owner, runID)
	clock.advance(logIdleSweepAfter)

	h.startLogIdleSweep(5 * time.Millisecond)
	waitFor(t, func() bool {
		_, live := h.lookupRunState(runID)
		return !live
	}, 2*time.Second, "the sweep goroutine did not release the run")
	h.Close()
	h.logSweepStop()
}

// runFailingRequester fails every owner lookup of failRun and answers
// every other run from inner. fails counts the failed lookups.
type runFailingRequester struct {
	inner   *fakeOwnerRequester
	failRun string
	fails   atomic.Int32
}

func (r *runFailingRequester) RequestWithContext(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	var req ownerLookupRequest
	_ = json.Unmarshal(data, &req)
	if req.RunID == r.failRun {
		r.fails.Add(1)
		return nil, errors.New("owner lookup timed out")
	}
	return r.inner.RequestWithContext(ctx, subject, data, timeout)
}

// TestIdleSweepRunWhoseLookupKeepsFailingHoldsBackNoOtherRun proves that
// one run whose owner lookup always fails cannot stop the sweep. The run
// that fails sorts first and ends the first round. The next round reads
// the other run first and releases it. Each round sends one failed lookup
// at most.
func TestIdleSweepRunWhoseLookupKeepsFailingHoldsBackNoOtherRun(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runs := []string{newTestRunID(), newTestRunID()}
	sort.Strings(runs)
	bad, good := runs[0], runs[1]
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	for _, runID := range runs {
		owner.setAnswer(runID, ownerAnswer())
		openHoleState(t, h, sender, runID)
		endRunInLedger(h, owner, runID)
	}
	failing := &runFailingRequester{inner: owner, failRun: bad}
	h.ledger = failing
	clock.advance(logIdleSweepAfter)

	h.sweepIdleLogRuns(context.Background())
	if _, live := h.lookupRunState(good); !live {
		t.Fatal("first round: expected the round to end at the failed lookup, before the second run")
	}
	if n := failing.fails.Load(); n != 1 {
		t.Fatalf("first round: failed lookups = %d, want 1", n)
	}

	clock.advance(logIdleSweepInterval)
	h.sweepIdleLogRuns(context.Background())
	if _, live := h.lookupRunState(good); live || !h.runRejectedForever(good) {
		t.Fatal("second round: expected the second run released")
	}
	if _, live := h.lookupRunState(bad); !live {
		t.Fatal("second round: expected the run whose lookup fails kept")
	}
	if n := failing.fails.Load(); n != 2 {
		t.Fatalf("after two rounds: failed lookups = %d, want 2", n)
	}
}

// TestIdleSweepKeepsAQuietBuildThatResumes proves that a live build with
// no output for the idle window is read and kept as it is. Its pending
// chunk above the hole stays, and the build's next chunk fills the hole
// on the same state.
func TestIdleSweepKeepsAQuietBuildThatResumes(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	store := newTestLogStore(t)
	h, clock, pub := newIdleSweepTestHandler(t, owner, sender, store)
	st := openHoleState(t, h, sender, runID)

	clock.advance(logIdleSweepAfter)
	h.sweepIdleLogRuns(context.Background())
	assertStateKept(t, h, runID, st)
	st.mu.Lock()
	swept := !st.lastSwept.IsZero()
	st.mu.Unlock()
	if !swept {
		t.Fatal("expected the round to read the quiet build's owner")
	}

	before := len(sender.logAcks())
	sendInst1Chunk(h, runID, 1, "b", false)
	if got := waitForLogAcks(t, sender, before+1)[before]; got.Status != logAckStatusStored || got.AckSeq != 2 {
		t.Fatalf("expected the resumed chunk STORED at ack_seq 2 with the kept chunk 2, got %+v", got)
	}
	assertStateKept(t, h, runID, st)
	if got := readStoredLog(t, store, runID); got != "abc" {
		t.Fatalf("stored log = %q, want %q", got, "abc")
	}
	if n := pub.publishedCount(); n != 0 {
		t.Fatalf("published %d envelopes, want 0", n)
	}
}

// TestIdleSweepReadsTheLedgerBeforeItDropsOnACachedAnswer proves that a
// stale owner-cache entry never drops the state of a live run that this
// replica still writes. The entry names another writer, no row, or a
// terminal status. The round reads the ledger again, keeps the state, and
// leaves the fresh answer in the cache.
func TestIdleSweepReadsTheLedgerBeforeItDropsOnACachedAnswer(t *testing.T) {
	cases := []struct {
		name  string
		stale func(h *BusHandler, runID string)
	}{
		{"another writer", func(h *BusHandler, runID string) {
			h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst2")
		}},
		{"no row", func(h *BusHandler, runID string) {
			h.ownerCache.set(runID, ownerCacheEntry{found: false, expires: time.Now().Add(ownerRefuseTTL)})
		}},
		{"terminal", func(h *BusHandler, runID string) {
			tuple := ownerTuple{cluster: "core", namespace: "ns1", cell: "cell1", instance: "inst1",
				status: "COMPLETED", anchorUnixMs: time.Now().Add(-2 * time.Hour).UnixMilli()}
			h.ownerCache.set(runID, ownerCacheEntry{found: true, tuple: tuple, expires: time.Now().Add(ownerMatchTTL)})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &recordingSender{}
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, ownerAnswer())
			h, clock, pub := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
			st := openHoleState(t, h, sender, runID)
			tc.stale(h, runID)
			clock.advance(logIdleSweepAfter)
			calls := owner.callCount()
			before := len(sender.logAcks())

			h.sweepIdleLogRuns(context.Background())

			if got := owner.callCount(); got != calls+1 {
				t.Fatalf("owner lookups = %d, want 1 fresh read", got-calls)
			}
			assertStateKept(t, h, runID, st)
			if got := len(sender.logAcks()); got != before {
				t.Fatalf("acks = %d, want none", got-before)
			}
			if n := pub.publishedCount(); n != 0 {
				t.Fatalf("published %d envelopes, want 0", n)
			}
			entry, ok := h.ownerCache.peek(runID)
			if !ok || decideOwner(entry, "core", "ns1", "cell1", "inst1") != ownerMatched || entry.tuple.status != "" {
				t.Fatalf("cached owner = %+v, %v, want the fresh live match", entry, ok)
			}
		})
	}
}

// indexReadFailStore fails every read of the run index while fail is
// set, so the first recovery of a run keeps failing. reads counts the
// failed reads.
type indexReadFailStore struct {
	blob.Store
	fail  *atomic.Bool
	reads *atomic.Int32
}

func (s indexReadFailStore) NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if s.fail.Load() && strings.HasPrefix(key, "logindex/") {
		s.reads.Add(1)
		return nil, errors.New("index read failed")
	}
	return s.Store.NewRangeReader(ctx, key, offset, length)
}

// TestIdleSweepLeavesAStateWhoseFirstRecoveryKeepsFailing proves that a
// state whose first recovery keeps failing is never idle. Its drain keeps
// the chunk queued and sets no writer, so the round reads no owner and
// drops nothing. Once the store answers, the chunk is stored and the
// writer is set.
func TestIdleSweepLeavesAStateWhoseFirstRecoveryKeepsFailing(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	fail, reads := &atomic.Bool{}, &atomic.Int32{}
	fail.Store(true)
	h, clock, _ := newIdleSweepTestHandler(t, owner, sender, indexReadFailStore{Store: newTestLogStore(t), fail: fail, reads: reads})

	sendInst1Chunk(h, runID, 0, "a", false)
	waitFor(t, func() bool { return reads.Load() >= 2 }, 2*time.Second, "the drain did not retry its recovery")
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state for the admitted chunk")
	}
	st.mu.Lock()
	draining, queued, writer := st.draining, len(st.queue), st.writer
	st.mu.Unlock()
	if !draining || queued != 1 || writer != "" {
		t.Fatalf("draining=%v queued=%d writer=%q, want a draining state with its chunk queued and no writer", draining, queued, writer)
	}

	endRunInLedger(h, owner, runID)
	clock.advance(logIdleSweepAfter)
	calls := owner.callCount()
	h.sweepIdleLogRuns(context.Background())
	if got := owner.callCount(); got != calls {
		t.Fatalf("owner lookups = %d, want 0", got-calls)
	}
	assertStateKept(t, h, runID, st)

	fail.Store(false)
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != 0 {
		t.Fatalf("expected the chunk STORED at ack_seq 0 once the store answers, got %+v", got)
	}
	waitForIdleRun(t, st)
	st.mu.Lock()
	writer = st.writer
	st.mu.Unlock()
	if writer != "ns1/cell1/inst1" {
		t.Fatalf("writer = %q, want ns1/cell1/inst1", writer)
	}
}

// TestIdleSweepReadsAWriterlessStateForItsCreator proves that an idle
// state that names no writer is still swept. The round reads the owner
// for the state's own controller with no instance. A terminal answer
// releases the state, and a live answer of a named instance forgets it.
func TestIdleSweepReadsAWriterlessStateForItsCreator(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	ended, live := newTestRunID(), newTestRunID()
	owner.setAnswer(ended, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	owner.setAnswer(live, ownerAnswer())
	h, clock, pub := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	h.logMu.Lock()
	h.newRunStateLocked(ended, "ns1", "cell1", "tenant-a/build")
	h.newRunStateLocked(live, "ns1", "cell1", "tenant-a/build")
	h.logMu.Unlock()
	clock.advance(logIdleSweepAfter)

	calls := owner.callCount()
	h.sweepIdleLogRuns(context.Background())

	if got := owner.callCount(); got != calls+2 {
		t.Fatalf("owner lookups = %d, want 2", got-calls)
	}
	if _, ok := h.lookupRunState(ended); ok || !h.runRejectedForever(ended) {
		t.Fatal("expected the ended run released to a rejected tombstone")
	}
	if h.hasRunLogRecord(live) {
		t.Fatal("expected the live run forgotten with no tombstone")
	}
	if n := pub.publishedCount(); n != 0 {
		t.Fatalf("published %d envelopes, want 0", n)
	}
}

// TestSetLogStoreRefusesASecondCall proves that a second SetLogStore call
// panics. Every log path and the idle sweep read the fields it sets with
// no lock, so the fields must not change while the gateway serves.
func TestSetLogStoreRefusesASecondCall(t *testing.T) {
	sender := &recordingSender{}
	h := newLogTestHandler(t, newFakeOwnerRequester(), sender)
	h.contentDone = make(chan struct{})
	h.SetLogStore(h.logStore, 0, 0, 0)
	t.Cleanup(h.Close)

	defer func() {
		if recover() == nil {
			t.Fatal("expected a second SetLogStore call to panic")
		}
	}()
	h.SetLogStore(h.logStore, 0, 0, 0)
}
