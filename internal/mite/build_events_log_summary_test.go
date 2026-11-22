package mite

import (
	"errors"
	"reflect"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// cell2Worker is the build worker key of cell2, the sender of every
// build event in these tests.
const cell2Worker = "ns1/cell2"

// queuedEnvelopes returns every envelope that OnBuildEvent queued on the
// build worker of cell2. No worker goroutine runs in these tests, so the
// queue keeps each item.
func queuedEnvelopes(t *testing.T, h *BusHandler) []ingestEnvelope {
	t.Helper()
	h.buildMu.Lock()
	reg := h.buildWorkers[cell2Worker]
	h.buildMu.Unlock()
	if reg == nil {
		t.Fatalf("no build worker for %s", cell2Worker)
	}
	reg.worker.mu.Lock()
	defer reg.worker.mu.Unlock()
	var out []ingestEnvelope
	for _, item := range reg.worker.queue {
		out = append(out, item.envelopes...)
	}
	return out
}

// queuedRunLog returns the run.log envelope that OnBuildEvent queued on
// the build worker of cell2. found is false when it queued none.
func queuedRunLog(t *testing.T, h *BusHandler) (ingestEnvelope, bool) {
	t.Helper()
	var env ingestEnvelope
	found := false
	for _, e := range queuedEnvelopes(t, h) {
		if e.Kind != kindRunLog {
			continue
		}
		if found {
			t.Fatal("OnBuildEvent queued more than one run.log envelope")
		}
		env, found = e, true
	}
	return env, found
}

// assertQueuedRunCompleted fails the test unless OnBuildEvent queued the
// run.completed envelope on the build worker of cell2. The recorder
// decides a foreign completion from that envelope, so the gateway never
// drops it.
func assertQueuedRunCompleted(t *testing.T, h *BusHandler) {
	t.Helper()
	for _, e := range queuedEnvelopes(t, h) {
		if e.Kind == kindRunCompleted {
			return
		}
	}
	t.Fatal("OnBuildEvent queued no run.completed envelope")
}

// lossyCompletedBuildEvent returns a COMPLETED event whose plugin summary
// reports a loss: the summary a stale cell's boot reconcile sends.
func lossyCompletedBuildEvent(runID string) *mitev1.BuildEvent {
	evt := completedBuildEvent(runID, time.Now())
	evt.LogSummary = &mitev1.LogSummary{
		ChunkCount: 9, DroppedBytes: 3, Incomplete: true, IncompleteReason: "jenkins-restart",
	}
	return evt
}

// assertQueuedSenderSummary fails the test unless OnBuildEvent queued the
// whole summary of lossyCompletedBuildEvent, stamped with cell2's
// registered instance.
func assertQueuedSenderSummary(t *testing.T, h *BusHandler) {
	t.Helper()
	env, found := queuedRunLog(t, h)
	if !found {
		t.Fatal("expected the sender's run.log envelope")
	}
	want := map[string]interface{}{
		"chunk_count": int64(9), "dropped_bytes": int64(3),
		"incomplete": true, "incomplete_reason": "jenkins-restart", "finalized": false,
	}
	if !reflect.DeepEqual(env.Payload, want) {
		t.Fatalf("run.log payload = %v, want the sender's whole summary %v", env.Payload, want)
	}
	if env.CellInstance != "inst2" {
		t.Fatalf("run.log cell_instance = %q, want the sender's registered inst2", env.CellInstance)
	}
}

// assertRunTerminal fails the test unless runID has a run state that is
// marked terminal.
func assertRunTerminal(t *testing.T, h *BusHandler, runID string) {
	t.Helper()
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected a run state for the owner's COMPLETED event")
	}
	st.mu.Lock()
	terminal := st.terminal
	st.mu.Unlock()
	if !terminal {
		t.Fatal("expected the owner's COMPLETED event to mark the run terminal")
	}
}

