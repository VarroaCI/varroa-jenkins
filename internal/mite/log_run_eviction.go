package mite

import (
	"container/list"
	"context"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// Bounds of the tombstones that replace a finished run's log state.
const (
	// logTombstoneTTL is how long a tombstone lives. It must not be
	// shorter than ownerMatchTTL. When a tombstone expires, every
	// owner-cache entry from before the end of the run has expired too.
	// A late chunk then reads the run's status and anchor from the ledger.
	logTombstoneTTL = ownerMatchTTL

	// logTombstoneMax bounds the number of tombstones. At the bound, the
	// oldest tombstone goes first.
	logTombstoneMax = 50000

	// logTombstoneReadTimeout bounds the stored-part read that compares a
	// moved writer's repeat with the part a finalized run holds.
	logTombstoneReadTimeout = 5 * time.Second

	// logCompareWorkers bounds the compare reads in flight across the
	// gateway. Each read holds one store call, so the bound keeps a
	// replay of many repeats from filling the store's connection pool.
	logCompareWorkers = 8

	// logCompareQueueMax bounds the repeats that wait for a compare read.
	// A repeat over the bound gets no compare.
	logCompareQueueMax = 1024
)

// logRunTombstone is what the gateway keeps of a run after it drops the
// run's state. It holds the answer for each later chunk of the run, and
// the log facts that the run.log envelope of a COMPLETED event reads.
type logRunTombstone struct {
	// rejected is true when abandonRun ended the run, or when the idle
	// sweep released a run that ended past its finalize deadline with
	// its log not finished. Every later chunk gets
	// REJECTED{finalize-deadline}.
	rejected bool
	// finalDurable is true when the final index entry is durable.
	finalDurable bool
	// ackSeq is the last STORED ack_seq of the run. A finalized run
	// answers a chunk at or below it with STORED{ackSeq}.
	ackSeq           int64
	incomplete       bool
	incompleteReason string
	// writer is the "ns/name/cell_instance" of the run's last writer. A
	// repeat of another writer is compared with the stored part after
	// its answer.
	writer string
}

// gatewayGap reports whether the tombstone already records the run as
// gateway-gap. A compare read can then change nothing.
func (t logRunTombstone) gatewayGap() bool {
	return t.incomplete && t.incompleteReason == logReasonGatewayGap
}

// movedWriterCompare is one repeat of a moved writer that waits for its
// compare read. It holds the digest of the chunk's bytes, not the bytes,
// so a full queue holds a few hundred bytes per repeat.
type movedWriterCompare struct {
	ns, name, runID, jobPath string
	seq                      int64
	digest                   string
}

// logTombstoneItem is the value of one element of logTombstones.order.
type logTombstoneItem struct {
	runID   string
	t       logRunTombstone
	expires time.Time
}

// logTombstones holds the tombstones in expiry order. Every tombstone
// gets the same lifetime, so the front of order expires first. The
// caller must hold h.logMu for every method.
type logTombstones struct {
	entries map[string]*list.Element
	order   *list.List // front = oldest
}

func newLogTombstones() *logTombstones {
	return &logTombstones{entries: make(map[string]*list.Element), order: list.New()}
}

// get returns the live tombstone of runID. It drops each expired
// tombstone first.
func (s *logTombstones) get(runID string, now time.Time) (logRunTombstone, bool) {
	s.expire(now)
	el, ok := s.entries[runID]
	if !ok {
		return logRunTombstone{}, false
	}
	return el.Value.(*logTombstoneItem).t, true
}

// put stores t for runID until now plus ttl. It replaces an older
// tombstone of the same run, drops each expired tombstone, and then drops
// the oldest tombstones until at most maxCount remain.
func (s *logTombstones) put(runID string, t logRunTombstone, now time.Time, ttl time.Duration, maxCount int) {
	s.drop(runID)
	s.entries[runID] = s.order.PushBack(&logTombstoneItem{runID: runID, t: t, expires: now.Add(ttl)})
	s.expire(now)
	for s.order.Len() > maxCount {
		s.remove(s.order.Front())
	}
}

// update replaces the tombstone of runID and keeps its expiry. It does
// nothing when runID has no tombstone.
func (s *logTombstones) update(runID string, t logRunTombstone) {
	if el, ok := s.entries[runID]; ok {
		el.Value.(*logTombstoneItem).t = t
	}
}

// drop removes the tombstone of runID.
func (s *logTombstones) drop(runID string) {
	if el, ok := s.entries[runID]; ok {
		s.remove(el)
	}
}

// expire removes every tombstone whose lifetime ended at or before now.
func (s *logTombstones) expire(now time.Time) {
	for el := s.order.Front(); el != nil; el = s.order.Front() {
		if now.Before(el.Value.(*logTombstoneItem).expires) {
			return
		}
		s.remove(el)
	}
}

func (s *logTombstones) remove(el *list.Element) {
	s.order.Remove(el)
	delete(s.entries, el.Value.(*logTombstoneItem).runID)
}

// len returns the number of tombstones, live or expired.
func (s *logTombstones) len() int { return s.order.Len() }

// tombstonesLocked returns the tombstone set and creates it on first
// use. The caller must hold h.logMu.
func (h *BusHandler) tombstonesLocked() *logTombstones {
	if h.logDone == nil {
		h.logDone = newLogTombstones()
	}
	return h.logDone
}

// finishedLocked reports whether the run holds no more work. No drain
// goroutine is marked as running and the queue is empty. Also, either
// abandonRun ended a terminal run, or the final index entry is durable
// with no entry pending above it. The caller must hold st.mu.
//
// A run that abandonRun ended before its terminal mark is not finished.
// The ledger can still name that run live, so after a tombstone expired
// the owner answer could not refuse its next chunk. Only the state keeps
// that rejection, until markRunTerminal or the idle sweep evicts it at
// the run's terminal answer. abandonRun already freed its queue and its
// pending maps, so the state holds no chunk bytes.
func (st *runLogState) finishedLocked() bool {
	if st.draining || len(st.queue) > 0 {
		return false
	}
	if st.rejectedForever {
		return st.terminal
	}
	return st.finalDurable && len(st.pendingEntries) == 0
}

// evictRunState replaces a finished run's state with a tombstone. The
// caller has sent every ack the run owes: the final STORED ack, or the
// REJECTED acks of abandonRun. It does nothing when the run holds work
// again, or when another call evicted the state first.
//
// It stops the writer heartbeat and the deadline timer and ends
// appendCtx inside the same critical section (dropRunStateLocked), so
// the dropped state holds no goroutine, timer, or context that outlives
// it. finishedLocked requires an empty queue and no drain goroutine
// marked as running. Lock order: h.logMu, then st.mu.
func (h *BusHandler) evictRunState(runID string, st *runLogState) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.evicted || h.logRuns[runID] != st || !st.finishedLocked() {
		return
	}
	h.tombstonesLocked().put(runID, logRunTombstone{
		rejected:         st.rejectedForever,
		finalDurable:     st.finalDurable,
		ackSeq:           st.ackSeq,
		incomplete:       st.incomplete,
		incompleteReason: st.incompleteReason,
		writer:           st.writer,
	}, time.Now(), logTombstoneTTL, logTombstoneMax)
	h.dropRunStateLocked(runID, st)
}

