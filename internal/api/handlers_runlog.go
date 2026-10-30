// GET /runs/{run}/log (design D5; run-log-viewer, log-live-tail): plays
// the run's history, then attaches the live tail while the run is not
// terminal, polling the run record every 10 seconds until the log is
// finalized. The read-and-authorize step reuses handlers_runs.go's
// resolveRunOrNotFound, the run-scoped surfaces' shared seam.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// runLogFlusher is the flush capability writeRunLogEvent needs.
// http.Flusher satisfies it; a test's own writer can satisfy it too,
// without needing to implement the rest of http.ResponseWriter.
type runLogFlusher interface{ Flush() }

// parseRunLogFrom reads the from query parameter: the byte offset of the
// first byte the client has not read. An absent or invalid value
// defaults to zero — the initial load starts at the object's start, so a
// permalink resolves on a cold load.
func parseRunLogFrom(r *http.Request) int64 {
	raw := r.URL.Query().Get("from")
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseRunLogFromLine reads the fromLine query parameter: the 1-based
// line number the client already knows starts at from. It is always
// optional (design D5). An absent or invalid value reports false, and
// the caller falls back to the line numbers splitRunLogLines already
// computed by counting newlines from offset zero.
func parseRunLogFromLine(r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("fromLine")
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// runLogBuildNumber reads run's build number, or zero when the run has
// none yet (a queued run with no build number assigned).
func runLogBuildNumber(run recordstore.Run) int64 {
	if run.BuildNumber == nil {
		return 0
	}
	return *run.BuildNumber
}

// runLogIsTerminal reports whether status is a runs.status value after
// which no further build event can arrive. StatusDuplicate is terminal
// exactly like StatusCompleted: a duplicate run's build number belongs
// to the run it duplicates, and its own run never reaches the gateway,
// so a live tail attached to it would watch a build that was never
// dispatched.
func runLogIsTerminal(status string) bool {
	return status == recordstore.StatusCompleted || status == recordstore.StatusDuplicate
}

func (s *Server) handleStreamRunLog(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	if s.deps.BlobStore == nil {
		s.pipelinesUnavailable(w)
		return
	}

	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	run, stages, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}

	if r.URL.Query().Get("raw") == "true" {
		consoleData, err := readRunLogObject(ctx, s.deps.BlobStore, run.RunID)
		if err != nil {
			s.deps.Logger.Error("read run log", "run_id", run.RunID, "error", err)
			s.writeJSONError(w, http.StatusInternalServerError, "read run log failed")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stripConsoleNotes(consoleData))
		return
	}

	flusher, flushOK := w.(http.Flusher)
	if !flushOK {
		s.writeJSONError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// The live subject is subscribed before history is read, not after.
	// C3 durably writes a chunk before it publishes it, so a
	// subscription active before the history read starts is guaranteed
	// to either see the chunk in the read (durable write landed first)
	// or receive it on the channel once the live loop starts (publish
	// landed first, and the channel buffers it in the meantime). Design
	// D5's dedup section presupposes exactly this: it describes a live
	// chunk arriving "while the handler is still playing history," which
	// needs the subscription active during that read, not only after it
	// as the "Live tail" step list's own order would otherwise read.
	// Subscribing after the read, as the step list's literal order
	// suggests, leaves a chunk durably written in between unreachable by
	// either path: the read has already happened, and core NATS keeps no
	// backlog for a subscriber that joins late.
	var liveCh <-chan []byte
	if s.runLogTailReg != nil && !runLogIsTerminal(run.Status) {
		ch, release, tailErr := s.runLogTailReg.attach(run.Cluster, run.Namespace, run.Cell, run.RunID, run.JobPath, runLogBuildNumber(run))
		if tailErr == nil {
			liveCh = ch
			defer release()
		} else {
			s.deps.Logger.Warn("attach run log tail failed", "run_id", run.RunID, "error", tailErr)
		}
	}

	consoleData, err := readRunLogObject(ctx, s.deps.BlobStore, run.RunID)
	if err != nil {
		s.deps.Logger.Error("read run log", "run_id", run.RunID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "read run log failed")
		return
	}

	entries, maxSeq, err := readRunLogIndex(ctx, s.deps.BlobStore, blob.LogIndexKey(run.RunID))
	if err != nil {
		s.deps.Logger.Error("read run log index", "run_id", run.RunID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "read run log index failed")
		return
	}

	summary, hasSummary := parseRunLogSummary(run.Payload)
	var summaryPtr *runLogSummaryFields
	if hasSummary {
		summaryPtr = &summary
	}
	unattributed, err := readRunLogUnattributed(ctx, s.deps.BlobStore, run.RunID, int64(len(consoleData)), entries, maxSeq)
	if err != nil {
		s.deps.Logger.Error("list run log parts", "run_id", run.RunID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list run log parts failed")
		return
	}
	manifest := computeRunLogManifest(int64(len(consoleData)), entries, maxSeq, unattributed, summaryPtr)
	lines := splitRunLogLines(consoleData, entries)
	attr := newRunLogStageAttributor(stages)
	for i := range lines {
		attr.attribute(&lines[i])
	}
	// An unfinished build's snapshot may end mid-line. That fragment is
	// held back for the live assembler, which completes it when the next
	// chunk arrives; emitting it here would split one console line in two.
	// A queued run counts as unfinished too: its log can land before the
	// recorder processes run.started. A finished build has no next chunk,
	// so its fragment stays a line.
	var tailFragment []byte
	tailOff := int64(-1)
	if !runLogIsTerminal(run.Status) && len(consoleData) > 0 && consoleData[len(consoleData)-1] != '\n' && len(lines) > 0 {
		last := lines[len(lines)-1]
		tailFragment = append([]byte(nil), consoleData[last.Off:]...)
		tailOff = last.Off
		lines = lines[:len(lines)-1]
	}
	firstErr := computeFirstErrorLine(lines)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	if writeRunLogEvent(w, flusher, "meta", runLogMetaEvent{
		RunID: run.RunID, ChunkCount: manifest.ChunkCount, ByteCount: manifest.ByteCount,
		DroppedBytes: manifest.DroppedBytes, TruncatedAt: manifest.TruncatedAt, MissingSeq: manifest.MissingSeq,
		Complete: manifest.Complete, Status: run.Status, FirstErrorLine: firstErr,
	}) != nil {
		return
	}

	for _, st := range stages {
		if writeRunLogEvent(w, flusher, "stage", buildRunLogStageEvent(st.StageID, st.Name, st.Status, lines)) != nil {
			return
		}
	}

	from := parseRunLogFrom(r)
	fromLine, hasFromLine := parseRunLogFromLine(r)
	resumed := lines[resumeLineIndex(lines, from):]

	// nextLiveLineNumber is the number the live assembler starts at,
	// once history stops emitting lines. Its default is the object's own
	// next absolute number; fromLine, when present, overrides every
	// emitted line's number from here on, so the live path must continue
	// that same override rather than reverting to the absolute count.
	nextLiveLineNumber := int64(len(lines) + 1)
	if hasFromLine {
		nextLiveLineNumber = fromLine + int64(len(resumed))
	}

	n := fromLine
	for _, ln := range resumed {
		if hasFromLine {
			ln.N = n
			n++
		}
		if writeRunLogEvent(w, flusher, "line", runLogLineToEvent(ln)) != nil {
			return
		}
	}

	for _, seq := range manifest.MissingSeq {
		off := int64(0)
		if e, entryOK := entries[seq]; entryOK {
			off = e.Offset
		}
		if writeRunLogEvent(w, flusher, "gap", runLogGapEvent{Seq: seq, Off: off}) != nil {
			return
		}
	}

	if writeRunLogEvent(w, flusher, "live", runLogLiveEvent{Active: liveCh != nil}) != nil {
		return
	}

	assembler := newRunLogLiveAssembler(manifest.ByteCount, nextLiveLineNumber)
	assembler.attr = attr
	if tailOff >= 0 {
		assembler.seedPartial(tailFragment, tailOff)
	}
	known := initialKnownStages(stages)

	ticker := time.NewTicker(runLogPollInterval)
	defer ticker.Stop()
	s.runLogStreamLoop(ctx, w, flusher, run, known, ticker.C, liveCh, assembler)
}

// runLogStreamLoop drains the live-tail channel (nil when no tail is
// attached) and re-reads the run record on every tick, until the run's
// finalized LogSummary emits end or ctx ends. ticks is a channel rather
// than a ticker so a test can drive it by hand (run-log-viewer, "the
// finalized flag ends the stream"; "the handler reads the run record
// every 10 seconds while the stream is open, tail or no tail").
func (s *Server) runLogStreamLoop(
	ctx context.Context,
	w io.Writer,
	flusher runLogFlusher,
	run recordstore.Run,
	known map[string]string,
	ticks <-chan time.Time,
	live <-chan []byte,
	assembler *runLogLiveAssembler,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case raw, chOK := <-live:
			if !chOK {
				live = nil
				continue
			}
			if s.emitRunLogLiveChunk(ctx, w, flusher, run, assembler, raw) {
				return
			}
		case <-ticks:
			if s.pollRunLogOnce(ctx, w, flusher, run, &known, assembler) {
				return
			}
		}
	}
}

// emitRunLogLiveChunk decodes one raw relay message and emits every line
// it (and any buffered partial) now yields. done is true when the stream
// must end: a write failed, or a gap could not be filled. A malformed
// message is dropped silently, because the next chunk then starts after
// the watermark and the gap path fills its bytes from the blob store.
//
// A chunk that starts after the watermark means the relay lost at least
// one chunk. The handler then fills the gap from the blob store, which
// holds every relayed chunk before its relay message is published. When
// that read fails or ends before the chunk does, the stream ends without
// an end event, and the client reconnects with its own from offset.
func (s *Server) emitRunLogLiveChunk(ctx context.Context, w io.Writer, flusher runLogFlusher, run recordstore.Run, assembler *runLogLiveAssembler, raw []byte) (done bool) {
	var chunk runLiveLogChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return false
	}
	lines, gap := assembler.feed(chunk)
	if gap {
		resynced, ok := s.resyncRunLogFromBlob(ctx, run, assembler, chunk.Offset+chunk.Len)
		if !ok {
			return true
		}
		lines = resynced
	}
	for _, ln := range lines {
		if writeRunLogEvent(w, flusher, "line", runLogLineToEvent(ln)) != nil {
			return true
		}
	}
	return false
}

