package mite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
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
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	return newLogTestHandlerOnStore(t, owner, sender, store)
}

// newLogTestHandlerOnStore builds the same handler over an existing blob
// store, so a test can model a gateway restart over stored state.
func newLogTestHandlerOnStore(t *testing.T, owner *fakeOwnerRequester, sender *recordingSender, store blob.Store) *BusHandler {
	t.Helper()
	const ns, name = "ns1", "cell1"
	h := &BusHandler{
		metrics:        noopGatewayMetrics(),
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

// seedIndexedRunWithUnindexedParts indexes chunks 0 and 1 through a
// live gateway, then stores each of extra as a log part with no index
// entry, the state a crash inside the index flush leaves. A nil value
// leaves that sequence missing.
func seedIndexedRunWithUnindexedParts(t *testing.T, owner *fakeOwnerRequester, runID string, extra map[int64][]byte) blob.Store {
	t.Helper()
	sender := &recordingSender{}
	h := newLogTestHandler(t, owner, sender)
	for i, seq := range []int64{0, 1} {
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: seq, Data: []byte(fmt.Sprintf("a%d ", seq)), CellInstance: "inst1"})
		waitForLogAcks(t, sender, i+1)
	}
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: runCreatedAtFromUUIDv7(runID)}
	for seq, data := range extra {
		if data == nil {
			continue
		}
		if err := h.logStore.Append(context.Background(), blob.LogKey(runID), seq, data, opts); err != nil {
			t.Fatalf("append part %d: %v", seq, err)
		}
	}
	return h.logStore
}

// assertIndexInsideLog asserts that every index entry of runID lies
// inside the readable log, and that the entries are contiguous.
func assertIndexInsideLog(t *testing.T, store blob.Store, runID string) map[int64]LogIndexEntry {
	t.Helper()
	logLen := int64(len(readStoredLog(t, store, runID)))
	entries, maxSeq, _, err := readLogIndex(context.Background(), store, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var end int64
	for seq := int64(0); seq <= maxSeq; seq++ {
		e, ok := entries[seq]
		if !ok {
			t.Fatalf("index has no entry at %d (maxSeq %d)", seq, maxSeq)
		}
		if e.Offset != end || e.Offset+e.Len > logLen {
			t.Fatalf("entry %d = %+v, want offset %d and an end inside the %d-byte log", seq, e, end, logLen)
		}
		end = e.Offset + e.Len
	}
	return entries
}

// TestRecoverRunStateWritesOneEntryPerReadablePart proves the per-part
// repair. Chunks 0 and 1 are indexed, and parts 2 and 3 hold bytes with
// no index entry. Recovery writes one entry for each part, with its own
// length. The mite's replay of chunk 3 is then already stored, and the
// next chunk lands at the true end of the log.
func TestRecoverRunStateWritesOneEntryPerReadablePart(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	store := seedIndexedRunWithUnindexedParts(t, owner, runID, map[int64][]byte{2: []byte("a2 "), 3: []byte("a3-long ")})

	sender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, owner, sender, store)
	restarted.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 3, Data: []byte("a3-long "), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusStored || acks[0].AckSeq != 3 {
		t.Fatalf("replay ack = %+v, want STORED ack_seq=3", acks[0])
	}

	entries := assertIndexInsideLog(t, store, runID)
	for seq, want := range map[int64]int64{2: 3, 3: 8} {
		if e := entries[seq]; !e.Recovered || e.Len != want {
			t.Fatalf("entry %d = %+v, want a recovered entry of length %d", seq, e, want)
		}
	}
	if len(entries) != 4 {
		t.Fatalf("index holds %d entries, want 4", len(entries))
	}

	restarted.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 4, Data: []byte("a4 "), CellInstance: "inst1"})
	waitForLogAcks(t, sender, 2)
	entries = assertIndexInsideLog(t, store, runID)
	if e := entries[4]; e.Offset != int64(len("a0 a1 a2 a3-long ")) {
		t.Fatalf("entry 4 = %+v, want the offset at the end of part 3", e)
	}
	if incomplete, reason := restarted.runIncompleteness(runID); incomplete {
		t.Fatalf("run incomplete with %q, want a complete run", reason)
	}
}

