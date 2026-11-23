package mite

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestOnLogChunkStaleOwnerEntryReadsTheLedgerAgain proves that a cached
// entry that names another owner does not refuse the new owner's chunk.
// A resume or a takeover moves the owner with no claim, so the entry of
// the old owner, or a refusal that the ledger answered for another
// sender, stays in the cache. The chunk path reads the ledger once, and
// the fresh answer admits the new owner.
func TestOnLogChunkStaleOwnerEntryReadsTheLedgerAgain(t *testing.T) {
	oldOwner := ownerTuple{cluster: "core", namespace: "ns1", cell: "cell2", instance: "inst2"}
	otherSender := map[string]time.Time{ownerCallerKey("core", "ns1", "cell3"): time.Now()}
	cases := []struct {
		name   string
		cached ownerCacheEntry
	}{
		{
			name:   "stale match of the old owner",
			cached: ownerCacheEntry{found: true, tuple: oldOwner, expires: time.Now().Add(ownerMatchTTL)},
		},
		{
			name:   "stale refusal read for another sender",
			cached: ownerCacheEntry{found: true, tuple: oldOwner, expires: time.Now().Add(ownerRefuseTTL), refused: otherSender, refusedUntil: time.Now().Add(ownerRefuseTTL)},
		},
		{
			name:   "stale no-row refusal read for another sender",
			cached: ownerCacheEntry{found: false, expires: time.Now().Add(ownerRefuseTTL), refused: otherSender, refusedUntil: time.Now().Add(ownerRefuseTTL)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &recordingSender{}
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, ownerAnswer())
			h := newLogTestHandler(t, owner, sender)
			h.ownerCache.set(runID, tc.cached)

			h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})

			if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusStored {
				t.Fatalf("expected the new owner's chunk STORED, got %+v", got)
			}
			if calls := owner.callCount(); calls != 1 {
				t.Fatalf("owner lookups = %d, want 1: a cached foreign answer reads the ledger once", calls)
			}
			entry, ok := h.ownerCache.peek(runID)
			if !ok || decideOwner(entry, "core", "ns1", "cell1", "inst1") != ownerMatched || len(entry.refused) != 0 {
				t.Fatalf("cached entry = %+v (ok=%v), want the fresh match of the new owner", entry, ok)
			}
		})
	}
}

// TestOnLogChunkRefusedSenderReadsTheLedgerOncePerEntry proves the bound
// on the extra reads. A sender that the ledger does not name pays one
// read on a cached entry that another caller filled. The answer marks the
// sender as refused, so its later chunks are refused from the cache.
func TestOnLogChunkRefusedSenderReadsTheLedgerOncePerEntry(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
	h := newLogTestHandler(t, owner, sender)
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")

	for seq := int64(0); seq < 3; seq++ {
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: seq, Data: []byte("x"), CellInstance: "inst1"})
		got := waitForLogAcks(t, sender, int(seq)+1)[seq]
		if got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun || got.AckSeq != seq {
			t.Fatalf("chunk %d: expected REJECTED foreign-run, got %+v", seq, got)
		}
	}
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups = %d, want 1 for three chunks of one refused sender", calls)
	}
	if _, exists := h.lookupRunState(runID); exists {
		t.Fatal("expected no run state for a refused sender")
	}
}

// TestOnLogChunkStaleOwnerEntryInALedgerOutageAnswersBusy proves that a
// cached foreign answer never refuses a chunk when the ledger read
// fails. The chunk gets BUSY, so the mite keeps it and sends it again.
// The ledger then names the sender, and the next attempt stores it.
func TestOnLogChunkStaleOwnerEntryInALedgerOutageAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	owner.err = errors.New("nats: timeout")
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")

	chunk := &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"}
	h.OnLogChunk("cell1", "ns1", chunk)
	if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusBusy || got.Reason != logReasonOwnerLookupFail {
		t.Fatalf("expected BUSY owner-lookup-failed in a ledger outage, got %+v", got)
	}
	if _, exists := h.lookupRunState(runID); exists {
		t.Fatal("expected no run state while the owner is unverified")
	}

	owner.mu.Lock()
	owner.err = nil
	owner.mu.Unlock()
	h.OnLogChunk("cell1", "ns1", chunk)
	if got := waitForLogAcks(t, sender, 2)[1]; got.Status != logAckStatusStored {
		t.Fatalf("expected the re-sent chunk STORED once the ledger answers, got %+v", got)
	}
}

