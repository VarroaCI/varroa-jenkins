package mite

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// LogIndexEntry is one JSON line of logindex/<run_id>. A full entry
// carries every field; a crash-repair entry carries only Seq, Offset,
// Len, and Recovered (design.md D4).
type LogIndexEntry struct {
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

// LogManifest is the computed, never-stored view of one run's console
// log (design.md D4, "The manifest is computed, never stored").
type LogManifest struct {
	RunID            string  `json:"runId"`
	ChunkCount       int64   `json:"chunkCount"`
	ByteCount        int64   `json:"byteCount"`
	DroppedBytes     int64   `json:"droppedBytes"`
	TruncatedAt      *int64  `json:"truncatedAt"`
	MissingSeq       []int64 `json:"missingSeq"`
	Finalized        bool    `json:"finalized"`
	Incomplete       bool    `json:"incomplete"`
	IncompleteReason string  `json:"incompleteReason"`
}

// StoredLogSummary is the subset of a run record's LogSummary the
// manifest reads. A nil *StoredLogSummary means the run carries no
// summary at all.
type StoredLogSummary struct {
	ChunkCount       int64
	DroppedBytes     int64
	Incomplete       bool
	IncompleteReason string
	Finalized        bool
}

// BuildLogManifest reads logs/<run_id> to io.EOF, records bytesRead, and
// reads every logindex/<run_id> entry, then computes the manifest.
// summary is the run record's LogSummary, or nil when the run has none.
//
// An index entry whose offset is at or above bytesRead is a hole: its
// metadata exists but its bytes are unreachable, because C1's range
// reader stops at the first missing part and hides every later one. A
// byte range below bytesRead that no entry covers is an unattributed
// chunk, not a hole — a writer crash between the log append and the
// index append leaves exactly one such range, at most, and the bytes are
// still served.
//
// A run with no summary has an unknown declared chunk count. The
// manifest then derives it from the index's own highest sequence, counts
// an uncovered trailing byte range as one more chunk, and always reports
// incomplete true with the reason no-summary.
func BuildLogManifest(ctx context.Context, store blob.Store, runID string, summary *StoredLogSummary) (LogManifest, error) {
	bytesRead, err := logObjectLength(ctx, store, blob.LogKey(runID))
	if err != nil {
		return LogManifest{}, err
	}

	entries, maxSeq, finalSeen, err := readLogIndex(ctx, store, blob.LogIndexKey(runID))
	if err != nil {
		return LogManifest{}, err
	}

	// maxEntryEnd is the byte total the index's entries account for. A
	// trailing byte range below bytesRead that no entry covers belongs to
	// exactly one sequence, maxSeq+1 (design.md D4: the shortfall from a
	// writer crash is always exactly one entry, and always the last one).
	// That sequence is unattributed, served data — not a hole — so it is
	// computed before the hole scan below, and the scan then skips it by
	// identity rather than by re-deriving the same condition.
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
	noSummary := summary == nil
	if !noSummary {
		declaredCount = summary.ChunkCount
	} else {
		// A run with no summary cannot know its declared count in
		// advance. The uncovered trailing range, if any, is real served
		// data: count it as one more chunk rather than silently dropping
		// it.
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
		e, ok := entries[seq]
		if !ok || e.Offset >= bytesRead {
			missing = append(missing, seq)
		}
	}

	incomplete := len(missing) > 0
	incompleteReason := ""
	var droppedBytes int64
	finalized := finalSeen
	if !noSummary {
		droppedBytes = summary.DroppedBytes
		if summary.Incomplete {
			incomplete = true
			incompleteReason = summary.IncompleteReason
		}
		finalized = finalized || summary.Finalized
	} else {
		incomplete = true
		incompleteReason = "no-summary"
	}

	var truncatedAt *int64
	if len(missing) > 0 {
		t := missing[0]
		truncatedAt = &t
	}

	return LogManifest{
		RunID:            runID,
		ChunkCount:       declaredCount,
		ByteCount:        bytesRead,
		DroppedBytes:     droppedBytes,
		TruncatedAt:      truncatedAt,
		MissingSeq:       missing,
		Finalized:        finalized,
		Incomplete:       incomplete,
		IncompleteReason: incompleteReason,
	}, nil
}

// logObjectLength reads key to io.EOF and returns the byte count. A
// missing object (no part ever written) reports zero, not an error.
func logObjectLength(ctx context.Context, store blob.Store, key string) (int64, error) {
	r, err := store.NewRangeReader(ctx, key, 0, -1)
	if err != nil {
		if errors.Is(err, blob.ErrObjectNotFound) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = r.Close() }()
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// readLogIndex reads key to io.EOF and parses each ndjson line into a
// LogIndexEntry, keyed by seq. It also reports the highest seq seen and
// whether any entry carries final true. A missing index reports an empty
// map, not an error.
func readLogIndex(ctx context.Context, store blob.Store, key string) (map[int64]LogIndexEntry, int64, bool, error) {
	entries := make(map[int64]LogIndexEntry)
	var maxSeq int64 = -1
	var finalSeen bool

	r, err := store.NewRangeReader(ctx, key, 0, -1)
	if err != nil {
		if errors.Is(err, blob.ErrObjectNotFound) {
			return entries, maxSeq, finalSeen, nil
		}
		return nil, 0, false, err
	}
	defer func() { _ = r.Close() }()

	scanner := bufio.NewScanner(r)
	// A line names one index entry; grow the buffer well past the default
	// 64 KiB so a line with a long error field or many fields still scans.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e LogIndexEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, 0, false, err
		}
		entries[e.Seq] = e
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		if e.Final {
			finalSeen = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, false, err
	}
	return entries, maxSeq, finalSeen, nil
}
