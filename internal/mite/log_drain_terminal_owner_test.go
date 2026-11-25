package mite

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// countingFailingAppendStore fails every Append and counts the calls, so
// a test can wait for the drain's retries on the wire.
type countingFailingAppendStore struct {
	blob.Store
	appends *atomic.Int32
}

func (s countingFailingAppendStore) Append(context.Context, string, int64, []byte, blob.ObjectOptions) error {
	s.appends.Add(1)
	return errors.New("blob store outage")
}

// newStuckDrainHandler builds a gateway whose drain of runID retries a
// failing store write. The ledger names ns1/cell1/inst1 as the owner of a
// live run, and each write attempt reads the owner again.
func newStuckDrainHandler(t *testing.T, runID string) (*BusHandler, *fakeOwnerRequester, *recordingSender, *fakePublisher, *atomic.Int32) {
	t.Helper()
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	owner.setAnswer(runID, ownerAnswer())
	appends := &atomic.Int32{}
	h := newLogTestHandlerOnStore(t, owner, sender, countingFailingAppendStore{Store: newTestLogStore(t), appends: appends})
	h.logOwnerRecheckAge = time.Millisecond
	pub := &fakePublisher{}
	h.publisher = pub
	return h, owner, sender, pub, appends
}

// waitForOwnerReads waits until the owner fake answered n more lookups
// than before.
func waitForOwnerReads(t *testing.T, owner *fakeOwnerRequester, before int32, n int32) {
	t.Helper()
	waitFor(t, func() bool { return owner.callCount() >= before+n }, 10*time.Second,
		"the drain did not read the owner again")
}

// runLogEnvelopes returns the payload of each run.log envelope pub holds.
func runLogEnvelopes(pub *fakePublisher) []map[string]interface{} {
	var out []map[string]interface{}
	for i := 0; i < pub.publishedCount(); i++ {
		if env := pub.envelopeAt(i); env.Kind == kindRunLog {
			out = append(out, env.Payload)
		}
	}
	return out
}

// TestDrainRetryEndsWhenTheOwnerReadFindsTheRunTerminal proves that a
// drain that retries a failing store write ends at the finalize deadline
// that the owner recheck reads. This gateway gets no COMPLETED event and
// no later chunk for the run.
func TestDrainRetryEndsWhenTheOwnerReadFindsTheRunTerminal(t *testing.T) {
	runID := newTestRunID()
	h, owner, sender, pub, appends := newStuckDrainHandler(t, runID)

	sendInst1Chunk(h, runID, 0, "a", false)
	waitFor(t, func() bool { return appends.Load() >= 1 }, 10*time.Second, "the drain did not try the write")
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state for the admitted chunk")
	}

	anchor := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	owner.setAnswer(runID, completedOwnerAnswer(anchor))
	ack := waitForLogAcks(t, sender, 1)[0]
	if ack.Status != logAckStatusRejected || ack.Reason != logReasonFinalizeDeadline || ack.AckSeq != 0 {
		t.Fatalf("expected chunk 0 REJECTED with finalize-deadline, got %+v", ack)
	}
	waitForEviction(t, h, runID)
	if !h.runRejectedForever(runID) {
		t.Fatal("expected a rejected tombstone after the retry ended")
	}

	st.mu.Lock()
	terminal, quiet, deadlineAt := st.terminal, st.ownerReadTerminal, st.deadlineAt
	st.mu.Unlock()
	if !terminal || !quiet {
		t.Fatalf("terminal=%v ownerReadTerminal=%v, want both set by the owner read", terminal, quiet)
	}
	if want := anchor.Add(h.runFinalizeWait()); !deadlineAt.Equal(want) {
		t.Fatalf("deadline = %v, want the anchor plus the finalize wait, %v", deadlineAt, want)
	}
	if incomplete, reason := h.runIncompleteness(runID); incomplete {
		t.Fatalf("run incomplete with %q, want no reason of the quiet end", reason)
	}
	if envs := runLogEnvelopes(pub); len(envs) != 0 {
		t.Fatalf("published run.log envelopes %v, want none", envs)
	}
}

