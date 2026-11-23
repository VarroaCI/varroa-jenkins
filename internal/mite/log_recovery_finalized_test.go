package mite

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// finalizedPublishes counts the run.log envelopes of runID that set
// finalized true.
func finalizedPublishes(pub *fakePublisher, runID string) int {
	n := 0
	for i := 0; i < pub.publishedCount(); i++ {
		env := pub.envelopeAt(i)
		if env.Kind != kindRunLog || env.RunID != runID {
			continue
		}
		if v, _ := env.Payload["finalized"].(bool); v {
			n++
		}
	}
	return n
}

// finalizedKeyPublishes counts the run.log envelopes of runID that carry
// the finalized key with any value.
func finalizedKeyPublishes(pub *fakePublisher, runID string) int {
	n := 0
	for i := 0; i < pub.publishedCount(); i++ {
		env := pub.envelopeAt(i)
		if env.Kind != kindRunLog || env.RunID != runID {
			continue
		}
		if _, ok := env.Payload["finalized"]; ok {
			n++
		}
	}
	return n
}

// syncPublishCalls returns how many JetStream publishes pub received,
// failed ones too.
func syncPublishCalls(pub *fakePublisher) int {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	return pub.syncCalls
}

// storeLogWithNoMarker stores chunks 0 and 1 of runID on a gateway with
// no publisher, so no run.log envelope reaches the bus. final sets the
// final flag on chunk 1. It models a gateway that stops after its last
// write and before its marker publish.
func storeLogWithNoMarker(t *testing.T, owner *fakeOwnerRequester, store blob.Store, runID string, final bool) {
	t.Helper()
	sender := &recordingSender{}
	h := newLogTestHandlerOnStore(t, owner, sender, store)
	sendInst1Chunk(h, runID, 0, "a", false)
	sendInst1Chunk(h, runID, 1, "b", final)
	if got := waitForLogAcks(t, sender, 2)[1]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("first gateway: expected chunk 1 STORED at ack_seq 1, got %+v", got)
	}
}

// TestRestartRecoveryOfAFinishedLogPublishesFinalizedOnce models a
// gateway that stored the final chunk and its index entry, then stopped
// before the mite got the STORED ack and before the marker reached the
// bus. The mite sends the final chunk again to a restarted gateway.
// Restart recovery reads the final entry back and publishes finalized
// true once, before the STORED ack. A later repeat of the chunk gets its
// answer from the tombstone with no store read, and an idle sweep round
// finds no state. Neither publishes the fact again.
func TestRestartRecoveryOfAFinishedLogPublishesFinalizedOnce(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	store := newTestLogStore(t)
	storeLogWithNoMarker(t, owner, store, runID, true)

	var reads atomic.Int32
	sender := &recordingSender{}
	h, clock, pub := newIdleSweepTestHandler(t, owner, sender, countingReadStore{Store: store, reads: &reads})

	sendInst1Chunk(h, runID, 1, "b", true)
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("restarted gateway: expected the repeat STORED at ack_seq 1, got %+v", got)
	}
	if got := finalizedPublishes(pub, runID); got != 1 {
		t.Fatalf("the STORED ack went out after %d finalized envelopes, want 1", got)
	}
	env := pub.envelopeAt(0)
	if len(env.Payload) != 1 || env.JobPath != "tenant-a/build" {
		t.Fatalf("finalized envelope payload=%v job_path=%q, want only finalized true and the chunk's job path", env.Payload, env.JobPath)
	}
	waitForEviction(t, h, runID)
	assertFinishedLogKept(t, h, runID)

	readsBefore := reads.Load()
	sendInst1Chunk(h, runID, 1, "b", true)
	if got := waitForLogAcks(t, sender, 2)[1]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("second repeat: expected STORED at ack_seq 1 from the tombstone, got %+v", got)
	}
	if n := reads.Load() - readsBefore; n != 0 {
		t.Fatalf("the second repeat read the store %d times, want 0: a tombstoned run must not be recovered again", n)
	}
	assertFinishedLogKept(t, h, runID)

	clock.advance(logIdleSweepAfter)
	h.sweepIdleLogRuns(context.Background())
	if got := finalizedPublishes(pub, runID); got != 1 {
		t.Fatalf("published %d finalized envelopes after the repeat and the sweep, want 1", got)
	}
}

