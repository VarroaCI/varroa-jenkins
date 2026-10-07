package mite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// gatedWriteStore wraps a store and holds one call on key at seq until
// the test closes release. op is "append", "overwrite", or "parts" (a
// listing of key from seq). It closes
// entered when the held call starts. The held call reaches the store
// only after release, so the store checks its condition at that moment:
// it models a store call that commits after another gateway replica
// acted.
type gatedWriteStore struct {
	blob.Store
	key     string
	seq     int64
	op      string
	entered chan struct{}
	release chan struct{}
	held    bool
	// skip is the count of matching calls that pass before the held one.
	skip int
}

func newGatedWriteStore(store blob.Store, key string, seq int64, op string) *gatedWriteStore {
	return &gatedWriteStore{Store: store, key: key, seq: seq, op: op,
		entered: make(chan struct{}), release: make(chan struct{})}
}

// hold blocks the first matching call until release. Every later call
// passes at once.
func (g *gatedWriteStore) hold(ctx context.Context, op, key string, seq int64) error {
	if g.held || op != g.op || key != g.key || seq != g.seq {
		return nil
	}
	if g.skip > 0 {
		g.skip--
		return nil
	}
	g.held = true
	close(g.entered)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gatedWriteStore) Append(ctx context.Context, key string, seq int64, p []byte, opts blob.ObjectOptions) error {
	if err := g.hold(ctx, "append", key, seq); err != nil {
		return err
	}
	return g.Store.Append(ctx, key, seq, p, opts)
}

func (g *gatedWriteStore) Overwrite(ctx context.Context, key string, seq int64, p []byte, expect blob.Stamp, opts blob.ObjectOptions) error {
	if err := g.hold(ctx, "overwrite", key, seq); err != nil {
		return err
	}
	return g.Store.Overwrite(ctx, key, seq, p, expect, opts)
}

func (g *gatedWriteStore) Parts(ctx context.Context, key string, from int64) ([]blob.Part, error) {
	if err := g.hold(ctx, "parts", key, from); err != nil {
		return nil, err
	}
	return g.Store.Parts(ctx, key, from)
}

func waitForHeldWrite(t *testing.T, g *gatedWriteStore) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the held write never started")
	}
}

// assertIndexMatchesParts fails the test unless every index entry of
// runID names a log part of its own length and its own writer.
func assertIndexMatchesParts(t *testing.T, store blob.Store, runID string) map[int64]LogIndexEntry {
	t.Helper()
	ctx := context.Background()
	entries, _, _, err := readLogIndex(ctx, store, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read log index: %v", err)
	}
	parts, err := store.Parts(ctx, blob.LogKey(runID), 0)
	if err != nil {
		t.Fatalf("list log parts: %v", err)
	}
	sizes := map[int64]int64{}
	for _, p := range parts {
		sizes[p.Seq] = p.Size
	}
	for seq, e := range entries {
		size, ok := sizes[seq]
		if !ok || size != e.Len {
			t.Fatalf("index entry %d names len %d, the part holds %d (stored %v)", seq, e.Len, size, ok)
		}
		stamp, err := store.PartStamp(ctx, blob.LogKey(runID), seq)
		if err != nil {
			t.Fatalf("part stamp %d: %v", seq, err)
		}
		if e.Writer != "" && stamp.Writer != e.Writer {
			t.Fatalf("index entry %d names writer %q, the part holds bytes of %q", seq, e.Writer, stamp.Writer)
		}
	}
	return entries
}

func sendChunk(h *BusHandler, runID string, seq int64, prefix, instance string) {
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: seq, Data: []byte(fmt.Sprintf("%s%d ", prefix, seq)), CellInstance: instance,
	})
}

func expectAck(t *testing.T, sender *recordingSender, n int, status string, ackSeq int64) {
	t.Helper()
	acks := waitForLogAcks(t, sender, n)
	if got := acks[n-1]; got.Status != status || got.AckSeq != ackSeq {
		t.Fatalf("ack %d: expected %s ack_seq=%d, got %+v", n, status, ackSeq, got)
	}
}

func setOwner(owner *fakeOwnerRequester, runID, instance string) {
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: instance})
}

