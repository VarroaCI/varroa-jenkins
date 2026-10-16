package mite

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// newRunStateTestHandler builds a BusHandler wired for the run-state
// path alone: a file:// blob store, a fake owner requester, and one
// controller's sender registered so sendRunStateAck has somewhere to
// deliver. Every caller in this file names the same controller,
// "ns1"/"cell1".
func newRunStateTestHandler(t *testing.T, owner *fakeOwnerRequester, sender *recordingSender) *BusHandler {
	t.Helper()
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	return newRunStateTestHandlerOver(t, owner, sender, store)
}

// newRunStateTestHandlerOver builds the same handler over a store the
// caller owns. Two handlers over one store are two gateway replicas that
// share one storage tier.
func newRunStateTestHandlerOver(t *testing.T, owner *fakeOwnerRequester, sender *recordingSender, store blob.Store) *BusHandler {
	t.Helper()
	const ns, name = "ns1", "cell1"
	h := &BusHandler{
		metrics:        noopGatewayMetrics(),
		cluster:        "core",
		cellInstances:  make(map[string]string),
		buildWorkers:   make(map[string]*registeredBuildWorker),
		claimCounts:    make(map[string]int),
		tenantResolver: defaultCellTenantResolver{},
		stateRuns:      make(map[string]*runStateEntry),
		ownerCache:     newOwnerCache(0),
		ledger:         owner,
		logStore:       store,
	}
	h.buildWorkers[ns+"/"+name] = &registeredBuildWorker{token: 0, worker: &buildEventWorker{send: sender.send}}
	// checkOwner compares the ledger's tuple against the cell instance
	// the gateway itself recorded at registration, never against the
	// chunk's own field (design.md D5), so every test names this one.
	h.RecordCellInstance(name, ns, "inst1")
	return h
}

// runStateAcksFrom extracts every RunStateAck a recordingSender has
// captured so far. OnRunStateChunk answers synchronously, so no wait
// loop is needed.
func runStateAcksFrom(sender *recordingSender) []*mitev1.RunStateAck {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	var out []*mitev1.RunStateAck
	for _, m := range sender.sent {
		if a := m.GetRunStateAck(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// oneChunkEpoch is the pusher epoch oneChunk stamps. The object key
// carries the epoch, so a test that reads a key names this value.
const oneChunkEpoch = int64(100)

// oneChunk builds a single-chunk RunStateChunk: seq 0 of 1, whose
// gzip_content is gz's compression of data and whose object_size is
// data's own length.
func oneChunk(runID, relpath string, stateGen int64, data []byte) *mitev1.RunStateChunk {
	gz, err := gzipBytes(data)
	if err != nil {
		panic(err)
	}
	return &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
		StateGen: stateGen, Seq: 0, Total: 1, GzipContent: gz, RawSize: int64(len(data)),
		ObjectSize: int64(len(data)), PusherEpoch: oneChunkEpoch,
	}
}

// --- 9.3: a foreign push is rejected ---

func TestOnRunStateChunk_ForeignPushRejected(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "other-cell", Instance: "inst-other"})
	h := newRunStateTestHandler(t, owner, sender)

	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, "jobs/team/builds/1/program.dat", 1, []byte("hello")))

	acks := runStateAcksFrom(sender)
	if len(acks) != 1 || acks[0].Status != stateAckStatusRejected || acks[0].Reason != stateReasonForeignRun {
		t.Fatalf("expected one REJECTED foreign-run ack, got %+v", acks)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, oneChunkEpoch, 1, "jobs/team/builds/1/program.dat"), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Errorf("expected no object stored for a foreign push, NewRangeReader error = %v", err)
	}
}

// --- 9.3: a missing middle chunk writes no object and counts incomplete ---

func TestOnRunStateChunk_MissingMiddleChunkIsIncomplete(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	relpath := "jobs/team/builds/1/program.dat"
	gz0, err := gzipBytes([]byte("part-zero"))
	if err != nil {
		t.Fatal(err)
	}
	gz2, err := gzipBytes([]byte("part-two"))
	if err != nil {
		t.Fatal(err)
	}

	// seq 0 of 3: buffered, no ack of note yet.
	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
		StateGen: 1, Seq: 0, Total: 3, GzipContent: gz0, RawSize: 9, ObjectSize: 999, CellInstance: "inst1", PusherEpoch: 100,
	})
	// seq 1 never arrives. seq 2, the last, arrives with the set still
	// short one chunk.
	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
		StateGen: 1, Seq: 2, Total: 3, GzipContent: gz2, RawSize: 8, ObjectSize: 999, CellInstance: "inst1", PusherEpoch: 100,
	})

	acks := runStateAcksFrom(sender)
	if len(acks) != 2 {
		t.Fatalf("expected 2 acks, got %d: %+v", len(acks), acks)
	}
	last := acks[len(acks)-1]
	if last.Status != stateAckStatusRejected || last.Reason != stateReasonIncomplete {
		t.Fatalf("expected the last chunk's ack to be REJECTED incomplete, got %+v", last)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, 100, 1, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Errorf("expected no object stored for an incomplete set, NewRangeReader error = %v", err)
	}
}

