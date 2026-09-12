package mite

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// --- test fakes ---

// fakeOwnerRequester answers varroa.ledger.owner lookups from a canned
// table keyed by run_id. delay simulates a slow recorder; err simulates
// an unreachable one. Every call is counted, so a test can assert one
// lookup served several queued chunks.
type fakeOwnerRequester struct {
	mu      sync.Mutex
	answers map[string]ownerLookupResponse
	delay   time.Duration
	err     error
	calls   int32
}

func newFakeOwnerRequester() *fakeOwnerRequester {
	return &fakeOwnerRequester{answers: make(map[string]ownerLookupResponse)}
}

func (f *fakeOwnerRequester) setAnswer(runID string, resp ownerLookupResponse) {
	f.mu.Lock()
	f.answers[runID] = resp
	f.mu.Unlock()
}

func (f *fakeOwnerRequester) RequestWithContext(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var req ownerLookupRequest
	_ = json.Unmarshal(data, &req)
	f.mu.Lock()
	resp, ok := f.answers[req.RunID]
	f.mu.Unlock()
	if !ok {
		resp = ownerLookupResponse{Found: false}
	}
	body, _ := json.Marshal(resp)
	return &nats.Msg{Subject: subject, Data: body}, nil
}

func (f *fakeOwnerRequester) callCount() int32 { return atomic.LoadInt32(&f.calls) }

// recordingSender records every OperatorMessage sent, safe for
// concurrent use from the log writer's per-run goroutines.
type recordingSender struct {
	mu   sync.Mutex
	sent []*mitev1.OperatorMessage
}

func (s *recordingSender) send(msg *mitev1.OperatorMessage) error {
	s.mu.Lock()
	s.sent = append(s.sent, msg)
	s.mu.Unlock()
	return nil
}

func (s *recordingSender) logAcks() []*mitev1.LogAck {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*mitev1.LogAck
	for _, m := range s.sent {
		if a := m.GetLogAck(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// newLogTestHandler builds a BusHandler wired for the log writer alone:
// a file:// blob store, a fake owner requester, and one controller's
// sender registered so sendLogAck has somewhere to deliver. Every caller
// in this file names the same controller, "ns1"/"cell1".
func newLogTestHandler(t *testing.T, owner *fakeOwnerRequester, sender *recordingSender) *BusHandler {
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
		logRuns:        make(map[string]*runLogState),
		ownerCache:     newOwnerCache(0),
		ledger:         owner,
		logStore:       store,
	}
	h.buildWorkers[ns+"/"+name] = &registeredBuildWorker{token: 0, worker: &buildEventWorker{send: sender.send}}
	return h
}

// newTestRunID returns a fresh UUIDv7 string, so every test run gets a
// distinct, well-formed run id.
func newTestRunID() string {
	id, _ := uuid.NewV7()
	return id.String()
}

// waitForLogAcks polls until at least n LogAck messages have arrived, or
// fails the test after a bounded wait.
func waitForLogAcks(t *testing.T, sender *recordingSender, n int) []*mitev1.LogAck {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if acks := sender.logAcks(); len(acks) >= n {
			return acks
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d LogAck(s), got %d", n, len(sender.logAcks()))
	return nil
}

// --- 3.2: append order, ObjectOptions, retries, empty run_id ---

func TestOnLogChunkAppendOrderAndCompleteObjectOptions(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)

	chunk := &mitev1.LogChunk{
		RunId: runID, JobPath: "j", BuildNumber: 1, Seq: 0,
		NodeId: "17", StageId: "12", Data: []byte("hello world"), Final: false, CellInstance: "inst1",
	}
	h.OnLogChunk("cell1", "ns1", chunk)

	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusStored || acks[0].AckSeq != 0 {
		t.Fatalf("expected STORED ack_seq=0, got %+v", acks[0])
	}

	// The log part is durable at the expected offset.
	r, err := h.logStore.NewRangeReader(context.Background(), blob.LogKey(runID), 0, -1)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	defer r.Close()
	buf := make([]byte, 64)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "hello world" {
		t.Fatalf("log bytes = %q, want %q", buf[:n], "hello world")
	}

	entries, maxSeq, finalSeen, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 0 || finalSeen {
		t.Fatalf("maxSeq=%d finalSeen=%v, want 0/false", maxSeq, finalSeen)
	}
	e := entries[0]
	if e.NodeID != "17" || e.StageID != "12" || e.Offset != 0 || e.Len != int64(len("hello world")) {
		t.Fatalf("index entry = %+v", e)
	}
}

func TestOnLogChunkEmptyRunIDRejected(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: "", Seq: 0, Data: []byte("x")})

	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusRejected || acks[0].Reason != logReasonEmptyRunID {
		t.Fatalf("expected REJECTED empty-run-id, got %+v", acks[0])
	}
}

