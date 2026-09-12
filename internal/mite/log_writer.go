package mite

import (
	"context"
	"encoding/json"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// Bounds, backoffs, and the writer heartbeat interval (design.md D4).
const (
	perRunPendingMaxChunks = 2000
	perRunPendingMaxBytes  = 64 * 1024 * 1024 // 64 MiB

	defaultLogPendingMaxBytes = 2147483648 // 2 GiB, VARROA_GATEWAY_LOG_PENDING_BYTES
	gatewayBudgetPauseMinMS   = 1000
	gatewayBudgetPauseMaxMS   = 8000

	logAppendBackoffFloor = 250 * time.Millisecond
	logAppendBackoffCap   = 2000 * time.Millisecond

	logHeartbeatInterval = 30 * time.Second

	defaultLogFinalizeWait = 60 * time.Second // VARROA_RECORDSTORE_LOG_FINALIZE_WAIT
)

// LogAck status and reason tokens (design.md D3/D4).
const (
	logAckStatusStored   = "STORED"
	logAckStatusBusy     = "BUSY"
	logAckStatusRejected = "REJECTED"

	logReasonEmptyRunID       = "empty-run-id"
	logReasonForeignRun       = "foreign-run"
	logReasonFinalizeDeadline = "finalize-deadline"
	logReasonRunPendingFull   = "run-pending-full"
	logReasonGatewayBudgetFul = "gateway-budget-full"
	logReasonOwnerLookupFail  = "owner-lookup-failed"
	logReasonGatewayGap       = "gateway-gap"
)

var (
	logChunksRejected metric.Int64Counter
	logAppendRetries  metric.Int64Counter
)

func init() {
	meter := otel.Meter("varroa-gateway")
	logChunksRejected, _ = meter.Int64Counter("varroa.gateway.log.chunk.rejected",
		metric.WithDescription("Console-log chunks the gateway rejected (foreign run, empty run id, or the finalize deadline)"),
	)
	logAppendRetries, _ = meter.Int64Counter("varroa.gateway.log.append.retries",
		metric.WithDescription("Blob store append retries for console-log chunks and index entries"),
	)
}

// runLogState is the gateway's per-run blob-writer state: the append
// pipeline's write progress, its pending queue, and the run's
// incompleteness and finalization knowledge. One instance lives for as
// long as the run has pending or unacknowledged work.
type runLogState struct {
	ns, name string

	// jobPath is the run's job path, recorded from the first call that
	// creates this state and never changed after. publishRunLogBestEffort
	// resolves the run's tenant from it, so a best-effort envelope stamps
	// the same tenant the ordered event group resolves through
	// CellTenantResolver, never the cell's own synthetic tenant.
	jobPath string

	mu          sync.Mutex
	established bool // restart recovery has run for this run
	queue       []*mitev1.LogChunk
	queueBytes  int64
	draining    bool

	offset int64 // running byte count appended to logs/<run_id>

	// ackSeq is the highest sequence with no hole below it: what
	// LogAck{STORED} reports (design.md D4). -1 = nothing durable yet.
	// pendingEntries holds the full index entry for a sequence whose log
	// bytes are already durable but whose position in the readable
	// object is not yet known, because the hole below it is still open.
	// C1's range reader concatenates parts in ascending sequence order
	// and stops at the first missing one, so an index entry's offset is
	// only ever correct once every lower sequence is durable too. The
	// entry's Offset field is left unset while it sits here; a later
	// chunk that fills the hole computes it and writes the entry, in
	// sequence order, for as much of pendingEntries as has become
	// contiguous.
	ackSeq         int64
	pendingEntries map[int64]LogIndexEntry

	// pendingData holds the raw console bytes for a sequence whose log
	// part is durable but whose index entry has not yet flushed. It
	// mirrors pendingEntries and clears with it. flushContiguousLogIndex
	// reads a sequence's bytes from here at the moment its index entry
	// becomes durable, to publish the live-tail relay message for it.
	pendingData map[int64][]byte

	incomplete       bool
	incompleteReason string

	finalDurable    bool // the logindex 'final' entry is durable
	rejectedForever bool // the finalize deadline passed

	terminal   bool
	deadlineAt time.Time

	heartbeatCancel context.CancelFunc

	// appendCtx ends at the run's finalize deadline, once one is known:
	// markRunTerminal schedules appendCancel to fire at deadlineAt. Every
	// Store.Append call for this run carries appendCtx, so a call already
	// blocked when the deadline is set still unblocks at that moment
	// (design.md D4: "The heartbeat and the retry both end at that
	// deadline"). Before a deadline is known, it never fires.
	appendCtx    context.Context
	appendCancel context.CancelFunc
}

// hasHeartbeatWorkLocked reports whether the run still holds pending or
// unacknowledged work, the condition that keeps the writer heartbeat
// running (design.md D4). Caller must hold st.mu.
func (st *runLogState) hasHeartbeatWorkLocked() bool {
	return len(st.queue) > 0 || (!st.finalDurable && !st.rejectedForever)
}

// SetLogStore wires the blob store the log writer appends to, the
// gateway-wide pending-bytes budget, the finalize-wait duration the
// writer heartbeat and the abandon deadline both read, and the run-owner
// cache's bound. Called once from cmd/gateway/main.go when the record
// store is enabled; a nil store (the default) makes OnLogChunk a no-op.
func (h *BusHandler) SetLogStore(store blob.Store, pendingMaxBytes int64, finalizeWait time.Duration, ownerCacheSize int) {
	h.logStore = store
	if pendingMaxBytes <= 0 {
		pendingMaxBytes = defaultLogPendingMaxBytes
	}
	h.logPendingMaxBytes = pendingMaxBytes
	if finalizeWait <= 0 {
		finalizeWait = defaultLogFinalizeWait
	}
	h.logFinalizeWait = finalizeWait
	h.ownerCache = newOwnerCache(ownerCacheSize)
}

// currentSender returns the send function of the controller's current
// connection, or nil when none is connected. OnLogChunk carries no
// Sender of its own (D4's literal signature), so the log writer looks up
// the same per-connection sender the build-event worker holds.
func (h *BusHandler) currentSender(ns, name string) Sender {
	h.buildMu.Lock()
	defer h.buildMu.Unlock()
	rw, ok := h.buildWorkers[ns+"/"+name]
	if !ok {
		return nil
	}
	return rw.worker.send
}

// sendLogAck sends one OperatorMessage.log_ack to the controller's
// current connection. It is a no-op when no connection is current: the
// mite already retains the chunk and will re-send after it reconnects.
func (h *BusHandler) sendLogAck(ns, name, runID string, ackSeq int64, status, reason string, pauseMS int) {
	send := h.currentSender(ns, name)
	if send == nil {
		return
	}
	msg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_LogAck{LogAck: &mitev1.LogAck{
			RunId: runID, AckSeq: ackSeq, Status: status, Reason: reason, PauseMs: pauseMS,
		}},
	}
	if err := send(msg); err != nil {
		h.log().Error("send log ack failed", "controller", ns+"/"+name, "run_id", runID, "status", status, "error", err)
	}
}