// TestOnLogChunkStalledBlankAcrossASecondMoveKeepsTheSecondOwnersPart
// proves the conditional Overwrite in the blank pass. The run moves from
// inst1 to inst2, whose replica stalls while it empties the old part 3.
// The run then moves to inst3, which takes over inst2's index
// reservation and writes and indexes parts 2 and 3. The stalled Overwrite
// lands last: it conflicts, and inst2's chunk is refused.
func TestOnLogChunkStalledBlankAcrossASecondMoveKeepsTheSecondOwnersPart(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeOldWriterPartsAboveAHole(t, old, oldSender, runID)
	store := old.logStore

	setOwner(owner, runID, "inst2")
	gate := newGatedWriteStore(store, blob.LogKey(runID), 3, "overwrite")
	firstSender := &recordingSender{}
	first := newLogTestHandlerOnStore(t, owner, firstSender, gate)
	sendChunk(first, runID, 2, "b", "inst2")
	waitForHeldWrite(t, gate)

	setOwner(owner, runID, "inst3")
	secondSender := &recordingSender{}
	second := newLogTestHandlerOnStore(t, owner, secondSender, store)
	sendChunk(second, runID, 2, "c", "inst3")
	expectAck(t, secondSender, 1, logAckStatusStored, 2)
	sendChunk(second, runID, 3, "c", "inst3")
	expectAck(t, secondSender, 2, logAckStatusStored, 3)

	close(gate.release)
	expectAck(t, firstSender, 1, logAckStatusRejected, 2)

	if got, want := readStoredLog(t, store, runID), "a0 a1 c2 c3 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	entries := assertIndexMatchesParts(t, store, runID)
	if entries[3].Writer != "ns1/cell1/inst3" {
		t.Fatalf("index entry 3 writer = %q, want ns1/cell1/inst3", entries[3].Writer)
	}
}

// TestOnLogChunkStalledReplaceAcrossASecondMoveKeepsTheSecondOwnersPart
// proves the conditional Overwrite in replaceStalePart. inst2's replica
// stalls while its chunk 2 replaces the empty reservation. The run moves
// to inst3, which replaces the same part and indexes it. The stalled
// Overwrite lands last, conflicts, and is refused, so the index entry
// at 2 still names inst3's bytes.
func TestOnLogChunkStalledReplaceAcrossASecondMoveKeepsTheSecondOwnersPart(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeOldWriterPartsAboveAHole(t, old, oldSender, runID)
	store := old.logStore

	setOwner(owner, runID, "inst2")
	gate := newGatedWriteStore(store, blob.LogKey(runID), 2, "overwrite")
	firstSender := &recordingSender{}
	first := newLogTestHandlerOnStore(t, owner, firstSender, gate)
	sendChunk(first, runID, 2, "b", "inst2")
	waitForHeldWrite(t, gate)

	setOwner(owner, runID, "inst3")
	secondSender := &recordingSender{}
	second := newLogTestHandlerOnStore(t, owner, secondSender, store)
	sendChunk(second, runID, 2, "c", "inst3")
	expectAck(t, secondSender, 1, logAckStatusStored, 2)

	close(gate.release)
	expectAck(t, firstSender, 1, logAckStatusRejected, 2)

	if got, want := readStoredLog(t, store, runID), "a0 a1 c2 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	entries := assertIndexMatchesParts(t, store, runID)
	if entries[2].Writer != "ns1/cell1/inst3" {
		t.Fatalf("index entry 2 writer = %q, want ns1/cell1/inst3", entries[2].Writer)
	}
}

// TestOnLogChunkStalledBlankOnASecondReplicaKeepsTheWritersChunk proves
// the conditional Overwrite for two gateway replicas that serve the same
// moved writer. Replica B reads part 3 as the old writer's part and
// stalls. Replica A empties it, writes inst2's chunk 3, and indexes it.
// B's Overwrite then conflicts, and B finds inst2's own part there.
func TestOnLogChunkStalledBlankOnASecondReplicaKeepsTheWritersChunk(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeOldWriterPartsAboveAHole(t, old, oldSender, runID)
	store := old.logStore

	setOwner(owner, runID, "inst2")
	gate := newGatedWriteStore(store, blob.LogKey(runID), 3, "overwrite")
	bSender := &recordingSender{}
	replicaB := newLogTestHandlerOnStore(t, owner, bSender, gate)
	sendChunk(replicaB, runID, 2, "b", "inst2")
	waitForHeldWrite(t, gate)

	aSender := &recordingSender{}
	replicaA := newLogTestHandlerOnStore(t, owner, aSender, store)
	sendChunk(replicaA, runID, 2, "b", "inst2")
	expectAck(t, aSender, 1, logAckStatusStored, 2)
	sendChunk(replicaA, runID, 3, "b", "inst2")
	expectAck(t, aSender, 2, logAckStatusStored, 3)

	close(gate.release)
	expectAck(t, bSender, 1, logAckStatusStored, 2)

	if got, want := readStoredLog(t, store, runID), "a0 a1 b2 b3 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	assertIndexMatchesParts(t, store, runID)
}

