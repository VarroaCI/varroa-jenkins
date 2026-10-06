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
	// Writer is the "namespace/cell/cell_instance" of the sender of the
	// chunk. A restarted gateway reads it back to tell a moved build's
	// first chunk from a repeat of the same writer. A crash-repair entry
	// carries the writer of the part it repairs.
	Writer string `json:"writer,omitempty"`
}

// indexWriter answers the writer of the highest entry at or below seq
// that names one, or "" when none does. blob.UnindexedLogParts takes it
// as the writer every recovered part must match.
func indexWriter(entries map[int64]LogIndexEntry, seq int64) string {
	for ; seq >= 0; seq-- {
		if w := entries[seq].Writer; w != "" {
			return w
		}
	}
	return ""
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

// BuildLogManifest reads every logindex/<run_id> entry, then the length
// of logs/<run_id> up to the bytes a reader may serve, and computes the
// manifest. summary is the run record's LogSummary, or nil when the run
// has none.
//
// An index entry whose offset is at or above bytesRead is a hole: its
// metadata exists but its bytes are unreachable, because C1's range
// reader stops at the first missing part and hides every later one. A
// part below bytesRead that no entry covers is an unattributed chunk,
// not a hole. A writer crash before the index append, or inside the
// index flush, leaves such parts above the highest entry, and their
// bytes are still served. blob.UnindexedLogParts names them, with the
// same rule the gateway's restart recovery uses. No byte past those
// parts counts: a lost cell's late part above the index is not the
// run's log.
//
// The index is read before the bytes. An indexed part is never written
// again, so the bound stays true while the bytes are read.
//
// A run with no summary has an unknown declared chunk count. The
// manifest then derives it from the index's own highest sequence, counts
// each unattributed part as one more chunk, and always reports
// incomplete true with the reason no-summary.
func BuildLogManifest(ctx context.Context, store blob.Store, runID string, summary *StoredLogSummary) (LogManifest, error) {
	entries, maxSeq, finalSeen, err := readLogIndex(ctx, store, blob.LogIndexKey(runID))
	if err != nil {
		return LogManifest{}, err
	}

	// maxEntryEnd is the byte total the index's entries account for.
	// Readable bytes past it belong to the parts directly above maxSeq
	// that no entry names. Those parts are unattributed, served data, not
	// holes, so they are computed before the hole scan below, and the
	// scan then skips them by identity.
	var maxEntryEnd int64
	for _, e := range entries {
		if end := e.Offset + e.Len; end > maxEntryEnd {
			maxEntryEnd = end
		}
	}
	parts, _, err := blob.UnindexedLogParts(ctx, store, runID, maxSeq+1, indexWriter(entries, maxSeq))
	if err != nil {
		return LogManifest{}, err
	}
	accepted := maxEntryEnd
	unattributed := map[int64]bool{}
	for _, part := range parts {
		accepted += part.Size
	}

	bytesRead, err := logObjectLength(ctx, store, blob.LogKey(runID), accepted)
	if err != nil {
		return LogManifest{}, err
	}
	if bytesRead > maxEntryEnd {
		for _, part := range parts {
			unattributed[part.Seq] = true
		}
	}

	var declaredCount int64
	noSummary := summary == nil
	if !noSummary {
		declaredCount = summary.ChunkCount
	} else {
		// A run with no summary cannot know its declared count in
		// advance. Each unattributed part is real served data: count it
		// as one more chunk rather than silently dropping it.
		declaredCount = maxSeq + 1 + int64(len(unattributed))
	}

	var missing []int64
	for seq := int64(0); seq < declaredCount; seq++ {
		if unattributed[seq] {
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

// logObjectLength reads at most limit bytes of key, or every byte for
// limit -1, and returns the byte count. A missing object (no part ever
// written) reports zero, not an error.
func logObjectLength(ctx context.Context, store blob.Store, key string, limit int64) (int64, error) {
	if limit == 0 {
		return 0, nil
	}
	r, err := store.NewRangeReader(ctx, key, 0, limit)
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