// TestOwnerReadQuietEndRefusesALaterChunkWithNoMark proves that a chunk
// that reaches the replica after the quiet end of an owner read gets
// REJECTED{finalize-deadline} and publishes nothing. The chunk can repeat
// a part that another replica stored, so the replica cannot tell a lost
// chunk from a repeat. The recorder's sweep reads the stored index.
func TestOwnerReadQuietEndRefusesALaterChunkWithNoMark(t *testing.T) {
	runID := newTestRunID()
	h, owner, sender, pub, appends := newStuckDrainHandler(t, runID)
	startStuckRun(t, h, runID, appends)

	owner.setAnswer(runID, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	if ack := waitForLogAcks(t, sender, 1)[0]; ack.Status != logAckStatusRejected || ack.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected chunk 0 REJECTED with finalize-deadline, got %+v", ack)
	}
	waitForEviction(t, h, runID)

	sendInst1Chunk(h, runID, 1, "b", true)
	if ack := waitForLogAcks(t, sender, 2)[1]; ack.Status != logAckStatusRejected ||
		ack.Reason != logReasonFinalizeDeadline || ack.AckSeq != 1 {
		t.Fatalf("expected chunk 1 REJECTED with finalize-deadline, got %+v", ack)
	}
	if envs := runLogEnvelopes(pub); len(envs) != 0 {
		t.Fatalf("published run.log envelopes %v, want none", envs)
	}
	if incomplete, reason := h.runIncompleteness(runID); incomplete {
		t.Fatalf("run incomplete with %q, want no reason of the quiet end", reason)
	}
}

// TestDrainRetryContinuesWhileTheOwnerReadFindsTheRunLive proves that a
// live owner answer sets no deadline. The drain keeps its chunk and keeps
// retrying.
func TestDrainRetryContinuesWhileTheOwnerReadFindsTheRunLive(t *testing.T) {
	runID := newTestRunID()
	h, owner, sender, _, appends := newStuckDrainHandler(t, runID)

	before := owner.callCount()
	sendInst1Chunk(h, runID, 0, "a", false)
	waitForOwnerReads(t, owner, before, 3)
	waitFor(t, func() bool { return appends.Load() >= 2 }, 10*time.Second, "the drain did not retry the write")

	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected the run state kept")
	}
	st.mu.Lock()
	terminal, rejected, draining, queued := st.terminal, st.rejectedForever, st.draining, len(st.queue)
	st.mu.Unlock()
	if terminal || rejected || !draining || queued != 1 {
		t.Fatalf("terminal=%v rejected=%v draining=%v queued=%d, want a live drain that holds its chunk",
			terminal, rejected, draining, queued)
	}
	if acks := sender.logAcks(); len(acks) != 0 {
		t.Fatalf("got acks %+v, want none while the run is live", acks)
	}

	// End the drain, so no goroutine outlives the test.
	owner.setAnswer(runID, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	waitForLogAcks(t, sender, 1)
	waitForEviction(t, h, runID)
}