// establishReplica makes a second gateway replica of runID read the
// store, through a redelivery of the old writer's chunk at seq, and
// waits for its STORED ack.
func establishReplica(t *testing.T, owner *fakeOwnerRequester, store blob.Store, runID string, seq int64) (*BusHandler, *recordingSender) {
	t.Helper()
	sender := &recordingSender{}
	h := newLogTestHandlerOnStore(t, owner, sender, store)
	sendChunk(h, runID, seq, "a", "inst1")
	expectAck(t, sender, 1, logAckStatusStored, seq)
	return h, sender
}

// TestOnLogChunkStalledOldIndexAppendLosesToTheReservation proves the
// index reservation. The old writer's replica stores part 2 and stalls
// on its index entry. The new writer's replica reserves index slot 2 and
// stalls while it empties part 2. The old entry lands in that window: it
// conflicts with the reservation, and the old replica refuses its chunk
// with no gateway-gap marker. With no reservation, the old entry would
// name part 2 while the new writer empties it.
func TestOnLogChunkStalledOldIndexAppendLosesToTheReservation(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	pub := &fakePublisher{}
	old.publisher = pub
	storeIndexedRunFromOneWriter(t, old, oldSender, runID, 1)
	store := old.logStore
	replica, replicaSender := establishReplica(t, owner, store, runID, 1)

	gate := newGatedWriteStore(store, blob.LogIndexKey(runID), 2, "append")
	old.logStore = gate
	sendChunk(old, runID, 2, "a", "inst1")
	waitForHeldWrite(t, gate)

	setOwner(owner, runID, "inst2")
	replica.ownerCache.invalidate(runID)
	blankGate := newGatedWriteStore(store, blob.LogKey(runID), 2, "overwrite")
	replica.logStore = blankGate
	sendChunk(replica, runID, 2, "b", "inst2")
	waitForHeldWrite(t, blankGate)

	close(gate.release)
	expectAck(t, oldSender, 3, logAckStatusRejected, 2)
	close(blankGate.release)
	expectAck(t, replicaSender, 2, logAckStatusStored, 2)
	if got := oldSender.logAcks()[2].Reason; got != logReasonForeignRun {
		t.Fatalf("old replica's refusal reason = %q, want %q", got, logReasonForeignRun)
	}
	if n := gatewayGapPublishes(pub); n != 0 {
		t.Fatalf("the old replica published %d gateway-gap markers, want none", n)
	}

	if got, want := readStoredLog(t, store, runID), "a0 a1 b2 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	entries := assertIndexMatchesParts(t, store, runID)
	if entries[2].Writer != "ns1/cell1/inst2" {
		t.Fatalf("index entry 2 writer = %q, want ns1/cell1/inst2", entries[2].Writer)
	}
}

// TestOnLogChunkOldIndexEntryBeforeTheReservationKeepsTheOldPart pins
// the other order. The old writer's index entry for part 2 lands before
// the new writer's replica reserves the slot. The reservation conflicts,
// the replica reads the index again, and it never empties part 2: the
// entry keeps naming the old writer's bytes. The new writer's chunk 2 is
// lost as a gap, and its chunk 3 continues the log.
func TestOnLogChunkOldIndexEntryBeforeTheReservationKeepsTheOldPart(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeIndexedRunFromOneWriter(t, old, oldSender, runID, 1)
	store := old.logStore
	replica, replicaSender := establishReplica(t, owner, store, runID, 1)

	sendChunk(old, runID, 2, "a", "inst1")
	expectAck(t, oldSender, 3, logAckStatusStored, 2)

	setOwner(owner, runID, "inst2")
	replica.ownerCache.invalidate(runID)
	sendChunk(replica, runID, 2, "b", "inst2")
	expectAck(t, replicaSender, 2, logAckStatusStored, 2)
	assertRunGatewayGap(t, replica, runID)
	sendChunk(replica, runID, 3, "b", "inst2")
	expectAck(t, replicaSender, 3, logAckStatusStored, 3)

	if got, want := readStoredLog(t, store, runID), "a0 a1 a2 b3 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	entries := assertIndexMatchesParts(t, store, runID)
	if entries[2].Writer != "ns1/cell1/inst1" || entries[3].Writer != "ns1/cell1/inst2" {
		t.Fatalf("index writers = %q, %q, want inst1 at 2 and inst2 at 3", entries[2].Writer, entries[3].Writer)
	}
}

