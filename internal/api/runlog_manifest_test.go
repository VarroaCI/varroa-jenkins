package api

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// newRunLogTestStore opens a file-backed blob.Store rooted at a fresh
// temp directory, standing in for "a fake blob store" (task 5.1).
func newRunLogTestStore(t *testing.T) blob.Store {
	t.Helper()
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	return store
}

// putRunLogIndex writes entries as logindex/<run_id>, one ndjson line
// per entry in the given order — "a known index".
func putRunLogIndex(t *testing.T, store blob.Store, runID string, entries []runLogIndexEntry) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal index entry: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "application/x-ndjson", CreatedAt: time.Now()}
	if err := store.Put(context.Background(), blob.LogIndexKey(runID), &buf, opts); err != nil {
		t.Fatalf("put index: %v", err)
	}
}

func putRunLogConsole(t *testing.T, store blob.Store, runID string, data []byte) {
	t.Helper()
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: time.Now()}
	if err := store.Put(context.Background(), blob.LogKey(runID), bytes.NewReader(data), opts); err != nil {
		t.Fatalf("put console: %v", err)
	}
}

// --- 5.1: the history reader ---

// TestRunLogHistory_LineNumbersOffsetsStageGroups proves the reader
// numbers lines from one, attributes each line's offset and stage group
// from the index, and reports the manifest's chunk-level fields.
func TestRunLogHistory_LineNumbersOffsetsStageGroups(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-history-1"

	console := []byte("alpha\nbeta\ngamma\n")
	entries := []runLogIndexEntry{
		{Seq: 0, NodeID: "n1", StageID: "s1", TsMs: 1000, Offset: 0, Len: 6, Final: false}, // "alpha\n"
		{Seq: 1, NodeID: "n1", StageID: "s2", TsMs: 2000, Offset: 6, Len: 5, Final: false}, // "beta\n"
		{Seq: 2, NodeID: "n1", StageID: "s2", TsMs: 3000, Offset: 11, Len: 6, Final: true}, // "gamma\n"
	}
	putRunLogConsole(t, store, runID, console)
	putRunLogIndex(t, store, runID, entries)

	consoleData, err := readRunLogObject(context.Background(), store, blob.LogKey(runID))
	if err != nil {
		t.Fatalf("read console: %v", err)
	}
	idx, maxSeq, err := readRunLogIndex(context.Background(), store, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 2 {
		t.Fatalf("maxSeq = %d, want 2", maxSeq)
	}

	lines := splitRunLogLines(consoleData, idx)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(lines), lines)
	}
	want := []runLogLine{
		{N: 1, Off: 0, TsMs: 1000, StageID: "s1", NodeID: "n1", Text: "alpha"},
		{N: 2, Off: 6, TsMs: 2000, StageID: "s2", NodeID: "n1", Text: "beta"},
		{N: 3, Off: 11, TsMs: 3000, StageID: "s2", NodeID: "n1", Text: "gamma"},
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("line %d = %+v, want %+v", i, lines[i], w)
		}
	}

	summary := runLogSummaryFields{ChunkCount: 3, Finalized: true}
	manifest := computeRunLogManifest(int64(len(consoleData)), idx, maxSeq, nil, &summary)
	if manifest.ChunkCount != 3 || manifest.ByteCount != int64(len(console)) || !manifest.Complete {
		t.Fatalf("manifest = %+v", manifest)
	}
	if len(manifest.MissingSeq) != 0 || manifest.TruncatedAt != nil {
		t.Fatalf("expected no gaps, got %+v", manifest)
	}

	stageEvent := buildRunLogStageEvent("s2", "Build", "SUCCESS", lines)
	if stageEvent.StartLine != 2 || stageEvent.StartOffset != 6 {
		t.Fatalf("stage event = %+v, want first s2 line (n=2, off=6)", stageEvent)
	}
}

// TestComputeRunLogManifest_ByteLessFinalChunkIsCovered proves the
// final chunk, which carries no bytes and sits at the object length, is
// not reported as a gap, while a non-empty chunk past the object length
// still is.
func TestComputeRunLogManifest_ByteLessFinalChunkIsCovered(t *testing.T) {
	entries := map[int64]runLogIndexEntry{
		0: {Seq: 0, Offset: 0, Len: 6},
		1: {Seq: 1, Offset: 6, Len: 5},
		2: {Seq: 2, Offset: 11, Len: 0, Final: true},
	}
	summary := runLogSummaryFields{ChunkCount: 3, Finalized: true}

	manifest := computeRunLogManifest(11, entries, 2, nil, &summary)
	if len(manifest.MissingSeq) != 0 || !manifest.Complete || manifest.TruncatedAt != nil {
		t.Fatalf("byte-less final chunk reported as a gap: %+v", manifest)
	}

	entries[2] = runLogIndexEntry{Seq: 2, Offset: 11, Len: 4, Final: true}
	manifest = computeRunLogManifest(11, entries, 2, nil, &summary)
	if len(manifest.MissingSeq) != 1 || manifest.MissingSeq[0] != 2 || manifest.Complete {
		t.Fatalf("unwritten final bytes not reported as a gap: %+v", manifest)
	}
}