// TestRecoverRunStateStopsAtAHoleAboveTheIndex asserts that recovery
// writes no entry above a missing part. Part 4 waits for the mite to
// send chunk 3 and chunk 4 again.
func TestRecoverRunStateStopsAtAHoleAboveTheIndex(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	store := seedIndexedRunWithUnindexedParts(t, owner, runID, map[int64][]byte{2: []byte("a2 "), 4: []byte("a4 ")})

	sender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, owner, sender, store)
	st := &runLogState{ns: "ns1", name: "cell1", ackSeq: -1}
	if err := restarted.recoverRunState(context.Background(), runID, st); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if st.ackSeq != 2 || st.offset != int64(len("a0 a1 a2 ")) {
		t.Fatalf("recovered state = ackSeq=%d offset=%d, want 2/%d", st.ackSeq, st.offset, len("a0 a1 a2 "))
	}
	entries := assertIndexInsideLog(t, store, runID)
	if _, ok := entries[4]; ok || len(entries) != 3 {
		t.Fatalf("index = %+v, want entries 0 to 2 only", entries)
	}
}

// TestRecoverRunStateStopsAtAnEmptyPart asserts that recovery writes no
// entry for an empty part, or for any part above it. A blank pass writes
// empty parts, and an entry for one would drop the new writer's chunk.
func TestRecoverRunStateStopsAtAnEmptyPart(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	store := seedIndexedRunWithUnindexedParts(t, owner, runID, map[int64][]byte{2: []byte("a2 "), 3: {}, 4: []byte("a4 ")})

	restarted := newLogTestHandlerOnStore(t, owner, &recordingSender{}, store)
	st := &runLogState{ns: "ns1", name: "cell1", ackSeq: -1}
	if err := restarted.recoverRunState(context.Background(), runID, st); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if st.ackSeq != 2 || st.offset != int64(len("a0 a1 a2 ")) {
		t.Fatalf("recovered state = ackSeq=%d offset=%d, want 2/%d", st.ackSeq, st.offset, len("a0 a1 a2 "))
	}
	entries, maxSeq, _, err := readLogIndex(context.Background(), store, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 2 || len(entries) != 3 {
		t.Fatalf("index = %+v (maxSeq %d), want entries 0 to 2 only", entries, maxSeq)
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
		metrics:        noopGatewayMetrics(),
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

// --- a moved build over the parts a lost writer left above a hole ---

// registerLogTestSender registers a second controller's sender, so a test
// can tell the acks of the moved build's cell from the old cell's.
func registerLogTestSender(h *BusHandler, ns, name string) *recordingSender {
	s := &recordingSender{}
	h.buildMu.Lock()
	h.buildWorkers[ns+"/"+name] = &registeredBuildWorker{token: 0, worker: &buildEventWorker{send: s.send}}
	h.buildMu.Unlock()
	return s
}

// readStoredLog answers the readable log of runID: every part up to the
// first missing sequence.
func readStoredLog(t *testing.T, store blob.Store, runID string) string {
	t.Helper()
	r, err := store.NewRangeReader(context.Background(), blob.LogKey(runID), 0, -1)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(got)
}

// storeOldWriterPartsAboveAHole stores chunks 0, 1, and 3 of runID from
// the old writer ns1/cell1/inst1 and waits for their three acks. Chunk 3
// stays pending above the hole at 2.
func storeOldWriterPartsAboveAHole(t *testing.T, h *BusHandler, sender *recordingSender, runID string) {
	t.Helper()
	for i, seq := range []int64{0, 1, 3} {
		h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
			RunId: runID, Seq: seq, Data: []byte(fmt.Sprintf("a%d ", seq)), CellInstance: "inst1",
		})
		waitForLogAcks(t, sender, i+1)
	}
	if last := sender.logAcks()[2]; last.Status != logAckStatusStored || last.AckSeq != 1 {
		t.Fatalf("expected the ack to stay at 1 below the hole, got %+v", last)
	}
}

// sendMovedBuildChunks sends chunks 2, 3, and 4 of runID from the moved
// build's writer and waits for each STORED ack on that writer's sender.
func sendMovedBuildChunks(t *testing.T, h *BusHandler, sender *recordingSender, ns, name, instance, runID string) {
	t.Helper()
	before := len(sender.logAcks())
	for i, seq := range []int64{2, 3, 4} {
		h.OnLogChunk(name, ns, &mitev1.LogChunk{
			RunId: runID, Seq: seq, Data: []byte(fmt.Sprintf("b%d ", seq)), CellInstance: instance,
		})
		acks := waitForLogAcks(t, sender, before+i+1)
		if got := acks[before+i]; got.Status != logAckStatusStored || got.AckSeq != seq {
			t.Fatalf("chunk %d: expected STORED ack_seq=%d, got %+v", seq, seq, got)
		}
	}
}

func assertRunGatewayGap(t *testing.T, h *BusHandler, runID string) {
	t.Helper()
	incomplete, reason := h.runIncompleteness(runID)
	if !incomplete || reason != logReasonGatewayGap {
		t.Fatalf("expected the run incomplete with %q, got incomplete=%v reason=%q", logReasonGatewayGap, incomplete, reason)
	}
}

// TestOnLogChunkMovedBuildDiscardsOldPartsAboveAHoleOnALiveGateway pins
// the case where the gateway still holds the old writer's pending part
// at 3. The moved build's chunks 2, 3, and 4 must all reach the log.
func TestOnLogChunkMovedBuildDiscardsOldPartsAboveAHoleOnALiveGateway(t *testing.T) {
	oldSender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, oldSender)
	newSender := registerLogTestSender(h, "ns1", "cell2")

	storeOldWriterPartsAboveAHole(t, h, oldSender, runID)

	// The mesh moves the run to cell2. The new cell's run-state fetch
	// drops the cached owner.
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
	h.ownerCache.invalidate(runID)

	sendMovedBuildChunks(t, h, newSender, "ns1", "cell2", "inst2", runID)

	if got, want := readStoredLog(t, h.logStore, runID), "a0 a1 b2 b3 b4 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	assertRunGatewayGap(t, h, runID)
	if n := len(oldSender.logAcks()); n != 3 {
		t.Fatalf("expected no ack of the moved build on the old cell, got %d acks", n)
	}
}