// --- 9.3: a replaced file is charged once ---

func TestOnRunStateChunk_ReplacedFileChargedOnce(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	relpath := "jobs/team/builds/1/program.dat"
	first := []byte("the first push of program.dat")
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 1, first))

	h.stateMu.Lock()
	entry, ok := h.stateRuns[runID]
	h.stateMu.Unlock()
	if !ok {
		t.Fatal("expected a run-state entry after the first push")
	}
	if entry.storedTotal != int64(len(first)) {
		t.Fatalf("storedTotal after the first push = %d, want %d", entry.storedTotal, len(first))
	}

	second := []byte("the replacement push, a different length entirely")
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 2, second))

	h.stateMu.Lock()
	storedTotal := entry.storedTotal
	h.stateMu.Unlock()
	if storedTotal != int64(len(second)) {
		t.Errorf("storedTotal after the replacement = %d, want %d (charged once, not summed)", storedTotal, len(second))
	}

	acks := runStateAcksFrom(sender)
	if len(acks) != 2 || acks[0].Status != stateAckStatusStored || acks[1].Status != stateAckStatusStored {
		t.Fatalf("expected two STORED acks, got %+v", acks)
	}
}

// --- 9.3: a chunk-size sum mismatch answers malformed ---

// TestOnRunStateChunk_InflatedSizeMismatchIsMalformed proves the
// gateway proves the pusher's declared object_size against the actual
// inflated size of the completed set, per design.md D6 step 5.
func TestOnRunStateChunk_InflatedSizeMismatchIsMalformed(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	relpath := "jobs/team/builds/1/program.dat"
	gz0, err := gzipBytes([]byte("01234"))
	if err != nil {
		t.Fatal(err)
	}
	gz1, err := gzipBytes([]byte("567"))
	if err != nil {
		t.Fatal(err)
	}
	// The two chunks inflate to 8 bytes total, but object_size declares
	// 99, so the completed set fails the size proof.
	const declaredObjectSize = 99

	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
		StateGen: 1, Seq: 0, Total: 2, GzipContent: gz0, RawSize: 5, ObjectSize: declaredObjectSize, CellInstance: "inst1", PusherEpoch: 100,
	})
	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
		StateGen: 1, Seq: 1, Total: 2, GzipContent: gz1, RawSize: 3, ObjectSize: declaredObjectSize, CellInstance: "inst1", PusherEpoch: 100,
	})

	acks := runStateAcksFrom(sender)
	last := acks[len(acks)-1]
	if last.Status != stateAckStatusRejected || last.Reason != stateReasonMalformed {
		t.Fatalf("expected the completed set's ack to be REJECTED malformed, got %+v", last)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, 100, 1, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Errorf("expected no object stored for a malformed set, NewRangeReader error = %v", err)
	}
}

// --- 9.3: the first run_state_request invalidates the cached owner ---

func TestOnRunStateRequest_FirstFetchInvalidatesOwnerCache(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	h := newRunStateTestHandler(t, owner, sender)

	cached := ownerCacheEntry{
		found:   true,
		tuple:   ownerTuple{cluster: "core", namespace: "ns1", cell: "cell1", instance: "inst1"},
		expires: time.Now().Add(time.Hour),
	}
	h.ownerCache.set(runID, cached)
	if _, ok := h.ownerCache.get(runID); !ok {
		t.Fatal("expected the cache to hold the seeded entry before the first fetch")
	}

	send := func(*mitev1.OperatorMessage) error { return nil }
	requestID := uint64(1)
	h.OnRunStateRequest("cell1", "ns1", &mitev1.RunStateRequest{
		RequestId: &requestID, RunId: runID, Relpath: "jobs/team/builds/1/program.dat",
		PusherEpoch: oneChunkEpoch, StateGen: 1,
	}, send)

	if _, ok := h.ownerCache.get(runID); ok {
		t.Error("expected the first run_state_request to invalidate the cached owner")
	}

	// A second fetch for the same run must not repeat the invalidation:
	// re-seed the cache and prove it survives the second call.
	h.ownerCache.set(runID, cached)
	requestID2 := uint64(2)
	h.OnRunStateRequest("cell1", "ns1", &mitev1.RunStateRequest{
		RequestId: &requestID2, RunId: runID, Relpath: "jobs/team/builds/1/program.dat",
		PusherEpoch: oneChunkEpoch, StateGen: 1,
	}, send)
	if _, ok := h.ownerCache.get(runID); !ok {
		t.Error("expected only the first run_state_request of a run to invalidate the cached owner")
	}
}

// --- Pre-flight round 1: a redelivered generation is not a refusal ---