// TestDrainRetryEndOnAReplicaThatLostTheStreamKeepsTheLogComplete proves
// that the quiet end never marks a finished log short. Replica A's store
// fails, so its drain retries chunk 0. The mite moves to replica B, which
// stores the whole log and finalizes it. A's owner read then finds the run
// terminal, and A ends its retry with no run.log envelope.
func TestDrainRetryEndOnAReplicaThatLostTheStreamKeepsTheLogComplete(t *testing.T) {
	runID := newTestRunID()
	shared := newTestLogStore(t)
	owner := newFakeOwnerRequester()
	owner.setAnswer(runID, ownerAnswer())

	senderA := &recordingSender{}
	appends := &atomic.Int32{}
	a := newLogTestHandlerOnStore(t, owner, senderA, countingFailingAppendStore{Store: shared, appends: appends})
	a.logOwnerRecheckAge = time.Millisecond
	pubA := &fakePublisher{}
	a.publisher = pubA

	senderB := &recordingSender{}
	b := newLogTestHandlerOnStore(t, owner, senderB, shared)
	pubB := &fakePublisher{}
	b.publisher = pubB

	sendInst1Chunk(a, runID, 0, "a0 ", false)
	waitFor(t, func() bool { return appends.Load() >= 1 }, 10*time.Second, "replica A did not try the write")

	sendInst1Chunk(b, runID, 0, "a0 ", false)
	sendInst1Chunk(b, runID, 1, "a1 ", true)
	acks := waitForLogAcks(t, senderB, 2)
	if last := acks[1]; last.Status != logAckStatusStored || last.AckSeq != 1 {
		t.Fatalf("expected replica B to store the final chunk at ack_seq 1, got %+v", last)
	}
	waitForEviction(t, b, runID)
	if !b.runFinalized(runID) {
		t.Fatal("expected replica B to finalize the log")
	}

	owner.setAnswer(runID, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	if ack := waitForLogAcks(t, senderA, 1)[0]; ack.Status != logAckStatusRejected || ack.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected replica A to end its retry with REJECTED finalize-deadline, got %+v", ack)
	}
	waitForEviction(t, a, runID)

	for _, p := range runLogEnvelopes(pubA) {
		if _, ok := p["incomplete"]; ok {
			t.Fatalf("replica A published %v, want no incomplete marker", p)
		}
	}
	if incomplete, reason := a.runIncompleteness(runID); incomplete {
		t.Fatalf("replica A records the run incomplete with %q, want no reason", reason)
	}
	if got, want := readStoredLog(t, shared, runID), "a0 a1 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	// A later COMPLETED on replica A finds the tombstone and creates no
	// state. Its log facts carry no incomplete flag.
	if a.markRunTerminal("ns1", "cell1", runID, "", time.Now()) {
		t.Fatal("expected no new terminal mark on the tombstone")
	}
}

// TestFirstRecoveryRetryEndsWhenTheOwnerReadFindsTheRunTerminal proves
// that a first recovery that keeps failing also reads the owner, and ends
// at the deadline of a terminal run.
func TestFirstRecoveryRetryEndsWhenTheOwnerReadFindsTheRunTerminal(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	fail, reads := &atomic.Bool{}, &atomic.Int32{}
	fail.Store(true)
	h := newLogTestHandlerOnStore(t, owner, sender, indexReadFailStore{Store: newTestLogStore(t), fail: fail, reads: reads})
	h.logOwnerRecheckAge = time.Millisecond

	before := owner.callCount()
	sendInst1Chunk(h, runID, 0, "a", false)
	waitForOwnerReads(t, owner, before, 2)
	if acks := sender.logAcks(); len(acks) != 0 {
		t.Fatalf("got acks %+v, want none while the run is live", acks)
	}

	owner.setAnswer(runID, completedOwnerAnswer(time.Now().Add(-2*time.Hour)))
	if ack := waitForLogAcks(t, sender, 1)[0]; ack.Status != logAckStatusRejected || ack.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected the chunk REJECTED with finalize-deadline, got %+v", ack)
	}
	waitForEviction(t, h, runID)
}

// TestFirstRecoveryRetryRefusesTheChunkOfALostOwner proves that a first
// recovery that keeps failing refuses the chunk of a sender that the
// ledger no longer names, and leaves the run open.
func TestFirstRecoveryRetryRefusesTheChunkOfALostOwner(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	fail, reads := &atomic.Bool{}, &atomic.Int32{}
	fail.Store(true)
	h := newLogTestHandlerOnStore(t, owner, sender, indexReadFailStore{Store: newTestLogStore(t), fail: fail, reads: reads})
	h.logOwnerRecheckAge = time.Millisecond

	sendInst1Chunk(h, runID, 0, "a", false)
	waitFor(t, func() bool { return reads.Load() >= 1 }, 10*time.Second, "the drain did not try its recovery")

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst1"})
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
	acks := waitForLogAcks(t, sender, 2)
	for _, ack := range acks {
		if ack.Status != logAckStatusRejected || ack.Reason != logReasonForeignRun {
			t.Fatalf("expected each chunk REJECTED with foreign-run, got %+v", ack)
		}
	}
	if h.runRejectedForever(runID) {
		t.Fatal("expected the run kept open for its owner")
	}
}

