package mite

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// The bounds of one restart copy. They match the plugin's own caps, so a
// copy the plugin builds always fits here.
const (
	restartSourceMaxFiles = 512
	restartSourceMaxBytes = 32 << 20

	// restartSourceIdleTTL ends a copy that stopped receiving chunks. Its
	// buffers return their bytes to the gateway-wide bound, and the
	// recorder keeps the copy incomplete.
	restartSourceIdleTTL = 15 * time.Minute

	// restartSourceKind names the copy in the rejection metric.
	restartSourceKind = "restart_source"
)

// restartSourceEntry is the gateway's knowledge of one run's restart copy:
// the files stored so far and the buffers of the files still arriving. A
// copy has one pusher epoch, because the plugin posts a copy once, after
// the run finalizes. The entry lives until every declared file is stored,
// or until its cell disconnects or it goes idle.
type restartSourceEntry struct {
	ns, name, jobPath string
	buildNumber       int64
	epoch             int64
	copyFiles         int64
	copyBytes         int64

	buffers map[string]*runStateBuffer
	// stored maps each relpath this copy wrote to its inflated size.
	stored      map[string]int64
	storedTotal int64
	touched     time.Time
	// writeMu keeps two writes of one key apart. It is never taken under
	// h.stateMu.
	writeMu sync.Mutex
}

// restartSourceBuildPrefix returns the relpath prefix of one build's
// directory. It matches RunStatePusher.buildRelPathPrefix in the plugin.
func restartSourceBuildPrefix(jobPath string, buildNumber int64) string {
	var b strings.Builder
	for _, seg := range strings.Split(jobPath, "/") {
		if seg == "" {
			continue
		}
		b.WriteString("jobs/")
		b.WriteString(seg)
		b.WriteString("/")
	}
	b.WriteString("builds/")
	b.WriteString(strconv.FormatInt(buildNumber, 10))
	b.WriteString("/")
	return b.String()
}

// validRestartSourceRelpath reports whether relpath names a file the copy
// may hold: build.xml, a file under workflow-completed/, or a file under
// libs/, all inside the exact build directory of jobPath.
func validRestartSourceRelpath(relpath, jobPath string, buildNumber int64) bool {
	if !validRunStateRelpath(relpath) {
		return false
	}
	prefix := restartSourceBuildPrefix(jobPath, buildNumber)
	if !strings.HasPrefix(relpath, prefix) {
		return false
	}
	rest := relpath[len(prefix):]
	switch {
	case rest == "build.xml":
		return true
	case strings.HasPrefix(rest, "workflow-completed/"):
		return len(rest) > len("workflow-completed/")
	case strings.HasPrefix(rest, "libs/"):
		return len(rest) > len("libs/")
	}
	return false
}

// validRestartSourceChunk checks the shape of one restart-copy chunk.
func validRestartSourceChunk(chunk *mitev1.RunStateChunk) bool {
	return chunk.RunId != "" && chunk.PusherEpoch > 0 && chunk.StateGen > 0 &&
		chunk.JobPath != "" && chunk.BuildNumber > 0 &&
		chunk.CopyFiles >= 1 && chunk.CopyFiles <= restartSourceMaxFiles &&
		chunk.CopyBytes >= 1 && chunk.CopyBytes <= restartSourceMaxBytes &&
		validRestartSourceRelpath(chunk.Relpath, chunk.JobPath, chunk.BuildNumber) &&
		chunk.ObjectSize > 0 && chunk.Seq >= 0 && chunk.Seq < chunk.Total
}

// onRestartSourceChunk serves one chunk of a restart copy. A copy is
// posted after its run finalizes, so the terminal-status drop of the mesh
// path does not apply. Copy chunks use their own entry, so a refusal of
// one kind never blocks the other.
func (h *BusHandler) onRestartSourceChunk(name, ns string, chunk *mitev1.RunStateChunk) {
	ctx := context.Background()
	if !validRestartSourceChunk(chunk) {
		h.rejectRestartSource(ctx, ns, name, chunk, stateReasonMalformed)
		return
	}

	instance := h.lookupCellInstance(ns, name)
	decision, _ := h.checkOwner(ctx, h.cluster, ns, name, instance, chunk.RunId)
	switch decision {
	case ownerForeign:
		h.rejectRestartSource(ctx, ns, name, chunk, stateReasonForeignRun)
		return
	case ownerUnknown:
		h.sendRunStateAckKind(ns, name, chunk, stateAckStatusBusy, stateReasonBudgetFull, stateBudgetPauseMinMS, true)
		return
	}

	outcome, e, complete := h.bufferRestartSourceChunk(ns, name, chunk)
	if complete != nil {
		body, rawTotal, err := inflateRunStateSet(complete)
		if err != nil || rawTotal != complete.objectSize {
			outcome = runStateOutcome{status: stateAckStatusRejected, reason: stateReasonMalformed}
		} else {
			outcome = h.storeRestartSourceObject(ctx, ns, name, e, chunk, body)
		}
	}
	if outcome.status == stateAckStatusRejected {
		h.metrics.runStateRejected.Add(ctx, 1, metric.WithAttributes(
			attribute.String("reason", outcome.reason),
			attribute.String("kind", restartSourceKind),
		))
	}

	h.sendRunStateAckKind(ns, name, chunk, outcome.status, outcome.reason, outcome.pauseMS, true)
}