// TestOnRunStateChunk_RedeliveredGenerationAnswersStored proves the
// gateway answers a retry of a generation it already stored as stored.
// The mite can retry a chunk whose answer it never saw, and without the
// completed-generation marker the last chunk of a multi-chunk file would
// open a fresh buffer and read as incomplete.
func TestOnRunStateChunk_RedeliveredGenerationAnswersStored(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	gz0, err := gzipBytes([]byte("aaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	gz1, err := gzipBytes([]byte("bbb"))
	if err != nil {
		t.Fatal(err)
	}
	last := &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
		StateGen: 1, Seq: 1, Total: 2, GzipContent: gz1, RawSize: 3, ObjectSize: 8, CellInstance: "inst1", PusherEpoch: 100,
	}

	h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
		RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
		StateGen: 1, Seq: 0, Total: 2, GzipContent: gz0, RawSize: 5, ObjectSize: 8, CellInstance: "inst1", PusherEpoch: 100,
	})
	h.OnRunStateChunk("cell1", "ns1", last)

	acks := runStateAcksFrom(sender)
	if acks[len(acks)-1].Status != stateAckStatusStored {
		t.Fatalf("the completed set answered %+v, want STORED", acks[len(acks)-1])
	}

	// The mite retries the last chunk, because it never saw the answer.
	h.OnRunStateChunk("cell1", "ns1", last)

	acks = runStateAcksFrom(sender)
	redelivery := acks[len(acks)-1]
	if redelivery.Status != stateAckStatusStored || redelivery.Reason != "" {
		t.Fatalf("the redelivery answered %+v, want STORED with no reason", redelivery)
	}
}

