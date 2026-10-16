package mite

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// Bounds of the run-state store path (design.md D6).
const (
	// perRunStateMaxBytes bounds the stored state of one run. The
	// counter lives in gateway memory, so a restart loses it and the
	// recorder is the durable authority.
	perRunStateMaxBytes = 64 * 1024 * 1024

	// runStateResponseChunkBytes bounds one run_state_response body, so
	// a multi-megabyte file crosses the stream as several messages.
	runStateResponseChunkBytes = 512 * 1024

	// runStateObjectClass is the blob object class of every run-state
	// object. Migration 00010 widens the check constraint for it.
	runStateObjectClass = "state"

	// runStateContentType names the gzip body every run-state object
	// holds.
	runStateContentType = "application/gzip"
)

// RunStateAck status and reason tokens. The status set mirrors LogAck's,
// and the reason set is the whole set mite-loopback-api fixes.
const (
	stateAckStatusStored   = "STORED"
	stateAckStatusBusy     = "BUSY"
	stateAckStatusRejected = "REJECTED"

	stateReasonMalformed  = "malformed"
	stateReasonForeignRun = "foreign-run"
	stateReasonStateCap   = "state-cap"
	stateReasonIncomplete = "incomplete"
	stateReasonBudgetFull = "gateway-budget-full"
)

// runStateBuffer holds the chunks of one file of one push generation,
// until every sequence is present.
type runStateBuffer struct {
	// epoch names the Jenkins process that opened this buffer. A chunk
	// of another epoch replaces it, because its generation counts on a
	// different clock.
	epoch      int64
	stateGen   int64
	objectSize int64
	total      int64
	chunks     map[int64][]byte
	// pending is the gzip byte count this buffer charges against the
	// gateway-wide bound.
	pending int64
}

// runStateEntry is the gateway's per-run run-state knowledge: the stored
// size of each relative path, their sum, and the buffers of each partial
// push. One entry lives until the run reads terminal, or until its cell
// disconnects.
type runStateEntry struct {
	ns, name, jobPath string

	storedSizes map[string]int64
	storedTotal int64
	// newestEpoch is the newest pushing process this run has been seen
	// from, in this replica alone. The epoch is that process's start
	// time, so it only ever rises for one run, and a chunk below it
	// comes from a Jenkins process that is already gone.
	//
	// It fences this replica's buffers. It does not order the stored
	// objects of two epochs: the object key carries the epoch.
	newestEpoch int64
	buffers     map[string]*runStateBuffer
	// completed holds the push this gateway last stored for each path,
	// named by its epoch and its generation together. A chunk that
	// repeats both is a redelivery and is answered as stored, so an
	// ordinary retry never reads as a refusal.
	completed map[string]storedPush
	// fetched records that this run has already served one state fetch,
	// so the owner-cache invalidation runs on the first fetch alone.
	fetched bool
	// tenantUnresolvedSince is when a stored file of this run first found
	// no tenant. A resolved publish clears it. It is read and written
	// under h.stateMu.
	tenantUnresolvedSince time.Time
	// writeMu keeps two writes of one key apart. A whole-object replace
	// is not atomic on the object tier: it deletes the key and then
	// writes part 0. A redelivery repeats a push that this replica is
	// still writing, and both writes name one key. It is never taken
	// under h.stateMu.
	//
	// It does not order two pushes. The object key carries the pusher
	// epoch and the push generation, so two pushes write two keys.
	writeMu sync.Mutex
}

// storedPush is the push a gateway last stored for one path: the
// pusher process that sent it and that process's own generation.
//
// The epoch is part of the identity because a generation counts inside
// one Jenkins process, and a restart starts it again at 1 while this
// entry lives on. An identity the restart could repeat would answer
// every push of the restarted build as a duplicate and freeze the run's
// stored state at its pre-restart copy.
type storedPush struct {
	epoch int64
	gen   int64
}

// runStateOutcome is what one chunk produced: the ack the mite receives,
// and the envelope the recorder receives.
type runStateOutcome struct {
	status   string
	reason   string
	pauseMS  int
	stored   bool
	relpath  string
	bytes    int64
	seq      int64
	stateGen int64
}