// indexAppendFailStore fails every Append to a log index and counts the
// calls. A log part write still lands, so a store fault starts between
// the part write and the index write.
type indexAppendFailStore struct {
	blob.Store
	appends *atomic.Int32
}

func (s indexAppendFailStore) Append(ctx context.Context, key string, seq int64, data []byte, opts blob.ObjectOptions) error {
	if strings.HasPrefix(key, "logindex/") {
		s.appends.Add(1)
		return errors.New("index write failed")
	}
	return s.Store.Append(ctx, key, seq, data, opts)
}

// writerAnswer is the ledger answer that names ns1/cell1/instance as the
// owner of a live run.
func writerAnswer(instance string) ownerLookupResponse {
	return ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: instance}
}

// TestIndexRetryEndsAtTheOwnerRead proves that a store fault that starts
// after the part write ends at the owner read. The index write of a
// writer's chunk and the index reservation of a moved writer both retry a
// failing index append. A terminal answer ends the retry at the finalize
// deadline, quietly. An answer that names another owner refuses the
// chunk and leaves the run open.
func TestIndexRetryEndsAtTheOwnerRead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		moved      bool
		terminal   bool
		wantReason string
	}{
		{name: "index entry, terminal run", terminal: true, wantReason: logReasonFinalizeDeadline},
		{name: "index entry, another owner", wantReason: logReasonForeignRun},
		{name: "index reservation, terminal run", moved: true, terminal: true, wantReason: logReasonFinalizeDeadline},
		{name: "index reservation, another owner", moved: true, wantReason: logReasonForeignRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := newTestRunID()
			owner := newFakeOwnerRequester()
			owner.setAnswer(runID, writerAnswer("inst1"))
			shared := newTestLogStore(t)
			instance, seq := "inst1", int64(0)
			if tc.moved {
				// The first writer stores chunk 0, so the chunk of inst2
				// is a move that reserves index slot 1.
				firstSender := &recordingSender{}
				first := newLogTestHandlerOnStore(t, owner, firstSender, shared)
				sendInst1Chunk(first, runID, 0, "a", false)
				if got := waitForLogAcks(t, firstSender, 1)[0]; got.Status != logAckStatusStored {
					t.Fatalf("first writer: expected chunk 0 STORED, got %+v", got)
				}
				instance, seq = "inst2", 1
				owner.setAnswer(runID, writerAnswer("inst2"))
			}

			sender := &recordingSender{}
			appends := &atomic.Int32{}
			h := newLogTestHandlerOnStore(t, owner, sender, indexAppendFailStore{Store: shared, appends: appends})
			h.logOwnerRecheckAge = time.Millisecond
			pub := &fakePublisher{}
			h.publisher = pub

			h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
				RunId: runID, Seq: seq, Data: []byte("b"), CellInstance: instance, JobPath: "tenant-a/build",
			})
			waitFor(t, func() bool { return appends.Load() >= 2 }, 10*time.Second, "the drain did not retry the index append")
			if acks := sender.logAcks(); len(acks) != 0 {
				t.Fatalf("got acks %+v, want none while the run is live", acks)
			}

			if tc.terminal {
				a := writerAnswer(instance)
				a.Status, a.AnchorUnixMs = "COMPLETED", time.Now().Add(-2*time.Hour).UnixMilli()
				owner.setAnswer(runID, a)
			} else {
				owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst9"})
			}
			ack := waitForLogAcks(t, sender, 1)[0]
			if ack.Status != logAckStatusRejected || ack.Reason != tc.wantReason || ack.AckSeq != seq {
				t.Fatalf("expected chunk %d REJECTED with %s, got %+v", seq, tc.wantReason, ack)
			}
			if tc.terminal {
				waitForEviction(t, h, runID)
				if !h.runRejectedForever(runID) {
					t.Fatal("expected a rejected tombstone after the retry ended")
				}
				if envs := runLogEnvelopes(pub); len(envs) != 0 {
					t.Fatalf("published run.log envelopes %v, want none for the quiet end", envs)
				}
				return
			}
			st, ok := h.lookupRunState(runID)
			if !ok {
				t.Fatal("expected the run state kept for its owner")
			}
			waitForIdleRun(t, st)
			if h.runRejectedForever(runID) {
				t.Fatal("expected the run kept open for its owner")
			}
		})
	}
}