func TestOnLogChunkRedeliveredChunkDoesNotChangeOffset(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)

	chunk := &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("abc"), CellInstance: "inst1"}
	h.OnLogChunk("cell1", "ns1", chunk)
	waitForLogAcks(t, sender, 1)

	// Identical redelivery of seq 0.
	h.OnLogChunk("cell1", "ns1", chunk)
	acks := waitForLogAcks(t, sender, 2)
	if acks[1].Status != logAckStatusStored || acks[1].AckSeq != 0 {
		t.Fatalf("expected a second STORED ack for the redelivery, got %+v", acks[1])
	}

	n, err := logObjectLength(context.Background(), h.logStore, blob.LogKey(runID))
	if err != nil {
		t.Fatalf("log length: %v", err)
	}
	if n != 3 {
		t.Fatalf("log length = %d, want 3 (no duplicate part written)", n)
	}
}

// --- 3.5: ownership ---

func TestOnLogChunkOwnershipMatchAdmits(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusStored {
		t.Fatalf("expected STORED for a matching owner, got %+v", acks[0])
	}
}

func TestOnLogChunkForeignCellRefused(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "other-cell", Instance: "inst-other"})
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusRejected || acks[0].Reason != logReasonForeignRun {
		t.Fatalf("expected REJECTED foreign-run, got %+v", acks[0])
	}
	n, _ := logObjectLength(context.Background(), h.logStore, blob.LogKey(runID))
	if n != 0 {
		t.Fatalf("expected nothing written for a foreign run, wrote %d bytes", n)
	}
}

func TestOnLogChunkNoLedgerRowRefused(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester() // no answer configured -> Found: false
	runID := newTestRunID()
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusRejected || acks[0].Reason != logReasonForeignRun {
		t.Fatalf("expected REJECTED foreign-run for no ledger row, got %+v", acks[0])
	}
}

func TestOnLogChunkOwnershipTimeoutAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	owner.err = errors.New("deadline exceeded")
	runID := newTestRunID()
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusBusy || acks[0].Reason != logReasonOwnerLookupFail {
		t.Fatalf("expected BUSY owner-lookup-failed, got %+v", acks[0])
	}
	n, _ := logObjectLength(context.Background(), h.logStore, blob.LogKey(runID))
	if n != 0 {
		t.Fatalf("expected nothing written on a failed lookup, wrote %d bytes", n)
	}
}

// TestOnLogChunkOwnerDatabaseErrorAnswersBusyAndIsNotCached proves
// design.md D4's fail-closed rule for a recorder-side failure: "Timeout,
// or the recorder is unreachable ... Answer BUSY." A database error is
// neither a timeout nor an unreachable recorder, but the same rule must
// apply — a recorder error reply must never be read as a genuine miss
// and cached as a foreign-run refusal.
func TestOnLogChunkOwnerDatabaseErrorAnswersBusyAndIsNotCached(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	// The recorder answers with an explicit error, not a genuine miss.
	owner.setAnswer(runID, ownerLookupResponse{Found: false, Error: "database error"})
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusBusy || acks[0].Reason != logReasonOwnerLookupFail {
		t.Fatalf("expected BUSY owner-lookup-failed for a recorder database error, got %+v", acks[0])
	}
	n, _ := logObjectLength(context.Background(), h.logStore, blob.LogKey(runID))
	if n != 0 {
		t.Fatalf("expected nothing written on a database error, wrote %d bytes", n)
	}

	// A later good answer is honoured, proving the error reply was never
	// cached as a foreign-run refusal.
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
	acks = waitForLogAcks(t, sender, 2)
	if acks[1].Status != logAckStatusStored {
		t.Fatalf("expected the retried lookup to admit the chunk once the recorder recovers, got %+v", acks[1])
	}
}

func TestOnLogChunkOneLookupServesQueuedChunks(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	owner.delay = 100 * time.Millisecond
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
		}()
	}
	wg.Wait()

	if calls := owner.callCount(); calls != 1 {
		t.Fatalf("expected one coalesced lookup for concurrent chunks of the same run, got %d", calls)
	}
	// The drain goroutine still writes after OnLogChunk returns; wait for
	// its acknowledgement so the temp directory cleanup finds no open file.
	waitForLogAcks(t, sender, 1)
}

