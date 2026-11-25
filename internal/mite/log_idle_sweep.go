package mite

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Bounds of the idle sweep. The sweep drops the log state of a run that
// ended, or that another writer took, while this gateway got no terminal
// mark for it: the COMPLETED event went to another gateway replica, its
// owner lookup failed, the cell was lost, or the run moved.
const (
	// logIdleSweepInterval is the time between two rounds.
	logIdleSweepInterval = time.Minute

	// logIdleSweepAfter is how long a state must show no activity before
	// a round reads its owner. A live run that still sends chunks gets
	// its COMPLETED event on the same stream, so a round reads only the
	// states that stopped hearing from their run.
	logIdleSweepAfter = 5 * time.Minute

	// logIdleSweepLookups bounds the ledger requests of one round. A round
	// sends them one at a time. An owner-cache hit that keeps a state costs
	// none, and each drop costs one. A round ends at the bound or at the
	// first failed lookup, so a ledger outage costs one lookup timeout per
	// round. The states it did not reach come first in the next round.
	logIdleSweepLookups = 64
)

// idleSweepAction is what one round does with one state.
type idleSweepAction int

const (
	// idleSweepKeep leaves the state as it is.
	idleSweepKeep idleSweepAction = iota
	// idleSweepRelease replaces the state with a tombstone. The run is
	// terminal in the ledger and past its finalize deadline, so every
	// gateway refuses its later chunks with finalize-deadline.
	idleSweepRelease
	// idleSweepForget drops the state and keeps no tombstone. Another
	// writer owns the run, or the ledger holds no row for it.
	idleSweepForget
)

// idleSweepCandidate is what a round reads of one idle state before its
// owner read. ns, name, and instance name the state's writer.
type idleSweepCandidate struct {
	runID              string
	st                 *runLogState
	activitySeq        uint64
	lastSwept          time.Time
	rejected           bool
	writer             string
	ns, name, instance string
}

// startLogIdleSweep starts the sweep goroutine once. logSweepStop stops
// it and waits for the round in progress to end.
//
// The sweep reads h.logStore, h.ownerCache, h.ledger, and the finalize
// wait with no lock. SetLogStore writes the first two and the wait before
// it starts the goroutine, NewBusHandler writes h.ledger, and nothing
// changes them while the gateway serves. SetLogStore refuses a second
// call for that reason.
func (h *BusHandler) startLogIdleSweep(every time.Duration) {
	if h.logSweepStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.runLogIdleSweep(ctx, every)
	}()
	h.logSweepStop = func() {
		cancel()
		<-done
	}
}

// runLogIdleSweep runs one round every interval until ctx ends. Each
// round first runs one resend pass of the owed finalized markers. The
// marker pass comes first because a bus outage also fails the owner
// reads, and the first failed owner read ends the round. The pass sends
// at most logMarkerResendPassMax markers, so it holds the round for a
// bounded time.
func (h *BusHandler) runLogIdleSweep(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		h.resendOwedMarkers(ctx)
		h.sweepIdleLogRuns(ctx)
	}
}

// sweepIdleLogRuns runs one round of the idle sweep. It reads the owner
// of each idle state with no terminal mark, through the owner cache
// first. A terminal run past its finalize deadline is released. A run
// that another writer owns, or that the ledger does not know, is
// forgotten. Every other state stays.
//
// The sweep publishes no run.log envelope, so it never marks a log
// incomplete. Another gateway replica can have finished the log of a run
// whose state this gateway kept, and only the store knows. A short log
// still gets its marker: the recorder's finalizeStaleLogs sweep marks
// every terminal run that no gateway finalized, and the computed manifest
// reports each hole.
//
// A cached owner can keep a state, but it never drops one. A cached
// entry can be stale: a grant or a lookup can name an earlier owner, or
// no row. Each release and each forget therefore follows a fresh
// ledger read (ownerCache.lookup), which shares a lookup already in
// flight for the run.
//
// A failed lookup is never a terminal answer: it ends the round, and the
// next round reads the owner again. The round stamps lastSwept before
// each lookup, so a run whose lookup keeps failing goes behind every
// candidate that the round did not reach. One such run therefore cannot
// hold back the other candidates. The round holds no lock during an owner
// read. It acts on a state only when the state still shows the activity
// count that it read before the owner read.
func (h *BusHandler) sweepIdleLogRuns(ctx context.Context) {
	if h.logStore == nil || h.ownerCache == nil || h.ledger == nil {
		return
	}
	now := h.logClock()
	budget := logIdleSweepLookups
	if h.logIdleSweepLookupsMax > 0 {
		budget = h.logIdleSweepLookupsMax
	}
	for _, c := range h.idleSweepCandidates(now) {
		if ctx.Err() != nil {
			return
		}
		if entry, ok := h.ownerCache.peek(c.runID); ok && h.idleSweepActionFor(c, entry, now) == idleSweepKeep {
			c.markSwept(now)
			continue
		}
		if budget <= 0 {
			return
		}
		budget--
		c.markSwept(now)
		entry, err := h.ownerCache.lookup(ctx, h.ledger, c.runID, h.cluster, c.ns, c.name, c.instance)
		if err != nil || ctx.Err() != nil {
			if ctx.Err() == nil {
				h.log().Warn("idle log sweep cannot read a run's owner, the round ends", "run_id", c.runID, "error", err)
			}
			return
		}
		switch h.idleSweepActionFor(c, entry, now) {
		case idleSweepRelease:
			h.releaseIdleRunState(c)
		case idleSweepForget:
			h.forgetIdleRunState(c)
		}
	}
}