// TestOnLogChunkMovedBuildOverwritesAnOldPartAfterAGatewayRestart pins
// the case where the gateway holds no state for the run: the stored part
// at 3 conflicts with the moved build's chunk 3.
func TestOnLogChunkMovedBuildOverwritesAnOldPartAfterAGatewayRestart(t *testing.T) {
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	runID := newTestRunID()
	oldOwner := newFakeOwnerRequester()
	oldOwner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	oldSender := &recordingSender{}
	storeOldWriterPartsAboveAHole(t, newLogTestHandlerOnStore(t, oldOwner, oldSender, store), oldSender, runID)

	newOwner := newFakeOwnerRequester()
	newOwner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst2"})
	newSender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, newOwner, newSender, store)

	start := time.Now()
	sendMovedBuildChunks(t, restarted, newSender, "ns1", "cell1", "inst2", runID)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("expected the conflict resolved without retries, took %s", elapsed)
	}

	if got, want := readStoredLog(t, store, runID), "a0 a1 b2 b3 b4 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	assertRunGatewayGap(t, restarted, runID)
}

// TestOnLogChunkStaleWriterConflictIsRefusedAndKeepsTheStoredPart pins
// the fresh owner read on a conflict. A gateway whose owner cache still
// matches the old writer must not overwrite the new writer's part, and
// the run stays open for the new writer.
func TestOnLogChunkStaleWriterConflictIsRefusedAndKeepsTheStoredPart(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst2"})
	h := newLogTestHandler(t, owner, sender)
	// The cache still matches the old writer inst1.
	h.ownerCache.observeGrant(runID, "core", "ns1", "cell1", "inst1")

	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: runCreatedAtFromUUIDv7(runID)}
	if err := h.logStore.Append(context.Background(), blob.LogKey(runID), 1, []byte("b1 "), opts); err != nil {
		t.Fatalf("store the new writer's part: %v", err)
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("a1 "), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusRejected || acks[0].Reason != logReasonForeignRun || acks[0].AckSeq != 1 {
		t.Fatalf("expected REJECTED foreign-run for seq 1, got %+v", acks[0])
	}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("b0 "), CellInstance: "inst2"})
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b1 "), CellInstance: "inst2"})
	acks = waitForLogAcks(t, sender, 3)
	if last := acks[2]; last.Status != logAckStatusStored || last.AckSeq != 1 {
		t.Fatalf("expected the new writer stored through 1, got %+v", last)
	}
	if got, want := readStoredLog(t, h.logStore, runID), "b0 b1 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
}

// indexConflictStore answers ErrChunkConflict for every log index
// append.
type indexConflictStore struct {
	blob.Store
	runID string
}