// TestOnRunStateChunk_RestartedGenerationIsNotADuplicate proves a push
// whose generation repeats an earlier one still replaces the object,
// because it names a different pusher process. A generation counts
// inside one Jenkins process and starts again at 1 after a restart,
// while this gateway entry lives on. An identity a restart could repeat
// would answer every push of the restarted build as a duplicate and
// freeze the run's stored state at its pre-restart copy.
func TestOnRunStateChunk_RestartedGenerationIsNotADuplicate(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	epoch := int64(100)
	store := func(gen int64, body string) {
		gz, err := gzipBytes([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
			RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
			StateGen: gen, Seq: 0, Total: 1, GzipContent: gz,
			RawSize: int64(len(body)), ObjectSize: int64(len(body)), CellInstance: "inst1", PusherEpoch: epoch,
		})
	}

	// The build pushes twice before the restart.
	store(1, "before-one")
	store(2, "before-two-longer")

	// Jenkins restarts. The pusher counts from 1 again under a new
	// epoch, and the build resumes with different content.
	epoch = 200
	store(1, "after-the-restart")

	r, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, epoch, 1, relpath), 0, -1)
	if err != nil {
		t.Fatalf("NewRangeReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The gateway stores the inflated bytes, never the gzip body.
	if string(got) != "after-the-restart" {
		t.Fatalf("stored object = %q, want the restarted build's own content", got)
	}
}

// TestOnRunStateChunk_LateChunkFromARetiredEpochIsDiscarded proves a
// chunk the previous Jenkins process sent, delivered after the new one
// started its own push, neither drops the new buffer nor overwrites the
// stored object. The epoch is that process's start time, so it only
// rises for one run.
func TestOnRunStateChunk_LateChunkFromARetiredEpochIsDiscarded(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	chunk := func(epoch, gen, seq, total int64, body string) *mitev1.RunStateChunk {
		gz, err := gzipBytes([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return &mitev1.RunStateChunk{
			RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
			StateGen: gen, Seq: seq, Total: total, GzipContent: gz,
			RawSize: int64(len(body)), ObjectSize: 18, CellInstance: "inst1", PusherEpoch: epoch,
		}
	}

	// The new process opens a two-chunk push.
	h.OnRunStateChunk("cell1", "ns1", chunk(2, 1, 0, 2, "new-first-"))

	// The retired process's own chunk arrives late.
	h.OnRunStateChunk("cell1", "ns1", chunk(1, 9, 0, 1, "stale-from-old"))

	// The new process finishes its push. Its buffer must have survived.
	h.OnRunStateChunk("cell1", "ns1", chunk(2, 1, 1, 2, "new-last"))

	r, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, 2, 1, relpath), 0, -1)
	if err != nil {
		t.Fatalf("NewRangeReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new-first-new-last" {
		t.Fatalf("stored object = %q, want the new process's own set", got)
	}
}

// TestOnRunStateChunk_MalformedRetiredChunkKeepsTheLiveBuffer proves a
// malformed chunk from the retired process does not drop the live
// process's partial push. The epoch fence runs before every test that
// can touch a buffer.
func TestOnRunStateChunk_MalformedRetiredChunkKeepsTheLiveBuffer(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	chunk := func(epoch, gen, seq, total, objectSize int64, body string) *mitev1.RunStateChunk {
		gz, err := gzipBytes([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return &mitev1.RunStateChunk{
			RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
			StateGen: gen, Seq: seq, Total: total, GzipContent: gz,
			RawSize: int64(len(body)), ObjectSize: objectSize, CellInstance: "inst1", PusherEpoch: epoch,
		}
	}

	h.OnRunStateChunk("cell1", "ns1", chunk(2, 1, 0, 2, 18, "new-first-"))

	// The retired process sends a chunk that fails the shape test. It
	// must not reach that test at all.
	h.OnRunStateChunk("cell1", "ns1", chunk(1, 9, 0, 0, 0, "bad"))

	h.OnRunStateChunk("cell1", "ns1", chunk(2, 1, 1, 2, 18, "new-last"))

	r, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, 2, 1, relpath), 0, -1)
	if err != nil {
		t.Fatalf("NewRangeReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new-first-new-last" {
		t.Fatalf("stored object = %q, want the live process's own set", got)
	}
}

// TestOnRunStateChunk_ANewEpochRetiresEveryOlderBuffer proves the
// pending-byte bound is released when a process is replaced. A retired
// process sends no more chunks, so its partial pushes can never
// complete.
func TestOnRunStateChunk_ANewEpochRetiresEveryOlderBuffer(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	partial := func(epoch int64, relpath string) *mitev1.RunStateChunk {
		gz, err := gzipBytes([]byte("half-a-file"))
		if err != nil {
			t.Fatal(err)
		}
		return &mitev1.RunStateChunk{
			RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
			StateGen: 1, Seq: 0, Total: 2, GzipContent: gz,
			RawSize: 11, ObjectSize: 22, CellInstance: "inst1", PusherEpoch: epoch,
		}
	}

	// Two paths hold a partial push under the old process.
	h.OnRunStateChunk("cell1", "ns1", partial(1, "jobs/team/jobs/pipeline/builds/1/program.dat"))
	h.OnRunStateChunk("cell1", "ns1", partial(1, "jobs/team/jobs/pipeline/builds/1/build.xml"))
	if h.stateTotalPendingBytes <= 0 {
		t.Fatal("expected the two partial pushes to charge the gateway bound")
	}

	// One chunk of the new process retires both.
	h.OnRunStateChunk("cell1", "ns1", partial(2, "jobs/team/jobs/pipeline/builds/1/program.dat"))

	h.stateMu.Lock()
	entry := h.stateRuns[runID]
	buffers := len(entry.buffers)
	h.stateMu.Unlock()
	if buffers != 1 {
		t.Fatalf("live buffers = %d, want only the new process's own", buffers)
	}
}

// blockingStateStore holds the first Replace call inside the store, so
// a test can keep one write in flight while another push of the same
// path runs. Every other call passes straight through.
type blockingStateStore struct {
	blob.Store
	entered chan struct{}
	release chan struct{}

	mu    sync.Mutex
	held  bool
	order []string
}

// Replace records the body of every write in call order, and gates the
// first one until the test releases it.
func (s *blockingStateStore) Replace(ctx context.Context, key string, r io.Reader, opts blob.ObjectOptions) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	first := !s.held
	s.held = true
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-s.release
	}
	s.mu.Lock()
	s.order = append(s.order, string(body))
	s.mu.Unlock()
	return s.Store.Replace(ctx, key, bytes.NewReader(body), opts)
}

// writeOrder answers the bodies the store wrote, in call order.
func (s *blockingStateStore) writeOrder() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// waitForNewestEpoch blocks until the run's entry reports epoch, so a
// test can act on a push that is already inside the handler.
func waitForNewestEpoch(t *testing.T, h *BusHandler, runID string, epoch int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.stateMu.Lock()
		e := h.stateRuns[runID]
		var newest int64
		if e != nil {
			newest = e.newestEpoch
		}
		h.stateMu.Unlock()
		if newest >= epoch {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the run's newest epoch never reached %d", epoch)
}

// TestOnRunStateChunk_AnInFlightRetiredWriteNeverOverwritesTheNewEpoch
// proves the stored object holds the newest process's content after a
// retired process's write finishes late. The retired write runs inside
// the store while the new process pushes the same path.
func TestOnRunStateChunk_AnInFlightRetiredWriteNeverOverwritesTheNewEpoch(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)
	gate := &blockingStateStore{Store: h.logStore, entered: make(chan struct{}), release: make(chan struct{})}
	h.logStore = gate

	const relpath = "jobs/team/jobs/pipeline/builds/1/build.xml"
	push := func(epoch int64, body string) {
		gz, err := gzipBytes([]byte(body))
		if err != nil {
			t.Error(err)
			return
		}
		h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
			RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
			StateGen: 1, Seq: 0, Total: 1, GzipContent: gz,
			RawSize: int64(len(body)), ObjectSize: int64(len(body)),
			CellInstance: "inst1", PusherEpoch: epoch,
		})
	}

	// The retired process's write enters the store and stays there.
	oldDone := make(chan struct{})
	go func() { defer close(oldDone); push(100, "old-content") }()
	<-gate.entered

	// The new process pushes the same path. Its chunk raises the run's
	// newest epoch before its own write starts.
	newDone := make(chan struct{})
	go func() { defer close(newDone); push(200, "new-content") }()
	waitForNewestEpoch(t, h, runID, 200)

	close(gate.release)
	<-oldDone
	<-newDone

	r, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, 200, 1, relpath), 0, -1)
	if err != nil {
		t.Fatalf("NewRangeReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new-content" {
		t.Fatalf("stored object = %q, want the newest process's content", got)
	}
	if order := gate.writeOrder(); len(order) != 2 || order[0] != "old-content" || order[1] != "new-content" {
		t.Fatalf("write order = %v, want the retired write first and the new one last", order)
	}
}

// TestStoreRunStateObject_ARetiredWriteIsDropped proves a write whose
// process is already retired stores nothing, charges nothing, and
// publishes no envelope. The new process's write holds the path.
func TestStoreRunStateObject_ARetiredWriteIsDropped(t *testing.T) {
	h := newRunStateTestHandler(t, newFakeOwnerRequester(), &recordingSender{})
	runID := newTestRunID()
	const relpath = "jobs/team/jobs/pipeline/builds/1/build.xml"

	e := h.getOrCreateStateEntry(runID, "ns1", "cell1", "team/pipeline")
	e.newestEpoch = 200

	chunk := oneChunk(runID, relpath, 1, []byte("stale"))
	chunk.PusherEpoch = 100
	out := h.storeRunStateObject(context.Background(), "ns1", "cell1", e, chunk, []byte("stale"),
		runStateOutcome{stored: true, relpath: relpath, bytes: 5, stateGen: 1})

	if out.stored {
		t.Errorf("outcome.stored = true, want no envelope for a retired write")
	}
	if out.status != stateAckStatusStored {
		t.Errorf("outcome.status = %q, want %q", out.status, stateAckStatusStored)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, oneChunkEpoch, 1, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Errorf("expected no object stored, NewRangeReader error = %v", err)
	}
	h.stateMu.Lock()
	total := e.storedTotal
	_, completed := e.completed[relpath]
	h.stateMu.Unlock()
	if total != 0 || completed {
		t.Errorf("storedTotal = %d, completed = %v, want a retired write to charge nothing", total, completed)
	}
}

// TestOnRunStateChunk_TwoReplicasEpochOrdering is the cross-replica
// proof. Two gateway replicas share one store and hold no shared order
// between them. The retired process's write stays in flight on the first
// replica while the live process's write completes on the second, and
// the retired write then finishes last.
//
// The second half repeats that landing order against one shared key, so
// the test also proves the order is harmful without the epoch.
func TestOnRunStateChunk_TwoReplicasEpochOrdering(t *testing.T) {
	ctx := context.Background()
	shared, err := blob.Open(ctx, "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	gate := &blockingStateStore{Store: shared, entered: make(chan struct{}), release: make(chan struct{})}

	runID := newTestRunID()
	ownerAnswer := ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"}

	ownerA := newFakeOwnerRequester()
	ownerA.setAnswer(runID, ownerAnswer)
	replicaA := newRunStateTestHandlerOver(t, ownerA, &recordingSender{}, gate)

	ownerB := newFakeOwnerRequester()
	ownerB.setAnswer(runID, ownerAnswer)
	replicaB := newRunStateTestHandlerOver(t, ownerB, &recordingSender{}, gate)

	const relpath = "jobs/team/jobs/pipeline/builds/1/workflow/5.xml"
	push := func(h *BusHandler, epoch int64, body string) {
		gz, gerr := gzipBytes([]byte(body))
		if gerr != nil {
			t.Error(gerr)
			return
		}
		h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
			RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
			StateGen: 1, Seq: 0, Total: 1, GzipContent: gz,
			RawSize: int64(len(body)), ObjectSize: int64(len(body)),
			CellInstance: "inst1", PusherEpoch: epoch,
		})
	}

	// The retired process pushes through the first replica. Its write
	// enters the store and stays there.
	oldDone := make(chan struct{})
	go func() { defer close(oldDone); push(replicaA, 100, "old-content") }()
	<-gate.entered

	// The live process reconnects through the second replica, which
	// knows nothing of the first replica's epoch.
	push(replicaB, 200, "new-content")

	close(gate.release)
	<-oldDone

	if order := gate.writeOrder(); len(order) != 2 || order[1] != "old-content" {
		t.Fatalf("write order = %v, want the retired write last", order)
	}

	assertObject := func(epoch int64, want string) {
		t.Helper()
		r, rerr := shared.NewRangeReader(ctx, blob.RunStateKey(runID, epoch, 1, relpath), 0, -1)
		if rerr != nil {
			t.Fatalf("NewRangeReader at epoch %d: %v", epoch, rerr)
		}
		defer func() { _ = r.Close() }()
		got, rerr := io.ReadAll(r)
		if rerr != nil {
			t.Fatalf("read at epoch %d: %v", epoch, rerr)
		}
		if string(got) != want {
			t.Fatalf("object at epoch %d = %q, want %q", epoch, got, want)
		}
	}
	assertObject(200, "new-content")
	assertObject(100, "old-content")

	// The same landing order against one shared key loses the live
	// process's content, which is the defect the epoch removes.
	oneKey := "runstate/" + runID + "/shared/" + relpath
	opts := blob.ObjectOptions{ObjectClass: runStateObjectClass, ContentType: runStateContentType, CreatedAt: time.Now().UTC()}
	if err := shared.Replace(ctx, oneKey, bytes.NewReader([]byte("new-content")), opts); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := shared.Replace(ctx, oneKey, bytes.NewReader([]byte("old-content")), opts); err != nil {
		t.Fatalf("replace: %v", err)
	}
	r, err := shared.NewRangeReader(ctx, oneKey, 0, -1)
	if err != nil {
		t.Fatalf("NewRangeReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "old-content" {
		t.Fatalf("one shared key holds %q, want the retired bytes that prove the defect", got)
	}
}

// TestOnRunStateRequest_ReadsTheRequestedEpoch proves a fetch reads the
// object of the epoch it names, while another epoch's object exists.
func TestOnRunStateRequest_ReadsTheRequestedEpoch(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, &recordingSender{})

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	push := func(epoch int64, body string) {
		gz, gerr := gzipBytes([]byte(body))
		if gerr != nil {
			t.Fatal(gerr)
		}
		h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
			RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
			StateGen: 1, Seq: 0, Total: 1, GzipContent: gz,
			RawSize: int64(len(body)), ObjectSize: int64(len(body)),
			CellInstance: "inst1", PusherEpoch: epoch,
		})
	}
	push(100, "old-content")
	push(200, "new-content")

	fetch := func(epoch int64) []*mitev1.RunStateResponse {
		t.Helper()
		var out []*mitev1.RunStateResponse
		requestID := uint64(epoch)
		h.OnRunStateRequest("cell1", "ns1", &mitev1.RunStateRequest{
			RequestId: &requestID, RunId: runID, Relpath: relpath, PusherEpoch: epoch, StateGen: 1,
		}, func(m *mitev1.OperatorMessage) error {
			out = append(out, m.GetRunStateResponse())
			return nil
		})
		return out
	}

	for _, tc := range []struct {
		epoch int64
		want  string
	}{{100, "old-content"}, {200, "new-content"}} {
		got := fetch(tc.epoch)
		if len(got) != 1 || got[0].Error != "" {
			t.Fatalf("fetch at epoch %d answered %+v, want one body", tc.epoch, got)
		}
		body, err := inflateOneResponse(got[0])
		if err != nil {
			t.Fatalf("inflate: %v", err)
		}
		if body != tc.want {
			t.Fatalf("fetch at epoch %d = %q, want %q", tc.epoch, body, tc.want)
		}
	}
}

