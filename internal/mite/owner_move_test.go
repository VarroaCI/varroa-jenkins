package mite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// watchOwnerMovesOn connects h to the NATS server srv and starts its owner
// move listener, as cmd/gateway does after SetLogStore.
func watchOwnerMovesOn(t *testing.T, h *BusHandler, srv *server.Server) {
	t.Helper()
	conn, err := bus.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)
	h.conn = conn
	if err := h.WatchOwnerMoves(); err != nil {
		t.Fatalf("watch owner moves: %v", err)
	}
	t.Cleanup(h.stopOwnerMoves)
}

// publishOwnerMove sends the signal that the recorder sends after it
// commits an owner move of runID.
func publishOwnerMove(t *testing.T, conn *bus.Conn, runID string) {
	t.Helper()
	data, err := json.Marshal(bus.OwnerMove{RunID: runID})
	if err != nil {
		t.Fatalf("marshal owner move: %v", err)
	}
	if err := conn.Publish(bus.LedgerMoveSubject("core"), data); err != nil {
		t.Fatalf("publish owner move: %v", err)
	}
}

// waitForOwnerEntryGone waits until h holds no cached owner for runID.
func waitForOwnerEntryGone(t *testing.T, h *BusHandler, runID string) {
	t.Helper()
	waitFor(t, func() bool {
		_, ok := h.ownerCache.peek(runID)
		return !ok
	}, 5*time.Second, "the owner move signal did not drop the cached owner")
}

// TestOwnerMoveSignalRefusesTheOldOwnerOnTheReplicaThatHeldItsMatch proves
// the signal on two gateway replicas. Replica B holds the old owner's
// match from the chunks it stored. The recorder moves the run with no
// claim that B relays and signals the move. The old owner's next chunk on
// B reads the ledger once and is refused, so none of its bytes land. The
// new owner continues the log on replica A. With no signal, B admits the
// old owner's chunk from its cached match for up to 30 minutes.
func TestOwnerMoveSignalRefusesTheOldOwnerOnTheReplicaThatHeldItsMatch(t *testing.T) {
	natsSrv := startNATS(t)
	recorder, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(recorder.Close)

	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	bSender := &recordingSender{}
	replicaB := newLogTestHandler(t, owner, bSender)
	watchOwnerMovesOn(t, replicaB, natsSrv)
	storeIndexedRunFromOneWriter(t, replicaB, bSender, runID, 1)
	if entry, ok := replicaB.ownerCache.peek(runID); !ok || decideOwner(entry, "core", "ns1", "cell1", "inst1") != ownerMatched {
		t.Fatalf("replica B entry = %+v (ok=%v), want the old owner's match", entry, ok)
	}

	setOwner(owner, runID, "inst2")
	publishOwnerMove(t, recorder, runID)
	waitForOwnerEntryGone(t, replicaB, runID)

	before := owner.callCount()
	sendChunk(replicaB, runID, 2, "a", "inst1")
	expectAck(t, bSender, 3, logAckStatusRejected, 2)
	if got := bSender.logAcks()[2].Reason; got != logReasonForeignRun {
		t.Fatalf("old owner's refusal reason = %q, want %q", got, logReasonForeignRun)
	}
	if calls := owner.callCount() - before; calls != 1 {
		t.Fatalf("owner lookups for the old owner's chunk = %d, want 1", calls)
	}

	aSender := &recordingSender{}
	replicaA := newLogTestHandlerOnStore(t, owner, aSender, replicaB.logStore)
	watchOwnerMovesOn(t, replicaA, natsSrv)
	sendChunk(replicaA, runID, 2, "b", "inst2")
	expectAck(t, aSender, 1, logAckStatusStored, 2)

	if got, want := readStoredLog(t, replicaA.logStore, runID), "a0 a1 b2 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
}

// TestOwnerMoveSignalAdmitsACallerThatAFreshReadRefused proves the other
// direction. A ledger read refuses the replacement pod of the cell, and
// the recorder moves the run to that pod within the re-read wait. The
// mark refuses the new owner from the cache until the signal drops it.
// The next chunk then reads the ledger once and is stored.
func TestOwnerMoveSignalAdmitsACallerThatAFreshReadRefused(t *testing.T) {
	natsSrv := startNATS(t)
	recorder, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(recorder.Close)

	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	sender := &recordingSender{}
	h := newLogTestHandler(t, owner, sender)
	watchOwnerMovesOn(t, h, natsSrv)

	sendChunk(h, runID, 0, "b", "inst2")
	expectAck(t, sender, 1, logAckStatusRejected, 0)

	setOwner(owner, runID, "inst2")
	sendChunk(h, runID, 0, "b", "inst2")
	expectAck(t, sender, 2, logAckStatusRejected, 0)
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups before the signal = %d, want 1: a fresh mark refuses from the cache", calls)
	}

	publishOwnerMove(t, recorder, runID)
	waitForOwnerEntryGone(t, h, runID)
	sendChunk(h, runID, 0, "b", "inst2")
	expectAck(t, sender, 3, logAckStatusStored, 0)
	if calls := owner.callCount(); calls != 2 {
		t.Fatalf("owner lookups after the signal = %d, want 2", calls)
	}
}