// sentEventAcks returns every EventAck the sender received.
func sentEventAcks(s *recordingSender) []*mitev1.EventAck {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*mitev1.EventAck
	for _, m := range s.sent {
		if a := m.GetEventAck(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// TestOnBuildEventCompletedFromAnotherOwnerInstancePublishesNoSenderLogSummary
// proves that a COMPLETED event publishes no run.log envelope when a
// ledger answer names another cell instance as the owner, and the
// gateway holds no fact of the run's log. The sender's summary describes
// a log that the owner does not write, and the recorder merges every
// run.log with no owner check. The run.completed envelope still goes
// out, so the recorder records the foreign completion.
func TestOnBuildEventCompletedFromAnotherOwnerInstancePublishesNoSenderLogSummary(t *testing.T) {
	cases := []struct {
		name   string
		answer ownerLookupResponse
	}{
		{
			name:   "foreign cell",
			answer: ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"},
		},
		{
			name:   "stale instance of the same cell",
			answer: ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst-old"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, tc.answer)
			h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})

			if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
				t.Fatalf("OnBuildEvent: %v", err)
			}
			if env, found := queuedRunLog(t, h); found {
				t.Fatalf("queued run.log payload %v, want no run.log for a sender that does not own the run", env.Payload)
			}
			assertQueuedRunCompleted(t, h)
			if calls := owner.callCount(); calls != 1 {
				t.Fatalf("owner lookups = %d, want 1 for a cache miss", calls)
			}
		})
	}
}

// TestOnBuildEventCompletedFromANonOwnerCarriesTheGatewayLogFacts proves
// that a COMPLETED event from a cell that does not own the run still
// carries the gateway's own facts of the run's log, and none of the
// sender's. A fact of the gateway describes the stored log, whatever
// cell sent the event. A finalized true dropped here could leave a
// complete log to the recorder's finalizeStaleLogs sweep, which marks it
// not-finalized. The owner's chunks cached the owner, so the cached
// entry refuses the sender. The gateway reads the ledger once more
// before it withholds the summary.
func TestOnBuildEventCompletedFromANonOwnerCarriesTheGatewayLogFacts(t *testing.T) {
	t.Run("finalized log", func(t *testing.T) {
		sender := &recordingSender{}
		owner := newFakeOwnerRequester()
		runID := newTestRunID()
		owner.setAnswer(runID, ownerAnswer())
		h, _ := newCompletedOwnerTestHandler(t, owner, sender)
		storeFinalizedRun(t, h, sender, runID)
		before := owner.callCount()

		if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
			t.Fatalf("OnBuildEvent: %v", err)
		}
		env, found := queuedRunLog(t, h)
		if !found {
			t.Fatal("expected a run.log envelope with the gateway's finalized fact")
		}
		if want := map[string]interface{}{"finalized": true}; !reflect.DeepEqual(env.Payload, want) {
			t.Fatalf("run.log payload = %v, want %v", env.Payload, want)
		}
		assertQueuedRunCompleted(t, h)
		if calls := owner.callCount() - before; calls != 1 {
			t.Fatalf("owner lookups for the COMPLETED event = %d, want 1 fresh read", calls)
		}
	})

	t.Run("abandoned log", func(t *testing.T) {
		sender := &recordingSender{}
		owner := newFakeOwnerRequester()
		runID := newTestRunID()
		owner.setAnswer(runID, ownerAnswer())
		h, _ := newCompletedOwnerTestHandler(t, owner, sender)
		pub := &fakePublisher{}
		h.publisher = pub
		abandonRunningRun(t, h, sender, pub, runID)

		if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
			t.Fatalf("OnBuildEvent: %v", err)
		}
		env, found := queuedRunLog(t, h)
		if !found {
			t.Fatal("expected a run.log envelope with the gateway's incomplete fact")
		}
		want := map[string]interface{}{"incomplete": true, "incomplete_reason": logReasonGatewayGap}
		if !reflect.DeepEqual(env.Payload, want) {
			t.Fatalf("run.log payload = %v, want %v", env.Payload, want)
		}
	})
}