// TestComputeRunLogManifest_UnattributedPartsAreServed proves that each
// log part above the highest index entry is its own served sequence. A
// gateway crash inside its index flush leaves several such parts, and
// none of them is a gap.
func TestComputeRunLogManifest_UnattributedPartsAreServed(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-unattributed-parts"
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8", CreatedAt: time.Now()}
	for seq, data := range []string{"a0\n", "a1\n", "a2\n", "a3\n"} {
		if err := store.Append(context.Background(), blob.LogKey(runID), int64(seq), []byte(data), opts); err != nil {
			t.Fatalf("append part %d: %v", seq, err)
		}
	}
	putRunLogIndex(t, store, runID, []runLogIndexEntry{{Seq: 0, Offset: 0, Len: 3}})

	consoleData, err := readRunLogObject(context.Background(), store, blob.LogKey(runID))
	if err != nil {
		t.Fatalf("read console: %v", err)
	}
	entries, maxSeq, err := readRunLogIndex(context.Background(), store, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	unattributed, err := readRunLogUnattributed(context.Background(), store, runID, int64(len(consoleData)), entries, maxSeq)
	if err != nil {
		t.Fatalf("read unattributed parts: %v", err)
	}
	if len(unattributed) != 3 || !unattributed[1] || !unattributed[2] || !unattributed[3] {
		t.Fatalf("unattributed = %v, want 1, 2, and 3", unattributed)
	}

	summary := runLogSummaryFields{ChunkCount: 4}
	manifest := computeRunLogManifest(int64(len(consoleData)), entries, maxSeq, unattributed, &summary)
	if len(manifest.MissingSeq) != 0 || !manifest.Complete {
		t.Fatalf("manifest = %+v, want every part served and the log complete", manifest)
	}
	manifest = computeRunLogManifest(int64(len(consoleData)), entries, maxSeq, unattributed, nil)
	if manifest.ChunkCount != 4 || len(manifest.MissingSeq) != 0 {
		t.Fatalf("manifest with no summary = %+v, want 4 chunks and none missing", manifest)
	}
}

// TestRunLogHistory_RecoveryFormUnattributed proves every line inside a
// recovery-form chunk carries no stage or node attribution and joins no
// stage group.
func TestRunLogHistory_RecoveryFormUnattributed(t *testing.T) {
	console := []byte("normal\nrecovered one\nrecovered two\n")
	entries := map[int64]runLogIndexEntry{
		0: {Seq: 0, NodeID: "n1", StageID: "s1", TsMs: 500, Offset: 0, Len: 7},
		1: {Seq: 1, Offset: 7, Len: int64(len("recovered one\nrecovered two\n")), Recovered: true},
	}

	lines := splitRunLogLines(console, entries)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(lines), lines)
	}
	if lines[0].StageID != "s1" || lines[0].NodeID != "n1" {
		t.Fatalf("first (attributed) line lost its attribution: %+v", lines[0])
	}
	for _, ln := range lines[1:] {
		if ln.StageID != "" || ln.NodeID != "" {
			t.Fatalf("line inside a recovery-form chunk carries attribution: %+v", ln)
		}
	}
}

// --- 5.3: firstErrorLine ---

func TestComputeFirstErrorLine(t *testing.T) {
	cases := []struct {
		name  string
		lines []runLogLine
		want  int64
	}{
		{"no match", []runLogLine{{N: 1, Text: "all good"}, {N: 2, Text: "still fine"}}, 0},
		{"match on second line", []runLogLine{{N: 1, Text: "starting build"}, {N: 2, Text: "Build FAILED with exit code 1"}, {N: 3, Text: "done"}}, 2},
		{"case-insensitive", []runLogLine{{N: 1, Text: "an ERROR occurred"}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeFirstErrorLine(tc.lines); got != tc.want {
				t.Fatalf("computeFirstErrorLine() = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- 5.4: from/fromLine resume ---

func TestResumeLineIndex(t *testing.T) {
	lines := []runLogLine{
		{N: 1, Off: 0},
		{N: 2, Off: 8192},
		{N: 3, Off: 8200},
	}
	cases := []struct {
		name string
		from int64
		want int
	}{
		{"cold load starts at zero", 0, 0},
		{"exact line boundary resumes at that line", 8192, 1},
		{"mid-line offset resumes at the next line", 8195, 2},
		{"past the end resumes at nothing", 100000, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeLineIndex(lines, tc.from); got != tc.want {
				t.Fatalf("resumeLineIndex(%d) = %d, want %d", tc.from, got, tc.want)
			}
		})
	}
}