// rejectRestartSource answers one refusal of a restart-copy chunk. A
// refusal publishes no envelope, because no file was recorded.
func (h *BusHandler) rejectRestartSource(ctx context.Context, ns, name string, chunk *mitev1.RunStateChunk, reason string) {
	h.metrics.runStateRejected.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason),
		attribute.String("kind", restartSourceKind),
	))
	h.sendRunStateAckKind(ns, name, chunk, stateAckStatusRejected, reason, 0, true)
}

// bufferRestartSourceChunk applies the epoch fence, the caps, and the
// buffering of one restart-copy chunk. It returns the outcome, the entry,
// and the completed buffer once every sequence of the file is present.
func (h *BusHandler) bufferRestartSourceChunk(ns, name string, chunk *mitev1.RunStateChunk) (runStateOutcome, *restartSourceEntry, *runStateBuffer) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	if h.restartRuns == nil {
		h.restartRuns = make(map[string]*restartSourceEntry)
	}
	now := time.Now()
	e, ok := h.restartRuns[chunk.RunId]
	if !ok {
		e = &restartSourceEntry{
			ns: ns, name: name, jobPath: chunk.JobPath, buildNumber: chunk.BuildNumber,
			epoch: chunk.PusherEpoch, copyFiles: chunk.CopyFiles, copyBytes: chunk.CopyBytes,
			buffers: make(map[string]*runStateBuffer),
			stored:  make(map[string]int64),
		}
		h.restartRuns[chunk.RunId] = e
	}
	e.touched = now
	// The sweep runs on every chunk, after the current entry is fresh. An
	// idle copy of any run is then refunded, not only when a new copy starts.
	h.sweepRestartSourcesLocked(now)

	switch {
	case chunk.PusherEpoch < e.epoch:
		// A chunk of a process that is gone. Its copy was superseded.
		return runStateOutcome{status: stateAckStatusStored}, e, nil
	case chunk.PusherEpoch > e.epoch:
		// A newer process posts the copy again, so the older copy resets.
		h.dropRestartBuffersLocked(e)
		e.epoch = chunk.PusherEpoch
		e.copyFiles = chunk.CopyFiles
		e.copyBytes = chunk.CopyBytes
		e.stored = make(map[string]int64)
		e.storedTotal = 0
	case chunk.CopyFiles != e.copyFiles || chunk.CopyBytes != e.copyBytes ||
		chunk.JobPath != e.jobPath || chunk.BuildNumber != e.buildNumber:
		// The same process named a different copy. Its chunks disagree, so
		// none of them is trusted.
		return runStateOutcome{status: stateAckStatusRejected, reason: stateReasonMalformed}, e, nil
	}

	// A file already stored answers as a redelivery.
	if _, done := e.stored[chunk.Relpath]; done {
		return runStateOutcome{status: stateAckStatusStored}, e, nil
	}

	// The caps refuse a new file. The file count includes the files still
	// arriving, so a copy never admits more files than it declared. The
	// path's buffer goes with the refusal.
	files := int64(len(e.stored) + len(e.buffers))
	if _, buffered := e.buffers[chunk.Relpath]; !buffered {
		files++
	}
	if files > e.copyFiles || e.storedTotal+chunk.ObjectSize > e.copyBytes {
		h.dropRestartBufferLocked(e, chunk.Relpath)
		return runStateOutcome{status: stateAckStatusRejected, reason: stateReasonStateCap}, e, nil
	}

	size := int64(len(chunk.GzipContent))
	if h.stateTotalPendingBytes+size > h.logPendingMaxBytesOrDefault() {
		if h.stateBudgetCrossedAt.IsZero() {
			h.stateBudgetCrossedAt = now
		}
		pause := h.stateBudgetPauseMSLocked()
		return runStateOutcome{status: stateAckStatusBusy, reason: stateReasonBudgetFull, pauseMS: pause}, e, nil
	}
	h.stateBudgetCrossedAt = time.Time{}

	b := e.buffers[chunk.Relpath]
	if b == nil {
		b = &runStateBuffer{
			epoch: chunk.PusherEpoch, stateGen: chunk.StateGen, objectSize: chunk.ObjectSize,
			total: chunk.Total, chunks: make(map[int64][]byte),
		}
		e.buffers[chunk.Relpath] = b
	}
	if b.objectSize != chunk.ObjectSize || b.total != chunk.Total {
		h.dropRestartBufferLocked(e, chunk.Relpath)
		return runStateOutcome{status: stateAckStatusRejected, reason: stateReasonMalformed}, e, nil
	}
	// A retried sequence replaces its earlier copy, so the earlier bytes
	// leave the bound before the new ones enter it.
	if old, again := b.chunks[chunk.Seq]; again {
		b.pending -= int64(len(old))
		h.stateTotalPendingBytes -= int64(len(old))
		if h.stateTotalPendingBytes < 0 {
			h.stateTotalPendingBytes = 0
		}
	}
	b.chunks[chunk.Seq] = chunk.GzipContent
	b.pending += size
	h.stateTotalPendingBytes += size

	if int64(len(b.chunks)) < b.total {
		if chunk.Seq == b.total-1 {
			h.dropRestartBufferLocked(e, chunk.Relpath)
			return runStateOutcome{status: stateAckStatusRejected, reason: stateReasonIncomplete}, e, nil
		}
		return runStateOutcome{status: stateAckStatusStored}, e, nil
	}

	h.dropRestartBufferLocked(e, chunk.Relpath)
	return runStateOutcome{stored: true, relpath: chunk.Relpath, bytes: b.objectSize, seq: chunk.Seq}, e, b
}