// markSwept records that the round read, or tried to read, the owner of
// c's state. Lock: st.mu only.
func (c idleSweepCandidate) markSwept(now time.Time) {
	c.st.mu.Lock()
	c.st.lastSwept = now
	c.st.mu.Unlock()
}

// idleSweepCandidates returns every state that the round reads, the
// state read longest ago first. Lock order: h.logMu, then st.mu.
func (h *BusHandler) idleSweepCandidates(now time.Time) []idleSweepCandidate {
	h.logMu.Lock()
	var out []idleSweepCandidate
	for runID, st := range h.logRuns {
		st.mu.Lock()
		if c, ok := idleSweepCandidateLocked(runID, st, now); ok {
			out = append(out, c)
		}
		st.mu.Unlock()
	}
	h.logMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].lastSwept.Equal(out[j].lastSwept) {
			return out[i].lastSwept.Before(out[j].lastSwept)
		}
		return out[i].runID < out[j].runID
	})
	return out
}

// idleSweepCandidateLocked reads st for the round. A state is a candidate
// when it has no terminal mark, no drain runs for it, its queue is
// empty, and no chunk entered or left the queue for logIdleSweepAfter. A
// state that is about to get its first writer holds a queued chunk and a
// drain, so it is never a candidate. A drain stuck in its first recovery
// keeps its chunk queued, so it stays out of the sweep too.
//
// A state that names no writer is read for its creator's controller
// (st.ns, st.name) with no cell instance. adoptWriter runs before the
// first dequeue, so only a state that abandonRun ended before adoptWriter
// names no writer. A live answer for another instance then forgets the
// state, as a gateway restart does. The caller holds st.mu.
func idleSweepCandidateLocked(runID string, st *runLogState, now time.Time) (idleSweepCandidate, bool) {
	if st.evicted || st.terminal || st.draining || len(st.queue) > 0 || now.Sub(st.lastActivity) < logIdleSweepAfter {
		return idleSweepCandidate{}, false
	}
	ns, name, instance, ok := splitWriterKey(st.writer)
	if !ok {
		ns, name, instance = st.ns, st.name, ""
	}
	return idleSweepCandidate{
		runID: runID, st: st, activitySeq: st.activitySeq, lastSwept: st.lastSwept,
		rejected: st.rejectedForever, writer: st.writer, ns: ns, name: name, instance: instance,
	}, true
}