// TestRestartRecoveryOfAnUnfinishedLogPublishesNoFinalized proves that
// recovery of a log with no final entry publishes no finalized fact. The
// final chunk that follows publishes it once, from the drain.
func TestRestartRecoveryOfAnUnfinishedLogPublishesNoFinalized(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	store := newTestLogStore(t)
	storeLogWithNoMarker(t, owner, store, runID, false)

	sender := &recordingSender{}
	h, _, pub := newIdleSweepTestHandler(t, owner, sender, store)

	sendInst1Chunk(h, runID, 1, "b", false)
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("restarted gateway: expected the repeat STORED at ack_seq 1, got %+v", got)
	}
	if got := finalizedKeyPublishes(pub, runID); got != 0 {
		t.Fatalf("recovery of an unfinished log published %d finalized envelopes, want 0", got)
	}
	if h.runFinalized(runID) {
		t.Fatal("expected the log not finalized with no final entry")
	}

	sendInst1Chunk(h, runID, 2, "", true)
	if got := waitForLogAcks(t, sender, 2)[1]; got.Status != logAckStatusStored || got.AckSeq != 2 {
		t.Fatalf("final chunk: expected STORED at ack_seq 2, got %+v", got)
	}
	if got := finalizedPublishes(pub, runID); got != 1 {
		t.Fatalf("the final chunk's STORED ack went out after %d finalized envelopes, want 1", got)
	}
}

// TestFinalizedPublishFailureIsSentAgainBeforeTheAck proves that a failed
// finalized publish is sent again, from the drain that writes the final
// entry and from the restart recovery that reads it back. The fact is
// out before the STORED ack releases the final chunk, so a gateway that
// stops before the publish leaves the chunk with the mite, and the
// mite's next send reaches restart recovery.
func TestFinalizedPublishFailureIsSentAgainBeforeTheAck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{name: "drain", restart: false},
		{name: "restart recovery", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, ownerAnswer())
			store := newTestLogStore(t)
			if tc.restart {
				storeLogWithNoMarker(t, owner, store, runID, true)
			} else {
				storeLogWithNoMarker(t, owner, store, runID, false)
			}

			sender := &recordingSender{}
			h := newLogTestHandlerOnStore(t, owner, sender, store)
			pub := &fakePublisher{syncFailLeft: 1, syncErr: errors.New("nats unavailable")}
			h.publisher = pub

			if tc.restart {
				sendInst1Chunk(h, runID, 1, "b", true)
			} else {
				sendInst1Chunk(h, runID, 1, "b", false)
				sendInst1Chunk(h, runID, 2, "", true)
			}
			acks := waitForLogAcks(t, sender, 1)
			if got := acks[len(acks)-1]; got.Status != logAckStatusStored {
				t.Fatalf("expected a STORED ack, got %+v", got)
			}
			if !tc.restart {
				acks = waitForLogAcks(t, sender, 2)
				if got := acks[1]; got.Status != logAckStatusStored || got.AckSeq != 2 {
					t.Fatalf("expected the final chunk STORED at ack_seq 2, got %+v", got)
				}
			}
			if got := finalizedPublishes(pub, runID); got != 1 {
				t.Fatalf("the final STORED ack went out after %d finalized envelopes, want 1", got)
			}
			if calls := syncPublishCalls(pub); calls != 2 {
				t.Fatalf("publish calls = %d, want 2: one failed and one sent again", calls)
			}
		})
	}
}

// TestPublishFinalizedEndsWithItsContext proves that the retry of a
// failed finalized publish ends when its context ends. The drain passes
// appendCtx, which the finalize deadline ends.
func TestPublishFinalizedEndsWithItsContext(t *testing.T) {
	h := newLogTestHandler(t, newFakeOwnerRequester(), &recordingSender{})
	pub := &fakePublisher{syncFailLeft: 1000, syncErr: errors.New("nats unavailable")}
	h.publisher = pub
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.publishFinalized(ctx, "ns1", "cell1", newTestRunID(), "tenant-a/build")

	if calls := syncPublishCalls(pub); calls != 1 {
		t.Fatalf("publish calls = %d, want 1: an ended context stops the retry", calls)
	}
	if pub.publishedCount() != 0 {
		t.Fatalf("published %d envelopes, want 0", pub.publishedCount())
	}
}