// TestOwnerMoveSignalForAnUnknownRunChangesNothing proves that a signal
// for a run this replica holds no entry for sends no ledger request and
// keeps every other entry. An unreadable signal changes nothing either.
func TestOwnerMoveSignalForAnUnknownRunChangesNothing(t *testing.T) {
	owner := newFakeOwnerRequester()
	h := newLogTestHandler(t, owner, &recordingSender{})
	cached := newTestRunID()
	h.ownerCache.observeGrant(cached, "core", "ns1", "cell1", "inst1")

	for _, body := range [][]byte{
		mustMarshalOwnerMove(t, newTestRunID()),
		[]byte("not json"),
		mustMarshalOwnerMove(t, ""),
	} {
		h.onOwnerMove(body)
	}

	if entry, ok := h.ownerCache.peek(cached); !ok || decideOwner(entry, "core", "ns1", "cell1", "inst1") != ownerMatched {
		t.Fatalf("cached entry = %+v (ok=%v), want the match kept", entry, ok)
	}
	if calls := owner.callCount(); calls != 0 {
		t.Fatalf("owner lookups = %d, want 0", calls)
	}
	h.ownerCache.mu.Lock()
	reads := len(h.ownerCache.reads)
	h.ownerCache.mu.Unlock()
	if reads != 0 {
		t.Fatalf("read records = %d, want 0", reads)
	}
}

// TestOwnerMoveSignalDuringALookupStoresNothing proves the order rule. A
// ledger read that began before the signal can answer the old owner. The
// signal makes that read fail, so its answer is not cached and admits
// nothing: the chunk gets BUSY, and its re-send reads the ledger again.
func TestOwnerMoveSignalDuringALookupStoresNothing(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	h := newLogTestHandler(t, owner, sender)
	gate := &gatedOwnerRequester{inner: owner, entered: make(chan struct{}), release: make(chan struct{})}
	h.ledger = gate

	done := make(chan struct{})
	go func() {
		defer close(done)
		sendChunk(h, runID, 0, "a", "inst1")
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		close(gate.release)
		<-done
		t.Fatal("the chunk sent no ledger request")
	}
	// The held read answers after the signal, with the owner from before
	// the move.
	h.onOwnerMove(mustMarshalOwnerMove(t, runID))
	close(gate.release)
	<-done
	setOwner(owner, runID, "inst2")

	got := waitForLogAcks(t, sender, 1)[0]
	if got.Status != logAckStatusBusy || got.Reason != logReasonOwnerLookupFail {
		t.Fatalf("expected BUSY owner-lookup-failed for a read the move overtook, got %+v", got)
	}
	if entry, ok := h.ownerCache.peek(runID); ok {
		t.Fatalf("cached entry = %+v, want none from a read the move overtook", entry)
	}
	if _, exists := h.lookupRunState(runID); exists {
		t.Fatal("expected no run state from a read the move overtook")
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a0 "), CellInstance: "inst1"})
	if got := waitForLogAcks(t, sender, 2)[1]; got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun {
		t.Fatalf("expected the re-send REJECTED foreign-run, got %+v", got)
	}
	if calls := owner.callCount(); calls != 2 {
		t.Fatalf("owner lookups = %d, want 2", calls)
	}
}

// TestCompletionAfterAnOwnerMoveDoesNotAdmitTheCachedOwner proves that a
// COMPLETED check whose ledger read the move overtakes does not fall back to
// the pre-move cached match. The old owner's completion must not pass.
func TestCompletionAfterAnOwnerMoveDoesNotAdmitTheCachedOwner(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	if _, err := h.ownerCache.doLookup(context.Background(), owner, runID, "core", "ns1", "cell1", "inst1"); err != nil {
		t.Fatalf("prime the owner cache: %v", err)
	}
	if _, ok := h.ownerCache.peek(runID); !ok {
		t.Fatal("expected the primed match in the owner cache")
	}
	gate := &gatedOwnerRequester{inner: owner, entered: make(chan struct{}), release: make(chan struct{})}
	h.ledger = gate

	type result struct {
		decision ownerDecision
		fresh    bool
	}
	out := make(chan result, 1)
	go func() {
		decision, _, fresh := h.checkOwnerForCompletion(context.Background(), "core", "ns1", "cell1", "inst1", runID)
		out <- result{decision: decision, fresh: fresh}
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		close(gate.release)
		<-out
		t.Fatal("the completion check sent no ledger request")
	}
	h.onOwnerMove(mustMarshalOwnerMove(t, runID))
	close(gate.release)
	got := <-out
	if got.decision == ownerMatched {
		t.Fatal("a completion whose read the move overtook was admitted on the pre-move cached owner")
	}
	if got.fresh {
		t.Fatal("a completion whose read the move overtook must not report a fresh answer")
	}
}

func mustMarshalOwnerMove(t *testing.T, runID string) []byte {
	t.Helper()
	data, err := json.Marshal(bus.OwnerMove{RunID: runID})
	if err != nil {
		t.Fatalf("marshal owner move: %v", err)
	}
	return data
}