// storeRestartSourceObject writes one whole file of the copy, publishes its
// envelope, and records it on the entry. The entry is deleted once every
// declared file is stored.
func (h *BusHandler) storeRestartSourceObject(ctx context.Context, ns, name string, e *restartSourceEntry, chunk *mitev1.RunStateChunk, body []byte) runStateOutcome {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	opts := blob.ObjectOptions{
		ObjectClass: runStateObjectClass,
		ContentType: runStateContentType,
		CreatedAt:   time.Now().UTC(),
	}
	key := blob.RestartSourceKey(chunk.RunId, chunk.Relpath)
	if err := h.logStore.Replace(ctx, key, bytes.NewReader(body), opts); err != nil {
		h.log().Warn("restart copy write failed", "run_id", chunk.RunId, "relpath", chunk.Relpath, "error", err)
		return runStateOutcome{status: stateAckStatusBusy, reason: stateReasonBudgetFull, pauseMS: stateBudgetPauseMinMS}
	}

	// The envelope publishes before the file enters the stored set. A STORED
	// ack tells the mite that the recorder holds the file. A failed publish
	// answers BUSY, and the plugin posts the file again.
	if err := h.publishRestartSourceFile(ns, name, chunk, int64(len(body))); err != nil {
		return runStateOutcome{status: stateAckStatusBusy, reason: stateReasonBudgetFull, pauseMS: stateBudgetPauseMinMS}
	}

	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.restartRuns[chunk.RunId] == e {
		if _, done := e.stored[chunk.Relpath]; !done {
			e.stored[chunk.Relpath] = int64(len(body))
			e.storedTotal += int64(len(body))
		}
		if int64(len(e.stored)) == e.copyFiles {
			delete(h.restartRuns, chunk.RunId)
		}
	}
	h.metrics.runStateBytes.Add(ctx, int64(len(body)))
	return runStateOutcome{status: stateAckStatusStored, stored: true, relpath: chunk.Relpath, bytes: int64(len(body))}
}

// publishRestartSourceFile publishes the run.state envelope of one stored
// file of the copy. It returns the publish error.
func (h *BusHandler) publishRestartSourceFile(ns, name string, chunk *mitev1.RunStateChunk, size int64) error {
	return h.publishRunStateEnvelope(ns, name, chunk, map[string]interface{}{
		"relpath":        chunk.Relpath,
		"bytes":          size,
		"pusher_epoch":   chunk.PusherEpoch,
		"restart_source": true,
		"copy_files":     chunk.CopyFiles,
		"copy_bytes":     chunk.CopyBytes,
	})
}

// dropRestartBufferLocked removes one file's buffer and refunds its
// pending bytes. Caller must hold h.stateMu.
func (h *BusHandler) dropRestartBufferLocked(e *restartSourceEntry, relpath string) {
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

// dropRestartBuffersLocked removes every buffer of one copy and refunds
// their pending bytes. Caller must hold h.stateMu.
func (h *BusHandler) dropRestartBuffersLocked(e *restartSourceEntry) {
	for relpath := range e.buffers {
		h.dropRestartBufferLocked(e, relpath)
	}
}

// sweepRestartSourcesLocked drops every copy that has been idle past the
// TTL and refunds its buffers. Caller must hold h.stateMu.
func (h *BusHandler) sweepRestartSourcesLocked(now time.Time) {
	for runID, e := range h.restartRuns {
		if now.Sub(e.touched) < restartSourceIdleTTL {
			continue
		}
		h.dropRestartBuffersLocked(e)
		delete(h.restartRuns, runID)
	}
}
