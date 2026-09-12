// The computed log manifest and the history reader (design D5;
// run-log-viewer). This reads logindex/<run_id> and logs/<run_id>
// through the blob store; it reads no run record. The run record's
// LogSummary lives in runlog_poll.go, because it is a different source
// (Run.Payload, not the blob store) that only the poll and the meta
// event's declared-count input need.
//
// internal/mite/log_manifest.go computes an equivalent-shaped manifest
// for the gateway's own use; it is not reused here. The coordinator
// ruled its StoredLogSummary type is "not the run record the BFF reads,
// and not a name a later batch should reuse for this seam" — this
// handler owns an independent copy of the whole computation for the
// same reason: it is the BFF's seam, not the gateway's.
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// runLogIndexEntry is one JSON line of logindex/<run_id>. A full entry
// carries every field; a recovery entry (the gateway's repair for an
// unindexed byte range after a restart) carries only seq, offset, len,
// and recovered.
type runLogIndexEntry struct {
	Seq       int64  `json:"seq"`
	NodeID    string `json:"nodeId,omitempty"`
	StageID   string `json:"stageId,omitempty"`
	TsMs      int64  `json:"tsMs,omitempty"`
	Offset    int64  `json:"offset"`
	Len       int64  `json:"len"`
	Dropped   int64  `json:"dropped,omitempty"`
	Final     bool   `json:"final,omitempty"`
	Recovered bool   `json:"recovered,omitempty"`
}

// readRunLogObject reads key to io.EOF and returns every byte. A missing
// object returns an empty slice, not an error: a queued run's console
// object may not exist yet (run-log-viewer, "A run with no log yet is
// not an error").
func readRunLogObject(ctx context.Context, store blob.Store, key string) ([]byte, error) {
	r, err := store.NewRangeReader(ctx, key, 0, -1)
	if err != nil {
		if errors.Is(err, blob.ErrObjectNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// readRunLogIndex reads key to io.EOF and parses each ndjson line into a
// runLogIndexEntry, keyed by seq, and reports the highest seq seen. A
// missing index returns an empty map, not an error.
func readRunLogIndex(ctx context.Context, store blob.Store, key string) (map[int64]runLogIndexEntry, int64, error) {
	entries := make(map[int64]runLogIndexEntry)
	maxSeq := int64(-1)

	r, err := store.NewRangeReader(ctx, key, 0, -1)
	if err != nil {
		if errors.Is(err, blob.ErrObjectNotFound) {
			return entries, maxSeq, nil
		}
		return nil, 0, err
	}
	defer func() { _ = r.Close() }()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e runLogIndexEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, 0, err
		}
		entries[e.Seq] = e
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return entries, maxSeq, nil
}

// runLogManifest is the computed, never-stored view of one run's console
// log the meta event carries, minus firstErrorLine and status, which the
// caller adds from elsewhere (the line scan and the run record).
type runLogManifest struct {
	ChunkCount   int64
	ByteCount    int64
	DroppedBytes int64
	TruncatedAt  *int64
	MissingSeq   []int64
	Complete     bool
}

// computeRunLogManifest scans bytesRead and entries the way
// run-log-viewer's manifest requirement states: a declared chunk count
// (from summary when the run carries one, else derived from the index
// itself), every declared sequence the index does not truthfully cover
// reported as missing, and completeness the negation of that. A run with
// no summary at all is always incomplete — its declared count is not yet
// known from anywhere but the index, so it cannot be trusted complete.
func computeRunLogManifest(bytesRead int64, entries map[int64]runLogIndexEntry, maxSeq int64, summary *runLogSummaryFields) runLogManifest {
	var maxEntryEnd int64
	for _, e := range entries {
		if end := e.Offset + e.Len; end > maxEntryEnd {
			maxEntryEnd = end
		}
	}
	unattributedSeq := int64(-1)
	if bytesRead > maxEntryEnd {
		unattributedSeq = maxSeq + 1
	}

	var declaredCount int64
	if summary != nil {
		declaredCount = summary.ChunkCount
	} else {
		declaredCount = maxSeq + 1
		if unattributedSeq >= 0 {
			declaredCount++
		}
	}

	var missing []int64
	for seq := int64(0); seq < declaredCount; seq++ {
		if seq == unattributedSeq {
			continue
		}
		// A sequence is covered when the console object holds its whole
		// range. The final chunk carries no bytes, so its offset equals
		// the object length and it is still covered.
		e, ok := entries[seq]
		if !ok || e.Offset+e.Len > bytesRead {
			missing = append(missing, seq)
		}
	}

	incomplete := len(missing) > 0
	var droppedBytes int64
	if summary != nil {
		droppedBytes = summary.DroppedBytes
		if summary.Incomplete {
			incomplete = true
		}
	} else {
		incomplete = true
	}

	var truncatedAt *int64
	if len(missing) > 0 {
		t := missing[0]
		truncatedAt = &t
	}

	return runLogManifest{
		ChunkCount:   declaredCount,
		ByteCount:    bytesRead,
		DroppedBytes: droppedBytes,
		TruncatedAt:  truncatedAt,
		MissingSeq:   missing,
		Complete:     !incomplete,
	}
}

// runLogLine is one split, note-stripped, chunk-attributed console line.
type runLogLine struct {
	N       int64  // 1-based, counted from the start of the console object
	Off     int64  // byte offset of the line's first byte
	TsMs    int64  // the attributing chunk's timestamp; zero when unattributed
	StageID string // empty for a line with no attribution
	NodeID  string // empty for a line with no attribution
	Text    string // console notes removed
}

// runLogSpan is one index entry's byte range, for attributing a line's
// offset to the chunk that produced it.
type runLogSpan struct {
	offset, end int64
	entry       runLogIndexEntry
}

// buildRunLogSpans converts entries into spans ordered by offset
// ascending, the order spanFor's scan needs.
func buildRunLogSpans(entries map[int64]runLogIndexEntry) []runLogSpan {
	spans := make([]runLogSpan, 0, len(entries))
	for _, e := range entries {
		spans = append(spans, runLogSpan{offset: e.Offset, end: e.Offset + e.Len, entry: e})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].offset < spans[j].offset })
	return spans
}