// getOrCreateStateEntry returns the run's state entry, creating it on
// first touch. Caller must not hold h.stateMu.
func (h *BusHandler) getOrCreateStateEntry(runID, ns, name, jobPath string) *runStateEntry {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.stateRuns == nil {
		h.stateRuns = make(map[string]*runStateEntry)
	}
	e, ok := h.stateRuns[runID]
	if !ok {
		e = &runStateEntry{
			ns: ns, name: name, jobPath: jobPath,
			storedSizes: make(map[string]int64),
			buffers:     make(map[string]*runStateBuffer),
			completed:   make(map[string]storedPush),
		}
		h.stateRuns[runID] = e
	}
	return e
}

// dropStateEntry removes one run's entry and refunds its pending bytes.
// A terminal run keeps no entry: without this drop an entry would live
// for the whole gateway lifetime.
func (h *BusHandler) dropStateEntry(runID string) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	e, ok := h.stateRuns[runID]
	if !ok {
		return
	}
	for _, b := range e.buffers {
		h.stateTotalPendingBytes -= b.pending
	}
	if h.stateTotalPendingBytes < 0 {
		h.stateTotalPendingBytes = 0
	}
	delete(h.stateRuns, runID)
}

// dropCellStateEntries removes every entry of one cell. A disconnect
// ends every push that cell had in flight.
func (h *BusHandler) dropCellStateEntries(ns, name string) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	for runID, e := range h.stateRuns {
		if e.ns != ns || e.name != name {
			continue
		}
		for _, b := range e.buffers {
			h.stateTotalPendingBytes -= b.pending
		}
		delete(h.stateRuns, runID)
	}
	for runID, e := range h.restartRuns {
		if e.ns != ns || e.name != name {
			continue
		}
		h.dropRestartBuffersLocked(e)
		delete(h.restartRuns, runID)
	}
	if h.stateTotalPendingBytes < 0 {
		h.stateTotalPendingBytes = 0
	}
}

// OnRunStateChunk implements StreamHandler for one run-state chunk a
// mite forwards. It applies D6's six ordered steps and answers one
// RunStateAck. A stored file publishes its run.state envelope before
// the push is recorded and before the STORED ack. A failed publish
// answers BUSY. A state-cap refusal publishes its envelope before the
// REJECTED ack, whatever the publish answers.
func (h *BusHandler) OnRunStateChunk(name, ns string, chunk *mitev1.RunStateChunk) {
	if h.logStore == nil {
		return
	}
	// A restart copy follows its own path. It is posted after the run
	// finalizes, so the terminal drop below never applies to it.
	if chunk.RestartSource {
		h.onRestartSourceChunk(name, ns, chunk)
		return
	}
	ctx := context.Background()

	// Step 1: the run must be named, the path must be usable as a key
	// suffix, and the pusher epoch and the push generation must both be
	// positive.
	//
	// The key carries both numbers, so a chunk missing either would
	// store an object no fetch can name. The run would then hold a
	// record that points at nothing, and it could never resume. The
	// test runs before every other step, so no such chunk reaches the
	// redelivery test or a write.
	if chunk.RunId == "" || !validRunStateRelpath(chunk.Relpath) ||
		chunk.PusherEpoch <= 0 || chunk.StateGen <= 0 {
		h.rejectRunState(ctx, ns, name, chunk, stateReasonMalformed)
		return
	}

	// Step 2: the sending cell must own the run. The gateway stamps the
	// instance from the registered identity, so the message's own value
	// is never read.
	instance := h.lookupCellInstance(ns, name)
	decision, tuple := h.checkOwner(ctx, h.cluster, ns, name, instance, chunk.RunId)
	switch decision {
	case ownerForeign:
		h.rejectRunState(ctx, ns, name, chunk, stateReasonForeignRun)
		return
	case ownerUnknown:
		h.sendRunStateAck(ns, name, chunk, stateAckStatusBusy, stateReasonBudgetFull, stateBudgetPauseMinMS)
		return
	}
	if tuple.status != "" && isTerminalRunStatus(tuple.status) {
		h.dropStateEntry(chunk.RunId)
		h.sendRunStateAck(ns, name, chunk, stateAckStatusStored, "", 0)
		return
	}

	e := h.getOrCreateStateEntry(chunk.RunId, ns, name, chunk.JobPath)

	outcome, complete := h.bufferRunStateChunk(e, chunk)
	if complete != nil {
		// The declared object size is proved here: a completed set whose
		// chunk sizes do not sum to it answers malformed, so the
		// gateway's charge and the recorder's record agree.
		body, rawTotal, err := inflateRunStateSet(complete)
		if err != nil || rawTotal != complete.objectSize {
			outcome = runStateOutcome{status: stateAckStatusRejected, reason: stateReasonMalformed}
		} else {
			outcome = h.storeRunStateObject(ctx, ns, name, e, chunk, body, outcome)
		}
	}
	if outcome.status == stateAckStatusRejected {
		h.metrics.runStateRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", outcome.reason)))
	}

	// The cap envelope goes out before the refusal. The refusal holds
	// whatever the publish answers.
	if outcome.status == stateAckStatusRejected && outcome.reason == stateReasonStateCap {
		_ = h.publishRunStateOutcome(ns, name, chunk, outcome)
	}
	h.sendRunStateAck(ns, name, chunk, outcome.status, outcome.reason, outcome.pauseMS)
}

