package mite

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
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

	// logBusyPauseMS is the BUSY pause for the per-run bound and for an
	// owner lookup that failed.
	logBusyPauseMS = 1000

	// The gateway-budget BUSY pause is the time the measured drain rate
	// needs to free the refused chunk's bytes, within this floor and
	// cap. A pause longer than that time leaves the drain idle while
	// every refused chunk waits, so a small budget caps the stored
	// throughput at one budget per pause.
	gatewayBudgetPauseFloor = 100 * time.Millisecond
	gatewayBudgetPauseCap   = 8000 * time.Millisecond

	// logDrainWindow is the shortest span one drain-rate sample covers.
	// A gap between two freed chunks longer than this starts a new
	// window, so idle time never lowers the measured rate.
	logDrainWindow = 250 * time.Millisecond

	// logDrainStallAfter is how long the drain may free no bytes before
	// the gateway-budget pause counts as stalled and grows toward its cap.
	logDrainStallAfter = time.Second

	logAppendBackoffFloor = 250 * time.Millisecond
	logAppendBackoffCap   = 2000 * time.Millisecond

	logHeartbeatInterval = 30 * time.Second

	defaultLogFinalizeWait = 60 * time.Second // VARROA_RECORDSTORE_LOG_FINALIZE_WAIT

	// logOwnerRecheckAfter is how long a chunk may wait after its
	// admission before a write of it needs a fresh owner read. It stays
	// below the record store's default confirmed-gone grace (30s), so a
	// chunk admitted before its cell died is checked again before a mesh
	// takeover can hand the run to another cell.
	logOwnerRecheckAfter = 15 * time.Second
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