// getOrCreateRunState returns the run's state, creating it on first
// touch. ns, name, and jobPath are recorded from the first call, which
// is always the sender the ownership check has already admitted.
func (h *BusHandler) getOrCreateRunState(runID, ns, name, jobPath string) *runLogState {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if h.logRuns == nil {
		h.logRuns = make(map[string]*runLogState)
	}
	st, ok := h.logRuns[runID]
	if !ok {
		appendCtx, appendCancel := context.WithCancel(context.Background())
		st = &runLogState{ns: ns, name: name, jobPath: jobPath, ackSeq: -1, appendCtx: appendCtx, appendCancel: appendCancel}
		h.logRuns[runID] = st
	}
	return st
}

// runFinalizeWait returns the configured finalize-wait duration, or the
// default when the log writer was never wired (SetLogStore not called).
func (h *BusHandler) runFinalizeWait() time.Duration {
	if h.logFinalizeWait <= 0 {
		return defaultLogFinalizeWait
	}
	return h.logFinalizeWait
}

// OnLogChunk implements StreamHandler for a console-log chunk a mite
// forwards. It checks run ownership, applies the per-run and
// gateway-wide pending bounds, and hands an admitted chunk to the run's
// append pipeline. It never writes on this call: the pipeline's drain
// goroutine owns every blob-store call, so a slow store never stalls the
// CommandStream read loop.
func (h *BusHandler) OnLogChunk(name, ns string, chunk *mitev1.LogChunk) {
	if h.logStore == nil {
		return
	}
	ctx := context.Background()

	if chunk.RunId == "" {
		logChunksRejected.Add(ctx, 1)
		h.log().Warn("dropping log chunk with empty run_id", "controller", ns+"/"+name)
		h.sendLogAck(ns, name, "", 0, logAckStatusRejected, logReasonEmptyRunID, 0)
		return
	}

	st := h.getOrCreateRunState(chunk.RunId, ns, name, chunk.JobPath)

	st.mu.Lock()
	rejected := st.rejectedForever
	st.mu.Unlock()
	if rejected {
		h.sendLogAck(ns, name, chunk.RunId, chunk.Seq, logAckStatusRejected, logReasonFinalizeDeadline, 0)
		return
	}

	decision, tuple := h.checkOwner(ctx, h.cluster, ns, name, chunk.CellInstance, chunk.RunId)
	switch decision {
	case ownerForeign:
		logChunksRejected.Add(ctx, 1)
		h.sendLogAck(ns, name, chunk.RunId, chunk.Seq, logAckStatusRejected, logReasonForeignRun, 0)
		return
	case ownerUnknown:
		h.sendLogAck(ns, name, chunk.RunId, chunk.Seq, logAckStatusBusy, logReasonOwnerLookupFail, gatewayBudgetPauseMinMS)
		return
	}

	if tuple.status != "" && isTerminalRunStatus(tuple.status) && tuple.anchorUnixMs > 0 {
		h.markRunTerminal(ns, name, chunk.RunId, chunk.JobPath, time.UnixMilli(tuple.anchorUnixMs))
	}

	size := int64(len(chunk.Data))

	// The per-run bound, the gateway-wide budget, and the enqueue that
	// spends them all happen under one critical section (h.logMu
	// outermost, then st.mu), so two concurrent chunks can never both
	// pass a check that only one slot remains for.
	h.logMu.Lock()
	st.mu.Lock()
	overRun := len(st.queue)+1 > perRunPendingMaxChunks || st.queueBytes+size > perRunPendingMaxBytes
	overBudget := h.logTotalPendingBytes+size > h.logPendingMaxBytesOrDefault()
	if overBudget {
		if h.logBudgetCrossedAt.IsZero() {
			h.logBudgetCrossedAt = time.Now()
		}
	} else {
		h.logBudgetCrossedAt = time.Time{}
	}
	pauseMS := h.gatewayBudgetPauseMSLocked()

	if overRun || overBudget {
		st.mu.Unlock()
		h.logMu.Unlock()
		if overRun {
			h.sendLogAck(ns, name, chunk.RunId, chunk.Seq, logAckStatusBusy, logReasonRunPendingFull, gatewayBudgetPauseMinMS)
		} else {
			h.sendLogAck(ns, name, chunk.RunId, chunk.Seq, logAckStatusBusy, logReasonGatewayBudgetFul, pauseMS)
		}
		return
	}

	st.queue = append(st.queue, chunk)
	st.queueBytes += size
	draining := st.draining
	st.draining = true
	h.logTotalPendingBytes += size
	st.mu.Unlock()
	h.logMu.Unlock()

	if !draining {
		go h.drainRun(st.appendCtx, chunk.RunId, ns, name, st)
	}
}

