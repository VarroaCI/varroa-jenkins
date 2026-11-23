package mite

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// owedMarkerCount returns the number of owed finalized markers.
func owedMarkerCount(h *BusHandler) int {
	h.logMarkerMu.Lock()
	defer h.logMarkerMu.Unlock()
	return len(h.logMarkersOwed)
}

// downPublisher returns a publisher whose every JetStream publish fails,
// as during a bus outage.
func downPublisher() *fakePublisher {
	return &fakePublisher{syncFailLeft: math.MaxInt32, syncErr: errors.New("nats unavailable")}
}

// bringPublisherUp ends the outage of pub.
func bringPublisherUp(pub *fakePublisher) {
	pub.mu.Lock()
	pub.syncFailLeft = 0
	pub.mu.Unlock()
}

// TestFinalizedMarkerOutagePastTheWaitIsSentAgain proves that a bus
// outage longer than the marker wait does not hold the final chunk. The
// final STORED ack goes out with no marker on the bus, and the marker is
// owed. A resend pass during the outage keeps it. The first pass after
// the bus returns publishes it once, and a later pass publishes nothing.
// A chunk that is not final waits for no marker: its ack follows no
// publish attempt. Both the drain and restart recovery owe the marker.
func TestFinalizedMarkerOutagePastTheWaitIsSentAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{name: "drain", restart: false},
		{name: "restart recovery", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			runID, otherRun := newTestRunID(), newTestRunID()
			owner.setAnswer(runID, ownerAnswer())
			owner.setAnswer(otherRun, ownerAnswer())
			store := newTestLogStore(t)
			if tc.restart {
				storeLogWithNoMarker(t, owner, store, runID, true)
			}

			sender := &recordingSender{}
			h, _, _ := newIdleSweepTestHandler(t, owner, sender, store)
			pub := downPublisher()
			h.publisher = pub
			h.logMarkerWait = time.Millisecond

			if tc.restart {
				sendInst1Chunk(h, runID, 1, "b", true)
			} else {
				sendInst1Chunk(h, runID, 0, "a", false)
				if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored || got.AckSeq != 0 {
					t.Fatalf("expected chunk 0 STORED at ack_seq 0, got %+v", got)
				}
				if calls := syncPublishCalls(pub); calls != 0 {
					t.Fatalf("the ack of a chunk that is not final followed %d publish attempts, want 0", calls)
				}
				sendInst1Chunk(h, runID, 1, "b", true)
			}
			acks := waitForLogAcks(t, sender, map[bool]int{true: 1, false: 2}[tc.restart])
			if got := acks[len(acks)-1]; got.Status != logAckStatusStored || got.AckSeq != 1 {
				t.Fatalf("expected the final chunk STORED at ack_seq 1, got %+v", got)
			}
			if syncPublishCalls(pub) == 0 {
				t.Fatal("expected a marker publish attempt before the final ack")
			}
			if got := finalizedPublishes(pub, runID); got != 0 {
				t.Fatalf("published %d finalized envelopes during the outage, want 0", got)
			}
			waitForEviction(t, h, runID)
			if n := owedMarkerCount(h); n != 1 {
				t.Fatalf("owed markers = %d, want 1", n)
			}

			callsBefore := syncPublishCalls(pub)
			sendInst1Chunk(h, otherRun, 0, "x", false)
			if got := sender.logAcks(); len(got) == 0 || got[len(got)-1].RunId != otherRun {
				waitFor(t, func() bool {
					a := sender.logAcks()
					return len(a) > 0 && a[len(a)-1].RunId == otherRun
				}, 5*time.Second, "the chunk of another run got no ack")
			}
			if calls := syncPublishCalls(pub); calls != callsBefore {
				t.Fatalf("the ack of another run's chunk followed %d publish attempts, want 0", calls-callsBefore)
			}

			h.resendOwedMarkers(context.Background())
			if got := finalizedPublishes(pub, runID); got != 0 {
				t.Fatalf("a resend during the outage published %d finalized envelopes, want 0", got)
			}
			if n := owedMarkerCount(h); n != 1 {
				t.Fatalf("owed markers after a failed resend = %d, want 1", n)
			}

			bringPublisherUp(pub)
			h.resendOwedMarkers(context.Background())
			if got := finalizedPublishes(pub, runID); got != 1 {
				t.Fatalf("the resend after the outage published %d finalized envelopes, want 1", got)
			}
			env := pub.envelopeAt(pub.publishedCount() - 1)
			if env.RunID != runID || len(env.Payload) != 1 || env.JobPath != "tenant-a/build" {
				t.Fatalf("resent envelope run=%s payload=%v job_path=%q, want only finalized true and the job path",
					env.RunID, env.Payload, env.JobPath)
			}
			if n := owedMarkerCount(h); n != 0 {
				t.Fatalf("owed markers after the resend = %d, want 0", n)
			}

			h.resendOwedMarkers(context.Background())
			if got := finalizedPublishes(pub, runID); got != 1 {
				t.Fatalf("a second pass published %d finalized envelopes in all, want 1", got)
			}
		})
	}
}