// newQuietEndHandler builds a stuck drain handler that also takes
// COMPLETED events. cell1 (instance inst1) owns the run, and cell2
// (instance inst2) hosts the same tenant. The finalize wait is one hour,
// so a test ends the retry with fireDeadline.
func newQuietEndHandler(t *testing.T, runID string) (*BusHandler, *fakeOwnerRequester, *recordingSender, *fakePublisher, *atomic.Int32) {
	t.Helper()
	h, owner, sender, pub, appends := newStuckDrainHandler(t, runID)
	h.logFinalizeWait = time.Hour
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"tenant-a/build": {tenant: "tenant-a", generation: 1},
	}})
	h.RecordCellInstance("cell1", "ns1", "inst1")
	h.buildMu.Lock()
	h.buildWorkers["ns1/cell2"] = &registeredBuildWorker{worker: &buildEventWorker{send: (&recordingSender{}).send}}
	h.buildMu.Unlock()
	h.RecordCellInstance("cell2", "ns1", "inst2")
	return h, owner, sender, pub, appends
}

// fireDeadline runs the run's finalize deadline as its timer does.
func fireDeadline(h *BusHandler, runID string, st *runLogState) {
	st.mu.Lock()
	cancel := st.appendCancel
	st.mu.Unlock()
	h.runDeadlineFired(runID, st, cancel)
}

// quietMark reports the terminal mark and the quiet flag of st.
func quietMark(st *runLogState) (terminal, quiet bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.terminal, st.ownerReadTerminal
}

// startStuckRun sends chunk 0 of runID to a handler whose store fails,
// and returns the run state once the drain tried the write.
func startStuckRun(t *testing.T, h *BusHandler, runID string, appends *atomic.Int32) *runLogState {
	t.Helper()
	sendInst1Chunk(h, runID, 0, "a", false)
	waitFor(t, func() bool { return appends.Load() >= 1 }, 10*time.Second, "the drain did not try the write")
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state for the admitted chunk")
	}
	return st
}

// assertLoudEnd waits for the REJECTED ack of the deadline and checks that
// the gateway records and publishes finalize-deadline.
func assertLoudEnd(t *testing.T, h *BusHandler, sender *recordingSender, pub *fakePublisher, runID string) {
	t.Helper()
	if ack := waitForLogAcks(t, sender, 1)[0]; ack.Status != logAckStatusRejected || ack.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected REJECTED with finalize-deadline, got %+v", ack)
	}
	waitForEviction(t, h, runID)
	if incomplete, reason := h.runIncompleteness(runID); !incomplete || reason != logReasonFinalizeDeadline {
		t.Fatalf("incomplete=%v reason=%q, want finalize-deadline", incomplete, reason)
	}
	for _, p := range runLogEnvelopes(pub) {
		if p["incomplete"] == true && p["incomplete_reason"] == logReasonFinalizeDeadline {
			return
		}
	}
	t.Fatalf("published run.log envelopes %v, want one with incomplete finalize-deadline", runLogEnvelopes(pub))
}