// logPendingMaxBytesOrDefault returns the configured gateway-wide budget,
// or the default when the log writer was never wired.
func (h *BusHandler) logPendingMaxBytesOrDefault() int64 {
	if h.logPendingMaxBytes <= 0 {
		return defaultLogPendingMaxBytes
	}
	return h.logPendingMaxBytes
}

// gatewayBudgetPauseMSLocked computes the BUSY pause_ms for the
// gateway-wide budget: 1000 ms at first, growing to 8000 ms while the
// budget stays crossed. Caller must hold h.logMu.
func (h *BusHandler) gatewayBudgetPauseMSLocked() int {
	if h.logBudgetCrossedAt.IsZero() {
		return gatewayBudgetPauseMinMS
	}
	ms := gatewayBudgetPauseMinMS + int(time.Since(h.logBudgetCrossedAt)/time.Second)*1000
	if ms > gatewayBudgetPauseMaxMS {
		ms = gatewayBudgetPauseMaxMS
	}
	return ms
}

// isTerminalRunStatus reports whether status is one of the record
// store's terminal run statuses. The literal values mirror
// internal/recordstore.StatusCompleted and StatusDuplicate; the gateway
// takes no dependency on that package for two string constants.
func isTerminalRunStatus(status string) bool {
	return status == "COMPLETED" || status == "DUPLICATE"
}