func TestOnLogChunkOwnershipTransferInvalidatesCache(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 1)

	// A relayed granted claim moves the run to a new instance.
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst2")

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("y"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 2)
	if acks[1].Status != logAckStatusRejected || acks[1].Reason != logReasonForeignRun {
		t.Fatalf("expected the old instance rejected after transfer, got %+v", acks[1])
	}
}

// --- 3.3c: pending bounds ---

// blockingAppendStore wraps a real store and blocks every Append call
// until the test closes release, so a test can fill the pending queue
// deterministically without racing the drain goroutine.
type blockingAppendStore struct {
	blob.Store
	release chan struct{}
}

func (b blockingAppendStore) Append(ctx context.Context, key string, seq int64, p []byte, opts blob.ObjectOptions) error {
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Store.Append(ctx, key, seq, p, opts)
}

func TestOnLogChunkPerRunPendingBoundAnswersBusy(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	// The drain goroutine's first append blocks forever (never
	// released), so every chunk this test sends stays queued and the
	// per-run bound is reached deterministically.
	h.logStore = blockingAppendStore{h.logStore, make(chan struct{})}

	for i := int64(0); i < perRunPendingMaxChunks+1; i++ {
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: i, Data: []byte("x"), CellInstance: "inst1"})
	}

	deadline := time.Now().Add(5 * time.Second)
	var sawBusy bool
	for time.Now().Before(deadline) {
		for _, a := range sender.logAcks() {
			if a.Status == logAckStatusBusy && a.Reason == logReasonRunPendingFull {
				sawBusy = true
			}
		}
		if sawBusy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawBusy {
		t.Fatal("expected at least one BUSY run-pending-full at the per-run bound")
	}
}

func TestOnLogChunkGatewayBudgetAnswersBusyForEveryRun(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	owner.delay = 200 * time.Millisecond
	runA, runB := newTestRunID(), newTestRunID()
	owner.setAnswer(runA, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	owner.setAnswer(runB, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	h.logPendingMaxBytes = 4 // tiny gateway-wide budget

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runA, Seq: 0, Data: []byte("aaaaa"), CellInstance: "inst1"})
	acksA := waitForLogAcks(t, sender, 1)
	if acksA[0].Status != logAckStatusBusy || acksA[0].Reason != logReasonGatewayBudgetFul {
		t.Fatalf("expected BUSY gateway-budget-full for run A, got %+v", acksA[0])
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runB, Seq: 0, Data: []byte("bbbbb"), CellInstance: "inst1"})
	acksB := waitForLogAcks(t, sender, 2)
	if acksB[1].Status != logAckStatusBusy || acksB[1].Reason != logReasonGatewayBudgetFul {
		t.Fatalf("expected BUSY gateway-budget-full for run B too, got %+v", acksB[1])
	}
}

// TestOnLogChunkGatewayBudgetAdmitsExactlyOneUnderRace proves the
// gateway-wide budget check and the enqueue it gates happen under one
// critical section: N concurrent chunks racing a budget with room for
// exactly one admit exactly one, never more (design.md D4, "neither one
// discards a chunk" — the flip side is that neither one over-admits
// either).
func TestOnLogChunkGatewayBudgetAdmitsExactlyOneUnderRace(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	h := newLogTestHandler(t, owner, sender)
	// Every append blocks forever, so an admitted chunk's bytes stay
	// charged to the budget for the whole test: nothing drains, so a
	// race that over-admits is directly visible in the pending total.
	h.logStore = blockingAppendStore{h.logStore, make(chan struct{})}
	const chunkSize = 5
	h.logPendingMaxBytes = chunkSize // room for exactly one chunk

	const n = 20
	runIDs := make([]string, n)
	for i := range runIDs {
		runIDs[i] = newTestRunID()
		owner.setAnswer(runIDs[i], ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runIDs[i], Seq: 0, Data: []byte("aaaaa"), CellInstance: "inst1"})
		}(i)
	}
	wg.Wait()

	// The one admitted chunk never acks (its append never returns), so
	// exactly n-1 BUSY acks arrive.
	acks := waitForLogAcks(t, sender, n-1)
	for _, a := range acks {
		if a.Status != logAckStatusBusy || a.Reason != logReasonGatewayBudgetFul {
			t.Fatalf("expected BUSY gateway-budget-full for every excluded chunk, got %+v", a)
		}
	}

	h.logMu.Lock()
	pending := h.logTotalPendingBytes
	h.logMu.Unlock()
	if pending != chunkSize {
		t.Fatalf("expected exactly one chunk's bytes charged to the gateway-wide budget under the race, got %d want %d", pending, chunkSize)
	}
}

