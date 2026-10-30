package api

import "testing"

// --- 5.6: the live path's watermark and de-duplication ---

func TestApplyLiveChunkWatermark(t *testing.T) {
	cases := []struct {
		name        string
		watermark   int64
		chunk       runLiveLogChunk
		wantVerdict liveChunkVerdict
		wantData    string
		wantWater   int64
	}{
		{
			name:        "a chunk below the watermark is dropped entirely",
			watermark:   100,
			chunk:       runLiveLogChunk{Offset: 0, Len: 50, Data: []byte(sample(50))},
			wantVerdict: liveChunkDrop,
			wantWater:   100,
		},
		{
			name:        "a chunk exactly at the watermark is dropped entirely",
			watermark:   50,
			chunk:       runLiveLogChunk{Offset: 0, Len: 50, Data: []byte(sample(50))},
			wantVerdict: liveChunkDrop,
			wantWater:   50,
		},
		{
			name:        "a straddling chunk is trimmed to the bytes after the watermark",
			watermark:   30,
			chunk:       runLiveLogChunk{Offset: 20, Len: 20, Data: []byte(sample(20))},
			wantVerdict: liveChunkApply,
			wantData:    sample(20)[10:],
			wantWater:   40,
		},
		{
			name:        "a chunk that starts at the watermark is kept whole",
			watermark:   10,
			chunk:       runLiveLogChunk{Offset: 10, Len: 5, Data: []byte("abcde")},
			wantVerdict: liveChunkApply,
			wantData:    "abcde",
			wantWater:   15,
		},
		{
			name:        "a chunk that starts after the watermark reports a gap",
			watermark:   10,
			chunk:       runLiveLogChunk{Offset: 12, Len: 5, Data: []byte("abcde")},
			wantVerdict: liveChunkGap,
			wantWater:   10,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, newWater, verdict := applyLiveChunkWatermark(tc.watermark, tc.chunk)
			if verdict != tc.wantVerdict {
				t.Fatalf("verdict = %v, want %v", verdict, tc.wantVerdict)
			}
			if verdict != liveChunkApply {
				if data != nil {
					t.Fatalf("data = %q, want none", data)
				}
				if newWater != tc.wantWater {
					t.Fatalf("watermark = %d, want unchanged %d", newWater, tc.wantWater)
				}
				return
			}
			if string(data) != tc.wantData {
				t.Fatalf("trimmed data = %q, want %q", data, tc.wantData)
			}
			if newWater != tc.wantWater {
				t.Fatalf("watermark = %d, want %d", newWater, tc.wantWater)
			}
		})
	}
}

// mustFeed feeds chunk and fails the test when the assembler reports a
// gap.
func mustFeed(t *testing.T, a *runLogLiveAssembler, chunk runLiveLogChunk) []runLogLine {
	t.Helper()
	lines, gap := a.feed(chunk)
	if gap {
		t.Fatalf("unexpected gap for chunk at offset %d (watermark %d)", chunk.Offset, a.watermark)
	}
	return lines
}

func sample(n int) string {
	s := make([]byte, n)
	for i := range s {
		s[i] = byte('a' + i%26)
	}
	return string(s)
}

// TestRunLogLiveAssembler_NoRepeatedLine feeds a chunk below the
// watermark and a straddling chunk and asserts no repeated line, per
// task 5.6's acceptance.
func TestRunLogLiveAssembler_NoRepeatedLine(t *testing.T) {
	// History replay already emitted "alpha\n" (bytes 0-5) and "beta\n"
	// (bytes 6-10), so the assembler starts at watermark 11, line 3.
	a := newRunLogLiveAssembler(11, 3)

	// A redelivered chunk covering bytes already emitted: dropped whole.
	stale := runLiveLogChunk{Offset: 0, Len: 11, Data: []byte("alpha\nbeta\n"), StageID: "s1"}
	if lines := mustFeed(t, a, stale); len(lines) != 0 {
		t.Fatalf("expected no lines from a stale chunk, got %+v", lines)
	}

	// A straddling chunk: bytes 6-16 overlap the watermark at 11 and add
	// "gamma\n" (11-17). Only the bytes at or after 11 should surface.
	straddle := runLiveLogChunk{Offset: 6, Len: 11, Data: []byte("beta\ngamma\n"), StageID: "s1", TsMs: 42}
	lines := mustFeed(t, a, straddle)
	if len(lines) != 1 {
		t.Fatalf("expected exactly one new line, got %+v", lines)
	}
	if lines[0].N != 3 || lines[0].Off != 11 || lines[0].Text != "gamma" {
		t.Fatalf("unexpected line: %+v", lines[0])
	}

	// A chunk arriving whole after that: no overlap, no repeat.
	next := runLiveLogChunk{Offset: 17, Len: 6, Data: []byte("delta\n"), StageID: "s1"}
	lines = mustFeed(t, a, next)
	if len(lines) != 1 || lines[0].N != 4 || lines[0].Text != "delta" {
		t.Fatalf("unexpected line: %+v", lines)
	}
}