// TestOnLogChunkStaleOwnerEntrySharesTheLookupInFlight proves that two
// chunks that meet the same stale entry send one ledger request. The
// second chunk waits on the first chunk's lookup.
func TestOnLogChunkStaleOwnerEntrySharesTheLookupInFlight(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newLogTestHandler(t, owner, sender)
	gate := &gatedOwnerRequester{inner: owner, entered: make(chan struct{}), release: make(chan struct{})}
	h.ledger = gate
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")

	var wg sync.WaitGroup
	send := func() {
		defer wg.Done()
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
	}
	wg.Add(1)
	go send()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		close(gate.release)
		wg.Wait()
		t.Fatal("the first chunk sent no ledger request for the stale entry")
	}
	wg.Add(1)
	go send()
	waitFor(t, func() bool {
		h.ownerCache.inflightMu.Lock()
		defer h.ownerCache.inflightMu.Unlock()
		return len(h.ownerCache.inflight[runID]) == 2
	}, 2*time.Second, "the second chunk did not join the lookup in flight")
	close(gate.release)
	wg.Wait()

	acks := waitForLogAcks(t, sender, 2)
	for _, got := range acks {
		if got.Status != logAckStatusStored {
			t.Fatalf("expected both chunks STORED, got %+v", got)
		}
	}
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups = %d, want 1 shared by both chunks", calls)
	}
}

// TestOnRunStateChunk_StaleOwnerEntryReadsTheLedgerAgain proves that the
// run-state path takes the same owner rule. The mite stops every push of
// a run after one foreign-run answer, so a stale match of the old owner
// must not refuse the new owner's first push.
func TestOnRunStateChunk_StaleOwnerEntryReadsTheLedgerAgain(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerAnswer())
	h := newRunStateTestHandler(t, owner, sender)
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")

	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, "jobs/a/builds/1/program.dat", 1, []byte("hello")))

	acks := runStateAcksFrom(sender)
	if len(acks) != 1 || acks[0].Status != stateAckStatusStored {
		t.Fatalf("expected one STORED ack for the new owner's push, got %+v", acks)
	}
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups = %d, want 1", calls)
	}
}

// TestOnLogChunkRefusedSendersKeepTheirMarksWhileTheOwnerIsUnchanged
// proves that one refusal does not clear another sender's refusal. Two
// foreign senders alternate on one entry. Each sender pays one ledger read
// in total, so the alternation costs two reads and not one per chunk.
func TestOnLogChunkRefusedSendersKeepTheirMarksWhileTheOwnerIsUnchanged(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
	senderA := &recordingSender{}
	h := newLogTestHandler(t, owner, senderA)
	senderB := &recordingSender{}
	h.buildMu.Lock()
	h.buildWorkers["ns1/cell3"] = &registeredBuildWorker{token: 0, worker: &buildEventWorker{send: senderB.send}}
	h.buildMu.Unlock()

	turns := []struct {
		cell, instance string
		sender         *recordingSender
		seq            int64
		acks           int
	}{
		{"cell1", "inst1", senderA, 0, 1},
		{"cell3", "inst3", senderB, 1, 1},
		{"cell1", "inst1", senderA, 2, 2},
		{"cell3", "inst3", senderB, 3, 2},
	}
	for _, turn := range turns {
		h.OnLogChunk(turn.cell, "ns1", &mitev1.LogChunk{RunId: runID, Seq: turn.seq, Data: []byte("x"), CellInstance: turn.instance})
		got := waitForLogAcks(t, turn.sender, turn.acks)[turn.acks-1]
		if got.AckSeq != turn.seq || got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun {
			t.Fatalf("chunk %d: expected REJECTED foreign-run, got %+v", turn.seq, got)
		}
	}
	if calls := owner.callCount(); calls != 2 {
		t.Fatalf("owner lookups = %d, want 2: one per foreign sender", calls)
	}
}