// resyncRunLogFromBlob moves assembler to the blob store's copy of the
// log and returns the lines past its position. need is the lowest blob
// length the caller accepts: the end offset of the chunk that showed a
// gap, or the watermark at the end of the stream. Each gap, and the end
// of each stream, costs one read of the whole console object and its
// index; the read stops at the blob end. ok is false when a read fails
// or the console object ends before need, and the assembler then stays
// unchanged.
func (s *Server) resyncRunLogFromBlob(ctx context.Context, run recordstore.Run, assembler *runLogLiveAssembler, need int64) (lines []runLogLine, ok bool) {
	consoleData, err := readRunLogObject(ctx, s.deps.BlobStore, run.RunID)
	if err != nil {
		s.deps.Logger.Warn("run log tail gap: read run log failed", "run_id", run.RunID, "error", err)
		return nil, false
	}
	if int64(len(consoleData)) < need {
		s.deps.Logger.Warn("run log tail gap: blob ends before the live chunk", "run_id", run.RunID, "blob_bytes", len(consoleData), "chunk_end", need)
		return nil, false
	}
	entries, _, err := readRunLogIndex(ctx, s.deps.BlobStore, blob.LogIndexKey(run.RunID))
	if err != nil {
		s.deps.Logger.Warn("run log tail gap: read run log index failed", "run_id", run.RunID, "error", err)
		return nil, false
	}
	return assembler.resync(consoleData, splitRunLogLines(consoleData, entries)), true
}