// --- 3.3b: restart recovery ---

func TestRecoverRunStateCleanRestart(t *testing.T) {
	owner := newFakeOwnerRequester()
	h := newLogTestHandler(t, owner, &recordingSender{})
	runID := newTestRunID()

	st := &runLogState{ns: "ns1", name: "cell1", ackSeq: -1}
	if err := h.recoverRunState(context.Background(), runID, st); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if st.offset != 0 || st.ackSeq != -1 {
		t.Fatalf("clean restart state = offset=%d ackSeq=%d, want 0/-1", st.offset, st.ackSeq)
	}
}

func TestRecoverRunStateIndexOneEntryShortWritesRecovered(t *testing.T) {
	owner := newFakeOwnerRequester()
	h := newLogTestHandler(t, owner, &recordingSender{})
	runID := newTestRunID()
	createdAt := runCreatedAtFromUUIDv7(runID)

	// Simulate a crash between the log append and the index append: the
	// log part landed, the index entry never did.
	logOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: createdAt}
	if err := h.logStore.Append(context.Background(), blob.LogKey(runID), 0, []byte("hello"), logOpts); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	st := &runLogState{ns: "ns1", name: "cell1", ackSeq: -1}
	if err := h.recoverRunState(context.Background(), runID, st); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if st.offset != 5 || st.ackSeq != 0 {
		t.Fatalf("recovered state = offset=%d ackSeq=%d, want 5/0", st.offset, st.ackSeq)
	}

	entries, maxSeq, _, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 0 || !entries[0].Recovered || entries[0].Offset != 0 || entries[0].Len != 5 {
		t.Fatalf("recovered entry = %+v (maxSeq=%d)", entries[0], maxSeq)
	}

	// The run continues: the writer must not re-repair the same gap.
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	sender := &recordingSender{}
	h.buildWorkers["ns1/cell1"] = &registeredBuildWorker{token: 0, worker: &buildEventWorker{send: sender.send}}
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte(" world"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 1)

	entries2, maxSeq2, _, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index after continuing: %v", err)
	}
	if maxSeq2 != 1 || entries2[1].Offset != 5 {
		t.Fatalf("continued entry = %+v (maxSeq=%d), want offset=5", entries2[1], maxSeq2)
	}
}

// TestRecoverRunStateRestoresFinalMarkerAndHoldsOffHeartbeat proves
// design.md D4's restart claim directly: "A restarted gateway reads the
// entry back and needs no memory." It seeds a durable final log part and
// index entry — exactly what a live gateway leaves behind if it crashes
// after writing both but before the mite's ack or the finalized publish
// — then asserts recovery restores the final marker and that the
// writer-heartbeat condition immediately reports no work, so a
// restarted gateway never restarts the heartbeat over an already-
// finished run and never leaves the recorder's finalize sweep free to
// call it not-finalized.
func TestRecoverRunStateRestoresFinalMarkerAndHoldsOffHeartbeat(t *testing.T) {
	owner := newFakeOwnerRequester()
	h := newLogTestHandler(t, owner, &recordingSender{})
	runID := newTestRunID()
	createdAt := runCreatedAtFromUUIDv7(runID)

	logOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: createdAt}
	if err := h.logStore.Append(context.Background(), blob.LogKey(runID), 0, nil, logOpts); err != nil {
		t.Fatalf("seed final log part: %v", err)
	}
	entry := LogIndexEntry{Seq: 0, Offset: 0, Len: 0, Final: true}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal seeded index entry: %v", err)
	}
	line = append(line, '\n')
	indexOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "application/x-ndjson", CreatedAt: createdAt}
	if err := h.logStore.Append(context.Background(), blob.LogIndexKey(runID), 0, line, indexOpts); err != nil {
		t.Fatalf("seed final index entry: %v", err)
	}

	st := &runLogState{ns: "ns1", name: "cell1", ackSeq: -1}
	if err := h.recoverRunState(context.Background(), runID, st); err != nil {
		t.Fatalf("recover: %v", err)
	}

	st.mu.Lock()
	finalDurable := st.finalDurable
	ackSeq := st.ackSeq
	hasWork := st.hasHeartbeatWorkLocked()
	st.mu.Unlock()

	if !finalDurable {
		t.Fatal("expected recovery to restore the final marker from the durable index entry")
	}
	if ackSeq != 0 {
		t.Fatalf("expected ackSeq=0 after recovering the final entry, got %d", ackSeq)
	}
	if hasWork {
		t.Fatal("expected no heartbeat work once the final marker is restored: a restarted gateway must not restart the heartbeat, and the recorder's sweep must not be left free to call this run not-finalized")
	}
}