// TestOnLogChunkSenderThatChangesItsInstanceCostsOneLookup proves that
// the refusal mark names the stream identity, not the cell instance that
// the sender stamps on each chunk. One cell that stamps a new instance on
// each chunk pays one ledger read and holds one mark.
func TestOnLogChunkSenderThatChangesItsInstanceCostsOneLookup(t *testing.T) {
	const chunks = 200
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
	h := newLogTestHandler(t, owner, sender)
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell2", "inst2")

	for seq := int64(0); seq < chunks; seq++ {
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
			RunId: runID, Seq: seq, Data: []byte("x"), CellInstance: fmt.Sprintf("inst-%d", seq),
		})
	}

	acks := waitForLogAcks(t, sender, chunks)
	for _, got := range acks {
		if got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun {
			t.Fatalf("expected REJECTED foreign-run for every chunk, got %+v", got)
		}
	}
	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("owner lookups = %d, want 1 for one cell with %d instances", calls, chunks)
	}
	entry, ok := h.ownerCache.peek(runID)
	if !ok || len(entry.refused) != 1 {
		t.Fatalf("cached entry = %+v (ok=%v), want one mark", entry, ok)
	}
}

// TestOnLogChunkRefusedSetStopsAtItsBound proves the hard bound on the
// marks of one entry. Foreign cells past ownerRefusedMax get no mark and
// send no ledger request: the cached answer refuses them.
func TestOnLogChunkRefusedSetStopsAtItsBound(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell-owner", Instance: "inst-owner"})
	h := newLogTestHandler(t, owner, &recordingSender{})
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell-owner", "inst-owner")

	for round := 0; round < 2; round++ {
		for i := 0; i < ownerRefusedMax+2; i++ {
			cell := fmt.Sprintf("stray-%d", i)
			sender := &recordingSender{}
			h.buildMu.Lock()
			h.buildWorkers["ns1/"+cell] = &registeredBuildWorker{token: 0, worker: &buildEventWorker{send: sender.send}}
			h.buildMu.Unlock()
			h.OnLogChunk(cell, "ns1", &mitev1.LogChunk{RunId: runID, Seq: int64(round), Data: []byte("x"), CellInstance: "inst-" + cell})
			if got := waitForLogAcks(t, sender, 1)[0]; got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun {
				t.Fatalf("round %d, %s: expected REJECTED foreign-run, got %+v", round, cell, got)
			}
		}
	}
	if calls := owner.callCount(); calls != ownerRefusedMax {
		t.Fatalf("owner lookups = %d, want %d: one per mark, none past the bound", calls, ownerRefusedMax)
	}
	entry, ok := h.ownerCache.peek(runID)
	if !ok || len(entry.refused) != ownerRefusedMax {
		t.Fatalf("cached entry = %+v (ok=%v), want %d marks", entry, ok, ownerRefusedMax)
	}
}