// TestOwedFinalizedMarkerAgeBound proves that a marker owed for
// logMarkerResendAge is dropped with no publish. The recorder's sweep
// marks such a run, and a later marker changes nothing.
func TestOwedFinalizedMarkerAgeBound(t *testing.T) {
	runID := newTestRunID()
	h, clock, _ := newIdleSweepTestHandler(t, newFakeOwnerRequester(), &recordingSender{}, newTestLogStore(t))
	pub := downPublisher()
	h.publisher = pub

	h.oweFinalizedMarker("ns1", "cell1", runID, "tenant-a/build")
	clock.advance(logMarkerResendAge - time.Second)
	h.resendOwedMarkers(context.Background())
	if n := owedMarkerCount(h); n != 1 {
		t.Fatalf("owed markers inside the age bound = %d, want 1", n)
	}

	clock.advance(time.Second)
	bringPublisherUp(pub)
	h.resendOwedMarkers(context.Background())
	if got := finalizedPublishes(pub, runID); got != 0 {
		t.Fatalf("published %d finalized envelopes for a marker past its age bound, want 0", got)
	}
	if n := owedMarkerCount(h); n != 0 {
		t.Fatalf("owed markers past the age bound = %d, want 0", n)
	}
}

// TestOwedFinalizedMarkerCountBound proves that the gateway keeps at most
// logMarkersOwedMax owed markers. A marker over the bound is not kept,
// and a repeat of a kept marker adds nothing.
func TestOwedFinalizedMarkerCountBound(t *testing.T) {
	h, _, _ := newIdleSweepTestHandler(t, newFakeOwnerRequester(), &recordingSender{}, newTestLogStore(t))
	h.logMarkerMu.Lock()
	h.logMarkersOwed = make(map[string]*owedFinalizedMarker, logMarkersOwedMax)
	for i := 0; i < logMarkersOwedMax; i++ {
		h.logMarkersOwed[fmt.Sprintf("run-%d", i)] = &owedFinalizedMarker{ns: "ns1", name: "cell1"}
	}
	h.logMarkerMu.Unlock()

	runID := newTestRunID()
	h.oweFinalizedMarker("ns1", "cell1", runID, "tenant-a/build")
	h.oweFinalizedMarker("ns1", "cell1", "run-0", "tenant-a/build")
	if n := owedMarkerCount(h); n != logMarkersOwedMax {
		t.Fatalf("owed markers = %d, want the bound %d", n, logMarkersOwedMax)
	}
	h.logMarkerMu.Lock()
	_, kept := h.logMarkersOwed[runID]
	h.logMarkerMu.Unlock()
	if kept {
		t.Fatal("expected the marker over the bound not kept")
	}
}

// TestCloseEndsTheFinalizedPublishWait proves that Close ends a drain that
// waits for a finalized publish. The stopping gateway sends no ack for
// the final chunk and owes no marker: the mite keeps the chunk, and the
// restart recovery of its next send publishes the marker.
func TestCloseEndsTheFinalizedPublishWait(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	sender := &recordingSender{}
	h, _, _ := newIdleSweepTestHandler(t, owner, sender, newTestLogStore(t))
	pub := downPublisher()
	h.publisher = pub
	h.logMarkerWait = time.Hour
	h.logStop = make(chan struct{})
	h.contentDone = make(chan struct{})

	sendInst1Chunk(h, runID, 0, "a", false)
	waitForLogAcks(t, sender, 1)
	sendInst1Chunk(h, runID, 1, "b", true)
	waitFor(t, func() bool { return syncPublishCalls(pub) >= 1 }, 10*time.Second, "the drain did not try the marker")

	h.Close()
	waitForEviction(t, h, runID)
	if acks := sender.logAcks(); len(acks) != 1 {
		t.Fatalf("got acks %+v, want only chunk 0: the mite must keep the final chunk", acks)
	}
	if n := owedMarkerCount(h); n != 0 {
		t.Fatalf("owed markers = %d, want 0 for a stopping gateway", n)
	}
	if got := finalizedPublishes(pub, runID); got != 0 {
		t.Fatalf("published %d finalized envelopes, want 0", got)
	}
}