func (s indexConflictStore) Append(ctx context.Context, key string, seq int64, p []byte, opts blob.ObjectOptions) error {
	if key == blob.LogIndexKey(s.runID) {
		return blob.ErrChunkConflict
	}
	return s.Store.Append(ctx, key, seq, p, opts)
}

// TestOnLogChunkIndexConflictAbandonsWithGatewayGapBeforeTheDeadline
// pins that a conflicting index entry ends the run at once as a gap. A
// retry of the same bytes can never succeed, and a live run has no
// finalize deadline to end the retry.
func TestOnLogChunkIndexConflictAbandonsWithGatewayGapBeforeTheDeadline(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	h.logStore = indexConflictStore{Store: h.logStore, runID: runID}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a"), CellInstance: "inst1"})
	acks := waitForLogAcks(t, sender, 1)
	if acks[0].Status != logAckStatusRejected {
		t.Fatalf("expected the chunk rejected once the run is abandoned, got %+v", acks[0])
	}
	assertRunGatewayGap(t, h, runID)
	st := h.getOrCreateRunState(runID, "ns1", "cell1", "")
	st.mu.Lock()
	rejected := st.rejectedForever
	st.mu.Unlock()
	if !rejected {
		t.Fatal("expected the run abandoned")
	}
}

// TestOnLogChunkMovedBuildAcksReachTheNewCellBehindTheOldDrain pins the
// ack routing: the old cell's drain goroutine is still busy when the
// moved build's chunk joins the same queue, and that chunk's ack must go
// to the cell that sent it.
func TestOnLogChunkMovedBuildAcksReachTheNewCellBehindTheOldDrain(t *testing.T) {
	oldSender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, oldSender)
	newSender := registerLogTestSender(h, "ns1", "cell2")
	release := make(chan struct{})
	h.logStore = blockingAppendStore{Store: h.logStore, release: release}

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a0 "), CellInstance: "inst1"})

	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
	h.ownerCache.invalidate(runID)
	h.OnLogChunk("cell2", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("b1 "), CellInstance: "inst2"})
	close(release)

	acks := waitForLogAcks(t, newSender, 1)
	if acks[0].Status != logAckStatusStored || acks[0].AckSeq != 1 {
		t.Fatalf("expected STORED ack_seq=1 on the new cell, got %+v", acks[0])
	}
	oldAcks := oldSender.logAcks()
	if len(oldAcks) != 1 || oldAcks[0].AckSeq != 0 {
		t.Fatalf("expected only the old chunk's ack on the old cell, got %+v", oldAcks)
	}
	if got, want := readStoredLog(t, h.logStore, runID), "a0 b1 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
}

// TestOnLogChunkMovedBuildSurvivesAGatewayRestartBeforeItsReplacement
// pins the blank step. The moved build fills the hole at 2, and the
// gateway restarts before chunk 3 replaces the old part. Recovery must
// not index the old part 3 as a recovered entry.
func TestOnLogChunkMovedBuildSurvivesAGatewayRestartBeforeItsReplacement(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	storeOldWriterPartsAboveAHole(t, h, sender, runID)

	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst2"})
	h.ownerCache.invalidate(runID)
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 2, Data: []byte("b2 "), CellInstance: "inst2"})
	acks := waitForLogAcks(t, sender, 4)
	if acks[3].Status != logAckStatusStored || acks[3].AckSeq != 2 {
		t.Fatalf("expected STORED ack_seq=2, got %+v", acks[3])
	}
	if got, want := readStoredLog(t, h.logStore, runID), "a0 a1 b2 "; got != want {
		t.Fatalf("readable log before the replacement = %q, want %q", got, want)
	}

	restartedSender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, owner, restartedSender, h.logStore)
	for i, seq := range []int64{3, 4} {
		restarted.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
			RunId: runID, Seq: seq, Data: []byte(fmt.Sprintf("b%d ", seq)), CellInstance: "inst2",
		})
		acks := waitForLogAcks(t, restartedSender, i+1)
		if got := acks[i]; got.Status != logAckStatusStored || got.AckSeq != seq {
			t.Fatalf("chunk %d: expected STORED ack_seq=%d, got %+v", seq, seq, got)
		}
	}
	if got, want := readStoredLog(t, h.logStore, runID), "a0 a1 b2 b3 b4 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
}