// validRunStateRelpath reports whether relpath is usable as a key
// suffix and as a path under the Jenkins home. It refuses an empty
// value, an absolute path, a Windows separator, and any dot segment, so
// no push can name a key or a file outside its own run.
func validRunStateRelpath(relpath string) bool {
	if relpath == "" || strings.HasPrefix(relpath, "/") || strings.Contains(relpath, `\`) {
		return false
	}
	for _, seg := range strings.Split(relpath, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// bufferRunStateChunk applies steps 3, 4, and 5a. It returns the
// outcome, and the completed buffer once every sequence is present. The
// caller inflates that buffer outside h.stateMu: an inflate of a
// multi-megabyte set under a gateway-wide lock would stall every other
// cell's push.
func (h *BusHandler) bufferRunStateChunk(e *runStateEntry, chunk *mitev1.RunStateChunk) (runStateOutcome, *runStateBuffer) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	// The epoch fence runs before the shape test and the cap test,
	// because both drop this path's buffer. A malformed or over-cap
	// chunk from a retired process would otherwise throw away the live
	// process's partial push, and that push's own later chunks would
	// then answer incomplete.
	//
	// This fence protects the buffers of one replica. The stored objects
	// of two epochs are ordered by the object key, not here.
	//
	// The epoch is the pushing process's start time, so it only rises
	// for one run. A lower epoch comes from a Jenkins process that is
	// already gone.
	if chunk.PusherEpoch < e.newestEpoch {
		return runStateOutcome{status: stateAckStatusStored}, nil
	}
	if chunk.PusherEpoch > e.newestEpoch {
		// The previous process is gone, so its partial buffers can
		// never complete. They are dropped here rather than at the end
		// of the run, so their bytes leave the gateway-wide bound at
		// once.
		//
		// The tenant grace clock stays. An unresolved tenant is a
		// property of the run's placement, not of the pusher process,
		// and a reset would let a restart loop extend the grace without
		// bound.
		h.retireEpochBuffersLocked(e, chunk.PusherEpoch)
		e.newestEpoch = chunk.PusherEpoch
	}

	if chunk.ObjectSize <= 0 || chunk.Total <= 0 || chunk.Seq < 0 || chunk.Seq >= chunk.Total {
		h.dropRunStateBufferLocked(e, chunk.Relpath)
		return runStateOutcome{status: stateAckStatusRejected, reason: stateReasonMalformed}, nil
	}

	// Step 3: the run's stored bytes, less this file's stored size, plus
	// this file's declared size, must stay at or below the cap.
	if e.storedTotal-e.storedSizes[chunk.Relpath]+chunk.ObjectSize > perRunStateMaxBytes {
		h.dropRunStateBufferLocked(e, chunk.Relpath)
		return runStateOutcome{status: stateAckStatusRejected, reason: stateReasonStateCap}, nil
	}

	size := int64(len(chunk.GzipContent))
	if h.stateTotalPendingBytes+size > h.logPendingMaxBytesOrDefault() {
		if h.stateBudgetCrossedAt.IsZero() {
			h.stateBudgetCrossedAt = time.Now()
		}
		pause := h.stateBudgetPauseMSLocked()
		return runStateOutcome{status: stateAckStatusBusy, reason: stateReasonBudgetFull, pauseMS: pause}, nil
	}
	h.stateBudgetCrossedAt = time.Time{}

	// A chunk that repeats the stored push is a redelivery, and it is
	// answered as stored. The mite can retry a chunk whose answer it
	// never saw, and without this test the last chunk of a multi-chunk
	// file would open a fresh buffer and read as incomplete.
	//
	// A chunk below the stored generation is answered the same way. The
	// store drops the path's buffer, so such a chunk would otherwise
	// open a fresh one, store a superseded push, and move this entry's
	// stored size and stored push back to it. The next retry of the
	// winning push would then read as incomplete.
	//
	// The test names the pusher process as well as its generation. A
	// generation alone repeats after a Jenkins restart, which counts
	// from 1 again while this entry lives, and every push of the
	// restarted build would then be discarded. A chunk of another epoch
	// always replaces.
	if done, ok := e.completed[chunk.Relpath]; ok &&
		done.epoch == chunk.PusherEpoch && chunk.StateGen <= done.gen {
		return runStateOutcome{status: stateAckStatusStored}, nil
	}

	// Step 4: buffer the chunk under the run, the path, and the push
	// generation. A chunk of an older generation is discarded, and a
	// newer one replaces the buffer. A newer epoch replaces it too: its
	// generation counts on a different clock.
	b := e.buffers[chunk.Relpath]
	switch {
	case b == nil || chunk.PusherEpoch > b.epoch || chunk.StateGen > b.stateGen:
		if b != nil {
			h.stateTotalPendingBytes -= b.pending
		}
		b = &runStateBuffer{
			epoch: chunk.PusherEpoch, stateGen: chunk.StateGen, objectSize: chunk.ObjectSize,
			total: chunk.Total, chunks: make(map[int64][]byte),
		}
		e.buffers[chunk.Relpath] = b
	case chunk.StateGen < b.stateGen:
		return runStateOutcome{status: stateAckStatusStored}, nil
	}
	b.chunks[chunk.Seq] = chunk.GzipContent
	b.pending += size
	h.stateTotalPendingBytes += size

	// Step 5a: a missing sequence when the last chunk arrives drops the
	// buffer. Each chunk is compressed on its own, so an incomplete set
	// would inflate into a truncated file with no error.
	if int64(len(b.chunks)) < b.total {
		if chunk.Seq == b.total-1 {
			h.dropRunStateBufferLocked(e, chunk.Relpath)
			return runStateOutcome{status: stateAckStatusRejected, reason: stateReasonIncomplete}, nil
		}
		return runStateOutcome{status: stateAckStatusStored}, nil
	}

	// Step 5: every sequence is present, so the caller inflates the set.
	h.dropRunStateBufferLocked(e, chunk.Relpath)
	return runStateOutcome{
		stored: true, relpath: chunk.Relpath, bytes: b.objectSize, seq: chunk.Seq, stateGen: b.stateGen,
	}, b
}

// storeRunStateObject writes the whole object and charges it. The charge
// replaces the path's previous size, so a replaced file counts once.
//
// The object key carries the pusher epoch and the push generation, so a
// superseded write lands on its own key and can never reach the live
// object. That holds for every gateway replica, because no replica has
// to agree with another about which push is newest. A gateway rollout
// leaves two replicas that forward chunks of one process, and the
// generation is what separates those writes.
//
// The retired-epoch test below is an optimization alone. It skips a
// write this replica already knows is superseded.
func (h *BusHandler) storeRunStateObject(ctx context.Context, ns, name string, e *runStateEntry, chunk *mitev1.RunStateChunk, body []byte, outcome runStateOutcome) runStateOutcome {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	h.stateMu.Lock()
	retired := chunk.PusherEpoch < e.newestEpoch
	h.stateMu.Unlock()
	if retired {
		// A retired process answers stored, as it does at the buffer
		// fence. The push came from a Jenkins process that is gone, so
		// no refusal can reach a pusher that could act on it.
		h.log().Debug("run-state write from a retired process dropped",
			"run_id", chunk.RunId, "relpath", chunk.Relpath, "pusher_epoch", chunk.PusherEpoch)
		return runStateOutcome{status: stateAckStatusStored}
	}

	if h.runStateTenantMissing(ns, name, chunk.JobPath) {
		return h.unresolvedTenantGrace(&e.tenantUnresolvedSince, "run-state", ns, name, chunk)
	}

	key := blob.RunStateKey(chunk.RunId, chunk.PusherEpoch, outcome.stateGen, chunk.Relpath)
	opts := blob.ObjectOptions{
		ObjectClass: runStateObjectClass,
		ContentType: runStateContentType,
		CreatedAt:   time.Now().UTC(),
	}
	if err := h.logStore.Replace(ctx, key, bytes.NewReader(body), opts); err != nil {
		h.log().Warn("run-state object write failed", "run_id", chunk.RunId, "relpath", chunk.Relpath, "error", err)
		return runStateOutcome{status: stateAckStatusBusy, reason: stateReasonBudgetFull, pauseMS: stateBudgetPauseMinMS}
	}

	// The envelope publishes before the push is recorded. A recorded push
	// answers every retry as a redelivery, so a lost envelope would never
	// publish again. A failed publish answers BUSY, and the plugin posts
	// the file again. The object write is idempotent under its key.
	// An unresolved tenant answers BUSY inside runStateTenantGrace and
	// foreign-run after it.
	if err := h.publishRunStateOutcome(ns, name, chunk, outcome); err != nil {
		if errors.Is(err, errRunStateTenantUnresolved) {
			return h.unresolvedTenantGrace(&e.tenantUnresolvedSince, "run-state", ns, name, chunk)
		}
		return runStateOutcome{status: stateAckStatusBusy, reason: stateReasonBudgetFull, pauseMS: stateBudgetPauseMinMS}
	}

	h.stateMu.Lock()
	e.storedTotal += outcome.bytes - e.storedSizes[chunk.Relpath]
	e.storedSizes[chunk.Relpath] = outcome.bytes
	e.completed[chunk.Relpath] = storedPush{epoch: chunk.PusherEpoch, gen: outcome.stateGen}
	e.tenantUnresolvedSince = time.Time{}
	h.stateMu.Unlock()

	h.metrics.runStateBytes.Add(ctx, outcome.bytes)
	outcome.status = stateAckStatusStored
	return outcome
}

// inflateRunStateSet concatenates the inflated bytes of every sequence
// of b, in ascending order, and reports the inflated total.
func inflateRunStateSet(b *runStateBuffer) ([]byte, int64, error) {
	var out bytes.Buffer
	for seq := int64(0); seq < b.total; seq++ {
		zr, err := gzip.NewReader(bytes.NewReader(b.chunks[seq]))
		if err != nil {
			return nil, 0, err
		}
		if _, err := io.Copy(&out, zr); err != nil {
			_ = zr.Close()
			return nil, 0, err
		}
		if err := zr.Close(); err != nil {
			return nil, 0, err
		}
	}
	return out.Bytes(), int64(out.Len()), nil
}

// retireEpochBuffersLocked drops every buffer of e whose epoch is below
// newEpoch and refunds their pending bytes. A process that has been
// replaced sends no more chunks, so its partial pushes can never
// complete and their bytes would otherwise stay charged against the
// gateway-wide bound for the rest of the run. Caller must hold
// h.stateMu.
func (h *BusHandler) retireEpochBuffersLocked(e *runStateEntry, newEpoch int64) {
	for relpath, b := range e.buffers {
		if b.epoch < newEpoch {
			h.stateTotalPendingBytes -= b.pending
			delete(e.buffers, relpath)
		}
	}
	if h.stateTotalPendingBytes < 0 {
		h.stateTotalPendingBytes = 0
	}
}

// dropRunStateBufferLocked removes one path's buffer and refunds its
// pending bytes. Caller must hold h.stateMu.
func (h *BusHandler) dropRunStateBufferLocked(e *runStateEntry, relpath string) {
	b, ok := e.buffers[relpath]
	if !ok {
		return
	}
	h.stateTotalPendingBytes -= b.pending
	if h.stateTotalPendingBytes < 0 {
		h.stateTotalPendingBytes = 0
	}
	delete(e.buffers, relpath)
}

// The run-state busy pause starts at stateBudgetPauseMinMS and grows to
// stateBudgetPauseMaxMS while the run-state bound stays crossed.
const (
	stateBudgetPauseMinMS = 1000
	stateBudgetPauseMaxMS = 8000
)

// runStateTenantGrace bounds how long a run whose tenant does not resolve
// answers BUSY. The resolver caches a placement for cellTenantResolveTTL,
// so two lifetimes let a placement change reach the cache and let one
// failed read retry. Past the grace the run is refused as foreign-run,
// because the cell does not host its tenant.
const runStateTenantGrace = 2 * cellTenantResolveTTL

// unresolvedTenantGrace answers a file whose tenant does not resolve. It
// takes the grace outcome under h.stateMu and logs a refusal. what names
// the path in the log line. Callers hold no lock.
func (h *BusHandler) unresolvedTenantGrace(since *time.Time, what, ns, name string, chunk *mitev1.RunStateChunk) runStateOutcome {
	h.stateMu.Lock()
	o := unresolvedTenantOutcomeLocked(since, time.Now())
	h.stateMu.Unlock()
	if o.status == stateAckStatusRejected {
		h.log().Warn(what+" tenant unresolved past the grace, refusing",
			"run_id", chunk.RunId, "controller", ns+"/"+name, "job_path", chunk.JobPath)
	}
	return o
}

// runStateTenantMissing reports whether the tenant of a chunk does not
// resolve. It runs before any write, so a refused file leaves no object
// behind. A handler with no publisher writes without a tenant check.
func (h *BusHandler) runStateTenantMissing(ns, name, jobPath string) bool {
	if h.publisher == nil {
		return false
	}
	if jobPath == "" {
		h.log().Warn("run-state chunk carries no job path", "controller", ns+"/"+name)
	}
	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, jobPath)
	if tenant == "" {
		h.log().Warn("run-state outcome unresolved, publishing no envelope",
			"controller", ns+"/"+name, "job_path", jobPath)
		return true
	}
	return false
}

// unresolvedTenantOutcomeLocked answers a stored file whose tenant does
// not resolve. The first observation starts the grace clock in since.
// Inside the grace the answer is BUSY, so a placement race or a failed
// read heals on a retry. Past it the answer is REJECTED foreign-run,
// which the pusher acknowledges and stops on. Caller must hold h.stateMu.
func unresolvedTenantOutcomeLocked(since *time.Time, now time.Time) runStateOutcome {
	if since.IsZero() {
		*since = now
	}
	if now.Sub(*since) < runStateTenantGrace {
		return runStateOutcome{status: stateAckStatusBusy, reason: stateReasonBudgetFull, pauseMS: stateBudgetPauseMinMS}
	}
	return runStateOutcome{status: stateAckStatusRejected, reason: stateReasonForeignRun}
}

// stateBudgetPauseMSLocked computes the busy pause for the gateway-wide
// run-state bound. Caller must hold h.stateMu.
func (h *BusHandler) stateBudgetPauseMSLocked() int {
	if h.stateBudgetCrossedAt.IsZero() {
		return stateBudgetPauseMinMS
	}
	ms := stateBudgetPauseMinMS + int(time.Since(h.stateBudgetCrossedAt)/time.Second)*1000
	if ms > stateBudgetPauseMaxMS {
		ms = stateBudgetPauseMaxMS
	}
	return ms
}

// rejectRunState answers one refusal and counts it by its reason.
func (h *BusHandler) rejectRunState(ctx context.Context, ns, name string, chunk *mitev1.RunStateChunk, reason string) {
	h.metrics.runStateRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	h.sendRunStateAck(ns, name, chunk, stateAckStatusRejected, reason, 0)
}

// sendRunStateAck answers one RunStateAck on the cell's current
// connection. It is a no-op when no connection is current: the pusher
// retries from its own spool.
func (h *BusHandler) sendRunStateAck(ns, name string, chunk *mitev1.RunStateChunk, status, reason string, pauseMS int) {
	h.sendRunStateAckKind(ns, name, chunk, status, reason, pauseMS, false)
}

// sendRunStateAckKind answers one RunStateAck and names the kind of chunk
// it answers, so the mite keeps a restart copy's refusal apart from a mesh
// refusal.
func (h *BusHandler) sendRunStateAckKind(ns, name string, chunk *mitev1.RunStateChunk, status, reason string, pauseMS int, restart bool) {
	send := h.currentSender(ns, name)
	if send == nil {
		return
	}
	msg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_RunStateAck{RunStateAck: &mitev1.RunStateAck{
			RunId: chunk.RunId, Relpath: chunk.Relpath, StateGen: chunk.StateGen,
			Status: status, Reason: reason, PauseMs: pauseMS, RestartSource: restart,
			Seq: chunk.Seq,
		}},
	}
	if err := send(msg); err != nil {
		h.log().Error("send run-state ack failed", "controller", ns+"/"+name, "run_id", chunk.RunId, "error", err)
	}
}

// publishRunStateOutcome publishes one run.state envelope and returns the
// publish error. A stored file carries the path, the whole object's
// inflated size, the sequence, the push generation, and the pusher epoch.
// The epoch is part of the object key, so the record cannot name the
// object without it.
// A cap refusal carries the reason alone, although
// no object was written: that envelope is the only carrier of
// mesh.cap_warned_at, which is an operator's one signal that the cap
// tripped.
func (h *BusHandler) publishRunStateOutcome(ns, name string, chunk *mitev1.RunStateChunk, outcome runStateOutcome) error {
	if h.publisher == nil {
		return nil
	}
	var payload map[string]interface{}
	switch {
	case outcome.stored:
		payload = map[string]interface{}{
			"relpath":      outcome.relpath,
			"bytes":        outcome.bytes,
			"seq":          outcome.seq,
			"state_gen":    outcome.stateGen,
			"pusher_epoch": chunk.PusherEpoch,
		}
	case outcome.reason == stateReasonStateCap:
		payload = map[string]interface{}{"rejected_reason": stateReasonStateCap}
	default:
		return nil
	}

	return h.publishRunStateEnvelope(ns, name, chunk, payload)
}

// publishRunStateEnvelope publishes one run.state envelope for chunk with
// the given payload. It returns an error when the tenant does not resolve,
// the envelope does not marshal, or the publish fails. A handler with no
// publisher publishes nothing and returns nil.
func (h *BusHandler) publishRunStateEnvelope(ns, name string, chunk *mitev1.RunStateChunk, payload map[string]interface{}) error {
	if h.publisher == nil {
		return nil
	}
	if chunk.JobPath == "" {
		// A cell derives the tenant from the first path segment, so no
		// envelope publishes there. A controller tenant publishes the
		// envelope with the empty path. The warning names the sender in
		// both cases.
		h.log().Warn("run-state chunk carries no job path",
			"run_id", chunk.RunId, "controller", ns+"/"+name)
	}

	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, chunk.JobPath)
	if tenant == "" {
		h.log().Warn("run-state outcome unresolved, publishing no envelope",
			"controller", ns+"/"+name, "job_path", chunk.JobPath)
		return errRunStateTenantUnresolved
	}

	env := ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kindRunState,
		RunID:         chunk.RunId,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		Tenant:        tenant,
		JobPath:       chunk.JobPath,
		OccurredAt:    time.Now().UTC(),
		Payload:       payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		h.log().Error("marshal run.state envelope failed", "run_id", chunk.RunId, "error", err)
		return fmt.Errorf("marshal run.state envelope: %w", err)
	}
	if _, err := h.publisher.PublishJetStream(bus.BuildSubject(h.cluster, ns, name), data); err != nil {
		h.log().Warn("run.state publish failed", "run_id", chunk.RunId, "error", err)
		return fmt.Errorf("publish run.state envelope: %w", err)
	}
	return nil
}

// errRunStateTenantUnresolved reports an envelope whose job path resolves
// to no tenant. The recorder cannot file such an envelope.
var errRunStateTenantUnresolved = errors.New("run.state tenant unresolved")

// OnRunStateRequest implements StreamHandler for a mite that fetches one
// stored file under a resume command. It answers one or more
// run_state_response messages, each carrying the request's own
// request_id. A read failure answers one response carrying error.
func (h *BusHandler) OnRunStateRequest(name, ns string, req *mitev1.RunStateRequest, send Sender) {
	if send == nil {
		return
	}
	if h.logStore == nil {
		h.sendRunStateError(send, req, "the gateway holds no blob store")
		return
	}
	if req.RunId == "" || !validRunStateRelpath(req.Relpath) {
		h.sendRunStateError(send, req, "the request names no run or no usable path")
		return
	}
	// A restart copy is final, so its key carries no epoch or generation
	// and the request names none. Every other fetch names both, because
	// the key carries both. A request without them names no object, and
	// it is refused here rather than read as a miss, because the two
	// failures need different repairs.
	var key string
	if req.RestartSource {
		key = blob.RestartSourceKey(req.RunId, req.Relpath)
	} else {
		if req.PusherEpoch <= 0 {
			h.sendRunStateError(send, req, "the request names no pusher epoch")
			return
		}
		if req.StateGen <= 0 {
			h.sendRunStateError(send, req, "the request names no push generation")
			return
		}
		key = blob.RunStateKey(req.RunId, req.PusherEpoch, req.StateGen, req.Relpath)
	}

	h.invalidateOwnerOnFirstFetch(req.RunId, ns, name)

	ctx := context.Background()
	// Replace writes through Put, which both drivers store as chunked
	// part 0, so the chunked read is the matching read.
	r, err := h.logStore.NewRangeReader(ctx, key, 0, -1)
	if err != nil {
		h.sendRunStateError(send, req, "the gateway cannot read the stored file")
		return
	}
	body, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		h.sendRunStateError(send, req, "the gateway cannot read the stored file")
		return
	}

	total := int64(len(body)) / runStateResponseChunkBytes
	if int64(len(body))%runStateResponseChunkBytes != 0 || len(body) == 0 {
		total++
	}
	for seq := int64(0); seq < total; seq++ {
		start := seq * runStateResponseChunkBytes
		end := start + runStateResponseChunkBytes
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		part := body[start:end]
		gz, gerr := gzipBytes(part)
		if gerr != nil {
			h.sendRunStateError(send, req, "the gateway cannot compress the stored file")
			return
		}
		msg := &mitev1.OperatorMessage{
			Message: &mitev1.OperatorMessage_RunStateResponse{RunStateResponse: &mitev1.RunStateResponse{
				RequestId: req.RequestId, Relpath: req.Relpath,
				Seq: seq, Total: total, GzipContent: gz, RawSize: int64(len(part)),
			}},
		}
		if err := send(msg); err != nil {
			h.log().Error("send run-state response failed", "controller", ns+"/"+name, "run_id", req.RunId, "error", err)
			return
		}
	}
}

// invalidateOwnerOnFirstFetch drops the run's cached owner the first
// time this gateway serves a fetch for it. A resume moves a run's owner
// with no claim behind it, so no grant can replace the entry. It only
// forces a re-read of the recorder, and sets no owner.
func (h *BusHandler) invalidateOwnerOnFirstFetch(runID, ns, name string) {
	h.stateMu.Lock()
	e, ok := h.stateRuns[runID]
	if !ok {
		if h.stateRuns == nil {
			h.stateRuns = make(map[string]*runStateEntry)
		}
		e = &runStateEntry{
			ns: ns, name: name,
			storedSizes: make(map[string]int64),
			buffers:     make(map[string]*runStateBuffer),
			completed:   make(map[string]storedPush),
		}
		h.stateRuns[runID] = e
	}
	first := !e.fetched
	e.fetched = true
	h.stateMu.Unlock()

	if first && h.ownerCache != nil {
		h.ownerCache.invalidate(runID)
	}
}

// sendRunStateError answers one response carrying error, so the mite
// fails the resume command rather than write a partial file.
func (h *BusHandler) sendRunStateError(send Sender, req *mitev1.RunStateRequest, reason string) {
	msg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_RunStateResponse{RunStateResponse: &mitev1.RunStateResponse{
			RequestId: req.RequestId, Relpath: req.Relpath, Error: reason,
		}},
	}
	if err := send(msg); err != nil {
		h.log().Error("send run-state error failed", "run_id", req.RunId, "error", err)
	}
}

// gzipBytes compresses p.
func gzipBytes(p []byte) ([]byte, error) {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(p); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