// markRunTerminal records that runID's status is terminal as of anchor,
// schedules appendCtx to end at the resulting deadline, and starts the
// writer heartbeat while the run still holds pending or unacknowledged
// work. The first anchor observed wins: a later call for the same run
// changes nothing.
//
// The heartbeat starts before the deadline is scheduled, never after: an
// anchor already past its deadline resolves inline below, through
// runDeadlineFired, which can abandon the run and cancel the heartbeat in
// the same call. Starting the heartbeat first means that cancellation
// always finds one to stop, instead of racing a later, unconditional
// start into overwriting an abandoned run's cleared state.
func (h *BusHandler) markRunTerminal(ns, name, runID, jobPath string, anchor time.Time) {
	st := h.getOrCreateRunState(runID, ns, name, jobPath)
	st.mu.Lock()
	if st.terminal {
		st.mu.Unlock()
		return
	}
	st.terminal = true
	st.deadlineAt = anchor.Add(h.runFinalizeWait())
	deadline := st.deadlineAt
	cancel := st.appendCancel
	st.mu.Unlock()

	h.startHeartbeat(ns, name, runID, st)

	if cancel != nil {
		if d := time.Until(deadline); d <= 0 {
			h.runDeadlineFired(ns, name, runID, st, cancel)
		} else {
			time.AfterFunc(d, func() { h.runDeadlineFired(ns, name, runID, st, cancel) })
		}
	}
}

// runDeadlineFired runs once the run's finalize deadline is due: inline
// from markRunTerminal when the anchor already put the deadline in the
// past, or from the timer markRunTerminal schedules otherwise. It cancels
// appendCtx, which unblocks any append or retry already in flight
// (design.md D4, "The heartbeat and the retry both end at that
// deadline"), and abandons the run directly when no drain goroutine is
// running for it.
//
// A running drain goroutine reaches the same deadline on its own next
// store call and abandons the run itself
// (drainRun -> appendChunk -> appendDeadlineHit); calling abandonRun here
// too would race that goroutine's queue mutation, so this path defers to
// it. The gap this call closes is the run that goes idle with a pending
// entry or an open hole and never gets another chunk: nothing else would
// ever call abandonRun for it, so the heartbeat would run forever and the
// stranded sequence would never be released or reported.
func (h *BusHandler) runDeadlineFired(ns, name, runID string, st *runLogState, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}

	st.mu.Lock()
	idle := !st.draining
	needsAbandon := !st.rejectedForever && (len(st.pendingEntries) > 0 || !st.finalDurable)
	st.mu.Unlock()

	if idle && needsAbandon {
		h.abandonRun(ns, name, runID, st)
	}
}

// startHeartbeat starts the writer-heartbeat goroutine for a run, unless
// one is already running.
func (h *BusHandler) startHeartbeat(ns, name, runID string, st *runLogState) {
	st.mu.Lock()
	if st.heartbeatCancel != nil {
		st.mu.Unlock()
		return
	}
	hbCtx, cancel := context.WithCancel(context.Background())
	st.heartbeatCancel = cancel
	st.mu.Unlock()
	go h.runHeartbeat(hbCtx, ns, name, runID, st)
}

// runHeartbeat publishes run.log every 30 seconds with
// writer_heartbeat_unix_ms while the run holds pending or unacknowledged
// work, and stops itself once no such work remains.
func (h *BusHandler) runHeartbeat(ctx context.Context, ns, name, runID string, st *runLogState) {
	ticker := time.NewTicker(logHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		st.mu.Lock()
		hasWork := st.hasHeartbeatWorkLocked()
		st.mu.Unlock()
		if !hasWork {
			st.mu.Lock()
			st.heartbeatCancel = nil
			st.mu.Unlock()
			return
		}
		h.publishRunLogBestEffort(ns, name, runID, st.jobPath, map[string]interface{}{
			"writer_heartbeat_unix_ms": time.Now().UnixMilli(),
		})
	}
}

// publishRunLogBestEffort publishes one run.log envelope with a single
// attempt: a failure is logged and never retried here. The 30-second
// heartbeat cadence, and the recorder's own finalize sweep, are the
// backstops for a lost publish — the sweep marks a stalled run
// not-finalized rather than leaving it unmarked forever.
//
// tenant is resolved the same way the ordered event group resolves it
// (buildEnvelope, through CellTenantResolver), not stamped as the
// cell's own synthetic tenant: applyRunLog's ON CONFLICT insert only
// sets runs.tenant when this envelope is the first to create the row,
// and never updates it later, so a wrong tenant here outlives every
// later run.started envelope for the same run.
func (h *BusHandler) publishRunLogBestEffort(ns, name, runID, jobPath string, payload map[string]interface{}) {
	if h.publisher == nil {
		return
	}
	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, jobPath)
	if tenant == "" {
		tenant = ns + "/" + name
	}
	env := ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kindRunLog,
		RunID:         runID,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		Tenant:        tenant,
		OccurredAt:    time.Now().UTC(),
		Payload:       payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		h.log().Error("marshal run.log envelope failed", "run_id", runID, "error", err)
		return
	}
	subject := bus.BuildSubject(h.cluster, ns, name)
	if _, err := h.publisher.PublishJetStream(subject, data); err != nil {
		h.log().Warn("run.log publish failed", "run_id", runID, "error", err)
	}
}