// TestOnLogChunkSameWriterRestartAboveAHoleBlanksNothing pins that a
// gateway restart with a hole open, and no move, keeps the writer's own
// parts and marks no gap.
func TestOnLogChunkSameWriterRestartAboveAHoleBlanksNothing(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	storeOldWriterPartsAboveAHole(t, h, sender, runID)

	restartedSender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, owner, restartedSender, h.logStore)
	for i, seq := range []int64{2, 3} {
		restarted.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
			RunId: runID, Seq: seq, Data: []byte(fmt.Sprintf("a%d ", seq)), CellInstance: "inst1",
		})
		waitForLogAcks(t, restartedSender, i+1)
	}
	if last := restartedSender.logAcks()[1]; last.Status != logAckStatusStored || last.AckSeq != 3 {
		t.Fatalf("expected STORED ack_seq=3, got %+v", last)
	}
	if got, want := readStoredLog(t, h.logStore, runID), "a0 a1 a2 a3 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	if incomplete, reason := restarted.runIncompleteness(runID); incomplete {
		t.Fatalf("expected no gap for the same writer, got reason %q", reason)
	}
}

// TestOnLogChunkLongQueuedChunkOfALostOwnerIsRefused pins the owner
// recheck before a write. A chunk that waited in the queue past the
// recheck age must not land after a mesh takeover moved the run.
func TestOnLogChunkLongQueuedChunkOfALostOwnerIsRefused(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	release := make(chan struct{})
	base := h.logStore
	h.logStore = blockingAppendStore{Store: base, release: release}
	h.logOwnerRecheckAge = 20 * time.Millisecond

	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 0, Data: []byte("a0 "), CellInstance: "inst1"})
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 1, Data: []byte("a1 "), CellInstance: "inst1"})

	// The run moves while chunk 1 still waits behind the stalled store.
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
	time.Sleep(50 * time.Millisecond)
	close(release)

	acks := waitForLogAcks(t, sender, 2)
	if acks[0].Status != logAckStatusStored || acks[0].AckSeq != 0 {
		t.Fatalf("expected chunk 0 stored before the move, got %+v", acks[0])
	}
	if acks[1].Status != logAckStatusRejected || acks[1].Reason != logReasonForeignRun || acks[1].AckSeq != 1 {
		t.Fatalf("expected chunk 1 refused foreign-run after the move, got %+v", acks[1])
	}
	if got, want := readStoredLog(t, base, runID), "a0 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
}

// TestOnLogChunkMovedBuildAfterRecoveryContinuesAboveTheRecoveredParts
// pins a move after a crash inside the index flush. Parts 2 and 3 are
// readable with no index entry. Recovery writes one entry for each, so
// the moved build continues at 4, and the run is not a gap.
func TestOnLogChunkMovedBuildAfterRecoveryContinuesAboveTheRecoveredParts(t *testing.T) {
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	store := seedIndexedRunWithUnindexedParts(t, owner, runID, map[int64][]byte{2: []byte("a2 "), 3: []byte("a3 ")})

	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst2"})
	restartedSender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, owner, restartedSender, store)
	restarted.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 4, Data: []byte("b4 "), CellInstance: "inst2"})

	acks := waitForLogAcks(t, restartedSender, 1)
	if acks[0].Status != logAckStatusStored || acks[0].AckSeq != 4 {
		t.Fatalf("expected STORED ack_seq=4 for the moved build's chunk, got %+v", acks[0])
	}
	if incomplete, reason := restarted.runIncompleteness(runID); incomplete {
		t.Fatalf("run incomplete with %q, want a complete run", reason)
	}
	if got, want := readStoredLog(t, store, runID), "a0 a1 a2 a3 b4 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
	assertIndexInsideLog(t, store, runID)
}