// TestOnLogChunkSkippedSequenceMarksIncompleteAtFinalizeDeadline proves
// design.md D4's gateway-gap rule fires only once a hole is known never
// to fill — the finalize deadline, checked on the append path — and not
// the instant an ahead chunk arrives, because a later chunk can still
// close the hole and prove the run complete after all.
func TestOnLogChunkSkippedSequenceMarksIncompleteAtFinalizeDeadline(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	h.logFinalizeWait = 50 * time.Millisecond

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 1)
	// A mite restart loses its retained buffer: the next chunk this
	// gateway sees jumps straight to seq 5, leaving a hole at 1-4.
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 5, Data: []byte("f"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 2)

	// The hole is still open, and it might still fill: no incompleteness
	// is recorded yet.
	if incomplete, _ := h.runIncompleteness(runID); incomplete {
		t.Fatal("expected no incompleteness while the hole could still fill")
	}

	// The run goes terminal with an anchor already in the past, so the
	// deadline is already due. A later touch of the append path is what
	// actually makes the gateway notice and abandon the run — the
	// deadline is polled there, never by a background sweep.
	h.markRunTerminal("ns1", "cell1", runID, "", time.Now().Add(-time.Hour))
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 6, Data: []byte("z"), CellInstance: "inst1"})

	deadline := time.Now().Add(3 * time.Second)
	var incomplete bool
	var reason string
	for time.Now().Before(deadline) {
		incomplete, reason = h.runIncompleteness(runID)
		if incomplete {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !incomplete || reason != logReasonGatewayGap {
		t.Fatalf("incomplete=%v reason=%q, want true/gateway-gap once the deadline passes with the hole still open", incomplete, reason)
	}

	// Seq 5's bytes are durable but stranded behind the never-filled
	// hole: nothing is ever written for it in the index. An absent entry
	// is already what the manifest treats as a hole, so writing one with
	// a made-up offset would claim a byte position through a gap a
	// reader can never reach.
	entries, maxSeq, _, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 0 {
		t.Fatalf("expected only seq 0 durable in the index, maxSeq=%d entries=%+v", maxSeq, entries)
	}
	if entries[0].Offset != 0 {
		t.Fatalf("expected seq 0's offset to stay truthful at 0, got %+v", entries[0])
	}
	n, err := logObjectLength(context.Background(), h.logStore, blob.LogKey(runID))
	if err != nil {
		t.Fatalf("log length: %v", err)
	}
	if n != 1 {
		t.Fatalf("log length = %d, want 1: the readable object stops exactly where the still-open hole begins", n)
	}
}

// TestOnLogChunkStoredAckNeverCrossesAHoleThenCatchesUpOnBackfill proves
// design.md D4's STORED rule directly: "it names the highest sequence
// stored with no hole below it." Seq 5 landing after seq 0 must not move
// the ack past 0, and a redelivered seq 1 must still be honoured rather
// than swallowed as already-acknowledged — the defect the cumulative
// ack_seq bug produced.
func TestOnLogChunkStoredAckNeverCrossesAHoleThenCatchesUpOnBackfill(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].AckSeq != 0 {
		t.Fatalf("expected ack_seq=0 for the first chunk, got %d", acks[0].AckSeq)
	}

	// Seq 5 arrives next, leaving a hole at 1-4. The chunk is still
	// stored, but the cumulative ack must not cross the hole.
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 5, Data: []byte("f"), CellInstance: "inst1"})
	acks = waitForLogAcks(t, sender, 2)
	if acks[1].Status != logAckStatusStored || acks[1].AckSeq != 0 {
		t.Fatalf("expected the STORED ack to stay at 0 across the hole, got %+v", acks[1])
	}

	// The hole backfills one sequence at a time. A redelivered seq 1
	// must be honoured — not treated as already covered by the ack_seq=5
	// the old, buggy bookkeeping would have sent — and seq 4 must catch
	// the ack all the way up through the already-stored seq 5.
	for i, seq := range []int64{1, 2, 3, 4} {
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: seq, Data: []byte("x"), CellInstance: "inst1"})
		acks = waitForLogAcks(t, sender, 3+i)
	}
	last := acks[len(acks)-1]
	if last.AckSeq != 5 {
		t.Fatalf("expected the ack to catch up to 5 once the hole is filled, got %d", last.AckSeq)
	}

	entries, maxSeq, _, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 5 {
		t.Fatalf("expected every sequence 0-5 durable, maxSeq=%d", maxSeq)
	}
	for seq := int64(0); seq <= 5; seq++ {
		if _, ok := entries[seq]; !ok {
			t.Fatalf("expected seq %d durable in the index, entries=%+v", seq, entries)
		}
	}

	// Every offset reflects the SEQUENCE-ordered byte layout — the order
	// C1's range reader concatenates parts in, never the order the
	// chunks arrived at the gateway in. Seq 5 arrived second (right
	// after seq 0) but reads back fifth, at offset 5 (one byte each for
	// seq 0-4), not at offset 1 where an arrival-order counter would
	// have placed it.
	wantOffset := map[int64]int64{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5}
	for seq, want := range wantOffset {
		if entries[seq].Offset != want {
			t.Fatalf("seq %d offset = %d, want %d (entries=%+v)", seq, entries[seq].Offset, want, entries)
		}
	}

	// A range read at each entry's own offset returns exactly that
	// chunk's own bytes.
	wantByte := map[int64]byte{0: 'a', 1: 'x', 2: 'x', 3: 'x', 4: 'x', 5: 'f'}
	for seq, want := range wantByte {
		r, err := h.logStore.NewRangeReader(context.Background(), blob.LogKey(runID), entries[seq].Offset, entries[seq].Len)
		if err != nil {
			t.Fatalf("range read seq %d: %v", seq, err)
		}
		buf := make([]byte, 8)
		n, _ := r.Read(buf)
		_ = r.Close()
		if n != 1 || buf[0] != want {
			t.Fatalf("range read at seq %d's offset = %q, want %q", seq, buf[:n], []byte{want})
		}
	}
}