// runIncompleteness returns the gateway's own incompleteness knowledge
// for runID: set on a rejected chunk and on a sequence gap.
func (h *BusHandler) runIncompleteness(runID string) (bool, string) {
	h.logMu.Lock()
	st, ok := h.logRuns[runID]
	h.logMu.Unlock()
	if !ok {
		return false, ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.incomplete, st.incompleteReason
}

// runFinalized reports whether the gateway has durably written the
// logindex 'final' entry for runID.
func (h *BusHandler) runFinalized(runID string) bool {
	h.logMu.Lock()
	st, ok := h.logRuns[runID]
	h.logMu.Unlock()
	if !ok {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.finalDurable
}

// deadlinePassed reports whether st's finalize deadline is set and has
// already passed.
func deadlinePassed(st *runLogState) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.terminal && !st.deadlineAt.IsZero() && time.Now().After(st.deadlineAt)
}

// appendOutcome is what appendChunk resolved to.
type appendOutcome int

const (
	appendDone appendOutcome = iota
	appendDeadlineHit
)

// drainRun is the run's single append-pipeline goroutine: one append in
// flight per run, chunks processed strictly in queue order. It exits
// once the queue empties, and restarts (from OnLogChunk) on the next
// arrival.
func (h *BusHandler) drainRun(ctx context.Context, runID, ns, name string, st *runLogState) {
	for {
		st.mu.Lock()
		if len(st.queue) == 0 {
			st.draining = false
			st.mu.Unlock()
			return
		}
		chunk := st.queue[0]
		st.mu.Unlock()

		if h.appendChunk(ctx, runID, ns, name, st, chunk) == appendDeadlineHit {
			h.abandonRun(ns, name, runID, st)
			return
		}

		size := int64(len(chunk.Data))
		st.mu.Lock()
		if st.rejectedForever {
			// runDeadlineFired's idle path ran abandonRun concurrently,
			// between this chunk's successful append and this dequeue:
			// it already cleared st.queue and charged its bytes back.
			// Nothing is left here to dequeue or charge again.
			st.mu.Unlock()
			return
		}
		st.queue = st.queue[1:]
		st.queueBytes -= size
		st.mu.Unlock()
		h.logMu.Lock()
		h.logTotalPendingBytes -= size
		h.logMu.Unlock()
	}
}

// appendChunk durably appends one chunk's log part, then holds its
// index entry pending until it can carry a truthful offset, acknowledges
// the chunk, and flushes whatever prefix of pendingEntries the chunk
// makes contiguous. It recovers the run's state from the store on first
// touch.
func (h *BusHandler) appendChunk(ctx context.Context, runID, ns, name string, st *runLogState, chunk *mitev1.LogChunk) appendOutcome {
	st.mu.Lock()
	established := st.established
	st.mu.Unlock()
	if !established {
		for {
			err := h.recoverRunState(ctx, runID, st)
			if err == nil {
				break
			}
			h.log().Warn("log run recovery failed, retrying", "run_id", runID, "error", err)
			if deadlinePassed(st) {
				return appendDeadlineHit
			}
			time.Sleep(logAppendBackoffFloor)
		}
		st.mu.Lock()
		st.established = true
		st.mu.Unlock()
	}

	st.mu.Lock()
	ackSeq := st.ackSeq
	_, pending := st.pendingEntries[chunk.Seq]
	alreadyDurable := chunk.Seq <= ackSeq || pending
	fillsHole := chunk.Seq == ackSeq+1
	wasFinalDurable := st.finalDurable
	st.mu.Unlock()

	if alreadyDurable {
		// Already durable: a redelivery after a lost acknowledgement, or
		// a repeat of a chunk this run already wrote out of order.
		// Nothing to append, but the mite still needs the release.
		h.ackStored(ns, name, runID, st)
		return appendDone
	}

	createdAt := runCreatedAtFromUUIDv7(runID)
	logOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: createdAt}
	if !h.retryAppend(ctx, runID, st, blob.LogKey(runID), chunk.Seq, chunk.Data, logOpts) {
		return appendDeadlineHit
	}

	// The log bytes are durable at their own sequence regardless of
	// arrival order — Store.Append is keyed by (key, seq) and does not
	// care what order it is called in. The index entry is different: it
	// carries an offset into the range-read view of logs/<run_id>, which
	// only exists once every lower sequence is durable too. So the entry
	// waits here until flushContiguousLogIndex below can give it a
	// truthful offset.
	entry := LogIndexEntry{
		Seq: chunk.Seq, NodeID: chunk.NodeId, StageID: chunk.StageId, TsMs: chunk.FirstLineUnixMs,
		Len: int64(len(chunk.Data)), Dropped: chunk.DroppedBytes, Final: chunk.Final,
	}
	st.mu.Lock()
	if st.pendingEntries == nil {
		st.pendingEntries = make(map[int64]LogIndexEntry)
	}
	st.pendingEntries[chunk.Seq] = entry
	if st.pendingData == nil {
		st.pendingData = make(map[int64][]byte)
	}
	st.pendingData[chunk.Seq] = chunk.Data
	st.mu.Unlock()

	var finalWritten bool
	if fillsHole {
		fw, outcome := h.flushContiguousLogIndex(ctx, ns, name, runID, st, createdAt)
		if outcome == appendDeadlineHit {
			return appendDeadlineHit
		}
		finalWritten = fw
	}

	h.ackStored(ns, name, runID, st)

	if wasFinalDurable {
		// This run was already finalized; a late chunk still landed.
		// C1's merge keeps a stored finalized true, so this only ever
		// widens incomplete, never narrows finalized.
		h.publishRunLogBestEffort(ns, name, runID, st.jobPath, map[string]interface{}{"incomplete": true})
	}
	if finalWritten {
		h.publishRunLogBestEffort(ns, name, runID, st.jobPath, map[string]interface{}{"finalized": true})
		// The build has ended: clear any live tail lease. No control
		// message is pushed — there is nothing left to tail.
		h.clearTailLeaseOnFinal(chunk.RunId)
	}
	return appendDone
}