// runRejectedForever reports whether abandonRun ended runID, from the
// run's state or its tombstone. It creates nothing.
func (h *BusHandler) runRejectedForever(runID string) bool {
	h.logMu.Lock()
	st, ok := h.logRuns[runID]
	if !ok {
		t, found := h.tombstonesLocked().get(runID, time.Now())
		h.logMu.Unlock()
		return found && t.rejected
	}
	h.logMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.rejectedForever
}

// hasRunLogRecord reports whether the gateway holds a state or a
// tombstone for runID.
func (h *BusHandler) hasRunLogRecord(runID string) bool {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if _, ok := h.logRuns[runID]; ok {
		return true
	}
	_, ok := h.tombstonesLocked().get(runID, time.Now())
	return ok
}

// finishedRunAnswer is the ack that a tombstone gives one chunk. lost is
// true for a chunk above a finalized run's final entry. Its lines are
// lost, so the run.log envelope must report the run short. compare is
// true for a repeat of another writer: its bytes are compared with the
// stored part after the STORED answer goes out.
type finishedRunAnswer struct {
	status, reason string
	ackSeq         int64
	lost           bool
	lostReason     string
	compare        bool
}

// answerFromTombstoneLocked answers a chunk of a run that has a
// tombstone. A finalized run answers a chunk at or below its last
// ack_seq with STORED{ackSeq}, the answer the state gave. Any other
// chunk gets REJECTED{finalize-deadline}. A chunk above a finalized
// run's final entry also turns the tombstone into a rejected one, so the
// run then answers each chunk as an abandoned run does. The caller holds
// h.logMu and has admitted the sender through checkOwner.
//
// A repeat of the run's last writer needs no store read. The state ran
// each overlap check of that writer when it first stored the chunk. A
// repeat of another writer needs the read: a mesh takeover can move a run
// whose final entry is durable, because the ledger still names the run
// RUNNING when its cell is lost before its COMPLETED event, and the
// resumed build can start its log at 0. A tombstone that already records
// gateway-gap needs no read, because a compare cannot change it.
func (h *BusHandler) answerFromTombstoneLocked(runID, ns, name string, chunk *mitev1.LogChunk, t logRunTombstone) finishedRunAnswer {
	if !t.rejected && chunk.Seq <= t.ackSeq {
		return finishedRunAnswer{
			status: logAckStatusStored, ackSeq: t.ackSeq,
			compare: t.writer != logWriterKey(ns, name, chunk) && !t.gatewayGap(),
		}
	}
	ans := finishedRunAnswer{status: logAckStatusRejected, reason: logReasonFinalizeDeadline, ackSeq: chunk.Seq}
	if t.rejected {
		return ans
	}
	t.rejected = true
	if t.incompleteReason != logReasonGatewayGap {
		t.incompleteReason = logReasonFinalizeDeadline
	}
	t.incomplete = true
	h.tombstonesLocked().update(runID, t)
	ans.lost, ans.lostReason = true, t.incompleteReason
	return ans
}