// TestOnLogChunkAgedRefusalReadsTheLedgerAgain proves the re-read of an
// aged mark and its bound. The ledger can name a caller after the read
// that refused it. A mark older than ownerRefusedRereadAfter reads the
// ledger again, at most ownerRefusedRereads times for one entry. A newer
// mark, or a mark with no re-read left, refuses from the cache.
func TestOnLogChunkAgedRefusalReadsTheLedgerAgain(t *testing.T) {
	oldOwner := ownerTuple{cluster: "core", namespace: "ns1", cell: "cell2", instance: "inst2"}
	oldOwnerAnswer := ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"}
	caller := ownerCallerKey("core", "ns1", "cell1")
	aged := time.Now().Add(-time.Minute)
	cases := []struct {
		name        string
		markedAt    time.Time
		rereads     int
		answer      ownerLookupResponse
		wantStatus  string
		wantLookups int32
		wantRereads int
	}{
		{name: "aged mark of the new owner", markedAt: aged, answer: ownerAnswer(),
			wantStatus: logAckStatusStored, wantLookups: 1, wantRereads: 0},
		{name: "aged mark still refused", markedAt: aged, answer: oldOwnerAnswer,
			wantStatus: logAckStatusRejected, wantLookups: 1, wantRereads: 1},
		{name: "new mark", markedAt: time.Now(), answer: oldOwnerAnswer,
			wantStatus: logAckStatusRejected, wantLookups: 0, wantRereads: 0},
		{name: "aged mark with no re-read left", markedAt: aged, rereads: ownerRefusedRereads, answer: oldOwnerAnswer,
			wantStatus: logAckStatusRejected, wantLookups: 0, wantRereads: ownerRefusedRereads},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &recordingSender{}
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, tc.answer)
			h := newLogTestHandler(t, owner, sender)
			h.ownerCache.set(runID, ownerCacheEntry{
				found: true, tuple: oldOwner, expires: time.Now().Add(ownerRefuseTTL),
				refused: map[string]time.Time{caller: tc.markedAt}, rereads: tc.rereads,
				refusedUntil: time.Now().Add(ownerRefuseTTL),
			})

			h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})

			if got := waitForLogAcks(t, sender, 1)[0]; got.Status != tc.wantStatus {
				t.Fatalf("expected %s, got %+v", tc.wantStatus, got)
			}
			if calls := owner.callCount(); calls != tc.wantLookups {
				t.Fatalf("owner lookups = %d, want %d", calls, tc.wantLookups)
			}
			entry, ok := h.ownerCache.peek(runID)
			if !ok || entry.rereads != tc.wantRereads {
				t.Fatalf("cached entry = %+v (ok=%v), want %d re-reads", entry, ok, tc.wantRereads)
			}
		})
	}
}

// TestOnLogChunkForeignReadKeepsTheOwnersMatch proves that the read of a
// foreign caller never shortens the owner's cached match. A stale pod of
// the owner's cell reads the ledger once. When the answer names the cached
// owner, the match keeps its expiry and only gains the caller's mark. The
// stale pod's next chunk is refused from the cache, and the owner is
// admitted with no lookup. When the answer names a new owner, it replaces
// the match.
func TestOnLogChunkForeignReadKeepsTheOwnersMatch(t *testing.T) {
	cases := []struct {
		name      string
		answer    ownerLookupResponse
		keepMatch bool
	}{
		{name: "ledger names the cached owner", answer: ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst-new"}, keepMatch: true},
		{name: "ledger names a new owner", answer: ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &recordingSender{}
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, tc.answer)
			h := newLogTestHandler(t, owner, sender)
			h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst-new")
			match, ok := h.ownerCache.peek(runID)
			if !ok {
				t.Fatal("expected the grant to cache a match")
			}

			for seq := int64(5); seq < 7; seq++ {
				h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: seq, Data: []byte("x"), CellInstance: "inst-old"})
				if got := lastLogAck(t, sender); got.Status != logAckStatusRejected || got.Reason != logReasonForeignRun || got.AckSeq != seq {
					t.Fatalf("stale chunk %d: expected REJECTED foreign-run, got %+v", seq, got)
				}
			}
			if calls := owner.callCount(); calls != 1 {
				t.Fatalf("owner lookups = %d, want 1: the stale pod's second chunk is refused from the cache", calls)
			}

			entry, ok := h.ownerCache.peek(runID)
			if !ok || len(entry.refused) != 1 {
				t.Fatalf("cached entry = %+v (ok=%v), want one mark", entry, ok)
			}
			if !tc.keepMatch {
				if entry.tuple.cell != "cell2" || !entry.expires.Before(match.expires) {
					t.Fatalf("cached entry = %+v, want the new owner with a refusal lifetime", entry)
				}
				return
			}
			if entry.tuple != match.tuple || !entry.expires.Equal(match.expires) {
				t.Fatalf("cached entry = %+v, want the match %+v with its expiry unchanged", entry, match)
			}
			h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("y"), CellInstance: "inst-new"})
			if got := waitForLogAcks(t, sender, 3)[2]; got.Status != logAckStatusStored {
				t.Fatalf("expected the owner's chunk STORED, got %+v", got)
			}
			if calls := owner.callCount(); calls != 1 {
				t.Fatalf("owner lookups = %d, want 1: the owner is admitted from the cache", calls)
			}
		})
	}
}