// TestOnLogChunkSecondMoveReplacesAnEmptyPartOfTheFirstMove pins that an
// empty part of an earlier blank pass never reads as counted bytes. The
// run moves from inst1 to inst2, which fills the hole at 2, and then to
// inst3 before inst2 replaces part 3.
func TestOnLogChunkSecondMoveReplacesAnEmptyPartOfTheFirstMove(t *testing.T) {
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})
	h := newLogTestHandler(t, owner, sender)
	storeOldWriterPartsAboveAHole(t, h, sender, runID)

	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst2"})
	h.ownerCache.invalidate(runID)
	h.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{RunId: runID, Seq: 2, Data: []byte("b2 "), CellInstance: "inst2"})
	waitForLogAcks(t, sender, 4)

	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst3"})
	restartedSender := &recordingSender{}
	restarted := newLogTestHandlerOnStore(t, owner, restartedSender, h.logStore)
	for i, seq := range []int64{3, 4} {
		restarted.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
			RunId: runID, Seq: seq, Data: []byte(fmt.Sprintf("c%d ", seq)), CellInstance: "inst3",
		})
		acks := waitForLogAcks(t, restartedSender, i+1)
		if got := acks[i]; got.Status != logAckStatusStored || got.AckSeq != seq {
			t.Fatalf("chunk %d: expected STORED ack_seq=%d, got %+v", seq, seq, got)
		}
	}
	if got, want := readStoredLog(t, h.logStore, runID), "a0 a1 b2 c3 c4 "; got != want {
		t.Fatalf("stored log = %q, want %q", got, want)
	}
}

// --- gateway-budget pause under a small budget ---

// latencyAppendStore adds a fixed latency to every Append.
type latencyAppendStore struct {
	blob.Store
	latency time.Duration
}

func (s latencyAppendStore) Append(ctx context.Context, key string, seq int64, p []byte, opts blob.ObjectOptions) error {
	select {
	case <-time.After(s.latency):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.Store.Append(ctx, key, seq, p, opts)
}

// busyHonoringMite follows the mite loopback's BUSY rules: it forwards a
// chunk at once unless the run's pause window is open, records the
// sequence each BUSY ack refuses, extends the pause window, and re-sends
// only the refused sequences it still retains when the window closes.
type busyHonoringMite struct {
	sendMu  sync.Mutex
	toGW    chan *mitev1.LogChunk
	mu      sync.Mutex
	held    map[int64]*mitev1.LogChunk
	paused  time.Time
	refused map[int64]struct{}
	slot    *busyResendSlot
}

type busyResendSlot struct{ timer *time.Timer }

func newBusyHonoringMite() *busyHonoringMite {
	return &busyHonoringMite{
		toGW:    make(chan *mitev1.LogChunk, 64),
		held:    make(map[int64]*mitev1.LogChunk),
		refused: make(map[int64]struct{}),
	}
}

// post answers what POST /v1/log answers: false is a 503.
func (m *busyHonoringMite) post(c *mitev1.LogChunk) bool {
	m.mu.Lock()
	paused := time.Now().Before(m.paused)
	m.mu.Unlock()
	if paused {
		return false
	}
	m.sendMu.Lock()
	m.toGW <- c
	m.mu.Lock()
	m.held[c.Seq] = c
	m.mu.Unlock()
	m.sendMu.Unlock()
	return true
}

func (m *busyHonoringMite) onAck(a *mitev1.LogAck) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch a.Status {
	case logAckStatusStored, logAckStatusRejected:
		for seq := range m.held {
			if seq <= a.AckSeq {
				delete(m.held, seq)
			}
		}
		for seq := range m.refused {
			if seq <= a.AckSeq {
				delete(m.refused, seq)
			}
		}
	case logAckStatusBusy:
		m.refused[a.AckSeq] = struct{}{}
		until := time.Now().Add(time.Duration(a.PauseMs) * time.Millisecond)
		if m.paused.After(until) {
			until = m.paused
		}
		m.paused = until
		if m.slot != nil {
			m.slot.timer.Stop()
		}
		slot := &busyResendSlot{}
		slot.timer = time.AfterFunc(time.Until(until), func() { m.resend(slot) })
		m.slot = slot
	}
}

func (m *busyHonoringMite) resend(slot *busyResendSlot) {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	m.mu.Lock()
	if m.slot != slot {
		m.mu.Unlock()
		return
	}
	m.slot = nil
	seqs := make([]int64, 0, len(m.refused))
	for seq := range m.refused {
		seqs = append(seqs, seq)
	}
	m.refused = make(map[int64]struct{})
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	out := make([]*mitev1.LogChunk, 0, len(seqs))
	for _, seq := range seqs {
		if c, ok := m.held[seq]; ok {
			out = append(out, c)
		}
	}
	m.mu.Unlock()
	for _, c := range out {
		m.toGW <- c
	}
}

