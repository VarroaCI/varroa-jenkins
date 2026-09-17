package mite

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	const ns, name = "ns1", "cell1"
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	h := &BusHandler{
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
		ObjectSize: int64(len(data)),
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
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, "jobs/team/builds/1/program.dat"), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
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
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
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
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
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

	r, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, relpath), 0, -1)
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

	r, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, relpath), 0, -1)
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

	r, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, relpath), 0, -1)
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

	r, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, relpath), 0, -1)
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
	out := h.storeRunStateObject(context.Background(), e, chunk, []byte("stale"),
		runStateOutcome{stored: true, relpath: relpath, bytes: 5, stateGen: 1})

	if out.stored {
		t.Errorf("outcome.stored = true, want no envelope for a retired write")
	}
	if out.status != stateAckStatusStored {
		t.Errorf("outcome.status = %q, want %q", out.status, stateAckStatusStored)
	}
	if _, err := h.logStore.NewRangeReader(context.Background(), blob.RunStateKey(runID, relpath), 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
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