// TestOnBuildEventCompletedKeepsTheSenderSummaryWithoutAnotherOwner
// proves that only an answer that names another cell instance withholds
// the sender's summary. The owner, a failed lookup, a run with no ledger
// row, an owner with no instance, and a gateway with no log writer all
// keep the whole summary, because it can be the owner's only record of a
// plugin-side loss. Only the owner's event marks the run terminal.
func TestOnBuildEventCompletedKeepsTheSenderSummaryWithoutAnotherOwner(t *testing.T) {
	cases := []struct {
		name         string
		grant        bool
		answer       *ownerLookupResponse
		lookupErr    error
		noLogStore   bool
		wantTerminal bool
		wantLookups  int32
	}{
		{
			name: "owner", grant: true, wantTerminal: true, wantLookups: 1,
			answer: &ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"},
		},
		{name: "owner lookup failed", lookupErr: errors.New("deadline exceeded"), wantLookups: 1},
		{name: "no ledger row", wantLookups: 1},
		{
			name:        "owner with no instance",
			answer:      &ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2"},
			wantLookups: 1,
		},
		{name: "no log store", noLogStore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			owner.err = tc.lookupErr
			runID := newTestRunID()
			if tc.answer != nil {
				owner.setAnswer(runID, *tc.answer)
			}
			h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
			if tc.grant {
				h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")
			}
			if tc.noLogStore {
				h.logStore = nil
			}
			h.logFinalizeWait = time.Hour

			if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
				t.Fatalf("OnBuildEvent: %v", err)
			}
			assertQueuedSenderSummary(t, h)
			assertQueuedRunCompleted(t, h)
			if _, exists := h.lookupRunState(runID); exists != tc.wantTerminal {
				t.Fatalf("run state exists = %v, want %v", exists, tc.wantTerminal)
			}
			if tc.wantTerminal {
				assertRunTerminal(t, h, runID)
			}
			if calls := owner.callCount(); calls != tc.wantLookups {
				t.Fatalf("owner lookups = %d, want %d", calls, tc.wantLookups)
			}
		})
	}
}

// TestOnBuildEventCompletedFromAMovedOwnerPublishesItsSummary proves that
// a cached entry that names an earlier owner does not withhold the new
// owner's summary. A resume moves the owner with no claim, so no grant
// replaces the entry. The gateway reads the ledger again, publishes the
// whole summary, and marks the run terminal.
func TestOnBuildEventCompletedFromAMovedOwnerPublishesItsSummary(t *testing.T) {
	newOwner := ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"}

	t.Run("refusal cached before the move", func(t *testing.T) {
		owner := newFakeOwnerRequester()
		runID := newTestRunID()
		owner.setAnswer(runID, ownerAnswer())
		h, other := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
		h.logFinalizeWait = time.Hour

		h.OnLogChunk("cell2", "ns1", &mitev1.LogChunk{
			RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst2", JobPath: "tenant-a/build",
		})
		if got := waitForLogAcks(t, other, 1)[0]; got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun {
			t.Fatalf("expected the chunk before the move REJECTED foreign-run, got %+v", got)
		}
		if _, cached := h.ownerCache.get(runID); !cached {
			t.Fatal("expected the refusal in the owner cache")
		}
		owner.setAnswer(runID, newOwner)
		before := owner.callCount()

		if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
			t.Fatalf("OnBuildEvent: %v", err)
		}
		assertQueuedSenderSummary(t, h)
		assertRunTerminal(t, h, runID)
		if calls := owner.callCount() - before; calls != 1 {
			t.Fatalf("owner lookups for the COMPLETED event = %d, want 1 fresh read", calls)
		}
	})

	t.Run("match of the old owner cached before the move", func(t *testing.T) {
		sender := &recordingSender{}
		owner := newFakeOwnerRequester()
		runID := newTestRunID()
		owner.setAnswer(runID, ownerAnswer())
		h, _ := newCompletedOwnerTestHandler(t, owner, sender)
		h.logFinalizeWait = time.Hour

		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
			RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1", JobPath: "tenant-a/build",
		})
		if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored {
			t.Fatalf("expected the old owner's chunk STORED, got %+v", got)
		}
		owner.setAnswer(runID, newOwner)
		before := owner.callCount()

		if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
			t.Fatalf("OnBuildEvent: %v", err)
		}
		assertQueuedSenderSummary(t, h)
		assertRunTerminal(t, h, runID)
		if calls := owner.callCount() - before; calls != 1 {
			t.Fatalf("owner lookups for the COMPLETED event = %d, want 1 fresh read", calls)
		}
	})
}

// cell2Owner is the ledger answer that names cell2's registered instance
// as the owner.
func cell2Owner() ownerLookupResponse {
	return ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"}
}