// TestRunLogLiveAssembler_PartialLineAcrossChunks proves a line split
// across two chunks by a write-buffer boundary still yields one line.
func TestRunLogLiveAssembler_PartialLineAcrossChunks(t *testing.T) {
	a := newRunLogLiveAssembler(0, 1)

	first := mustFeed(t, a, runLiveLogChunk{Offset: 0, Len: 5, Data: []byte("hello")})
	if len(first) != 0 {
		t.Fatalf("expected no line yet (no newline seen), got %+v", first)
	}

	second := mustFeed(t, a, runLiveLogChunk{Offset: 5, Len: 8, Data: []byte(" world\nx")})
	if len(second) != 1 {
		t.Fatalf("expected exactly one completed line, got %+v", second)
	}
	if second[0].Text != "hello world" || second[0].N != 1 || second[0].Off != 0 {
		t.Fatalf("unexpected line: %+v", second[0])
	}
}

// TestRunLogLiveAssembler_SeededPartialCompletesTheLine covers a snapshot
// that ends mid-line: the seeded fragment and the first live chunk form
// one line at the fragment's offset, not two.
func TestRunLogLiveAssembler_SeededPartialCompletesTheLine(t *testing.T) {
	a := newRunLogLiveAssembler(13, 4)
	a.seedPartial([]byte("hel"), 10)
	got := mustFeed(t, a, runLiveLogChunk{Offset: 13, Len: 3, Data: []byte("lo\n")})
	if len(got) != 1 || got[0].Text != "hello" || got[0].Off != 10 || got[0].N != 4 {
		t.Fatalf("lines = %+v, want one line \"hello\" at offset 10 numbered 4", got)
	}
}

// TestRunLogLiveAssembler_GapLeavesStateUnchanged feeds a chunk that
// starts after the watermark. The assembler emits no line and keeps its
// watermark, its line number, and its partial line.
func TestRunLogLiveAssembler_GapLeavesStateUnchanged(t *testing.T) {
	a := newRunLogLiveAssembler(13, 4)
	a.seedPartial([]byte("hel"), 10)

	lines, gap := a.feed(runLiveLogChunk{Offset: 20, Len: 4, Data: []byte("zz\nq")})
	if !gap {
		t.Fatalf("gap = false, want true")
	}
	if len(lines) != 0 {
		t.Fatalf("lines = %+v, want none", lines)
	}
	if a.watermark != 13 || a.nextLine != 4 || string(a.partial) != "hel" || a.partialOff != 10 {
		t.Fatalf("state changed: watermark=%d nextLine=%d partial=%q partialOff=%d", a.watermark, a.nextLine, a.partial, a.partialOff)
	}
}