// flushContiguousLogIndex writes the index entry for every sequence
// pendingEntries now covers contiguously above ackSeq, one at a time in
// ascending order, computing each entry's offset from the running total
// this loop itself advances. It stops at the first sequence not yet in
// pendingEntries, leaving it pending until a later chunk fills the
// remaining hole. finalWritten reports whether any entry this call wrote
// carried Final true.
//
// Each entry it writes is durable at that point — both its log part
// (appended when the chunk arrived) and its index entry (just appended
// here). Only then does it publish the run's live-tail relay message for
// that sequence, from st.pendingData, so a viewer that misses the
// message and re-reads the blob store never sees less than it saw live
// (log-live-tail, "the relay never leads the blob"). An out-of-order
// chunk's own entry, and its relay message, both wait here until the
// hole below it closes.
func (h *BusHandler) flushContiguousLogIndex(ctx context.Context, ns, name, runID string, st *runLogState, createdAt time.Time) (finalWritten bool, outcome appendOutcome) {
	for {
		st.mu.Lock()
		next := st.ackSeq + 1
		e, ok := st.pendingEntries[next]
		if ok {
			e.Offset = st.offset
		}
		st.mu.Unlock()
		if !ok {
			return finalWritten, appendDone
		}

		line, err := json.Marshal(e)
		if err != nil {
			h.log().Error("marshal log index entry failed", "run_id", runID, "seq", e.Seq, "error", err)
			return finalWritten, appendDeadlineHit
		}
		line = append(line, '\n')
		indexOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "application/x-ndjson", CreatedAt: createdAt}
		if !h.retryAppend(ctx, runID, st, blob.LogIndexKey(runID), e.Seq, line, indexOpts) {
			return finalWritten, appendDeadlineHit
		}

		st.mu.Lock()
		st.offset += e.Len
		st.ackSeq = e.Seq
		delete(st.pendingEntries, e.Seq)
		data := st.pendingData[e.Seq]
		delete(st.pendingData, e.Seq)
		if e.Final {
			st.finalDurable = true
			finalWritten = true
		}
		st.mu.Unlock()

		h.publishLiveLogChunk(ns, name, runID, e, data)
	}
}