// TestOnLogChunkRestartMidGapLosesNothingAfterMiteReplay proves a
// gateway crash between seq 0 and a backfilled seq 5 loses no data. Seq
// 5's index entry lived only in the dead process's memory; the mite
// still holds seq 5 too, because it was never covered by a STORED
// ack_seq, and replays it on the reconnect. A restarted gateway that
// receives that replay, followed by the sequences that fill the hole,
// produces exactly the same durable index and offsets as an
// uninterrupted run would have.
func TestOnLogChunkRestartMidGapLosesNothingAfterMiteReplay(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 1)
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 5, Data: []byte("f"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 2)

	// The gateway crashes here. A fresh handler over the same blob store
	// models the restart: it holds no memory of seq 5's pending entry.
	h2 := &BusHandler{
		cluster:        "core",
		cellInstances:  make(map[string]string),
		buildWorkers:   make(map[string]*registeredBuildWorker),
		claimCounts:    make(map[string]int),
		tenantResolver: defaultCellTenantResolver{},
		logRuns:        make(map[string]*runLogState),
		ownerCache:     newOwnerCache(0),
		ledger:         owner,
		logStore:       h.logStore,
	}
	sender2 := &recordingSender{}
	h2.buildWorkers["ns1/cell1"] = &registeredBuildWorker{token: 0, worker: &buildEventWorker{send: sender2.send}}

	// The mite replays seq 5 to the restarted gateway, identical bytes
	// and all — an idempotent re-append.
	h2.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 5, Data: []byte("f"), CellInstance: "inst1"})
	waitForLogAcks(t, sender2, 1)

	// The hole then fills in order.
	for i, seq := range []int64{1, 2, 3, 4} {
		h2.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: seq, Data: []byte("x"), CellInstance: "inst1"})
		waitForLogAcks(t, sender2, 2+i)
	}

	acks := sender2.logAcks()
	if last := acks[len(acks)-1]; last.AckSeq != 5 {
		t.Fatalf("expected the ack to reach 5 after the replay fills the hole, got %d", last.AckSeq)
	}

	entries, maxSeq, _, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 5 {
		t.Fatalf("expected every sequence 0-5 durable after the replay, maxSeq=%d", maxSeq)
	}
	wantOffset := map[int64]int64{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5}
	for seq, want := range wantOffset {
		e, ok := entries[seq]
		if !ok || e.Offset != want {
			t.Fatalf("seq %d = %+v, want offset %d", seq, e, want)
		}
	}

	r, err := h.logStore.NewRangeReader(context.Background(), blob.LogKey(runID), entries[5].Offset, entries[5].Len)
	if err != nil {
		t.Fatalf("range read seq 5: %v", err)
	}
	buf := make([]byte, 8)
	n, _ := r.Read(buf)
	_ = r.Close()
	if n != 1 || buf[0] != 'f' {
		t.Fatalf("range read at seq 5's offset = %q, want %q", buf[:n], "f")
	}
}