// TestOnRunStateRequest_NoEpochIsRefused proves a request that names no
// epoch answers an error, because the key carries the epoch.
func TestOnRunStateRequest_NoEpochIsRefused(t *testing.T) {
	h := newRunStateTestHandler(t, newFakeOwnerRequester(), &recordingSender{})
	runID := newTestRunID()

	var got []*mitev1.RunStateResponse
	requestID := uint64(1)
	h.OnRunStateRequest("cell1", "ns1", &mitev1.RunStateRequest{
		RequestId: &requestID, RunId: runID, Relpath: "jobs/team/builds/1/program.dat", StateGen: 1,
	}, func(m *mitev1.OperatorMessage) error {
		got = append(got, m.GetRunStateResponse())
		return nil
	})

	if len(got) != 1 || got[0].Error == "" {
		t.Fatalf("answer = %+v, want one response carrying an error", got)
	}
}

// inflateOneResponse returns the body of a one-message answer.
func inflateOneResponse(resp *mitev1.RunStateResponse) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(resp.GzipContent))
	if err != nil {
		return "", err
	}
	defer func() { _ = zr.Close() }()
	body, err := io.ReadAll(zr)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TestOnRunStateChunk_NoEpochIsMalformed proves a chunk that names no
// pusher epoch is refused. The key carries the epoch, so such a chunk
// would store an object no fetch can name, and the run could never
// resume.
func TestOnRunStateChunk_NoEpochIsMalformed(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	chunk := oneChunk(runID, relpath, 1, []byte("hello"))
	chunk.PusherEpoch = 0
	h.OnRunStateChunk("cell1", "ns1", chunk)

	acks := runStateAcksFrom(sender)
	if len(acks) != 1 || acks[0].Status != stateAckStatusRejected || acks[0].Reason != stateReasonMalformed {
		t.Fatalf("acks = %+v, want one malformed refusal", acks)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, 0, 1, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Fatalf("expected no object stored, NewRangeReader error = %v", err)
	}
}