// sendFinishedRunAnswer sends ans to the chunk's sender. A lost chunk
// also publishes the incomplete marker once. A repeat of another writer
// gets its STORED answer at once, and queueMovedWriterCompare then queues
// the compare of its bytes. The answer does not depend on the compare, so
// the mite releases the chunk with no wait for the store, and a slow store
// never stalls the stream read loop.
func (h *BusHandler) sendFinishedRunAnswer(ns, name string, chunk *mitev1.LogChunk, ans finishedRunAnswer) {
	h.sendLogAck(ns, name, chunk.RunId, ans.ackSeq, ans.status, ans.reason, 0)
	if ans.compare {
		h.queueMovedWriterCompare(ns, name, chunk)
		return
	}
	if ans.lost {
		h.log().Warn("refusing a log chunk above a finished log's final entry", "run_id", chunk.RunId,
			"controller", ns+"/"+name, "seq", chunk.Seq)
		h.publishRunLogBestEffort(ns, name, chunk.RunId, chunk.JobPath, map[string]interface{}{
			"incomplete": true, "incomplete_reason": ans.lostReason,
		})
	}
}

// queueMovedWriterCompare queues the compare read of a moved writer's
// repeat. At most logCompareWorkers reads run at once across the gateway,
// and at most logCompareQueueMax repeats wait. The bound never blocks the
// stream read loop and adds no BUSY reason.
//
// The STORED ack goes out before the compare, so a repeat whose bytes
// differ from the stored part marks no gateway-gap in four cases: the
// queue is full (the varroa.gateway.log.compare.skipped counter counts
// it), the gateway stops before the compare runs, the tombstone expires
// or leaves the count bound while the repeat waits, or the store read
// fails. Close does not drain the queue: a full queue can need minutes
// of reads.
//
// A worker starts only while fewer than logCompareWorkers run, and it
// exits when the queue is empty, so an idle gateway holds no compare
// goroutine. logCompareMu is a leaf lock: the caller holds no other lock.
func (h *BusHandler) queueMovedWriterCompare(ns, name string, chunk *mitev1.LogChunk) {
	item := movedWriterCompare{
		ns: ns, name: name, runID: chunk.RunId, jobPath: chunk.JobPath,
		seq: chunk.Seq, digest: blob.Digest(chunk.Data),
	}
	queueMax, workers := logCompareQueueMax, logCompareWorkers
	if h.logCompareQueueCap > 0 {
		queueMax = h.logCompareQueueCap
	}
	if h.logCompareWorkersMax > 0 {
		workers = h.logCompareWorkersMax
	}

	h.logCompareMu.Lock()
	if len(h.logCompareQueue) >= queueMax {
		h.logCompareMu.Unlock()
		h.metrics.logCompareSkipped.Add(context.Background(), 1)
		h.log().Debug("compare queue full, a moved writer's repeat is not compared",
			"run_id", chunk.RunId, "controller", ns+"/"+name, "seq", chunk.Seq)
		return
	}
	h.logCompareQueue = append(h.logCompareQueue, item)
	start := h.logCompareRunning < workers
	if start {
		h.logCompareRunning++
	}
	h.logCompareMu.Unlock()
	if start {
		go h.runMovedWriterCompares()
	}
}