// ackStored sends LogAck{STORED, ack_seq: st.ackSeq}: the cumulative
// release point, naming the highest sequence stored with no hole below
// it (design.md D4).
func (h *BusHandler) ackStored(ns, name, runID string, st *runLogState) {
	st.mu.Lock()
	ackSeq := st.ackSeq
	st.mu.Unlock()
	if ackSeq < 0 {
		return
	}
	h.sendLogAck(ns, name, runID, ackSeq, logAckStatusStored, "", 0)
}

// retryAppend retries store.Append with a 250ms-to-2000ms jittered
// backoff until it succeeds or the run's finalize deadline passes. It
// never substitutes different bytes at seq: every retry sends the exact
// same data, so an ambiguous timeout after a durable write resolves on
// the next attempt instead of returning ErrChunkConflict.
//
// ctx is st.appendCtx: it ends at the run's finalize deadline (design.md
// D4, "The heartbeat and the retry both end at that deadline"), so a
// call already blocked inside Store.Append when the deadline arrives
// still returns instead of holding the pending queue open forever.
func (h *BusHandler) retryAppend(ctx context.Context, runID string, st *runLogState, key string, seq int64, data []byte, opts blob.ObjectOptions) bool {
	delay := logAppendBackoffFloor
	for {
		if deadlinePassed(st) {
			return false
		}
		err := h.logStore.Append(ctx, key, seq, data, opts)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			// The deadline ended appendCtx while this call was in
			// flight: the append is abandoned, not retried.
			return false
		}
		logAppendRetries.Add(ctx, 1)
		h.log().Warn("log append retry", "run_id", runID, "key", key, "seq", seq, "error", err)
		if deadlinePassed(st) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(jitteredDelay(delay)):
		}
		delay *= 2
		if delay > logAppendBackoffCap {
			delay = logAppendBackoffCap
		}
	}
}

// jitteredDelay applies up to +/-20% jitter to d, floored at
// logAppendBackoffFloor and capped at logAppendBackoffCap.
func jitteredDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return logAppendBackoffFloor
	}
	spread := int64(d) / 5 // 20%
	jitter := time.Duration(0)
	if spread > 0 {
		jitter = time.Duration(rand.Int63n(2*spread+1) - spread)
	}
	out := d + jitter
	if out < logAppendBackoffFloor {
		out = logAppendBackoffFloor
	}
	if out > logAppendBackoffCap {
		out = logAppendBackoffCap
	}
	return out
}

// recoverRunState rebuilds a run's write progress from the store on the
// first chunk the gateway handles for it. When the log holds more bytes
// than the index covers — a crash between the log append and the index
// append — it writes one recovered entry for the missing one before
// anything else appends, so the index never carries a permanent gap for
// a shortfall the writer can still repair.
//
// It also restores the run's final-marker state from the index's own
// durable evidence: a restarted gateway that reads back an entry whose
// final field is true needs no memory to know the run is already
// finished (design.md D4, "A restarted gateway reads the entry back and
// needs no memory"). Without this, a redelivered final chunk after a
// restart would start the writer heartbeat over an already-finished run
// and leave the recorder's finalize sweep free to call it not-finalized
// despite the durable evidence sitting right there in the index.
func (h *BusHandler) recoverRunState(ctx context.Context, runID string, st *runLogState) error {
	bytesRead, err := logObjectLength(ctx, h.logStore, blob.LogKey(runID))
	if err != nil {
		return err
	}
	entries, maxSeq, finalSeen, err := readLogIndex(ctx, h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		return err
	}

	var indexCovered int64
	highestWritten := int64(-1)
	if maxSeq >= 0 {
		last := entries[maxSeq]
		indexCovered = last.Offset + last.Len
		highestWritten = maxSeq
	}

	if bytesRead > indexCovered {
		repairSeq := highestWritten + 1
		entry := LogIndexEntry{Seq: repairSeq, Offset: indexCovered, Len: bytesRead - indexCovered, Recovered: true}
		line, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		createdAt := runCreatedAtFromUUIDv7(runID)
		opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "application/x-ndjson", CreatedAt: createdAt}
		if err := h.logStore.Append(ctx, blob.LogIndexKey(runID), repairSeq, line, opts); err != nil {
			return err
		}
		entries[repairSeq] = entry
		highestWritten = repairSeq
	}

	// ackSeq is the highest sequence with no hole below it: the
	// contiguous run of index entries starting at 0. The index never
	// carries an entry above that prefix — an out-of-order chunk's entry
	// waits in st.pendingEntries, in memory only, until every lower
	// sequence is durable — so a gateway restart loses no index entry
	// this scan would otherwise have to skip over; it simply starts
	// pendingEntries empty. The mite still holds any sequence above
	// ackSeq, because such a sequence was never covered by a STORED
	// ack_seq, and re-sends it after reconnecting: the same arrival that
	// repopulates pendingEntries in the first place.
	ackSeq := int64(-1)
	for seq := int64(0); seq <= highestWritten; seq++ {
		if _, ok := entries[seq]; !ok {
			break
		}
		ackSeq = seq
	}

	st.mu.Lock()
	st.offset = bytesRead
	st.ackSeq = ackSeq
	st.pendingEntries = nil
	st.pendingData = nil
	st.finalDurable = finalSeen
	st.mu.Unlock()
	return nil
}