// TestOnRunStateChunk_TwoReplicasGenerationOrdering is the cross-replica
// proof for two push generations of one Jenkins process. A gateway
// rollout leaves two replicas that both forward chunks of one epoch, and
// the epoch fence cannot order them: it names one process, and both
// chunks carry that process.
//
// The older generation's write stays in flight on the first replica
// while the newer generation's write completes on the second, and the
// older write then lands last. The record names the newer generation, so
// the object that generation names must hold the newer bytes.
func TestOnRunStateChunk_TwoReplicasGenerationOrdering(t *testing.T) {
	ctx := context.Background()
	shared, err := blob.Open(ctx, "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	gate := &blockingStateStore{Store: shared, entered: make(chan struct{}), release: make(chan struct{})}

	runID := newTestRunID()
	ownerAnswer := ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"}

	ownerA := newFakeOwnerRequester()
	ownerA.setAnswer(runID, ownerAnswer)
	replicaA := newRunStateTestHandlerOver(t, ownerA, &recordingSender{}, gate)

	ownerB := newFakeOwnerRequester()
	ownerB.setAnswer(runID, ownerAnswer)
	replicaB := newRunStateTestHandlerOver(t, ownerB, &recordingSender{}, gate)

	const epoch = int64(100)
	const relpath = "jobs/team/jobs/pipeline/builds/1/workflow/5.xml"
	push := func(h *BusHandler, gen int64, body string) {
		gz, gerr := gzipBytes([]byte(body))
		if gerr != nil {
			t.Error(gerr)
			return
		}
		h.OnRunStateChunk("cell1", "ns1", &mitev1.RunStateChunk{
			RunId: runID, JobPath: "team/pipeline", BuildNumber: 1, Relpath: relpath,
			StateGen: gen, Seq: 0, Total: 1, GzipContent: gz,
			RawSize: int64(len(body)), ObjectSize: int64(len(body)),
			CellInstance: "inst1", PusherEpoch: epoch,
		})
	}

	// The older generation's write enters the store and stays there.
	oldDone := make(chan struct{})
	go func() { defer close(oldDone); push(replicaA, 5, "generation-5") }()
	<-gate.entered

	// The pusher reconnects through the second replica, which knows
	// nothing of the first replica's push.
	push(replicaB, 6, "generation-6")

	close(gate.release)
	<-oldDone

	if order := gate.writeOrder(); len(order) != 2 || order[1] != "generation-5" {
		t.Fatalf("write order = %v, want the older generation's write last", order)
	}

	assertObject := func(gen int64, want string) {
		t.Helper()
		r, rerr := shared.NewRangeReader(ctx, blob.RunStateKey(runID, epoch, gen, relpath), 0, -1)
		if rerr != nil {
			t.Fatalf("NewRangeReader at generation %d: %v", gen, rerr)
		}
		defer func() { _ = r.Close() }()
		got, rerr := io.ReadAll(r)
		if rerr != nil {
			t.Fatalf("read at generation %d: %v", gen, rerr)
		}
		if string(got) != want {
			t.Fatalf("object at generation %d = %q, want %q", gen, got, want)
		}
	}
	assertObject(6, "generation-6")
	assertObject(5, "generation-5")
}

// TestOnRunStateRequest_ReadsTheRequestedGeneration proves a fetch reads
// the object of the generation it names, while another generation of the
// same process and path exists.
func TestOnRunStateRequest_ReadsTheRequestedGeneration(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, &recordingSender{})

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 5, []byte("generation-5")))
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 6, []byte("generation-6")))

	fetcher := &recordingSender{}
	requestID := uint64(7)
	h.OnRunStateRequest("cell1", "ns1", &mitev1.RunStateRequest{
		RequestId: &requestID, RunId: runID, Relpath: relpath,
		PusherEpoch: oneChunkEpoch, StateGen: 5,
	}, fetcher.send)

	fetcher.mu.Lock()
	sent := append([]*mitev1.OperatorMessage(nil), fetcher.sent...)
	fetcher.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want one response", len(sent))
	}
	body, err := inflateOneResponse(sent[0].GetRunStateResponse())
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	if body != "generation-5" {
		t.Fatalf("fetched %q, want the generation the request named", body)
	}
}