// --- 3.9a: finalize deadline ---

// failingAppendStore wraps a real store and fails every Append call, to
// simulate a permanent blob outage while still answering reads (restart
// recovery) through the wrapped store.
type failingAppendStore struct {
	blob.Store
}

func (f failingAppendStore) Append(ctx context.Context, key string, seq int64, p []byte, opts blob.ObjectOptions) error {
	return errors.New("blob store outage")
}

// blockingUntilCtxStore wraps a real store and blocks every Append call
// until its ctx argument ends, returning ctx's error. Reads pass through
// to the wrapped store, so restart recovery still works. It proves a
// deadline actually unblocks a stuck append instead of holding it open
// forever.
type blockingUntilCtxStore struct {
	blob.Store
}

func (blockingUntilCtxStore) Append(ctx context.Context, key string, seq int64, p []byte, opts blob.ObjectOptions) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestOnLogChunkFinalizeDeadlineUnblocksAStuckAppend proves design.md
// D4's deadline-bound claim directly: "The heartbeat and the retry both
// end at that deadline." An append that blocks forever must still
// unblock and abandon the run once the finalize deadline passes, rather
// than holding the pending queue open indefinitely.
func TestOnLogChunkFinalizeDeadlineUnblocksAStuckAppend(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	h.logStore = blockingUntilCtxStore{h.logStore}
	h.logFinalizeWait = 50 * time.Millisecond

	// Mark the run terminal with a deadline 50ms out, then send a chunk
	// whose append blocks until that deadline cancels its context.
	h.markRunTerminal("ns1", "cell1", runID, "", time.Now())
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})

	deadline := time.Now().Add(3 * time.Second)
	var sawDeadlineReject bool
	for time.Now().Before(deadline) {
		for _, a := range sender.logAcks() {
			if a.Status == logAckStatusRejected && a.Reason == logReasonFinalizeDeadline {
				sawDeadlineReject = true
			}
		}
		if sawDeadlineReject {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawDeadlineReject {
		t.Fatal("expected a permanently blocked append to unblock at the finalize deadline and reject the chunk")
	}
}

func TestOnLogChunkFinalizeDeadlineAbandonsAndRejectsLateArrivals(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	h.logStore = failingAppendStore{h.logStore}
	h.logFinalizeWait = 50 * time.Millisecond

	// Mark the run terminal with an anchor already in the past, so the
	// deadline is already due.
	st := h.getOrCreateRunState(runID, "ns1", "cell1", "")
	h.markRunTerminal("ns1", "cell1", runID, "", time.Now().Add(-time.Hour))

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("x"), CellInstance: "inst1"})

	deadline := time.Now().Add(3 * time.Second)
	var sawDeadlineReject bool
	for time.Now().Before(deadline) {
		for _, a := range sender.logAcks() {
			if a.Status == logAckStatusRejected && a.Reason == logReasonFinalizeDeadline {
				sawDeadlineReject = true
			}
		}
		if sawDeadlineReject {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawDeadlineReject {
		t.Fatal("expected REJECTED finalize-deadline for the abandoned chunk")
	}

	st.mu.Lock()
	rejectedForever := st.rejectedForever
	st.mu.Unlock()
	if !rejectedForever {
		t.Fatal("expected the run permanently rejected after the deadline")
	}

	// A later arrival, including one that never entered the pending
	// queue, is rejected immediately.
	before := len(sender.logAcks())
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("y"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, before+1)
	last := acks[len(acks)-1]
	if last.Status != logAckStatusRejected || last.Reason != logReasonFinalizeDeadline {
		t.Fatalf("expected the late arrival rejected with finalize-deadline, got %+v", last)
	}
}

// TestOnLogChunkIdleRunStillAbandonsAtDeadlineWithNoLaterChunk proves the
// finalize deadline fires abandonment on its own when the run goes idle
// with an open hole and no later chunk ever touches the append path
// again. Without a deadline-side abandon, a stranded sequence would never
// be released, the run would never be reported gateway-gap, and the
// writer heartbeat would run forever.
func TestOnLogChunkIdleRunStillAbandonsAtDeadlineWithNoLaterChunk(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub
	h.logFinalizeWait = 50 * time.Millisecond

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 1)
	// A hole opens at 1-4: seq 5's bytes land, but its index entry stays
	// pending behind the hole.
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 5, Data: []byte("f"), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 2)

	st := h.getOrCreateRunState(runID, "ns1", "cell1", "")

	// The run goes terminal with an anchor already in the past, so the
	// deadline is already due. No chunk ever touches the append path
	// again: the drain goroutine has already exited, so only the
	// deadline timer itself can abandon this run.
	h.markRunTerminal("ns1", "cell1", runID, "", time.Now().Add(-time.Hour))

	deadline := time.Now().Add(3 * time.Second)
	var sawReject bool
	for time.Now().Before(deadline) {
		for _, a := range sender.logAcks() {
			if a.Status == logAckStatusRejected && a.Reason == logReasonFinalizeDeadline && a.AckSeq == 5 {
				sawReject = true
			}
		}
		if sawReject {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawReject {
		t.Fatal("expected REJECTED finalize-deadline for the stranded seq 5, released by the deadline timer with no later chunk ever arriving")
	}

	var sawGatewayGap bool
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for i := 0; i < pub.publishedCount(); i++ {
			env := pub.envelopeAt(i)
			if reason, _ := env.Payload["incomplete_reason"].(string); reason == logReasonGatewayGap {
				sawGatewayGap = true
			}
		}
		if sawGatewayGap {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawGatewayGap {
		t.Fatal("expected a run.log publish with incomplete_reason=gateway-gap from the deadline-timer abandon")
	}

	st.mu.Lock()
	rejectedForever := st.rejectedForever
	hasWork := st.hasHeartbeatWorkLocked()
	heartbeatRunning := st.heartbeatCancel != nil
	st.mu.Unlock()
	if !rejectedForever {
		t.Fatal("expected the run permanently rejected after the idle deadline fired")
	}
	if hasWork {
		t.Fatal("expected no heartbeat work once the deadline timer abandoned the idle run")
	}
	if heartbeatRunning {
		t.Fatal("expected the writer heartbeat stopped once the deadline timer abandoned the idle run")
	}
}

// --- 3.7: finalization evidence ---

func TestOnLogChunkFinalEntryDurableBeforeAckAndPublishesFinalized(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: nil, Final: true, CellInstance: "inst1"})
	waitForLogAcks(t, sender, 1)

	entries, maxSeq, finalSeen, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 0 || !finalSeen || !entries[0].Final {
		t.Fatalf("expected the final entry durable, got maxSeq=%d finalSeen=%v entries=%+v", maxSeq, finalSeen, entries)
	}
	if !h.runFinalized(runID) {
		t.Fatal("expected runFinalized true once the final entry is durable")
	}

	deadline := time.Now().Add(2 * time.Second)
	var sawFinalized bool
	for time.Now().Before(deadline) {
		if pub.publishedCount() > 0 {
			env := pub.envelopeAt(pub.publishedCount() - 1)
			if v, _ := env.Payload["finalized"].(bool); v {
				sawFinalized = true
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawFinalized {
		t.Fatal("expected a run.log publish with finalized true after the final entry became durable")
	}
}

// TestOnLogChunkBestEffortSummaryStampsResolvedTenant proves a
// best-effort run.log envelope (the "finalized" marker here) stamps the
// job's resolved tenant, not the cell's own synthetic
// "<namespace>/<cell>" tenant — the same tenant the ordered event group
// resolves through CellTenantResolver. A wrong tenant here would win
// applyRunLog's ON CONFLICT insert forever whenever this envelope beats
// run.started to the recorder.
func TestOnLogChunkBestEffortSummaryStampsResolvedTenant(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	pub := &fakePublisher{}
	h := newLogTestHandler(t, owner, sender)
	h.publisher = pub
	h.SetCellTenantResolver(stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/build": {tenant: "team-a", generation: 3},
	}})

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
		RunId: runID, Seq: 0, Data: nil, Final: true, CellInstance: "inst1", JobPath: "team-a/build",
	})
	waitForLogAcks(t, sender, 1)

	deadline := time.Now().Add(2 * time.Second)
	var sawFinalized bool
	var tenant string
	for time.Now().Before(deadline) {
		if pub.publishedCount() > 0 {
			env := pub.envelopeAt(pub.publishedCount() - 1)
			if v, _ := env.Payload["finalized"].(bool); v {
				sawFinalized = true
				tenant = env.Tenant
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawFinalized {
		t.Fatal("expected a run.log publish with finalized true after the final entry became durable")
	}
	if tenant != "team-a" {
		t.Fatalf("envelope tenant = %q, want the resolved job tenant %q, not the cell's synthetic tenant", tenant, "team-a")
	}
}