// storeCell2Chunk stores chunk 0 of runID from cell2, so the owner cache
// holds cell2's match from the chunk path.
func storeCell2Chunk(t *testing.T, h *BusHandler, sender *recordingSender, runID string) {
	t.Helper()
	h.OnLogChunk("cell2", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst2", JobPath: "tenant-a/build",
	})
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored {
		t.Fatalf("expected cell2's chunk STORED, got %+v", got)
	}
}

// TestOnBuildEventCompletedFromAStaleOwnerWithACachedMatchWithholdsItsSummary
// proves the main case of a stale cell. The old owner streamed its chunks
// through this gateway, so its match is cached. A takeover then moves the
// run with no claim through this gateway, so no grant replaces the entry.
// The old owner's COMPLETED event reads the ledger, which names the new
// owner. The gateway withholds the old owner's summary and sets no
// deadline on its behalf.
func TestOnBuildEventCompletedFromAStaleOwnerWithACachedMatchWithholdsItsSummary(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, cell2Owner())
	h, other := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	h.logFinalizeWait = time.Hour
	storeCell2Chunk(t, h, other, runID)
	if entry, ok := h.ownerCache.get(runID); !ok || decideOwner(entry, "core", "ns1", "cell2", "inst2") != ownerMatched {
		t.Fatal("expected cell2's match in the owner cache")
	}

	owner.setAnswer(runID, ownerAnswer())
	before := owner.callCount()
	if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	if env, found := queuedRunLog(t, h); found {
		t.Fatalf("queued run.log payload %v, want no run.log for the stale owner", env.Payload)
	}
	assertQueuedRunCompleted(t, h)
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected the run state of cell2's chunk")
	}
	st.mu.Lock()
	terminal := st.terminal
	st.mu.Unlock()
	if terminal {
		t.Fatal("expected no terminal mark on behalf of the stale owner")
	}
	if calls := owner.callCount() - before; calls != 1 {
		t.Fatalf("owner lookups for the COMPLETED event = %d, want 1", calls)
	}
}

// TestOnBuildEventCompletedFromTheOwnerReadsTheLedgerOnce proves that the
// owner's COMPLETED event reads the ledger once, even with a cached match,
// and keeps the whole summary and the terminal mark. The fresh match
// keeps the full match lifetime, so the owner's next chunk needs no new
// request.
func TestOnBuildEventCompletedFromTheOwnerReadsTheLedgerOnce(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, cell2Owner())
	h, other := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")
	h.logFinalizeWait = time.Hour

	if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	assertQueuedSenderSummary(t, h)
	assertRunTerminal(t, h, runID)
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups = %d, want 1 for the COMPLETED event", calls)
	}

	entry, ok := h.ownerCache.get(runID)
	if !ok || decideOwner(entry, "core", "ns1", "cell2", "inst2") != ownerMatched {
		t.Fatal("expected the fresh match in the owner cache")
	}
	if floor := time.Now().Add(ownerMatchTTL - time.Minute); entry.expires.Before(floor) {
		t.Fatalf("cached match expires at %v, want the full match lifetime, after %v", entry.expires, floor)
	}
	storeCell2Chunk(t, h, other, runID)
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups after the owner's chunk = %d, want 1: the cached match admits the chunk", calls)
	}
}

// TestOnBuildEventCompletedAfterAFailedOwnerRead proves what a COMPLETED
// event does when its ledger request fails. The summary always stays. A
// cached match still sets the deadline, as the chunk path does in the
// same state, so a recorder outage does not cost each finishing build its
// deadline. A cached refusal and no cached entry set none.
func TestOnBuildEventCompletedAfterAFailedOwnerRead(t *testing.T) {
	cases := []struct {
		name         string
		cache        func(h *BusHandler, runID string)
		wantTerminal bool
	}{
		{
			name: "cached match",
			cache: func(h *BusHandler, runID string) {
				h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")
			},
			wantTerminal: true,
		},
		{
			name: "cached refusal",
			cache: func(h *BusHandler, runID string) {
				h.ownerCache.set(runID, ownerCacheEntry{
					found:   true,
					tuple:   ownerTuple{cluster: "core", namespace: "ns1", cell: "cell1", instance: "inst1"},
					expires: time.Now().Add(ownerRefuseTTL),
				})
			},
		},
		{name: "no cached entry", cache: func(*BusHandler, string) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			owner.err = errors.New("deadline exceeded")
			runID := newTestRunID()
			h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
			tc.cache(h, runID)
			h.logFinalizeWait = time.Hour

			if err := h.OnBuildEvent("cell2", "ns1", 0, lossyCompletedBuildEvent(runID)); err != nil {
				t.Fatalf("OnBuildEvent: %v", err)
			}
			assertQueuedSenderSummary(t, h)
			assertQueuedRunCompleted(t, h)
			if _, exists := h.lookupRunState(runID); exists != tc.wantTerminal {
				t.Fatalf("run state exists = %v, want %v", exists, tc.wantTerminal)
			}
			if tc.wantTerminal {
				assertRunTerminal(t, h, runID)
			}
			if calls := owner.callCount(); calls != 1 {
				t.Fatalf("owner lookups = %d, want 1 attempt for the COMPLETED event", calls)
			}
		})
	}
}