// runLogState is the gateway's per-run blob-writer state: the append
// pipeline's write progress, its pending queue, and the run's
// incompleteness and finalization knowledge. One instance lives for as
// long as the run has pending or unacknowledged work.
type runLogState struct {
	// ns and name name the run's current writer: set by the first call
	// that creates this state, and moved to a new writer by appendChunk.
	// abandonRun answers and publishes for that writer.
	ns, name string

	// writer is the "ns/name/cell_instance" of the cell whose chunks
	// this state last appended. A chunk from another writer means the
	// run moved to another cell: the parts the old writer left above a
	// hole belong to a log the new writer does not continue.
	writer string

	// jobPath is the run's job path, recorded from the first call that
	// creates this state and never changed after. publishRunLogBestEffort
	// resolves the run's tenant from it, so a best-effort envelope stamps
	// the same tenant the ordered event group resolves through
	// CellTenantResolver, never the cell's own synthetic tenant.
	jobPath string

	mu          sync.Mutex
	established bool // restart recovery has run for this run
	queue       []queuedLogChunk
	queueBytes  int64
	draining    bool

	// lastDequeue is when the drain goroutine last took a chunk off
	// queue. The writer heartbeat compares it with the oldest queued
	// chunk to find a drain that makes no progress.
	lastDequeue time.Time
	// lastStallWarn is when warnStalledDrainLocked last warned for
	// this run. It bounds the warning to one per heartbeat interval.
	lastStallWarn time.Time

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

	// adoptedThrough is the ackSeq at the moment the current writer was
	// adopted from another writer, or -1 when it never was. A part at or
	// below it can hold the old writer's bytes, so a chunk of the current
	// writer at or below it is checked against the stored part.
	adoptedThrough int64

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

// queuedLogChunk is one admitted chunk and the controller that sent it.
// The drain goroutine answers each chunk to its own sender, because a
// run that moves to another cell can hold chunks of two cells in one
// queue.
type queuedLogChunk struct {
	chunk    *mitev1.LogChunk
	ns, name string
	admitted time.Time
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
		st = &runLogState{ns: ns, name: name, jobPath: jobPath, ackSeq: -1, adoptedThrough: -1, appendCtx: appendCtx, appendCancel: appendCancel}
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
		h.metrics.logChunksRejected.Add(ctx, 1)
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
		h.metrics.logChunksRejected.Add(ctx, 1)
		h.sendLogAck(ns, name, chunk.RunId, chunk.Seq, logAckStatusRejected, logReasonForeignRun, 0)
		return
	case ownerUnknown:
		h.answerBusy(ns, name, chunk, logReasonOwnerLookupFail, logBusyPauseMS)
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

	if overRun || overBudget {
		h.warnStalledDrainLocked(chunk.RunId, st, time.Now())
		pauseMS := logBusyPauseMS
		reason := logReasonRunPendingFull
		if !overRun {
			pauseMS = h.gatewayBudgetPauseMSLocked(time.Now(), h.logTotalPendingBytes+size-h.logPendingMaxBytesOrDefault())
			reason = logReasonGatewayBudgetFul
		}
		st.mu.Unlock()
		h.logMu.Unlock()
		h.answerBusy(ns, name, chunk, reason, pauseMS)
		return
	}

	st.queue = append(st.queue, queuedLogChunk{chunk: chunk, ns: ns, name: name, admitted: time.Now()})
	st.queueBytes += size
	draining := st.draining
	st.draining = true
	h.logTotalPendingBytes += size
	st.mu.Unlock()
	h.logMu.Unlock()

	if !draining {
		go h.drainRun(st.appendCtx, chunk.RunId, st)
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

// answerBusy answers BUSY for one refused chunk and counts it by reason.
func (h *BusHandler) answerBusy(ns, name string, chunk *mitev1.LogChunk, reason string, pauseMS int) {
	h.metrics.logChunksBusy.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
	h.log().Debug("log chunk busy", "run_id", chunk.RunId, "controller", ns+"/"+name,
		"seq", chunk.Seq, "reason", reason, "pause_ms", pauseMS)
	h.sendLogAck(ns, name, chunk.RunId, chunk.Seq, logAckStatusBusy, reason, pauseMS)
}

// logDrainMeter measures the bytes the drain goroutines free per
// second, across every run. Caller must hold h.logMu for every method.
type logDrainMeter struct {
	windowStart time.Time
	windowBytes int64
	rate        float64 // bytes per second; 0 until one window completes
	lastFreed   time.Time
}

// freed records n bytes that a drain goroutine released at now.
func (m *logDrainMeter) freed(now time.Time, n int64) {
	if m.windowStart.IsZero() || now.Sub(m.lastFreed) > logDrainWindow {
		m.windowStart = now
		m.windowBytes = 0
	}
	m.windowBytes += n
	m.lastFreed = now
	age := now.Sub(m.windowStart)
	if age < logDrainWindow {
		return
	}
	sample := float64(m.windowBytes) / age.Seconds()
	if m.rate == 0 {
		m.rate = sample
	} else {
		m.rate = (m.rate + sample) / 2
	}
	m.windowStart = now
	m.windowBytes = 0
}

// gatewayBudgetPauseMSLocked computes the BUSY pause_ms for the
// gateway-wide budget. need is how many bytes the drain must free
// before the refused chunk fits. While the drain makes progress, the
// pause is the time the measured rate needs to free need. While it
// makes no progress, the pause grows from the floor by the time it has
// stalled. The result carries up to 20% jitter, within floor and cap,
// so refused runs do not all re-send at one moment. Caller must hold
// h.logMu.
func (h *BusHandler) gatewayBudgetPauseMSLocked(now time.Time, need int64) int {
	m := &h.logDrain
	var d time.Duration
	if !m.lastFreed.IsZero() && now.Sub(m.lastFreed) < logDrainStallAfter {
		if m.rate > 0 && need > 0 {
			d = time.Duration(float64(need) / m.rate * float64(time.Second))
		}
	} else {
		stallStart := m.lastFreed
		if h.logBudgetCrossedAt.After(stallStart) {
			stallStart = h.logBudgetCrossedAt
		}
		d = gatewayBudgetPauseFloor + now.Sub(stallStart)
	}
	d = clampBudgetPause(d)
	if spread := int64(d) / 5; spread > 0 {
		d += time.Duration(rand.Int63n(2*spread+1) - spread)
	}
	return int(clampBudgetPause(d) / time.Millisecond)
}

// clampBudgetPause bounds d to the gateway-budget pause floor and cap.
func clampBudgetPause(d time.Duration) time.Duration {
	if d < gatewayBudgetPauseFloor {
		return gatewayBudgetPauseFloor
	}
	if d > gatewayBudgetPauseCap {
		return gatewayBudgetPauseCap
	}
	return d
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
			h.runDeadlineFired(runID, st, cancel)
		} else {
			time.AfterFunc(d, func() { h.runDeadlineFired(runID, st, cancel) })
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
// (drainRun -> appendChunk -> appendAbandon); calling abandonRun here
// too would race that goroutine's queue mutation, so this path defers to
// it. The gap this call closes is the run that goes idle with a pending
// entry or an open hole and never gets another chunk: nothing else would
// ever call abandonRun for it, so the heartbeat would run forever and the
// stranded sequence would never be released or reported.
func (h *BusHandler) runDeadlineFired(runID string, st *runLogState, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}

	st.mu.Lock()
	idle := !st.draining
	needsAbandon := !st.rejectedForever && (len(st.pendingEntries) > 0 || !st.finalDurable)
	st.mu.Unlock()

	if idle && needsAbandon {
		h.abandonRun(runID, st)
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
		h.warnStalledDrainLocked(runID, st, time.Now())
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

// warnStalledDrainLocked logs a warning when the run holds queued chunks
// and no chunk has left the queue for one heartbeat interval: the drain
// goroutine is gone, or it is blocked. Either holds the queued bytes
// against the gateway-wide budget. The writer heartbeat calls it for a
// terminal run, and a BUSY refusal calls it for a live run, so it warns
// at most once per heartbeat interval. Caller must hold st.mu.
func (h *BusHandler) warnStalledDrainLocked(runID string, st *runLogState, now time.Time) {
	if len(st.queue) == 0 {
		return
	}
	since := st.queue[0].admitted
	if st.lastDequeue.After(since) {
		since = st.lastDequeue
	}
	if now.Sub(since) < logHeartbeatInterval || now.Sub(st.lastStallWarn) < logHeartbeatInterval {
		return
	}
	st.lastStallWarn = now
	h.log().Warn("log run drain made no progress", "run_id", runID,
		"queued", len(st.queue), "queued_bytes", st.queueBytes, "draining", st.draining,
		"ack_seq", st.ackSeq, "stalled_for", now.Sub(since).Round(time.Second))
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
	// appendAbandon means the run cannot continue: the finalize
	// deadline passed, or the index holds a conflicting entry. The
	// caller abandons the run.
	appendAbandon
)

// writeResult is what retryWrite resolved to.
type writeResult int

const (
	writeDone writeResult = iota
	// writeDeadline means the finalize deadline ended the retry.
	writeDeadline
	// writeConflict means the store holds different bytes at the
	// sequence. A retry of the same bytes can never succeed.
	writeConflict
	// writeRefused means a fresh owner read found that the chunk's
	// sender no longer owns the run.
	writeRefused
)

// drainRun is the run's single append-pipeline goroutine: one append in
// flight per run, chunks processed strictly in queue order. It exits
// once the queue empties, and restarts (from OnLogChunk) on the next
// arrival.
func (h *BusHandler) drainRun(ctx context.Context, runID string, st *runLogState) {
	for {
		st.mu.Lock()
		if len(st.queue) == 0 {
			st.draining = false
			st.mu.Unlock()
			return
		}
		item := st.queue[0]
		st.mu.Unlock()

		if h.appendChunk(ctx, runID, item.ns, item.name, st, item.chunk, item.admitted) == appendAbandon {
			h.abandonRun(runID, st)
			return
		}

		size := int64(len(item.chunk.Data))
		st.mu.Lock()
		if st.rejectedForever {
			// runDeadlineFired's idle path ran abandonRun concurrently,
			// between this chunk's successful append and this dequeue:
			// it already cleared st.queue and charged its bytes back.
			// Nothing is left here to dequeue or charge again.
			st.mu.Unlock()
			return
		}
		now := time.Now()
		st.queue = st.queue[1:]
		st.queueBytes -= size
		st.lastDequeue = now
		st.mu.Unlock()
		h.logMu.Lock()
		h.logTotalPendingBytes -= size
		h.logDrain.freed(now, size)
		h.logMu.Unlock()
	}
}

// appendChunk durably appends one chunk's log part, then holds its
// index entry pending until it can carry a truthful offset, acknowledges
// the chunk, and flushes whatever prefix of pendingEntries the chunk
// makes contiguous. It recovers the run's state from the store on first
// touch.
func (h *BusHandler) appendChunk(ctx context.Context, runID, ns, name string, st *runLogState, chunk *mitev1.LogChunk, admitted time.Time) appendOutcome {
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
				return appendAbandon
			}
			time.Sleep(logAppendBackoffFloor)
		}
		st.mu.Lock()
		st.established = true
		st.mu.Unlock()
	}

	if proceed, outcome := h.adoptWriter(ctx, runID, ns, name, st, chunk); !proceed {
		return outcome
	}

	st.mu.Lock()
	ackSeq := st.ackSeq
	_, pending := st.pendingEntries[chunk.Seq]
	alreadyDurable := chunk.Seq <= ackSeq || pending
	fillsHole := chunk.Seq == ackSeq+1
	wasFinalDurable := st.finalDurable
	overlapsOldWriter := alreadyDurable && chunk.Seq <= st.adoptedThrough
	st.mu.Unlock()

	if alreadyDurable {
		// Already durable: a redelivery after a lost acknowledgement, or
		// a repeat of a chunk this run already wrote out of order.
		// Nothing to append, but the mite still needs the release.
		if overlapsOldWriter {
			if outcome, ok := h.checkOldWriterPart(ctx, runID, ns, name, st, chunk, admitted); !ok {
				return outcome
			}
		}
		h.ackStored(ns, name, runID, st)
		return appendDone
	}

	createdAt := runCreatedAtFromUUIDv7(runID)
	logOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: createdAt}
	recheck := h.ownerRecheck(ctx, runID, ns, name, st, chunk, admitted)
	logKey := blob.LogKey(runID)
	switch h.retryWrite(ctx, runID, st, logKey, chunk.Seq, recheck, func(c context.Context) error {
		return h.logStore.Append(c, logKey, chunk.Seq, chunk.Data, logOpts)
	}) {
	case writeDeadline:
		return appendAbandon
	case writeRefused:
		h.refuseStaleWriterChunk(ns, name, runID, chunk)
		return appendDone
	case writeConflict:
		if proceed, outcome := h.replaceStalePart(ctx, runID, ns, name, st, chunk, logOpts, recheck); !proceed {
			return outcome
		}
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
		Writer: logWriterKey(ns, name, chunk),
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
		if outcome == appendAbandon {
			return appendAbandon
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

// checkOldWriterPart handles a chunk of the current writer at or below
// adoptedThrough. The part at that sequence can hold the old writer's
// bytes, or the current writer's own bytes that restart recovery indexed
// with no writer. Append with the same bytes succeeds and changes
// nothing, so the chunk is a plain repeat. A conflict means the part
// holds the old writer's bytes: the chunk's lines are lost, and the run
// is marked gateway-gap. A sender that no longer owns the run is refused
// and marks nothing. ok is false when the finalize deadline passed.
func (h *BusHandler) checkOldWriterPart(ctx context.Context, runID, ns, name string, st *runLogState, chunk *mitev1.LogChunk, admitted time.Time) (appendOutcome, bool) {
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: runCreatedAtFromUUIDv7(runID)}
	key := blob.LogKey(runID)
	recheck := h.ownerRecheck(ctx, runID, ns, name, st, chunk, admitted)
	switch h.retryWrite(ctx, runID, st, key, chunk.Seq, recheck, func(c context.Context) error {
		return h.logStore.Append(c, key, chunk.Seq, chunk.Data, opts)
	}) {
	case writeDone:
		return appendDone, true
	case writeRefused:
		h.refuseStaleWriterChunk(ns, name, runID, chunk)
		return appendDone, false
	case writeConflict:
	default:
		return appendAbandon, false
	}

	st.mu.Lock()
	newGap := !st.incomplete || st.incompleteReason != logReasonGatewayGap
	st.incomplete = true
	st.incompleteReason = logReasonGatewayGap
	st.mu.Unlock()
	if newGap {
		h.log().Warn("moved writer sent a chunk on a part of the old writer, its lines are lost",
			"run_id", runID, "seq", chunk.Seq)
		h.publishRunLogBestEffort(ns, name, runID, st.jobPath, map[string]interface{}{
			"incomplete": true, "incomplete_reason": logReasonGatewayGap,
		})
	}
	return appendDone, true
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
			return finalWritten, appendAbandon
		}
		line = append(line, '\n')
		indexOpts := blob.ObjectOptions{ObjectClass: "log", ContentType: "application/x-ndjson", CreatedAt: createdAt}
		switch h.retryAppend(ctx, runID, st, blob.LogIndexKey(runID), e.Seq, line, indexOpts) {
		case writeDone:
		case writeConflict:
			// The index only ever grows in sequence order, so a
			// conflicting entry at the next sequence leaves no offset
			// this writer can trust. The run ends as a gap.
			h.log().Error("log index entry conflicts with a stored entry", "run_id", runID, "seq", e.Seq)
			st.mu.Lock()
			st.incomplete = true
			st.incompleteReason = logReasonGatewayGap
			st.mu.Unlock()
			return finalWritten, appendAbandon
		default:
			return finalWritten, appendAbandon
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

// retryAppend appends data at seq through retryWrite.
func (h *BusHandler) retryAppend(ctx context.Context, runID string, st *runLogState, key string, seq int64, data []byte, opts blob.ObjectOptions) writeResult {
	return h.retryWrite(ctx, runID, st, key, seq, nil, func(c context.Context) error {
		return h.logStore.Append(c, key, seq, data, opts)
	})
}

// retryWrite retries write with a 250ms-to-2000ms jittered backoff until
// it succeeds or the run's finalize deadline passes. It never
// substitutes different bytes at seq: every retry sends the exact same
// data, so an ambiguous timeout after a durable write resolves on the
// next attempt. ErrChunkConflict therefore means that another writer
// stored the part, and a retry can never succeed: it returns
// writeConflict at once.
//
// ctx is st.appendCtx: it ends at the run's finalize deadline (design.md
// D4, "The heartbeat and the retry both end at that deadline"), so a
// call already blocked inside the store when the deadline arrives still
// returns instead of holding the pending queue open forever.
//
// recheck, when not nil, runs before each attempt. Any answer other
// than writeDone ends the retry with that answer.
func (h *BusHandler) retryWrite(ctx context.Context, runID string, st *runLogState, key string, seq int64, recheck func() writeResult, write func(context.Context) error) writeResult {
	delay := logAppendBackoffFloor
	for {
		if deadlinePassed(st) {
			return writeDeadline
		}
		if recheck != nil {
			if r := recheck(); r != writeDone {
				return r
			}
		}
		err := write(ctx)
		if err == nil {
			return writeDone
		}
		if errors.Is(err, blob.ErrChunkConflict) {
			return writeConflict
		}
		if ctx.Err() != nil {
			// The deadline ended appendCtx while this call was in
			// flight: the write is abandoned, not retried.
			return writeDeadline
		}
		h.metrics.logAppendRetries.Add(ctx, 1)
		h.log().Warn("log append retry", "run_id", runID, "key", key, "seq", seq, "error", err)
		if deadlinePassed(st) {
			return writeDeadline
		}
		select {
		case <-ctx.Done():
			return writeDeadline
		case <-time.After(jitteredDelay(delay)):
		}
		delay *= 2
		if delay > logAppendBackoffCap {
			delay = logAppendBackoffCap
		}
	}
}

// adoptWriter records the chunk's sender as the run's writer. It reports
// false, with the outcome to return, when the chunk must not be
// appended.
//
// A chunk from a writer other than the current one means the run moved
// to another cell. A stale owner-cache match can still admit the old
// writer's chunk after the move, so the new writer must own the run in a
// fresh ledger read. The old writer's pending parts are then dropped:
// they sit above a hole that the old writer can no longer fill, and the
// new writer continues the log at that hole. The new writer's own chunks
// at those sequences replace the dropped parts through replaceStalePart.
func (h *BusHandler) adoptWriter(ctx context.Context, runID, ns, name string, st *runLogState, chunk *mitev1.LogChunk) (bool, appendOutcome) {
	writer := logWriterKey(ns, name, chunk)
	st.mu.Lock()
	if st.writer == "" || st.writer == writer {
		st.writer = writer
		st.mu.Unlock()
		return true, appendDone
	}
	st.mu.Unlock()

	decision, ok := h.confirmOwner(ctx, runID, ns, name, chunk.CellInstance, st)
	if !ok {
		return false, appendAbandon
	}
	if decision != ownerMatched {
		h.refuseStaleWriterChunk(ns, name, runID, chunk)
		return false, appendDone
	}

	blanked, ok := h.blankPartsAboveAckSeq(ctx, runID, st)
	if !ok {
		return false, appendAbandon
	}

	st.mu.Lock()
	dropped := len(st.pendingEntries)
	st.pendingEntries = nil
	st.pendingData = nil
	st.writer = writer
	st.adoptedThrough = st.ackSeq
	st.ns, st.name = ns, name
	if dropped > 0 || blanked > 0 {
		st.incomplete = true
		st.incompleteReason = logReasonGatewayGap
	}
	st.mu.Unlock()

	if dropped > 0 || blanked > 0 {
		h.log().Warn("run moved to another cell, dropping the old cell's log parts above a hole",
			"run_id", runID, "writer", writer, "dropped", dropped, "blanked", blanked)
		h.publishRunLogBestEffort(ns, name, runID, st.jobPath, map[string]interface{}{
			"incomplete": true, "incomplete_reason": logReasonGatewayGap,
		})
	}
	return true, appendDone
}

// logWriterKey names the writer of a chunk: the sender's namespace and
// cell, and the cell instance the mite stamped.
func logWriterKey(ns, name string, chunk *mitev1.LogChunk) string {
	return ns + "/" + name + "/" + chunk.CellInstance
}

// blankPartsAboveAckSeq replaces every stored log part above ackSeq with
// an empty part, before a new writer appends anything. No index entry
// names such a part: restart recovery indexes every readable part, so
// the first sequence above ackSeq is always missing or empty. Once the
// new writer fills the hole below an old part, the range reader would
// read the old part's bytes after the new writer's, and restart recovery
// would index them as a recovered entry and drop the new writer's chunk
// at that sequence. An empty part adds no bytes, and recovery stops at
// it. The new writer's own chunk at that sequence then conflicts with
// the empty part and replaces it through replaceStalePart. ok is false
// when the finalize deadline passed first.
func (h *BusHandler) blankPartsAboveAckSeq(ctx context.Context, runID string, st *runLogState) (blanked int, ok bool) {
	st.mu.Lock()
	from := st.ackSeq + 1
	st.mu.Unlock()

	key := blob.LogKey(runID)
	var parts []blob.Part
	for {
		var err error
		parts, err = h.logStore.Parts(ctx, key, from)
		if err == nil {
			break
		}
		h.log().Warn("list log parts failed, retrying", "run_id", runID, "error", err)
		if deadlinePassed(st) {
			return 0, false
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(logAppendBackoffFloor):
		}
	}

	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: runCreatedAtFromUUIDv7(runID)}
	for _, part := range parts {
		seq := part.Seq
		if h.retryWrite(ctx, runID, st, key, seq, nil, func(c context.Context) error {
			return h.logStore.Overwrite(c, key, seq, []byte{}, opts)
		}) != writeDone {
			return blanked, false
		}
		blanked++
	}
	return blanked, true
}

// replaceStalePart handles a log part that already holds other bytes at
// the chunk's sequence. The sequence is above ackSeq and not in
// pendingEntries, so no index entry names that part: a writer that lost
// the run left it above a hole. When the chunk's sender owns the run in
// a fresh ledger read, the chunk replaces the part, and the run is
// marked as a gap because the old part's lines are lost. Otherwise the
// sender is the stale writer, and its chunk is refused, because it must
// not replace a part of the writer that now owns the run.
func (h *BusHandler) replaceStalePart(ctx context.Context, runID, ns, name string, st *runLogState, chunk *mitev1.LogChunk, opts blob.ObjectOptions, recheck func() writeResult) (bool, appendOutcome) {
	decision, ok := h.confirmOwner(ctx, runID, ns, name, chunk.CellInstance, st)
	if !ok {
		return false, appendAbandon
	}
	if decision != ownerMatched {
		h.refuseStaleWriterChunk(ns, name, runID, chunk)
		return false, appendDone
	}

	key := blob.LogKey(runID)
	switch h.retryWrite(ctx, runID, st, key, chunk.Seq, recheck, func(c context.Context) error {
		return h.logStore.Overwrite(c, key, chunk.Seq, chunk.Data, opts)
	}) {
	case writeDone:
	case writeRefused:
		h.refuseStaleWriterChunk(ns, name, runID, chunk)
		return false, appendDone
	default:
		return false, appendAbandon
	}

	st.mu.Lock()
	st.incomplete = true
	st.incompleteReason = logReasonGatewayGap
	st.mu.Unlock()
	h.log().Warn("replaced a log part that a lost writer left above a hole", "run_id", runID, "seq", chunk.Seq)
	h.publishRunLogBestEffort(ns, name, runID, st.jobPath, map[string]interface{}{
		"incomplete": true, "incomplete_reason": logReasonGatewayGap,
	})
	return true, appendDone
}

// confirmOwner reads the run's owner from the ledger again, past the
// owner cache, and compares it with the caller. It retries an
// unverifiable answer until the finalize deadline. ok is false when the
// deadline passed first.
func (h *BusHandler) confirmOwner(ctx context.Context, runID, ns, name, instance string, st *runLogState) (ownerDecision, bool) {
	for {
		if h.ownerCache != nil {
			h.ownerCache.invalidate(runID)
		}
		decision, _ := h.checkOwner(ctx, h.cluster, ns, name, instance, runID)
		if decision != ownerUnknown {
			return decision, true
		}
		if deadlinePassed(st) {
			return ownerUnknown, false
		}
		select {
		case <-ctx.Done():
			return ownerUnknown, false
		case <-time.After(logAppendBackoffFloor):
		}
	}
}

// ownerRecheck answers the recheck that retryWrite runs before each
// attempt to write chunk. A chunk that waited longer than
// logOwnerRecheckAfter since its admission, or since its last check,
// needs a fresh owner read: a store stall can hold an old cell's chunk
// in this queue past a mesh takeover, and the chunk must not land on a
// sequence the new owner already emptied or wrote.
func (h *BusHandler) ownerRecheck(ctx context.Context, runID, ns, name string, st *runLogState, chunk *mitev1.LogChunk, admitted time.Time) func() writeResult {
	after := logOwnerRecheckAfter
	if h.logOwnerRecheckAge > 0 {
		after = h.logOwnerRecheckAge
	}
	last := admitted
	return func() writeResult {
		if time.Since(last) < after {
			return writeDone
		}
		decision, ok := h.confirmOwner(ctx, runID, ns, name, chunk.CellInstance, st)
		if !ok {
			return writeDeadline
		}
		if decision != ownerMatched {
			return writeRefused
		}
		last = time.Now()
		return writeDone
	}
}

// refuseStaleWriterChunk answers REJECTED{foreign-run} for one chunk of
// a writer that no longer owns the run. The run itself stays open for
// the writer that owns it.
func (h *BusHandler) refuseStaleWriterChunk(ns, name, runID string, chunk *mitev1.LogChunk) {
	h.metrics.logChunksRejected.Add(context.Background(), 1)
	h.log().Warn("refusing a log chunk from a writer that no longer owns the run",
		"run_id", runID, "controller", ns+"/"+name, "cell_instance", chunk.CellInstance, "seq", chunk.Seq)
	h.sendLogAck(ns, name, runID, chunk.Seq, logAckStatusRejected, logReasonForeignRun, 0)
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
// first chunk the gateway handles for it. A crash between a log append
// and its index append, or inside flushContiguousLogIndex, leaves log
// parts above the highest index entry with no entry of their own. It
// writes one recovered entry for each such part, with that part's own
// size, before anything else appends. blob.UnindexedLogParts names the
// parts, and the recorder's next log sequence uses the same rule.
//
// st.offset is the end of the index entry at ackSeq, never the log's
// byte total: a part above the recovered run is not counted, and its
// writer's later chunk gets its entry at the true offset.
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
	entries, maxSeq, finalSeen, err := readLogIndex(ctx, h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		return err
	}

	var offset int64
	highestWritten := int64(-1)
	if maxSeq >= 0 {
		last := entries[maxSeq]
		offset = last.Offset + last.Len
		highestWritten = maxSeq
	}

	unindexed, err := blob.UnindexedLogParts(ctx, h.logStore, runID, highestWritten+1)
	if err != nil {
		return err
	}
	createdAt := runCreatedAtFromUUIDv7(runID)
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "application/x-ndjson", CreatedAt: createdAt}
	for _, part := range unindexed {
		entry := LogIndexEntry{Seq: part.Seq, Offset: offset, Len: part.Size, Recovered: true}
		line, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		if err := h.logStore.Append(ctx, blob.LogIndexKey(runID), part.Seq, line, opts); err != nil {
			return err
		}
		entries[part.Seq] = entry
		highestWritten = part.Seq
		offset += part.Size
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

	// The writer of the last indexed entry is the run's writer, so a
	// moved build's first chunk after a gateway restart still reads as
	// a writer change.
	writer := ""
	for seq := ackSeq; seq >= 0 && writer == ""; seq-- {
		writer = entries[seq].Writer
	}

	// adoptedThrough is the sequence below the writer's lowest own entry,
	// once an entry of another writer lies below it. A recovered entry
	// names no writer and does not end the walk. An index that names no
	// writer at all cannot show whose bytes it holds, so every entry is
	// checked as a part of another writer.
	adoptedThrough, own := int64(-1), int64(-1)
	if writer == "" {
		adoptedThrough = ackSeq
	}
	for seq := ackSeq; seq >= 0; seq-- {
		if w := entries[seq].Writer; w == writer {
			own = seq
		} else if w != "" {
			adoptedThrough = own - 1
			break
		}
	}

	// The next entry this writer flushes is ackSeq+1, so its offset is
	// the end of the entry at ackSeq.
	var ackEnd int64
	if ackSeq >= 0 {
		ackEnd = entries[ackSeq].Offset + entries[ackSeq].Len
	}

	st.mu.Lock()
	st.offset = ackEnd
	st.ackSeq = ackSeq
	st.pendingEntries = nil
	st.pendingData = nil
	st.finalDurable = finalSeen
	if st.writer == "" {
		st.writer = writer
		st.adoptedThrough = adoptedThrough
	}
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
func (h *BusHandler) abandonRun(runID string, st *runLogState) {
	st.mu.Lock()
	if st.rejectedForever {
		st.mu.Unlock()
		return
	}
	ns, name := st.ns, st.name
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
	wasDraining := st.draining
	st.draining = false
	hbCancel := st.heartbeatCancel
	st.heartbeatCancel = nil
	appendCancel := st.appendCancel
	// A gap this run will now never fill is a more precise cause than a
	// slow store: reasonGap wins when both are true of one run.
	reason := logReasonFinalizeDeadline
	if len(stranded) > 0 || st.incompleteReason == logReasonGatewayGap {
		reason = logReasonGatewayGap
	}
	st.incomplete = true
	st.incompleteReason = reason
	ackSeq := st.ackSeq
	st.mu.Unlock()

	h.log().Warn("abandoning a run's log", "run_id", runID, "controller", ns+"/"+name,
		"reason", reason, "ack_seq", ackSeq, "queued", len(pending), "queued_bytes", freedBytes,
		"stranded", len(stranded), "draining", wasDraining)

	if hbCancel != nil {
		hbCancel()
	}
	if appendCancel != nil {
		appendCancel()
	}

	h.logMu.Lock()
	h.logTotalPendingBytes -= freedBytes
	h.logMu.Unlock()

	for _, item := range pending {
		h.sendLogAck(item.ns, item.name, runID, item.chunk.Seq, logAckStatusRejected, logReasonFinalizeDeadline, 0)
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
