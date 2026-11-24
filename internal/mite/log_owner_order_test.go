package mite

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// staleGatedOwnerRequester takes the ledger answer of its first owner
// request when the request starts, and holds that answer until release
// closes. It models a read that the ledger answered before a change and
// that reaches the gateway after the change. entered closes when that
// request starts. Every later request goes straight to inner.
type staleGatedOwnerRequester struct {
	inner   *fakeOwnerRequester
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newStaleGatedOwnerRequester(inner *fakeOwnerRequester) *staleGatedOwnerRequester {
	return &staleGatedOwnerRequester{inner: inner, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *staleGatedOwnerRequester) RequestWithContext(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	first := false
	g.once.Do(func() { first = true })
	msg, err := g.inner.RequestWithContext(ctx, subject, data, timeout)
	if first {
		close(g.entered)
		<-g.release
	}
	return msg, err
}

// waitEntered waits until the held request of g starts. On a timeout it
// releases the request, waits for done, and fails the test.
func (g *staleGatedOwnerRequester) waitEntered(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		close(g.release)
		<-done
		t.Fatal("the check sent no ledger request")
	}
}

// overtakingOwnerRequester calls overtake in each owner request, after
// the ledger answered and before the answer reaches the cache. A change
// to the run's entry therefore overtakes every read.
type overtakingOwnerRequester struct {
	inner    *fakeOwnerRequester
	overtake func()
}

func (o *overtakingOwnerRequester) RequestWithContext(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	msg, err := o.inner.RequestWithContext(ctx, subject, data, timeout)
	o.overtake()
	return msg, err
}

// TestOwnerReadsOfTwoCallersAtOnceKeepTheNewerOwner proves the write order
// of two reads of one run that overlap. The stale cell's chunk read starts
// first and gets the owner from before a move. The move signal is lost.
// The new owner's COMPLETED read starts after the move and stores the new
// owner first. The older answer then arrives. It must not replace the
// newer entry: the read is overtaken, reads once more, and refuses the
// stale cell from the fresh answer.
func TestOwnerReadsOfTwoCallersAtOnceKeepTheNewerOwner(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, cell2Owner())
	h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
	gate := newStaleGatedOwnerRequester(owner)
	h.ledger = gate

	var stale ownerDecision
	done := make(chan struct{})
	go func() {
		defer close(done)
		stale, _ = h.checkOwner(context.Background(), "core", "ns1", "cell2", "inst2", runID)
	}()
	gate.waitEntered(t, done)

	owner.setAnswer(runID, ownerAnswer())
	decision, _, fresh := h.checkOwnerForCompletion(context.Background(), "core", "ns1", "cell1", "inst1", runID)
	if decision != ownerMatched || !fresh {
		t.Fatalf("new owner's completion = %v (fresh=%v), want a fresh match", decision, fresh)
	}
	match, ok := h.ownerCache.peek(runID)
	if !ok {
		t.Fatal("expected the new owner's match in the cache")
	}
	close(gate.release)
	<-done

	if stale != ownerForeign {
		t.Fatalf("stale cell's check = %v, want ownerForeign from the second read", stale)
	}
	entry, ok := h.ownerCache.peek(runID)
	if !ok || decideOwner(entry, "core", "ns1", "cell1", "inst1") != ownerMatched {
		t.Fatalf("cached entry = %+v (ok=%v), want the new owner's match", entry, ok)
	}
	if !entry.expires.Equal(match.expires) {
		t.Fatalf("cached expiry = %v, want the new owner's match expiry %v", entry.expires, match.expires)
	}
	if calls := owner.callCount(); calls != 3 {
		t.Fatalf("owner lookups = %d, want 3: the stale read, the completion read, and one more read", calls)
	}
	if again, _ := h.checkOwner(context.Background(), "core", "ns1", "cell2", "inst2", runID); again != ownerForeign {
		t.Fatalf("stale cell's next check = %v, want ownerForeign", again)
	}
	if calls := owner.callCount(); calls != 3 {
		t.Fatalf("owner lookups = %d, want 3: the stale cell's mark refuses from the cache", calls)
	}
}