// TestOnBuildEventOtherEventsAskNoOwner proves that the owner check runs
// only for a hosted COMPLETED event. A STARTED event from a cell that
// does not own the run publishes as before. An event whose tenant the
// cell does not host is refused before any owner lookup.
func TestOnBuildEventOtherEventsAskNoOwner(t *testing.T) {
	t.Run("started event", func(t *testing.T) {
		owner := newFakeOwnerRequester()
		runID := newTestRunID()
		owner.setAnswer(runID, ownerAnswer())
		h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})

		evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: runID, Seq: 1, JobPath: "tenant-a/build"}
		if err := h.OnBuildEvent("cell2", "ns1", 0, evt); err != nil {
			t.Fatalf("OnBuildEvent: %v", err)
		}
		envs := queuedEnvelopes(t, h)
		if len(envs) != 1 || envs[0].Kind != kindRunStarted {
			t.Fatalf("queued %d envelopes %+v, want one run.started", len(envs), envs)
		}
		if calls := owner.callCount(); calls != 0 {
			t.Fatalf("owner lookups = %d, want 0 for a STARTED event", calls)
		}
	})

	t.Run("unhosted tenant", func(t *testing.T) {
		owner := newFakeOwnerRequester()
		runID := newTestRunID()
		owner.setAnswer(runID, ownerAnswer())
		h, other := newCompletedOwnerTestHandler(t, owner, &recordingSender{})

		evt := lossyCompletedBuildEvent(runID)
		evt.JobPath = "tenant-b/build"
		if err := h.OnBuildEvent("cell2", "ns1", 0, evt); err != nil {
			t.Fatalf("OnBuildEvent: %v", err)
		}
		if envs := queuedEnvelopes(t, h); len(envs) != 0 {
			t.Fatalf("queued %d envelopes, want none for an unhosted tenant", len(envs))
		}
		acks := sentEventAcks(other)
		if len(acks) != 1 || acks[0].Status != eventAckRejected || acks[0].Reason != "unhosted tenant" {
			t.Fatalf("event acks = %+v, want one REJECTED unhosted tenant", acks)
		}
		if calls := owner.callCount(); calls != 0 {
			t.Fatalf("owner lookups = %d, want 0 for an unhosted tenant", calls)
		}
	})
}

// TestGatewayLogFactsSkipsAFlagWithNoReason proves that the gateway's
// facts never carry incomplete true with no reason. Each gateway site
// that sets the flag sets a reason too, so a flag with no reason is not a
// fact of the gateway.
func TestGatewayLogFactsSkipsAFlagWithNoReason(t *testing.T) {
	h := newLogTestHandler(t, newFakeOwnerRequester(), &recordingSender{})
	runID := newTestRunID()
	st := h.getOrCreateRunState(runID, "ns1", "cell1", "tenant-a/build")
	st.mu.Lock()
	st.incomplete = true
	st.mu.Unlock()

	if p, ok := h.gatewayLogFacts(runID); ok {
		t.Fatalf("gateway facts = %v, want none for a flag with no reason", p)
	}

	st.mu.Lock()
	st.finalDurable = true
	st.mu.Unlock()
	p, ok := h.gatewayLogFacts(runID)
	if want := map[string]interface{}{"finalized": true}; !ok || !reflect.DeepEqual(p, want) {
		t.Fatalf("gateway facts = %v ok=%v, want %v", p, ok, want)
	}
}
