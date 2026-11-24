package mite

import (
	"context"
	"reflect"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// sendOwnerChunk sends one chunk of runID from cell2, the owner in these
// tests.
func sendOwnerChunk(h *BusHandler, runID string, seq int64, data string, final bool) {
	h.OnLogChunk("cell2", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: seq, Data: []byte(data), Final: final, CellInstance: "inst2", JobPath: "tenant-a/build",
	})
}

// assertNoGatewayLogMark fails the test unless the COMPLETED event left
// no log state and no tombstone on h, published no run.log envelope, and
// queued a run.log envelope with no gateway fact: finalized false and no
// incomplete mark.
func assertNoGatewayLogMark(t *testing.T, h *BusHandler, pub *fakePublisher, runID string) {
	t.Helper()
	if h.hasRunLogRecord(runID) {
		t.Fatal("expected no log state and no tombstone for a late COMPLETED event")
	}
	if n := pub.publishedCount(); n != 0 {
		t.Fatalf("published %d envelopes, want none: another replica can hold the finished log", n)
	}
	env, found := queuedRunLog(t, h)
	if !found {
		t.Fatal("expected the run.log envelope of the COMPLETED group")
	}
	if want := map[string]interface{}{"incomplete": false, "finalized": false}; !reflect.DeepEqual(env.Payload, want) {
		t.Fatalf("run.log payload = %v, want %v", env.Payload, want)
	}
	assertQueuedRunCompleted(t, h)
}

// TestOnBuildEventLateCompletedOnTheOtherReplicaKeepsAFinishedLogComplete
// proves that a COMPLETED event past the finalize deadline, on a replica
// with no log state and no tombstone, does not mark a log incomplete that
// the other replica finished. The record store keeps a finalize-deadline
// mark over a finalized log in both arrival orders, so the build page
// would show an incomplete log. Replica B creates nothing for the run, so
// a repeat of the final chunk on B reads the stored final entry.
func TestOnBuildEventLateCompletedOnTheOtherReplicaKeepsAFinishedLogComplete(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, cell2Owner())

	a, senderA := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	pubA := &fakePublisher{}
	a.publisher = pubA
	sendOwnerChunk(a, runID, 0, "a", false)
	sendOwnerChunk(a, runID, 1, "", true)
	if last := waitForLogAcks(t, senderA, 2)[1]; last.Status != logAckStatusStored || last.AckSeq != 1 {
		t.Fatalf("expected the final chunk STORED at ack_seq 1 on replica A, got %+v", last)
	}
	waitForEviction(t, a, runID)
	if n := finalizedPublishes(pubA, runID); n != 1 {
		t.Fatalf("replica A published %d finalized markers, want 1", n)
	}

	b, senderB := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	b.logStore = a.logStore
	pubB := &fakePublisher{}
	b.publisher = pubB
	b.logFinalizeWait = time.Minute

	if err := b.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(runID, time.Now().Add(-time.Hour))); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	assertNoGatewayLogMark(t, b, pubB, runID)

	sendOwnerChunk(b, runID, 1, "", true)
	if got := waitForLogAcks(t, senderB, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("expected the repeat of the final chunk STORED at ack_seq 1 on replica B, got %+v", got)
	}
	waitFor(t, func() bool { return finalizedPublishes(pubB, runID) == 1 }, 5*time.Second,
		"replica B did not publish the finalized marker of the stored final entry")
	if n := incompletePublishes(pubB); n != 0 {
		t.Fatalf("replica B published %d incomplete markers, want none", n)
	}
}

// TestOnBuildEventLateCompletedWithNoLogStateLeavesAShortLogToTheSweep
// proves that a late COMPLETED event on a replica with no log state still
// leaves a short log to the recorder. Here the gateway restarted after
// the owner stored chunk 0, and the final chunk never came. The gateway
// publishes no mark, and the run.log envelope keeps finalized false, so
// the recorder's finalizeStaleLogs sweep reads the stored index, finds no
// final entry, and marks the log not-finalized. A later chunk of the
// terminal run gets the release of the chunk path and creates no state.
func TestOnBuildEventLateCompletedWithNoLogStateLeavesAShortLogToTheSweep(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, cell2Owner())

	before, senderBefore := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	before.logFinalizeWait = time.Hour
	sendOwnerChunk(before, runID, 0, "a", false)
	if got := waitForLogAcks(t, senderBefore, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != 0 {
		t.Fatalf("expected chunk 0 STORED before the restart, got %+v", got)
	}

	completedAt := time.Now().Add(-time.Hour)
	owner.setAnswer(runID, func() ownerLookupResponse {
		a := cell2Owner()
		a.Status, a.AnchorUnixMs = "COMPLETED", completedAt.UnixMilli()
		return a
	}())
	h, sender := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	h.logStore = before.logStore
	pub := &fakePublisher{}
	h.publisher = pub
	h.logFinalizeWait = time.Minute

	if err := h.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(runID, completedAt)); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	assertNoGatewayLogMark(t, h, pub, runID)

	entries, _, _, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read the stored log index: %v", err)
	}
	if _, ok := entries[0]; !ok {
		t.Fatal("expected the stored index entry of chunk 0")
	}
	for seq, e := range entries {
		if e.Final {
			t.Fatalf("stored index holds a final entry at seq %d, want none for a short log", seq)
		}
	}

	sendOwnerChunk(h, runID, 1, "b", true)
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected the late chunk REJECTED finalize-deadline, got %+v", got)
	}
	if h.hasRunLogRecord(runID) {
		t.Fatal("expected the late chunk to create no log state")
	}
	if n := pub.publishedCount(); n != 0 {
		t.Fatalf("published %d envelopes after the late chunk, want none", n)
	}
}

// TestOnBuildEventLateCompletedOnTheWritersReplicaStillEndsAShortLog
// proves that the rule covers only a replica with no log state. The
// replica that holds the run's state got the writer's chunks, so a late
// COMPLETED event there still ends the short log at once and publishes
// its finalize-deadline marker.
func TestOnBuildEventLateCompletedOnTheWritersReplicaStillEndsAShortLog(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, cell2Owner())
	h, sender := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	pub := &fakePublisher{}
	h.publisher = pub
	h.logFinalizeWait = time.Minute

	sendOwnerChunk(h, runID, 0, "a", false)
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != 0 {
		t.Fatalf("expected chunk 0 STORED, got %+v", got)
	}
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected the chunk to create the run state")
	}
	waitForIdleRun(t, st)

	if err := h.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(runID, time.Now().Add(-time.Hour))); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	if !h.runRejectedForever(runID) {
		t.Fatal("expected the late COMPLETED event to end the short log of the writer's replica")
	}
	if incomplete, reason := h.runIncompleteness(runID); !incomplete || reason != logReasonFinalizeDeadline {
		t.Fatalf("incomplete=%v reason=%q, want finalize-deadline", incomplete, reason)
	}
	if n := incompletePublishes(pub); n != 1 {
		t.Fatalf("published %d incomplete markers, want 1", n)
	}
	env, found := queuedRunLog(t, h)
	if !found || env.Payload["incomplete"] != true || env.Payload["incomplete_reason"] != logReasonFinalizeDeadline {
		t.Fatalf("run.log payload = %v found=%v, want incomplete finalize-deadline", env.Payload, found)
	}
}