// TestRunLogLiveAssembler_ResyncContinuesNumberingAndOffsets resyncs from
// a console object that holds the lost bytes. The lines start at the
// buffered partial line, carry their true offsets, and continue the
// assembler's numbering. An unterminated last line stays buffered.
func TestRunLogLiveAssembler_ResyncContinuesNumberingAndOffsets(t *testing.T) {
	console := []byte("alpha\nbeta\nhello\nlost\ngamma\ndel")
	// Lines alpha and beta were emitted as 10 and 11 (fromLine override),
	// and "hel" (offset 11) is buffered.
	a := newRunLogLiveAssembler(14, 12)
	a.seedPartial([]byte("hel"), 11)

	all := splitRunLogLines(console, nil)
	got := a.resync(console, all)

	want := []runLogLine{
		{N: 12, Off: 11, Text: "hello"},
		{N: 13, Off: 17, Text: "lost"},
		{N: 14, Off: 22, Text: "gamma"},
	}
	if len(got) != len(want) {
		t.Fatalf("lines = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].N != want[i].N || got[i].Off != want[i].Off || got[i].Text != want[i].Text {
			t.Fatalf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if a.watermark != int64(len(console)) {
		t.Fatalf("watermark = %d, want %d", a.watermark, len(console))
	}
	if string(a.partial) != "del" || a.partialOff != 28 {
		t.Fatalf("partial = %q at %d, want \"del\" at 28", a.partial, a.partialOff)
	}

	// The next live chunk completes the buffered line with the next number.
	next := mustFeed(t, a, runLiveLogChunk{Offset: 31, Len: 3, Data: []byte("ta\n")})
	if len(next) != 1 || next[0].Text != "delta" || next[0].N != 15 || next[0].Off != 28 {
		t.Fatalf("lines = %+v, want one line \"delta\" at offset 28 numbered 15", next)
	}
}

// TestRunLogLiveAssembler_AttributesNoteLines proves the live path joins
// a nodeless note line to its block through the same attributor as the
// history replay, and leaves a noded chunk's line as the chunk named it.
func TestRunLogLiveAssembler_AttributesNoteLines(t *testing.T) {
	block := testNewNodeNoteLine("17", "18", "18", "[Pipeline] { (boom)")
	step := testNewNodeNoteLine("18", "19", "", "[Pipeline] error")
	raw := append(testNoteRawBytes(block), '\n')
	raw = append(raw, testNoteRawBytes(step)...)
	raw = append(raw, '\n')

	a := newRunLogLiveAssembler(0, 1)
	a.attr = newRunLogStageAttributor(nil)
	got := mustFeed(t, a, runLiveLogChunk{Offset: 0, Len: int64(len(raw)), Data: raw})
	if len(got) != 2 {
		t.Fatalf("lines = %+v, want two", got)
	}
	if got[0].StageID != "18" || got[1].StageID != "18" {
		t.Fatalf("StageIDs = %q, %q; want both 18", got[0].StageID, got[1].StageID)
	}

	noded := newRunLogLiveAssembler(0, 1)
	noded.attr = newRunLogStageAttributor(nil)
	out := mustFeed(t, noded, runLiveLogChunk{NodeID: "n1", StageID: "chunk", Offset: 0, Len: int64(len(raw)), Data: raw})
	if len(out) != 2 || out[0].StageID != "chunk" || out[1].StageID != "chunk" {
		t.Fatalf("noded lines = %+v, want chunk stage on both", out)
	}
}

// TestRunLogLiveAssembler_FlushPartialAttributesNoteLine proves the final
// unterminated line of a finalized log takes its flow-node stage.
func TestRunLogLiveAssembler_FlushPartialAttributesNoteLine(t *testing.T) {
	block := testNewNodeNoteLine("17", "18", "18", "[Pipeline] { (boom)")
	step := testNewNodeNoteLine("18", "19", "", "[Pipeline] error")
	raw := append(testNoteRawBytes(block), '\n')
	raw = append(raw, testNoteRawBytes(step)...)

	a := newRunLogLiveAssembler(0, 1)
	a.attr = newRunLogStageAttributor(nil)
	if got := mustFeed(t, a, runLiveLogChunk{Offset: 0, Len: int64(len(raw)), Data: raw}); len(got) != 1 {
		t.Fatalf("lines = %+v, want one terminated line", got)
	}
	line, ok := a.flushPartial()
	if !ok {
		t.Fatal("flushPartial ok = false, want the unterminated line")
	}
	if line.StageID != "18" {
		t.Fatalf("StageID = %q, want 18", line.StageID)
	}
}

// TestRunLogLiveAssembler_FlushPartialKeepsChunkStage proves an
// unterminated final line keeps the stage of the chunk that started it.
func TestRunLogLiveAssembler_FlushPartialKeepsChunkStage(t *testing.T) {
	a := newRunLogLiveAssembler(0, 1)
	mustFeed(t, a, runLiveLogChunk{NodeID: "n1", StageID: "s9", Offset: 0, Len: 4, Data: []byte("tail")})
	line, ok := a.flushPartial()
	if !ok || line.StageID != "s9" || line.NodeID != "n1" || line.Text != "tail" {
		t.Fatalf("flushPartial = %+v, %v; want stage s9 node n1 text tail", line, ok)
	}
}