// spanFor returns the span whose range holds byte off, and whether one
// exists. spans MUST be sorted by offset ascending.
func spanFor(spans []runLogSpan, off int64) (runLogSpan, bool) {
	for _, s := range spans {
		if off >= s.offset && off < s.end {
			return s, true
		}
		if off < s.offset {
			break
		}
	}
	return runLogSpan{}, false
}

// splitRunLogLines splits consoleData into lines numbered from one,
// attributing each line to the index entry whose [offset, offset+len)
// range holds the line's first byte. A line inside a recovery-form
// entry's range, or inside no entry's range at all, carries an empty
// stageId and an empty nodeId and joins no stage group
// (run-log-viewer, "A recovery index entry carries no attribution").
func splitRunLogLines(consoleData []byte, entries map[int64]runLogIndexEntry) []runLogLine {
	spans := buildRunLogSpans(entries)

	var lines []runLogLine
	pos := int64(0)
	n := int64(1)
	total := int64(len(consoleData))
	for pos < total {
		nl := bytes.IndexByte(consoleData[pos:], '\n')
		var end int64
		if nl < 0 {
			end = total
		} else {
			end = pos + int64(nl)
		}
		text := string(stripConsoleNotes(consoleData[pos:end]))
		line := runLogLine{N: n, Off: pos, Text: text}
		if s, ok := spanFor(spans, pos); ok {
			line.TsMs = s.entry.TsMs
			if !s.entry.Recovered {
				line.StageID = s.entry.StageID
				line.NodeID = s.entry.NodeID
			}
		}
		lines = append(lines, line)
		n++
		if nl < 0 {
			break
		}
		pos = end + 1
	}
	return lines
}

// firstErrorLinePattern is run-log-viewer's case-insensitive error
// pattern.
var firstErrorLinePattern = regexp.MustCompile(`(?i)\b(error|failed|failure|exception)\b`)

// computeFirstErrorLine returns the 1-based number of the first line in
// lines whose text matches firstErrorLinePattern, or zero when none
// matches.
func computeFirstErrorLine(lines []runLogLine) int64 {
	for _, ln := range lines {
		if firstErrorLinePattern.MatchString(ln.Text) {
			return ln.N
		}
	}
	return 0
}

// resumeLineIndex returns the index into lines (sorted by Off ascending,
// which splitRunLogLines always produces) of the first line to replay
// for a client that has already consumed every byte before from. A from
// that falls inside a line resumes at the next line, never a partial one
// (run-log-viewer, "the handler SHALL start at the next newline").
// resumeLineIndex only locates the starting position; it reports no line
// number. The handler renumbers the lines it emits from that position
// when the caller supplied fromLine, and otherwise keeps the absolute
// number this reader already computed from the object's start.
func resumeLineIndex(lines []runLogLine, from int64) int {
	for i, ln := range lines {
		if ln.Off >= from {
			return i
		}
	}
	return len(lines)
}

// buildRunLogStageEvent computes one stage's boundary: the first console
// line the index attributes to it, or offset and line zero when the
// stage has produced no console byte in this history replay yet
// (design D5, "Stage spans... computes every stage span from the
// index").
func buildRunLogStageEvent(stageID, name, status string, lines []runLogLine) runLogStageEvent {
	for _, ln := range lines {
		if ln.StageID == stageID {
			return runLogStageEvent{StageID: stageID, Name: name, Status: status, StartOffset: ln.Off, StartLine: ln.N}
		}
	}
	return runLogStageEvent{StageID: stageID, Name: name, Status: status}
}
