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
// Every other caller gets stored. The fake records each request. When
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
	if req.Cluster == resp.Cluster && req.Namespace == resp.Namespace && req.Cell == resp.Cell &&
		req.Instance != "" && req.Instance == current {
		resp.Instance = req.Instance
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