// TestOnRunStateRequest_NoGenerationIsRefused proves a fetch that names
// no push generation is refused. The key carries the generation, so such
// a request names no object.
func TestOnRunStateRequest_NoGenerationIsRefused(t *testing.T) {
	h := newRunStateTestHandler(t, newFakeOwnerRequester(), &recordingSender{})
	fetcher := &recordingSender{}
	requestID := uint64(1)
	h.OnRunStateRequest("cell1", "ns1", &mitev1.RunStateRequest{
		RequestId: &requestID, RunId: newTestRunID(),
		Relpath:     "jobs/team/jobs/pipeline/builds/1/program.dat",
		PusherEpoch: 100, StateGen: 0,
	}, fetcher.send)

	fetcher.mu.Lock()
	sent := append([]*mitev1.OperatorMessage(nil), fetcher.sent...)
	fetcher.mu.Unlock()
	if len(sent) != 1 || sent[0].GetRunStateResponse().Error == "" {
		t.Fatalf("sent = %+v, want one response carrying an error", sent)
	}
}

// TestOnRunStateChunk_ALateOlderGenerationIsDropped proves one replica
// discards a chunk below the generation it already stored for the path.
// The store drops the path's buffer, so such a chunk would otherwise
// open a fresh one and move the entry's stored size and stored push back
// to a superseded copy.
func TestOnRunStateChunk_ALateOlderGenerationIsDropped(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 6, []byte("generation-6-body")))
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 5, []byte("five")))

	if _, err := h.logStore.NewRangeReader(context.Background(),
		blob.RunStateKey(runID, oneChunkEpoch, 5, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Errorf("expected no object for the older generation, NewRangeReader error = %v", err)
	}

	h.stateMu.Lock()
	done := h.stateRuns[runID].completed[relpath]
	size := h.stateRuns[runID].storedSizes[relpath]
	h.stateMu.Unlock()
	if done.gen != 6 || size != int64(len("generation-6-body")) {
		t.Fatalf("entry holds push %+v of %d bytes, want the generation-6 push", done, size)
	}
}

