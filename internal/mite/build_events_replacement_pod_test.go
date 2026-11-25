package mite

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// replacementPodRequester answers each owner read as the recorder answers
// it for a run of a controller with no cell role. The intent names
// stored. A caller that names the same controller and the pod current
// gets its own instance back, because the placement row names that pod.
// While current is empty or names the stored owner, another pod of the
// same controller gets stored with NotReady set: the operator has not
// announced that pod yet. Every other caller gets stored. The fake
// records each request. When
// duringFirst is set, the fake calls it inside the first request, before
// it answers.
type replacementPodRequester struct {
	mu          sync.Mutex
	stored      ownerLookupResponse
	current     string
	requests    []ownerLookupRequest
	duringFirst func()
}

func (f *replacementPodRequester) RequestWithContext(_ context.Context, subject string, data []byte, _ time.Duration) (*nats.Msg, error) {
	var req ownerLookupRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	first := len(f.requests) == 1
	hook := f.duringFirst
	resp := f.stored
	current := f.current
	f.mu.Unlock()
	if first && hook != nil {
		hook()
	}
	sameController := req.Cluster == resp.Cluster && req.Namespace == resp.Namespace && req.Cell == resp.Cell
	switch {
	case sameController && req.Instance != "" && req.Instance == current:
		resp.Instance = req.Instance
	case sameController && req.Instance != "" && req.Instance != resp.Instance &&
		(current == "" || current == resp.Instance):
		resp.NotReady = true
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &nats.Msg{Subject: subject, Data: body}, nil
}

// lastRequest returns the last request the fake received, or fails the
// test when it received none.
func (f *replacementPodRequester) lastRequest(t *testing.T) ownerLookupRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the gateway sent no owner request")
	}
	return f.requests[len(f.requests)-1]
}

// newReplacementPodHandler returns the completed-owner test handler with
// a ledger whose stored owner of every run is inst-old on cell2, and
// whose placement row names inst-new. It also returns cell2's sender.
func newReplacementPodHandler(t *testing.T) (*BusHandler, *recordingSender, *replacementPodRequester) {
	t.Helper()
	h, other := newCompletedOwnerTestHandler(t, newFakeOwnerRequester(), &recordingSender{})
	ledger := &replacementPodRequester{
		stored:  ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst-old"},
		current: "inst-new",
	}
	h.ledger = ledger
	h.logFinalizeWait = time.Hour
	return h, other, ledger
}

// TestOnStreamBuildEventCompletedFromTheReplacementPodKeepsItsFacts
// proves that the gateway names its caller in the owner request, and
// that a COMPLETED event from the replacement pod of a controller with
// no cell role keeps every sender fact. The recorder answers that pod as
// the owner, so the gateway must not withhold the stages, tests, quality
// reports, artifacts, and log summary of the build it resumed. A pod that
// the placement row does not name still gets its facts withheld.
func TestOnStreamBuildEventCompletedFromTheReplacementPodKeepsItsFacts(t *testing.T) {
	t.Run("replacement pod", func(t *testing.T) {
		h, _, ledger := newReplacementPodHandler(t)
		runID := newTestRunID()

		if err := h.OnStreamBuildEvent("cell2", "ns1", "inst-new", 0, fullCompletedBuildEvent(runID)); err != nil {
			t.Fatalf("OnStreamBuildEvent: %v", err)
		}

		want := ownerLookupRequest{RunID: runID, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst-new"}
		if got := ledger.lastRequest(t); got != want {
			t.Fatalf("owner request = %+v, want %+v", got, want)
		}
		if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(fullCompletedGroup) {
			t.Fatalf("group = %v, want the whole group %v", got, fullCompletedGroup)
		}
		assertRunTerminal(t, h, runID)
	})

	t.Run("pod the placement does not name", func(t *testing.T) {
		h, _, _ := newReplacementPodHandler(t)
		runID := newTestRunID()

		if err := h.OnStreamBuildEvent("cell2", "ns1", "inst-other", 0, fullCompletedBuildEvent(runID)); err != nil {
			t.Fatalf("OnStreamBuildEvent: %v", err)
		}

		want := []string{kindRunCompleted}
		if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("group = %v, want %v", got, want)
		}
		if _, exists := h.lookupRunState(runID); exists {
			t.Fatal("expected no run state for a pod that does not own the run")
		}
	})
}