// runMovedWriterCompares is one compare worker. It takes the oldest queued
// repeat until the queue is empty, and then exits. The empty check and the
// worker count share one critical section with the enqueue, so a repeat
// queued as the last worker exits starts a new worker.
func (h *BusHandler) runMovedWriterCompares() {
	for {
		h.logCompareMu.Lock()
		if len(h.logCompareQueue) == 0 {
			h.logCompareQueue = nil
			h.logCompareRunning--
			h.logCompareMu.Unlock()
			return
		}
		item := h.logCompareQueue[0]
		h.logCompareQueue[0] = movedWriterCompare{}
		h.logCompareQueue = h.logCompareQueue[1:]
		h.logCompareMu.Unlock()
		h.compareMovedWriterRepeat(item)
	}
}

// compareMovedWriterRepeat compares a moved writer's repeat at or below a
// finalized run's ackSeq with the stored part. It holds no lock while it
// reads the part. The comparison is the one checkOldWriterPart makes on
// the live path, read-only: Append accepts the same bytes at a stored
// sequence and refuses any others. The same bytes change nothing. Other
// bytes mean that the chunk's lines are lost, so the run is marked
// gateway-gap once. A failed read marks nothing: it cannot show a loss.
// A tombstone that expired, or that already records gateway-gap, needs no
// read. No state, drain, or deadline takes part, so the compare never
// turns a finished log rejected.
func (h *BusHandler) compareMovedWriterRepeat(c movedWriterCompare) {
	if !h.tombstoneAwaitsCompare(c.runID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), logTombstoneReadTimeout)
	defer cancel()
	stamp, err := h.logStore.PartStamp(ctx, blob.LogKey(c.runID), c.seq)
	switch {
	case err != nil:
		h.log().Warn("cannot read the stored log part for a moved writer's repeat, its bytes are not compared",
			"run_id", c.runID, "controller", c.ns+"/"+c.name, "seq", c.seq, "error", err)
	case stamp.Digest != c.digest:
		h.markTombstoneGap(c)
	}
}

// tombstoneAwaitsCompare reports whether a compare of runID can still
// change its tombstone: the tombstone is live and does not yet record
// gateway-gap. An earlier compare of the same run can mark the gap while
// this repeat waits in the queue.
func (h *BusHandler) tombstoneAwaitsCompare(runID string) bool {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	t, ok := h.tombstonesLocked().get(runID, time.Now())
	return ok && !t.gatewayGap()
}

// markTombstoneGap marks a finalized run's tombstone gateway-gap and
// publishes the incomplete marker, once per run. A tombstone that is
// already gateway-gap, or that expired, publishes nothing.
func (h *BusHandler) markTombstoneGap(c movedWriterCompare) {
	h.logMu.Lock()
	t, ok := h.tombstonesLocked().get(c.runID, time.Now())
	newGap := ok && !t.gatewayGap()
	if newGap {
		t.incomplete, t.incompleteReason = true, logReasonGatewayGap
		h.tombstonesLocked().update(c.runID, t)
	}
	h.logMu.Unlock()
	if !newGap {
		return
	}
	h.log().Warn("moved writer sent a chunk on a part of the old writer, its lines are lost",
		"run_id", c.runID, "controller", c.ns+"/"+c.name, "seq", c.seq)
	h.publishRunLogBestEffort(c.ns, c.name, c.runID, c.jobPath, map[string]interface{}{
		"incomplete": true, "incomplete_reason": logReasonGatewayGap,
	})
}