// TestQuietEndClearsWhenTheWritersStreamReachesThisReplica proves that the
// end of a mark that the owner read set stops being quiet when the
// writer's stream reaches this replica later: the owner's COMPLETED event,
// or a chunk of the terminal run. The replica then publishes its exact
// reason at the deadline.
func TestQuietEndClearsWhenTheWritersStreamReachesThisReplica(t *testing.T) {
	for _, trigger := range []string{"completed event", "terminal-run chunk"} {
		t.Run(trigger, func(t *testing.T) {
			runID := newTestRunID()
			h, owner, sender, pub, appends := newQuietEndHandler(t, runID)
			st := startStuckRun(t, h, runID, appends)

			anchor := time.Now().Truncate(time.Millisecond)
			owner.setAnswer(runID, completedOwnerAnswer(anchor))
			waitFor(t, func() bool {
				terminal, quiet := quietMark(st)
				return terminal && quiet
			}, 10*time.Second, "the owner read did not set the quiet terminal mark")

			if trigger == "completed event" {
				if err := h.OnBuildEvent("cell1", "ns1", 0, completedBuildEvent(runID, anchor)); err != nil {
					t.Fatalf("OnBuildEvent: %v", err)
				}
			} else {
				sendInst1Chunk(h, runID, 1, "b", false)
			}
			if _, quiet := quietMark(st); quiet {
				t.Fatalf("the %s left the end quiet", trigger)
			}

			fireDeadline(h, runID, st)
			assertLoudEnd(t, h, sender, pub, runID)
		})
	}
}

// TestCompletedEventBeforeTheOwnerReadKeepsTheEndLoud proves the other
// order. The owner's COMPLETED event sets the mark first, and a later
// owner read that finds the run terminal does not make the end quiet.
func TestCompletedEventBeforeTheOwnerReadKeepsTheEndLoud(t *testing.T) {
	runID := newTestRunID()
	h, owner, sender, pub, appends := newQuietEndHandler(t, runID)
	st := startStuckRun(t, h, runID, appends)

	anchor := time.Now().Truncate(time.Millisecond)
	if err := h.OnBuildEvent("cell1", "ns1", 0, completedBuildEvent(runID, anchor)); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	if terminal, quiet := quietMark(st); !terminal || quiet {
		t.Fatalf("terminal=%v quiet=%v after the COMPLETED event, want a terminal mark that is not quiet", terminal, quiet)
	}

	before := owner.callCount()
	owner.setAnswer(runID, completedOwnerAnswer(anchor))
	waitForOwnerReads(t, owner, before, 2)
	if _, quiet := quietMark(st); quiet {
		t.Fatal("the owner read made the end of the COMPLETED event's mark quiet")
	}

	fireDeadline(h, runID, st)
	assertLoudEnd(t, h, sender, pub, runID)
}

// TestCompletedEventOfANonOwnerKeepsTheEndQuiet proves that a COMPLETED
// event from a cell that does not own the run leaves the quiet end. Such
// a sender does not hold the writer's stream, so the end publishes
// nothing and records no reason.
func TestCompletedEventOfANonOwnerKeepsTheEndQuiet(t *testing.T) {
	runID := newTestRunID()
	h, owner, sender, pub, appends := newQuietEndHandler(t, runID)
	st := startStuckRun(t, h, runID, appends)

	anchor := time.Now().Truncate(time.Millisecond)
	owner.setAnswer(runID, completedOwnerAnswer(anchor))
	waitFor(t, func() bool {
		terminal, quiet := quietMark(st)
		return terminal && quiet
	}, 10*time.Second, "the owner read did not set the quiet terminal mark")

	if err := h.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(runID, anchor)); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	if _, quiet := quietMark(st); !quiet {
		t.Fatal("a COMPLETED event of a non-owner cleared the quiet end")
	}

	fireDeadline(h, runID, st)
	if ack := waitForLogAcks(t, sender, 1)[0]; ack.Status != logAckStatusRejected || ack.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected REJECTED with finalize-deadline, got %+v", ack)
	}
	waitForEviction(t, h, runID)
	if incomplete, reason := h.runIncompleteness(runID); incomplete {
		t.Fatalf("run incomplete with %q, want no reason of the quiet end", reason)
	}
	if envs := runLogEnvelopes(pub); len(envs) != 0 {
		t.Fatalf("published run.log envelopes %v, want none", envs)
	}
}