// TestOnLogChunkFromTheReplacementPodIsStored proves that the first chunk
// of a resumed build is stored. The gateway still caches the replaced
// pod's match, so the chunk reads the ledger, and the answer names the
// replacement pod.
func TestOnLogChunkFromTheReplacementPodIsStored(t *testing.T) {
	h, other, ledger := newReplacementPodHandler(t)
	runID := newTestRunID()
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst-old")

	h.OnLogChunk("cell2", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: 0, Data: []byte("Resuming build"), CellInstance: "inst-new", JobPath: "tenant-a/build",
	})

	if got := waitForLogAcks(t, other, 1)[0]; got.Status != logAckStatusStored {
		t.Fatalf("ack = %+v, want STORED", got)
	}
	if got := ledger.lastRequest(t); got.Instance != "inst-new" {
		t.Fatalf("owner request names %q, want inst-new", got.Instance)
	}
}

// TestOnStreamBuildEventCompletedSecondOwnerReadNamesTheCaller proves that
// the second read of readOwner names the caller too. The recorder's owner
// move signal for the takeover can overtake the first read. The second
// read must still carry the caller, or the recorder answers the replaced
// pod and the gateway withholds the resumed build's facts.
func TestOnStreamBuildEventCompletedSecondOwnerReadNamesTheCaller(t *testing.T) {
	h, _, ledger := newReplacementPodHandler(t)
	runID := newTestRunID()
	ledger.duringFirst = func() { h.ownerCache.invalidate(runID) }

	if err := h.OnStreamBuildEvent("cell2", "ns1", "inst-new", 0, fullCompletedBuildEvent(runID)); err != nil {
		t.Fatalf("OnStreamBuildEvent: %v", err)
	}

	ledger.mu.Lock()
	requests := append([]ownerLookupRequest(nil), ledger.requests...)
	ledger.mu.Unlock()
	want := ownerLookupRequest{RunID: runID, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst-new"}
	if len(requests) != 2 {
		t.Fatalf("owner requests = %d, want 2: the overtaken first read reads again", len(requests))
	}
	for i, got := range requests {
		if got != want {
			t.Fatalf("owner request %d = %+v, want %+v", i, got, want)
		}
	}
	if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(fullCompletedGroup) {
		t.Fatalf("group = %v, want the whole group %v", got, fullCompletedGroup)
	}
	assertRunTerminal(t, h, runID)
}

// announcePod sets the pod that the fake's placement row names.
func (f *replacementPodRequester) announcePod(instance string) {
	f.mu.Lock()
	f.current = instance
	f.mu.Unlock()
}

// requestCount returns the number of owner requests the fake received.
func (f *replacementPodRequester) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// unannouncedChunk returns the first chunk of a resumed build from
// inst-new.
func unannouncedChunk(runID string) *mitev1.LogChunk {
	return &mitev1.LogChunk{
		RunId: runID, Seq: 0, Data: []byte("Resuming build"), CellInstance: "inst-new", JobPath: "tenant-a/build",
	}
}

// TestOnLogChunkFromAnUnannouncedReplacementPodWaits proves the retryable
// answer of the log path. The replacement pod of a controller with no
// cell role sends its first chunk before the operator announces it. The
// chunk gets BUSY, not REJECTED, so the mite keeps it and sends it again.
// The answer marks no caller, so the first chunk after the announce reads
// the ledger and is stored at once. Both cases of the placement row are
// covered: it still names the replaced pod, and it names no pod.
func TestOnLogChunkFromAnUnannouncedReplacementPodWaits(t *testing.T) {
	for _, placement := range []string{"inst-old", ""} {
		t.Run("placement "+placement, func(t *testing.T) {
			h, other, ledger := newReplacementPodHandler(t)
			ledger.announcePod(placement)
			runID := newTestRunID()
			h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst-old")

			h.OnLogChunk("cell2", "ns1", unannouncedChunk(runID))

			ack := waitForLogAcks(t, other, 1)[0]
			if ack.Status != logAckStatusBusy || ack.Reason != logReasonOwnerNotReady || ack.PauseMs != logNotReadyPauseMS {
				t.Fatalf("ack before the announce = %+v, want BUSY %s with a %d ms pause",
					ack, logReasonOwnerNotReady, logNotReadyPauseMS)
			}
			if entry, ok := h.ownerCache.peek(runID); !ok || len(entry.refused) != 0 {
				t.Fatalf("cached entry = %+v (found %v), want the replaced pod's match with no refusal mark", entry, ok)
			}
			if _, exists := h.lookupRunState(runID); exists {
				t.Fatal("expected no run state for a chunk that waits for the announce")
			}

			ledger.announcePod("inst-new")
			before := ledger.requestCount()
			h.OnLogChunk("cell2", "ns1", unannouncedChunk(runID))

			if got := waitForLogAcks(t, other, 2)[1]; got.Status != logAckStatusStored {
				t.Fatalf("ack after the announce = %+v, want STORED", got)
			}
			if got := ledger.requestCount() - before; got != 1 {
				t.Fatalf("owner requests after the announce = %d, want 1", got)
			}
		})
	}
}

// TestOnLogChunkFromAnUnannouncedPodIsRefusedAfterTheWait proves the
// bound of the retry. When no announce comes within ownerAnnounceWait,
// the next chunk of the run gets REJECTED{foreign-run}, as a chunk of any
// other foreign sender does, and the read marks the caller.
func TestOnLogChunkFromAnUnannouncedPodIsRefusedAfterTheWait(t *testing.T) {
	h, other, ledger := newReplacementPodHandler(t)
	ledger.announcePod("inst-old")
	runID := newTestRunID()

	h.OnLogChunk("cell2", "ns1", unannouncedChunk(runID))
	if ack := waitForLogAcks(t, other, 1)[0]; ack.Status != logAckStatusBusy {
		t.Fatalf("first ack = %+v, want BUSY", ack)
	}

	h.ownerCache.mu.Lock()
	h.ownerCache.announceWaits[runID] = time.Now().Add(-ownerAnnounceWait)
	h.ownerCache.mu.Unlock()
	h.OnLogChunk("cell2", "ns1", unannouncedChunk(runID))

	ack := waitForLogAcks(t, other, 2)[1]
	if ack.Status != logAckStatusRejected || ack.Reason != logReasonForeignRun {
		t.Fatalf("ack after the wait = %+v, want REJECTED %s", ack, logReasonForeignRun)
	}
	entry, ok := h.ownerCache.peek(runID)
	if _, marked := entry.refused[ownerCallerKey("core", "ns1", "cell2")]; !ok || !marked {
		t.Fatalf("cached entry = %+v (found %v), want a refusal mark for the caller", entry, ok)
	}
}

// TestOnStreamBuildEventCompletedFromAnUnannouncedPodKeepsItsFacts proves
// the completion path. A COMPLETED event of the replacement pod before
// the announce keeps every sender fact, because the recorder holds the
// completion until the announce. It sets no log deadline, because no
// answer names the sender as the owner yet.
func TestOnStreamBuildEventCompletedFromAnUnannouncedPodKeepsItsFacts(t *testing.T) {
	h, _, ledger := newReplacementPodHandler(t)
	ledger.announcePod("")
	runID := newTestRunID()

	if err := h.OnStreamBuildEvent("cell2", "ns1", "inst-new", 0, fullCompletedBuildEvent(runID)); err != nil {
		t.Fatalf("OnStreamBuildEvent: %v", err)
	}

	if got := queuedKinds(t, h); fmt.Sprint(got) != fmt.Sprint(fullCompletedGroup) {
		t.Fatalf("group = %v, want the whole group %v", got, fullCompletedGroup)
	}
	if _, exists := h.lookupRunState(runID); exists {
		t.Fatal("expected no run state: no answer names the sender as the owner")
	}
}

// TestOwnerAnnounceWaitClock proves the clock of the wait. The first
// not-ready answer of a run starts it. An answer inside ownerAnnounceWait
// waits, and an answer past it does not. A clock past ownerAnnounceRetain
// starts again. An owner move signal drops the clock.
func TestOwnerAnnounceWaitClock(t *testing.T) {
	c := newOwnerCache(0)
	now := time.Now()
	setStart := func(at time.Time) {
		c.mu.Lock()
		c.announceWaits["run-1"] = at
		c.mu.Unlock()
	}

	if !c.awaitAnnounce("run-1", now) {
		t.Fatal("first answer: want a wait")
	}
	if !c.awaitAnnounce("run-1", now.Add(ownerAnnounceWait-time.Second)) {
		t.Fatal("answer inside the wait: want a wait")
	}
	if c.awaitAnnounce("run-1", now.Add(ownerAnnounceWait)) {
		t.Fatal("answer at the end of the wait: want no wait")
	}
	setStart(now.Add(-ownerAnnounceRetain))
	if !c.awaitAnnounce("run-1", now) {
		t.Fatal("answer after the retain bound: want a new wait")
	}
	setStart(now.Add(-ownerAnnounceWait))
	c.invalidate("run-1")
	if !c.awaitAnnounce("run-1", now) {
		t.Fatal("answer after an owner move: want a new wait")
	}
}