// TestOnRunStateChunk_NoGenerationIsMalformed proves a chunk that names
// no push generation is refused before every other step. The key carries
// the generation, so such a chunk would store an object no fetch can
// name, and the redelivery test must never see it.
func TestOnRunStateChunk_NoGenerationIsMalformed(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newRunStateTestHandler(t, owner, sender)

	const relpath = "jobs/team/jobs/pipeline/builds/1/program.dat"
	h.OnRunStateChunk("cell1", "ns1", oneChunk(runID, relpath, 0, []byte("hello")))

	acks := runStateAcksFrom(sender)
	if len(acks) != 1 || acks[0].Status != stateAckStatusRejected || acks[0].Reason != stateReasonMalformed {
		t.Fatalf("acks = %+v, want one malformed refusal", acks)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(),
		blob.RunStateKey(runID, oneChunkEpoch, 0, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Fatalf("expected no object stored, NewRangeReader error = %v", err)
	}
}

// TestOnRunStateChunk_EmptyJobPathWarnsAndStillPublishes proves a chunk
// with no job path still stores its file and still publishes. The
// gateway warns once, so an operator finds the pusher that omitted the
// path.
func TestOnRunStateChunk_EmptyJobPathWarnsAndStillPublishes(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	pub := &fakePublisher{}
	var logs bytes.Buffer
	h := newRunStateTestHandler(t, owner, sender)
	h.publisher = pub
	h.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	const relpath = "jobs/team/builds/1/program.dat"
	chunk := oneChunk(runID, relpath, 1, []byte("hello"))
	chunk.JobPath = ""
	h.OnRunStateChunk("cell1", "ns1", chunk)

	acks := runStateAcksFrom(sender)
	if len(acks) != 1 || acks[0].Status != stateAckStatusStored {
		t.Fatalf("acks = %+v, want one STORED ack", acks)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(),
		blob.RunStateKey(runID, oneChunkEpoch, 1, relpath), 0, -1); err != nil {
		t.Fatalf("expected the file stored, NewRangeReader error = %v", err)
	}
	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("published %d envelopes, want 1", got)
	}
	env := pub.envelopeAt(0)
	if env.Kind != kindRunState || env.JobPath != "" {
		t.Fatalf("envelope kind/job_path = %q/%q, want %q/empty", env.Kind, env.JobPath, kindRunState)
	}
	if !strings.Contains(logs.String(), "run-state chunk carries no job path") {
		t.Fatalf("log = %q, want the empty-job-path warning", logs.String())
	}
	if !strings.Contains(logs.String(), runID) {
		t.Fatalf("log = %q, want the run id", logs.String())
	}
}

// TestOnRunStateChunk_EmptyJobPathOnACellAnswersBusy proves a chunk whose
// job path resolves to no tenant is not stored. The envelope publishes
// before the push is recorded, so the unresolved tenant answers BUSY, and
// the plugin posts the file again. No envelope publishes, and the log
// holds both warnings.
func TestOnRunStateChunk_EmptyJobPathOnACellAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	pub := &fakePublisher{}
	var logs bytes.Buffer
	h := newRunStateTestHandler(t, owner, sender)
	h.publisher = pub
	// A cell derives the tenant from the first path segment, so an
	// empty path answers no tenant.
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{}})
	h.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	const relpath = "jobs/team/builds/1/program.dat"
	chunk := oneChunk(runID, relpath, 1, []byte("hello"))
	chunk.JobPath = ""
	h.OnRunStateChunk("cell1", "ns1", chunk)

	acks := runStateAcksFrom(sender)
	if len(acks) != 1 || acks[0].Status != stateAckStatusBusy || acks[0].Reason != stateReasonBudgetFull {
		t.Fatalf("acks = %+v, want one BUSY gateway-budget-full ack", acks)
	}
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("published %d envelopes, want none", got)
	}
	h.stateMu.Lock()
	e := h.stateRuns[runID]
	recorded := e != nil && e.completed[relpath] != (storedPush{})
	h.stateMu.Unlock()
	if recorded {
		t.Fatalf("the unresolved push was recorded, want it left for the retry")
	}
	if !strings.Contains(logs.String(), "run-state chunk carries no job path") {
		t.Fatalf("log = %q, want the empty-job-path warning", logs.String())
	}
	if !strings.Contains(logs.String(), "run-state outcome unresolved, publishing no envelope") {
		t.Fatalf("log = %q, want the unresolved warning", logs.String())
	}
}