// pollRunLogOnce re-reads run's record, emits a stage event for every
// stage diffRunLogStages reports new or changed, and — only once
// LogSummary.finalized is set — reads the blob store a last time, emits
// the lines past assembler's watermark, flushes its buffered partial
// line, emits end, and reports done true. assembler may be nil, when
// the caller tracks no live-tail state; the caller then needs no blob
// store.
func (s *Server) pollRunLogOnce(ctx context.Context, w io.Writer, flusher runLogFlusher, run recordstore.Run, known *map[string]string, assembler *runLogLiveAssembler) bool {
	latest, stages, err := s.deps.RecordStore.GetRun(ctx, run.RunID)
	if err != nil {
		// Best-effort: the stream stays open and retries on the next
		// tick rather than ending on a transient read failure.
		s.deps.Logger.Warn("poll run log record failed", "run_id", run.RunID, "error", err)
		return false
	}

	events, next := diffRunLogStages(*known, stages)
	*known = next
	for _, e := range events {
		if writeRunLogEvent(w, flusher, "stage", e) != nil {
			return true
		}
	}

	summary, hasSummary := parseRunLogSummary(latest.Payload)
	if !hasSummary || !summary.Finalized {
		return false
	}

	// The run is ending, and the blob store now holds the whole log. A
	// lost last relay message shows no gap, because no later chunk
	// arrives, and a run that stopped before the stream opened has no
	// tail at all. One last read from the blob store emits every line
	// past the watermark. When that read fails, the stream ends without
	// end, so the client reconnects and replays the finalized log.
	//
	// A fragment still buffered after that read is the log's unterminated
	// last line, so it is emitted now as its own final line, before end.
	if assembler != nil {
		lines, ok := s.resyncRunLogFromBlob(ctx, run, assembler, assembler.watermark)
		if !ok {
			return true
		}
		for _, ln := range lines {
			if writeRunLogEvent(w, flusher, "line", runLogLineToEvent(ln)) != nil {
				return true
			}
		}
		if ln, flushed := assembler.flushPartial(); flushed {
			if writeRunLogEvent(w, flusher, "line", runLogLineToEvent(ln)) != nil {
				return true
			}
		}
	}

	result := ""
	if latest.Result != nil {
		result = *latest.Result
	}
	_ = writeRunLogEvent(w, flusher, "end", runLogEndEvent{
		Result: result, LogComplete: !summary.Incomplete, IncompleteReason: summary.IncompleteReason,
	})
	return true
}
