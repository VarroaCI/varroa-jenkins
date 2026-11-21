package mite

import (
	"errors"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// newCompletedOwnerTestHandler builds a log test handler with two cells
// that host the tenant of job "tenant-a/build": cell1 (instance inst1)
// and cell2 (instance inst2). It returns the handler and the sender of
// cell2. The sender of cell1 is the one the caller passes in.
func newCompletedOwnerTestHandler(t *testing.T, owner *fakeOwnerRequester, sender *recordingSender) (*BusHandler, *recordingSender) {
	t.Helper()
	h := newLogTestHandler(t, owner, sender)
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"tenant-a/build": {tenant: "tenant-a", generation: 1},
	}})
	h.RecordCellInstance("cell1", "ns1", "inst1")
	other := &recordingSender{}
	h.buildMu.Lock()
	h.buildWorkers["ns1/cell2"] = &registeredBuildWorker{token: 0, worker: &buildEventWorker{send: other.send}}
	h.buildMu.Unlock()
	h.RecordCellInstance("cell2", "ns1", "inst2")
	return h, other
}

// completedBuildEvent returns a COMPLETED event of job "tenant-a/build"
// that ended at completedAt.
func completedBuildEvent(runID string, completedAt time.Time) *mitev1.BuildEvent {
	return &mitev1.BuildEvent{
		Phase: phaseCompleted, RunId: runID, Seq: 1, JobPath: "tenant-a/build",
		Result: "SUCCESS", CompletedAtMillis: completedAt.UnixMilli(),
	}
}

// waitForIdleRun waits until no drain goroutine runs for st.
func waitForIdleRun(t *testing.T, st *runLogState) {
	t.Helper()
	waitFor(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return !st.draining
	}, 2*time.Second, "the run's drain goroutine did not exit")
}

// TestOnBuildEventCompletedFromANonOwnerCreatesNoRunState proves that a
// COMPLETED event from a cell that does not own the run leaves no log
// run state. The record store keeps such a completion out of the run's
// terminal state, so it must not set the writer, the job path, or the
// finalize deadline of the owner's log.
func TestOnBuildEventCompletedFromANonOwnerCreatesNoRunState(t *testing.T) {
	cases := []struct {
		name      string
		answer    *ownerLookupResponse
		lookupErr error
	}{
		{
			name:   "foreign cell",
			answer: &ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"},
		},
		{
			name:   "stale instance of the same cell",
			answer: &ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst-old"},
		},
		{
			name: "no ledger row",
		},
		{
			name:      "owner lookup failed",
			lookupErr: errors.New("deadline exceeded"),
		},
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

			if err := h.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(runID, time.Now())); err != nil {
				t.Fatalf("OnBuildEvent: %v", err)
			}
			if _, exists := h.lookupRunState(runID); exists {
				t.Fatal("expected no run state for a COMPLETED event of a cell that does not own the run")
			}
		})
	}
}

// TestOnBuildEventCompletedFromTheOwnerMarksTheRunTerminal proves that
// the owner's COMPLETED event still sets the finalize deadline from its
// own completion time. A cached match from the relayed claim grant
// answers the owner check, so the event costs no ledger request.
func TestOnBuildEventCompletedFromTheOwnerMarksTheRunTerminal(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst1")
	h.logFinalizeWait = time.Hour

	completedAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	if err := h.OnBuildEvent("cell1", "ns1", 0, completedBuildEvent(runID, completedAt)); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected the owner's COMPLETED event to create the run state")
	}
	st.mu.Lock()
	terminal, deadlineAt := st.terminal, st.deadlineAt
	ns, name, jobPath := st.ns, st.name, st.jobPath
	st.mu.Unlock()
	if !terminal {
		t.Fatal("expected the owner's COMPLETED event to mark the run terminal")
	}
	if want := completedAt.Add(time.Hour); !deadlineAt.Equal(want) {
		t.Fatalf("deadline = %v, want the completion time plus the finalize wait, %v", deadlineAt, want)
	}
	if ns != "ns1" || name != "cell1" || jobPath != "tenant-a/build" {
		t.Fatalf("run state writer=%s/%s job_path=%q, want ns1/cell1 and tenant-a/build", ns, name, jobPath)
	}
	if calls := owner.callCount(); calls != 0 {
		t.Fatalf("owner lookups = %d, want 0: the relayed grant answers the check", calls)
	}
}