// TestOnLogChunkRestartAtAReservationRecoversTheReservingWritersPart
// pins restart recovery over a reservation. inst2 reserved slot 2 and
// stored part 2, and its gateway replica stopped before the index entry.
// A restarted replica indexes part 2 over the reservation, stops at the
// old writer's part 3, and inst2's chunk 3 replaces that part.
func TestOnLogChunkRestartAtAReservationRecoversTheReservingWritersPart(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeOldWriterPartsAboveAHole(t, old, oldSender, runID)
	store := old.logStore

	ctx := context.Background()
	const inst2 = "ns1/cell1/inst2"
	createdAt := runCreatedAtFromUUIDv7(runID)
	if err := store.Append(ctx, blob.LogIndexKey(runID), 2, reservationLine(2, inst2), indexOptions(createdAt, inst2)); err != nil {
		t.Fatalf("store the reservation: %v", err)
	}
	logOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: createdAt, Writer: inst2}
	if err := store.Append(ctx, blob.LogKey(runID), 2, []byte("b2 "), logOpts); err != nil {
		t.Fatalf("store inst2's part 2: %v", err)
	}
	if next, err := storedNextSeqForTest(ctx, store, runID); err != nil || next != 3 {
		t.Fatalf("the reservation baseline: next sequence = %d, %v, want 3", next, err)
	}

	setOwner(owner, runID, "inst2")
	sender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, owner, sender, store)
	sendChunk(restarted, runID, 3, "b", "inst2")
	expectAck(t, sender, 1, logAckStatusStored, 3)

	if got, want := readStoredLog(t, store, runID), "a0 a1 b2 b3 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	entries := assertIndexMatchesParts(t, store, runID)
	if !entries[2].Recovered || entries[2].Writer != inst2 {
		t.Fatalf("index entry 2 = %+v, want a recovered entry of inst2", entries[2])
	}
}

// storedNextSeqForTest answers the next sequence the way restart recovery
// computes it, from the index and blob.UnindexedLogParts.
func storedNextSeqForTest(ctx context.Context, store blob.Store, runID string) (int64, error) {
	entries, maxSeq, _, err := readLogIndex(ctx, store, blob.LogIndexKey(runID))
	if err != nil {
		return 0, err
	}
	parts, _, err := blob.UnindexedLogParts(ctx, store, runID, maxSeq+1, indexWriter(entries, maxSeq))
	if err != nil {
		return 0, err
	}
	if len(entries) != int(maxSeq+1) {
		return 0, errors.New("the index holds a hole")
	}
	return maxSeq + 1 + int64(len(parts)), nil
}

// TestOnLogChunkStalledBlankPassAfterASecondMoveEmptiesNothing pins the
// owner read inside the blank pass. inst2's replica reserves slot 2 and
// stalls before it lists the parts. The run moves to inst3, which takes
// over the reservation and writes and indexes parts 2 and 3. inst2's
// listing then finds inst3's parts: the owner read refuses inst2, and it
// empties nothing.
func TestOnLogChunkStalledBlankPassAfterASecondMoveEmptiesNothing(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeIndexedRunFromOneWriter(t, old, oldSender, runID, 1)
	store := old.logStore

	setOwner(owner, runID, "inst2")
	gate := newGatedWriteStore(store, blob.LogKey(runID), 2, "parts")
	// Restart recovery lists the parts first. The blank pass lists them
	// second, after the reservation.
	gate.skip = 1
	firstSender := &recordingSender{}
	first := newLogTestHandlerOnStore(t, owner, firstSender, gate)
	sendChunk(first, runID, 2, "b", "inst2")
	waitForHeldWrite(t, gate)

	setOwner(owner, runID, "inst3")
	secondSender := &recordingSender{}
	second := newLogTestHandlerOnStore(t, owner, secondSender, store)
	sendChunk(second, runID, 2, "c", "inst3")
	expectAck(t, secondSender, 1, logAckStatusStored, 2)
	sendChunk(second, runID, 3, "c", "inst3")
	expectAck(t, secondSender, 2, logAckStatusStored, 3)

	close(gate.release)
	expectAck(t, firstSender, 1, logAckStatusRejected, 2)

	if got, want := readStoredLog(t, store, runID), "a0 a1 c2 c3 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	assertIndexMatchesParts(t, store, runID)
}