// pluginBackoff mirrors LogChunkSender.backoffMillis: a jittered wait
// from 250 ms up to a base that doubles per attempt, capped at 5 s.
func pluginBackoff(attempt int) time.Duration {
	base := 250 * time.Millisecond << min(attempt, 8)
	if base > 5*time.Second {
		base = 5 * time.Second
	}
	span := int64(base - 250*time.Millisecond)
	return 250*time.Millisecond + time.Duration(rand.Int63n(span+1))
}

// TestOnLogChunkSmallBudgetStoresEveryChunkOfAChattyBuild sends a chatty
// build through a 2048-byte budget. Each chunk is about one tenth of the
// budget, so each pause admits about ten chunks. A pause that does not
// track the drain leaves the drain idle for most of each pause, and the
// finalize deadline then cuts the log's tail.
func TestOnLogChunkSmallBudgetStoresEveryChunkOfAChattyBuild(t *testing.T) {
	const (
		chunks       = 120
		chunkBytes   = 205
		finalizeWait = 6 * time.Second
	)
	owner := newFakeOwnerRequester()
	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{
		Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1",
		Status: "COMPLETED", AnchorUnixMs: time.Now().UnixMilli(),
	})
	base, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	h := newLogTestHandlerOnStore(t, owner, &recordingSender{}, latencyAppendStore{base, time.Millisecond})
	h.logPendingMaxBytes = 2048
	h.logFinalizeWait = finalizeWait

	m := newBusyHonoringMite()
	acks := make(chan *mitev1.LogAck, 64)
	h.buildWorkers["ns1/cell1"] = &registeredBuildWorker{worker: &buildEventWorker{send: func(msg *mitev1.OperatorMessage) error {
		if a := msg.GetLogAck(); a != nil {
			acks <- a
		}
		return nil
	}}}
	done := make(chan struct{})
	defer close(done)
	go func() { // the gateway's stream read loop
		for {
			select {
			case c := <-m.toGW:
				h.OnLogChunk("cell1", "ns1", c)
			case <-done:
				return
			}
		}
	}()
	go func() { // the mite's dispatch loop
		for {
			select {
			case a := <-acks:
				m.onAck(a)
			case <-done:
				return
			}
		}
	}()
	go func() { // the plugin's per-run sender
		for seq := 0; seq < chunks; seq++ {
			c := &mitev1.LogChunk{
				RunId: runID, JobPath: "j", BuildNumber: 1, Seq: int64(seq), CellInstance: "inst1",
				Data: []byte(fmt.Sprintf("line %04d %0*d\n", seq, chunkBytes-11, 0)), Final: seq == chunks-1,
			}
			for attempt := 1; !m.post(c); attempt++ {
				select {
				case <-time.After(pluginBackoff(attempt)):
				case <-done:
					return
				}
			}
		}
	}()

	deadline := time.Now().Add(finalizeWait + time.Second)
	for !h.runFinalized(runID) {
		if time.Now().After(deadline) {
			h.logMu.Lock()
			st := h.logRuns[runID]
			h.logMu.Unlock()
			st.mu.Lock()
			defer st.mu.Unlock()
			t.Fatalf("stored through seq %d of %d before the finalize deadline (incomplete=%v reason=%q)",
				st.ackSeq, chunks-1, st.incomplete, st.incompleteReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if incomplete, reason := h.runIncompleteness(runID); incomplete {
		t.Fatalf("run finalized but incomplete: %s", reason)
	}
	if got := len(readStoredLog(t, h.logStore, runID)); got != chunks*chunkBytes {
		t.Fatalf("stored log holds %d bytes, want %d", got, chunks*chunkBytes)
	}
}

func TestGatewayBudgetPauseTracksDrainRate(t *testing.T) {
	now := time.Now()
	floorMS := int(gatewayBudgetPauseFloor / time.Millisecond)
	capMS := int(gatewayBudgetPauseCap / time.Millisecond)
	cases := []struct {
		name         string
		rate         float64
		lastFreedAgo time.Duration
		crossedAgo   time.Duration
		need         int64
		minMS, maxMS int
	}{
		{name: "fast drain answers the floor", rate: 1 << 20, lastFreedAgo: 10 * time.Millisecond, crossedAgo: time.Second, need: 2048, minMS: floorMS, maxMS: floorMS * 6 / 5},
		{name: "pause is the time to free need", rate: 20480, lastFreedAgo: 10 * time.Millisecond, crossedAgo: time.Second, need: 10240, minMS: 400, maxMS: 600},
		{name: "slow drain answers the cap", rate: 100, lastFreedAgo: 10 * time.Millisecond, crossedAgo: time.Second, need: 10240, minMS: capMS * 4 / 5, maxMS: capMS},
		{name: "no progress grows from the floor", rate: 20480, lastFreedAgo: 3 * time.Second, crossedAgo: 3 * time.Second, need: 2048, minMS: (floorMS + 3000) * 4 / 5, maxMS: (floorMS + 3000) * 6 / 5},
		{name: "a long stall answers the cap", rate: 20480, lastFreedAgo: 30 * time.Second, crossedAgo: 30 * time.Second, need: 2048, minMS: capMS * 4 / 5, maxMS: capMS},
		{name: "a fresh crossing after idle answers the floor", lastFreedAgo: time.Minute, crossedAgo: 0, need: 2048, minMS: floorMS, maxMS: floorMS * 6 / 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &BusHandler{metrics: noopGatewayMetrics()}
			h.logDrain = logDrainMeter{rate: tc.rate, lastFreed: now.Add(-tc.lastFreedAgo)}
			h.logBudgetCrossedAt = now.Add(-tc.crossedAgo)
			for i := 0; i < 50; i++ {
				got := h.gatewayBudgetPauseMSLocked(now, tc.need)
				if got < tc.minMS || got > tc.maxMS {
					t.Fatalf("pause = %d ms, want %d..%d ms", got, tc.minMS, tc.maxMS)
				}
			}
		})
	}
}

func TestGatewayBudgetPauseGrowsOnlyWithoutDrainProgress(t *testing.T) {
	now := time.Now()
	h := &BusHandler{metrics: noopGatewayMetrics()}
	// The budget has stayed crossed for five seconds, but the drain
	// freed bytes 10 ms ago: the pause stays at the drain's own pace.
	h.logBudgetCrossedAt = now.Add(-5 * time.Second)
	h.logDrain = logDrainMeter{rate: 1 << 20, lastFreed: now.Add(-10 * time.Millisecond)}
	if got, limit := h.gatewayBudgetPauseMSLocked(now, 2048), int(gatewayBudgetPauseFloor/time.Millisecond)*6/5; got > limit {
		t.Fatalf("pause = %d ms with the drain making progress, want at most %d ms", got, limit)
	}
}

func TestLogDrainMeterIgnoresIdleTime(t *testing.T) {
	var m logDrainMeter
	start := time.Now()
	// 2048 bytes over 300 ms: about 6827 bytes per second.
	for i := 0; i <= 6; i++ {
		m.freed(start.Add(time.Duration(i)*50*time.Millisecond), 2048/6)
	}
	first := m.rate
	if first < 5000 || first > 9000 {
		t.Fatalf("rate after a busy window = %.0f B/s, want about 6800", first)
	}
	// A ten-second idle gap, then the same pace again: the gap starts a
	// new window and does not lower the rate.
	idle := start.Add(10 * time.Second)
	for i := 0; i <= 6; i++ {
		m.freed(idle.Add(time.Duration(i)*50*time.Millisecond), 2048/6)
	}
	if m.rate < first/2 {
		t.Fatalf("rate after an idle gap = %.0f B/s, want near %.0f", m.rate, first)
	}
}

func TestWarnStalledDrainNamesARunWhoseQueueMakesNoProgress(t *testing.T) {
	var buf bytes.Buffer
	h := &BusHandler{metrics: noopGatewayMetrics(), Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	now := time.Now()
	st := &runLogState{queue: []queuedLogChunk{{chunk: &mitev1.LogChunk{Seq: 4}, admitted: now.Add(-2 * logHeartbeatInterval)}}}

	st.lastDequeue = now.Add(-time.Second)
	h.warnStalledDrainLocked("run-1", st, now)
	if buf.Len() != 0 {
		t.Fatalf("warned for a drain that dequeued a second ago: %s", buf.String())
	}

	st.lastDequeue = time.Time{}
	h.warnStalledDrainLocked("run-1", st, now)
	if !strings.Contains(buf.String(), "log run drain made no progress") || !strings.Contains(buf.String(), "run_id=run-1") {
		t.Fatalf("no stalled-drain warning, got %q", buf.String())
	}
	warned := buf.Len()
	h.warnStalledDrainLocked("run-1", st, now.Add(time.Second))
	if buf.Len() != warned {
		t.Fatalf("warned twice inside one heartbeat interval: %s", buf.String())
	}
}