// TestOnBuildEventCompletedFromANonOwnerKeepsTheOwnersLogOpen proves
// that a COMPLETED event with an old completion time, from a cell that
// does not own the run, does not abandon the owner's log. The owner's
// later chunk is stored, not refused with finalize-deadline.
func TestOnBuildEventCompletedFromANonOwnerKeepsTheOwnersLogOpen(t *testing.T) {
	ownerSender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h, otherSender := newCompletedOwnerTestHandler(t, owner, ownerSender)
	h.logFinalizeWait = 50 * time.Millisecond

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1", JobPath: "tenant-a/build",
	})
	if got := waitForLogAcks(t, ownerSender, 1)[0]; got.Status != logAckStatusStored {
		t.Fatalf("expected the owner's first chunk STORED, got %+v", got)
	}
	st, ok := h.lookupRunState(runID)
	if !ok {
		t.Fatal("expected the owner's chunk to create the run state")
	}
	waitForIdleRun(t, st)

	if err := h.OnBuildEvent("cell2", "ns1", 0, completedBuildEvent(runID, time.Now().Add(-time.Hour))); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst1", JobPath: "tenant-a/build",
	})
	acks := waitForLogAcks(t, ownerSender, 2)
	if got := acks[1]; got.Status != logAckStatusStored || got.AckSeq != 1 {
		t.Fatalf("expected the owner's second chunk STORED at ack_seq 1, got %+v", got)
	}

	st.mu.Lock()
	terminal, rejected := st.terminal, st.rejectedForever
	st.mu.Unlock()
	if terminal || rejected {
		t.Fatalf("run state terminal=%v rejected=%v, want neither after a non-owner's COMPLETED", terminal, rejected)
	}
	if n := len(otherSender.logAcks()); n != 0 {
		t.Fatalf("non-owner received %d log acks, want 0", n)
	}
}

// TestAdoptWriterFirstWriterReceivesTheAbandonAcks proves that the
// first writer the drain adopts also becomes the writer abandonRun
// answers. Here the state's creator is not its first writer: the
// creator is an admitted sender whose chunk never drains, and the run's
// first drained chunk comes from cell2. At the finalize deadline, the
// REJECTED ack for cell2's stranded sequence must reach cell2.
func TestAdoptWriterFirstWriterReceivesTheAbandonAcks(t *testing.T) {
	creatorSender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
	h, writerSender := newCompletedOwnerTestHandler(t, owner, creatorSender)
	pub := &fakePublisher{}
	h.publisher = pub

	st := h.getOrCreateRunState(runID, "ns1", "cell1", "tenant-a/build")

	// Sequence 0 never arrives, so sequence 1 stays pending above a hole.
	// A pending chunk below which nothing is durable gets no STORED ack.
	h.OnLogChunk("cell2", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: 1, Data: []byte("b"), CellInstance: "inst2", JobPath: "tenant-a/build",
	})
	waitFor(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		_, pending := st.pendingEntries[1]
		return pending && !st.draining
	}, 2*time.Second, "seq 1 never became pending above the hole")

	st.mu.Lock()
	ns, name := st.ns, st.name
	st.mu.Unlock()
	if ns != "ns1" || name != "cell2" {
		t.Fatalf("run state writer = %s/%s, want the first drained writer ns1/cell2", ns, name)
	}

	h.markRunTerminal("ns1", "cell2", runID, "tenant-a/build", time.Now().Add(-time.Hour))

	acks := waitForLogAcks(t, writerSender, 1)
	if got := acks[0]; got.Status != logAckStatusRejected || got.Reason != logReasonFinalizeDeadline || got.AckSeq != 1 {
		t.Fatalf("expected REJECTED finalize-deadline for the stranded seq 1, got %+v", got)
	}
	if n := len(creatorSender.logAcks()); n != 0 {
		t.Fatalf("the state's creator received %d log acks, want 0", n)
	}

	var abandon ingestEnvelope
	waitFor(t, func() bool {
		for i := 0; i < pub.publishedCount(); i++ {
			if env := pub.envelopeAt(i); env.Payload["incomplete"] == true {
				abandon = env
				return true
			}
		}
		return false
	}, 2*time.Second, "the abandon published no incomplete run.log")
	if abandon.Namespace != "ns1" || abandon.Cell != "cell2" {
		t.Fatalf("abandon envelope writer = %s/%s, want ns1/cell2", abandon.Namespace, abandon.Cell)
	}
}