// TestOnLogChunkStalledReservationAfterASecondMoveLeavesNoReservation
// pins the owner read after a lost reservation. inst2's replica stalls on
// its reservation of slot 2. inst3 then writes and indexes parts 2 and 3.
// inst2's reservation conflicts with inst3's entry: the owner read refuses
// inst2, and it reserves no slot above inst3's entries.
func TestOnLogChunkStalledReservationAfterASecondMoveLeavesNoReservation(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeIndexedRunFromOneWriter(t, old, oldSender, runID, 1)
	store := old.logStore

	setOwner(owner, runID, "inst2")
	gate := newGatedWriteStore(store, blob.LogIndexKey(runID), 2, "append")
	firstSender := &recordingSender{}
	first := newLogTestHandlerOnStore(t, owner, firstSender, gate)
	sendChunk(first, runID, 2, "b", "inst2")
	waitForHeldWrite(t, gate)

	setOwner(owner, runID, "inst3")
	secondSender := &recordingSender{}
	second := newLogTestHandlerOnStore(t, owner, secondSender, store)
	sendChunk(second, runID, 2, "c", "inst3")
	expectAck(t, secondSender, 1, logAckStatusStored, 2)
	sendChunk(second, runID, 3, "c", "inst3")
	expectAck(t, secondSender, 2, logAckStatusStored, 3)

	close(gate.release)
	expectAck(t, firstSender, 1, logAckStatusRejected, 2)

	if _, err := store.PartStamp(context.Background(), blob.LogIndexKey(runID), 4); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Fatalf("index slot 4: got %v, want no reservation", err)
	}
	if got, want := readStoredLog(t, store, runID), "a0 a1 c2 c3 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	assertIndexMatchesParts(t, store, runID)
}

// unsupportedOverwriteStore answers ErrOverwriteUnsupported to every
// Overwrite, as a gs or azblob bucket does.
type unsupportedOverwriteStore struct {
	blob.Store
}

func (unsupportedOverwriteStore) Overwrite(context.Context, string, int64, []byte, blob.Stamp, blob.ObjectOptions) error {
	return blob.ErrOverwriteUnsupported
}

// TestOnLogChunkMoveOnAStoreWithNoConditionalOverwriteAbandonsAtOnce
// pins that a store that can never fence an Overwrite ends the moved
// run's log at once, and does not retry until a finalize deadline that a
// live run does not have.
func TestOnLogChunkMoveOnAStoreWithNoConditionalOverwriteAbandonsAtOnce(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeOldWriterPartsAboveAHole(t, old, oldSender, runID)

	setOwner(owner, runID, "inst2")
	sender := &recordingSender{}
	moved := newLogTestHandlerOnStore(t, owner, sender, unsupportedOverwriteStore{Store: old.logStore})
	sendChunk(moved, runID, 2, "b", "inst2")
	expectAck(t, sender, 1, logAckStatusRejected, 2)
	if incomplete, _ := moved.runIncompleteness(runID); !incomplete {
		t.Fatal("expected the abandoned run incomplete")
	}
}

// TestOnLogChunkRecoveryOnAStoreWithNoConditionalOverwriteAbandonsAtOnce
// pins the same rule for restart recovery over a reservation, which
// replaces the reservation through Overwrite.
func TestOnLogChunkRecoveryOnAStoreWithNoConditionalOverwriteAbandonsAtOnce(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	setOwner(owner, runID, "inst1")
	oldSender := &recordingSender{}
	old := newLogTestHandler(t, owner, oldSender)
	storeIndexedRunFromOneWriter(t, old, oldSender, runID, 1)
	store := old.logStore

	ctx := context.Background()
	const inst2 = "ns1/cell1/inst2"
	createdAt := runCreatedAtFromUUIDv7(runID)
	if err := store.Append(ctx, blob.LogIndexKey(runID), 2, reservationLine(2, inst2), indexOptions(createdAt, inst2)); err != nil {
		t.Fatalf("store the reservation: %v", err)
	}
	logOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: createdAt, Writer: inst2}
	if err := store.Append(ctx, blob.LogKey(runID), 2, []byte("b2 "), logOpts); err != nil {
		t.Fatalf("store inst2's part 2: %v", err)
	}

	setOwner(owner, runID, "inst2")
	sender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, owner, sender, unsupportedOverwriteStore{Store: store})
	sendChunk(restarted, runID, 3, "b", "inst2")
	expectAck(t, sender, 1, logAckStatusRejected, 3)
}
