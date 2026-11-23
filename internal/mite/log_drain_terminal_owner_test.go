package mite

import (
	"context"
	"errors"
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