// abandonRun runs once at the finalize deadline: it drops the run's
// pending queue, releases every sequence whose log bytes are durable but
// whose index entry never got to flush because the hole below it never
// filled, answers REJECTED{finalize-deadline} for all of them, stops the
// heartbeat, and publishes the incomplete marker. st.rejectedForever
// then makes OnLogChunk answer the same REJECTED to every later arrival
// for this run, including one that was answered BUSY and never entered
// the pending queue.
//
// It is idempotent, and callers rely on that: the drain goroutine calls
// it when its own append hits the deadline, and runDeadlineFired's idle
// path can call it for the same run at nearly the same moment. The
// rejectedForever check and the flag it guards sit in the same critical
// section, so whichever caller acquires st.mu first does the one real
// cleanup and every other caller finds it already done and returns
// without acting a second time.
//
// A stranded pendingEntries sequence gets no index entry of its own,
// here or ever: an absent entry is already what the manifest treats as
// missing (design.md D4's hole rule reads an index miss the same as a
// hole), so writing one with a synthetic offset would claim a byte
// position through the gap a reader can never actually reach. Leaving it
// unwritten is the smaller, and the only truthful, shape.
func (h *BusHandler) abandonRun(ns, name, runID string, st *runLogState) {
	st.mu.Lock()
	if st.rejectedForever {
		st.mu.Unlock()
		return
	}
	pending := st.queue
	st.queue = nil
	freedBytes := st.queueBytes
	st.queueBytes = 0
	stranded := make([]int64, 0, len(st.pendingEntries))
	for seq := range st.pendingEntries {
		stranded = append(stranded, seq)
	}
	st.pendingEntries = nil
	st.pendingData = nil
	st.rejectedForever = true
	st.draining = false
	hbCancel := st.heartbeatCancel
	st.heartbeatCancel = nil
	appendCancel := st.appendCancel
	// A gap this run will now never fill is a more precise cause than a
	// slow store: reasonGap wins when both are true of one run.
	reason := logReasonFinalizeDeadline
	if len(stranded) > 0 {
		reason = logReasonGatewayGap
	}
	st.incomplete = true
	st.incompleteReason = reason
	st.mu.Unlock()

	if hbCancel != nil {
		hbCancel()
	}
	if appendCancel != nil {
		appendCancel()
	}

	h.logMu.Lock()
	h.logTotalPendingBytes -= freedBytes
	h.logMu.Unlock()

	for _, chunk := range pending {
		h.sendLogAck(ns, name, runID, chunk.Seq, logAckStatusRejected, logReasonFinalizeDeadline, 0)
	}
	// The mite still retains a stranded sequence: it was never covered
	// by a STORED ack_seq, so LogAck's own reason enumeration applies —
	// finalize-deadline is the release token regardless of the more
	// precise gateway-gap cause the summary above records.
	for _, seq := range stranded {
		h.sendLogAck(ns, name, runID, seq, logAckStatusRejected, logReasonFinalizeDeadline, 0)
	}

	h.publishRunLogBestEffort(ns, name, runID, st.jobPath, map[string]interface{}{
		"incomplete": true, "incomplete_reason": reason,
	})
}

// runCreatedAtFromUUIDv7 decodes the Unix-millisecond timestamp inside a
// UUIDv7 run id, per design.md D4: the value is identical for every part
// of both keys by construction, survives a gateway restart, and needs no
// request to the recorder. A run id that fails to parse as a UUIDv7
// falls back to the current time — the chunk is still written; only its
// partition placement may differ from the recorder's.
func runCreatedAtFromUUIDv7(runID string) time.Time {
	id, err := uuid.Parse(runID)
	if err != nil || id.Version() != 7 {
		return time.Now().UTC()
	}
	sec, nsec := id.Time().UnixTime()
	return time.Unix(sec, nsec).UTC()
}