// TestOnBuildEventCompletedWhileTheLedgerIsBehindANewClaim proves that a
// relayed grant overtakes a ledger read. The old owner's COMPLETED read
// gets the owner from before a new claim. The gateway relays the claim's
// grant before that answer arrives. The stale answer must not replace the
// grant or admit the old owner: the read reads once more, the old owner's
// facts are withheld, and the new owner keeps its match.
func TestOnBuildEventCompletedWhileTheLedgerIsBehindANewClaim(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, cell2Owner())
	h, _ := newCompletedOwnerTestHandler(t, owner, sender)
	h.logFinalizeWait = time.Hour
	gate := newStaleGatedOwnerRequester(owner)
	h.ledger = gate

	done := make(chan struct{})
	var eventErr error
	go func() {
		defer close(done)
		eventErr = h.OnBuildEvent("cell2", "ns1", 0, fullCompletedBuildEvent(runID))
	}()
	gate.waitEntered(t, done)

	owner.setAnswer(runID, ownerAnswer())
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst1")
	grant, ok := h.ownerCache.peek(runID)
	if !ok {
		t.Fatal("expected the grant in the cache")
	}
	close(gate.release)
	<-done
	if eventErr != nil {
		t.Fatalf("OnBuildEvent: %v", eventErr)
	}

	if got, want := queuedKinds(t, h), []string{kindRunCompleted}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("old owner group = %v, want %v", got, want)
	}
	if _, exists := h.lookupRunState(runID); exists {
		t.Fatal("expected no run state from the old owner's completion")
	}
	if calls := owner.callCount(); calls != 2 {
		t.Fatalf("owner lookups = %d, want 2: the stale read and one more read", calls)
	}
	entry, ok := h.ownerCache.peek(runID)
	if !ok || decideOwner(entry, "core", "ns1", "cell1", "inst1") != ownerMatched || !entry.expires.Equal(grant.expires) {
		t.Fatalf("cached entry = %+v (ok=%v), want the grant's match with its expiry", entry, ok)
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1", JobPath: "tenant-a/build"})
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored {
		t.Fatalf("expected the new owner's chunk STORED, got %+v", got)
	}
	if calls := owner.callCount(); calls != 2 {
		t.Fatalf("owner lookups = %d, want 2: the new owner is admitted from the cache", calls)
	}
}

// TestOwnerLookupOvertakenTwiceAnswersBusy proves the bound on the extra
// read. When a change overtakes both reads, the chunk gets BUSY
// owner-lookup-failed after two requests, and no entry is cached.
func TestOwnerLookupOvertakenTwiceAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	h := newLogTestHandler(t, owner, sender)
	h.ledger = &overtakingOwnerRequester{inner: owner, overtake: func() {
		h.onOwnerMove(mustMarshalOwnerMove(t, runID))
	}}

	sendChunk(h, runID, 0, "a", "inst1")
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusBusy || got.Reason != logReasonOwnerLookupFail {
		t.Fatalf("expected BUSY owner-lookup-failed, got %+v", got)
	}
	if calls := owner.callCount(); calls != 2 {
		t.Fatalf("owner lookups = %d, want 2", calls)
	}
	if entry, ok := h.ownerCache.peek(runID); ok {
		t.Fatalf("cached entry = %+v, want none", entry)
	}
}

// TestCompletionOvertakenTwiceSetsNoDeadline proves the exception to the
// cached-match rule of a COMPLETED check. A grant overtakes both reads,
// so the cache holds a match for the sender, but the entry changed during
// each read. The check answers ownerUnknown and is not fresh.
func TestCompletionOvertakenTwiceSetsNoDeadline(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, &recordingSender{})
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst1")
	h.ledger = &overtakingOwnerRequester{inner: owner, overtake: func() {
		h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst1")
	}}

	decision, _, fresh := h.checkOwnerForCompletion(context.Background(), "core", "ns1", "cell1", "inst1", runID)
	if decision != ownerUnknown || fresh {
		t.Fatalf("completion = %v (fresh=%v), want ownerUnknown and not fresh", decision, fresh)
	}
	if calls := owner.callCount(); calls != 2 {
		t.Fatalf("owner lookups = %d, want 2", calls)
	}
}
