package mite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// newManifestTestStore opens a file:// blob store rooted at a fresh temp
// dir, and writes seq-numbered chunks to key.
func newManifestTestStore(t *testing.T) blob.Store {
	t.Helper()
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	return store
}

func appendLogPart(t *testing.T, store blob.Store, runID string, seq int64, data string) {
	t.Helper()
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: runCreatedAtFromUUIDv7(runID)}
	if err := store.Append(context.Background(), blob.LogKey(runID), seq, []byte(data), opts); err != nil {
		t.Fatalf("append log seq %d: %v", seq, err)
	}
}

func appendIndexEntry(t *testing.T, store blob.Store, runID string, e LogIndexEntry) {
	t.Helper()
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal index entry: %v", err)
	}
	line = append(line, '\n')
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "application/x-ndjson", CreatedAt: runCreatedAtFromUUIDv7(runID)}
	if err := store.Append(context.Background(), blob.LogIndexKey(runID), e.Seq, line, opts); err != nil {
		t.Fatalf("append index seq %d: %v", e.Seq, err)
	}
}

// TestBuildLogManifestMissingInteriorChunk asserts a hole in the middle
// of the log — an index entry whose offset lands past bytesRead —
// reports that sequence, and every sequence after it, as missing.
func TestBuildLogManifestMissingInteriorChunk(t *testing.T) {
	runID := newTestRunID()
	store := newManifestTestStore(t)

	// seq 0 durable and indexed; seq 1 never reached the gateway (its
	// bytes are absent, so the log stops at 5 bytes); seq 2's index entry
	// exists but its bytes are unreachable behind the hole at seq 1.
	appendLogPart(t, store, runID, 0, "hello")
	appendIndexEntry(t, store, runID, LogIndexEntry{Seq: 0, Offset: 0, Len: 5})
	appendIndexEntry(t, store, runID, LogIndexEntry{Seq: 2, Offset: 10, Len: 5})

	summary := &StoredLogSummary{ChunkCount: 3, Finalized: true}
	m, err := BuildLogManifest(context.Background(), store, runID, summary)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if len(m.MissingSeq) != 2 || m.MissingSeq[0] != 1 || m.MissingSeq[1] != 2 {
		t.Fatalf("missingSeq = %v, want [1 2]", m.MissingSeq)
	}
	if !m.Incomplete {
		t.Error("expected incomplete true with a hole present")
	}
	if m.TruncatedAt == nil || *m.TruncatedAt != 1 {
		t.Errorf("truncatedAt = %v, want 1", m.TruncatedAt)
	}
}

// TestBuildLogManifestMissingTerminalRun asserts a hole at the very end
// (the run stopped short of its declared chunk count) reports every
// trailing sequence as missing.
func TestBuildLogManifestMissingTerminalRun(t *testing.T) {
	runID := newTestRunID()
	store := newManifestTestStore(t)

	appendLogPart(t, store, runID, 0, "abc")
	appendIndexEntry(t, store, runID, LogIndexEntry{Seq: 0, Offset: 0, Len: 3})

	summary := &StoredLogSummary{ChunkCount: 3, Incomplete: true, IncompleteReason: "finalize-deadline"}
	m, err := BuildLogManifest(context.Background(), store, runID, summary)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if len(m.MissingSeq) != 2 || m.MissingSeq[0] != 1 || m.MissingSeq[1] != 2 {
		t.Fatalf("missingSeq = %v, want [1 2]", m.MissingSeq)
	}
	if m.IncompleteReason != "finalize-deadline" {
		t.Errorf("incompleteReason = %q, want finalize-deadline", m.IncompleteReason)
	}
}

// TestBuildLogManifestUnattributedChunk asserts a byte range below
// bytesRead that no index entry covers — a writer crash between the log
// append and the index append, on a run that never continued — is served
// (not reported as a hole).
func TestBuildLogManifestUnattributedChunk(t *testing.T) {
	runID := newTestRunID()
	store := newManifestTestStore(t)

	appendLogPart(t, store, runID, 0, "hello")
	appendLogPart(t, store, runID, 1, " world") // durable, but its index entry never landed
	appendIndexEntry(t, store, runID, LogIndexEntry{Seq: 0, Offset: 0, Len: 5})

	summary := &StoredLogSummary{ChunkCount: 2, Finalized: false}
	m, err := BuildLogManifest(context.Background(), store, runID, summary)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if len(m.MissingSeq) != 0 {
		t.Fatalf("missingSeq = %v, want none: an unattributed chunk is served, not a hole", m.MissingSeq)
	}
	if m.ByteCount != 11 {
		t.Fatalf("byteCount = %d, want 11", m.ByteCount)
	}
}

// TestBuildLogManifestNoSummaryReportsIncomplete asserts a run with no
// LogSummary always reports incomplete with the reason no-summary, and
// still serves every stored chunk by counting an uncovered trailing byte
// range as one more chunk.
func TestBuildLogManifestNoSummaryReportsIncomplete(t *testing.T) {
	runID := newTestRunID()
	store := newManifestTestStore(t)

	appendLogPart(t, store, runID, 0, "abc")
	appendIndexEntry(t, store, runID, LogIndexEntry{Seq: 0, Offset: 0, Len: 3})
	appendLogPart(t, store, runID, 1, "def") // durable, no index entry, no summary either

	m, err := BuildLogManifest(context.Background(), store, runID, nil)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if !m.Incomplete || m.IncompleteReason != "no-summary" {
		t.Fatalf("incomplete=%v reason=%q, want true/no-summary", m.Incomplete, m.IncompleteReason)
	}
	if m.ChunkCount != 2 {
		t.Fatalf("chunkCount = %d, want 2 (the uncovered trailing range counted as one more chunk)", m.ChunkCount)
	}
	if len(m.MissingSeq) != 0 {
		t.Fatalf("missingSeq = %v, want none: every stored chunk is still served", m.MissingSeq)
	}
}

// TestBuildLogManifestFinalizedFromIndexOrSweep asserts finalized is true
// when the index holds the final entry, even with no summary, and true
// when the summary alone reports it (the sweep's backstop).
func TestBuildLogManifestFinalizedFromIndexOrSweep(t *testing.T) {
	t.Run("from the index final entry", func(t *testing.T) {
		runID := newTestRunID()
		store := newManifestTestStore(t)
		appendLogPart(t, store, runID, 0, "x")
		appendIndexEntry(t, store, runID, LogIndexEntry{Seq: 0, Offset: 0, Len: 1, Final: true})

		m, err := BuildLogManifest(context.Background(), store, runID, &StoredLogSummary{ChunkCount: 1})
		if err != nil {
			t.Fatalf("build manifest: %v", err)
		}
		if !m.Finalized {
			t.Error("expected finalized true from the index final entry")
		}
	})

	t.Run("from the sweep with no final entry", func(t *testing.T) {
		runID := newTestRunID()
		store := newManifestTestStore(t)
		appendLogPart(t, store, runID, 0, "x")
		appendIndexEntry(t, store, runID, LogIndexEntry{Seq: 0, Offset: 0, Len: 1})

		m, err := BuildLogManifest(context.Background(), store, runID, &StoredLogSummary{ChunkCount: 1, Finalized: true, Incomplete: true, IncompleteReason: "not-finalized"})
		if err != nil {
			t.Fatalf("build manifest: %v", err)
		}
		if !m.Finalized {
			t.Error("expected finalized true from the sweep's summary")
		}
	})
}