// splitWriterKey splits a writer key "ns/name/cell_instance" (the form of
// logWriterKey). A Kubernetes namespace and a controller name hold no
// slash, so the first two slashes end them.
func splitWriterKey(w string) (ns, name, instance string, ok bool) {
	parts := strings.SplitN(w, "/", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// idleSweepActionFor decides what the round does with c from the owner
// entry of its run, judged for c's writer. A terminal run
// whose finalize deadline passed is released: no gateway admits its
// chunks any more. A run that abandonRun ended is released at its
// terminal answer, as markRunTerminal does. A terminal run inside its
// deadline stays, because another replica can still write its log. A
// live run of another writer, and a run with no ledger row, are
// forgotten. A state that abandonRun ended stays while the ledger names
// its run live, so each later chunk stays rejected.
func (h *BusHandler) idleSweepActionFor(c idleSweepCandidate, entry ownerCacheEntry, now time.Time) idleSweepAction {
	decision, tuple := decideOwner(entry, h.cluster, c.ns, c.name, c.instance), entry.tuple
	if isTerminalRunStatus(tuple.status) && tuple.anchorUnixMs > 0 {
		deadline := time.UnixMilli(tuple.anchorUnixMs).Add(h.runFinalizeWait())
		if c.rejected || !now.Before(deadline) {
			return idleSweepRelease
		}
		return idleSweepKeep
	}
	if decision != ownerForeign {
		return idleSweepKeep
	}
	if c.rejected && tuple != (ownerTuple{}) {
		return idleSweepKeep
	}
	return idleSweepForget
}

// idleSweepStillTargetLocked reports whether c's state is unchanged since
// the round read it: the same state of the run, still idle, with no
// terminal mark, and the same activity count, writer, and rejection. A
// chunk that arrived during the owner read, an eviction, an adoption, and
// a terminal mark each fail it. The caller holds h.logMu and st.mu.
func (h *BusHandler) idleSweepStillTargetLocked(c idleSweepCandidate) bool {
	st := c.st
	return h.logRuns[c.runID] == st && !st.evicted && !st.terminal && !st.draining && len(st.queue) == 0 &&
		st.activitySeq == c.activitySeq && st.rejectedForever == c.rejected && st.writer == c.writer
}

// dropRunStateLocked removes st from h.logRuns and frees what it holds. It
// stops the writer heartbeat and the deadline timer and ends appendCtx in
// the same critical section, so a caller that sees the state gone also
// sees its context ended. A cancel and Timer.Stop take no lock of this
// package and do not wait for a running callback. The caller holds
// h.logMu and st.mu, and has checked that st holds no queued chunk, so no
// pending bytes go back to the budget.
func (h *BusHandler) dropRunStateLocked(runID string, st *runLogState) {
	st.evicted = true
	delete(h.logRuns, runID)
	if st.heartbeatCancel != nil {
		st.heartbeatCancel()
		st.heartbeatCancel = nil
	}
	if st.deadlineTimer != nil {
		st.deadlineTimer.Stop()
		st.deadlineTimer = nil
	}
	if st.appendCancel != nil {
		st.appendCancel()
	}
	st.queue, st.pendingEntries, st.pendingData = nil, nil, nil
}

// releaseIdleRunState replaces c's state with a tombstone. A run whose
// log is not finished gets a rejected tombstone, so each later chunk gets
// REJECTED{finalize-deadline}, the answer that the owner lookup gives
// once the deadline passed. A later COMPLETED event creates no state.
// The tombstone keeps the state's own incompleteness, which this
// gateway already published when it set it.
//
// Each sequence held pending above a hole gets REJECTED{finalize-
// deadline}, so its writer releases its copy. The sweep publishes
// nothing. Lock order: h.logMu, then st.mu; the acks go out after both
// are released.
func (h *BusHandler) releaseIdleRunState(c idleSweepCandidate) {
	st := c.st
	h.logMu.Lock()
	st.mu.Lock()
	if !h.idleSweepStillTargetLocked(c) {
		st.mu.Unlock()
		h.logMu.Unlock()
		return
	}
	finished := st.finalDurable && len(st.pendingEntries) == 0
	tomb := logRunTombstone{
		rejected:         st.rejectedForever || !finished,
		finalDurable:     st.finalDurable,
		ackSeq:           st.ackSeq,
		incomplete:       st.incomplete,
		incompleteReason: st.incompleteReason,
		writer:           st.writer,
	}
	stranded := make([]int64, 0, len(st.pendingEntries))
	for seq := range st.pendingEntries {
		stranded = append(stranded, seq)
	}
	ns, name := st.ns, st.name
	h.dropRunStateLocked(c.runID, st)
	h.tombstonesLocked().put(c.runID, tomb, time.Now(), logTombstoneTTL, logTombstoneMax)
	st.mu.Unlock()
	h.logMu.Unlock()

	sort.Slice(stranded, func(i, j int) bool { return stranded[i] < stranded[j] })
	h.log().Info("idle log sweep released the log state of a run that ended with no terminal mark on this gateway",
		"run_id", c.runID, "writer", c.writer, "ack_seq", tomb.ackSeq, "stranded", len(stranded))
	for _, seq := range stranded {
		h.sendLogAck(ns, name, c.runID, seq, logAckStatusRejected, logReasonFinalizeDeadline, 0)
	}
}

// forgetIdleRunState drops c's state and keeps no tombstone, as a
// gateway restart does for that run. The ledger names another writer, or
// holds no row. A chunk of the writer that owns the run creates a new
// state, which recoverRunState rebuilds from the store, and adoptWriter
// then empties the old writer's parts above the hole. A chunk of the old
// writer gets REJECTED{foreign-run} from the owner check. The sweep sends
// no ack: the controller's current stream can belong to the new writer,
// and a REJECTED ack would release that writer's retained chunks. Lock
// order: h.logMu, then st.mu.
func (h *BusHandler) forgetIdleRunState(c idleSweepCandidate) {
	st := c.st
	h.logMu.Lock()
	st.mu.Lock()
	if !h.idleSweepStillTargetLocked(c) {
		st.mu.Unlock()
		h.logMu.Unlock()
		return
	}
	h.dropRunStateLocked(c.runID, st)
	st.mu.Unlock()
	h.logMu.Unlock()
	h.log().Info("idle log sweep dropped the log state of a run that this writer no longer owns",
		"run_id", c.runID, "writer", c.writer)
}
