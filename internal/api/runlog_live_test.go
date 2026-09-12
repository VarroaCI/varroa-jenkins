package api

import "testing"

// --- 5.6: the live path's watermark and de-duplication ---

func TestApplyLiveChunkWatermark(t *testing.T) {
	cases := []struct {
		name      string
		watermark int64
		chunk     runLiveLogChunk
		wantOK    bool
		wantData  string
		wantWater int64
	}{
		{
			name:      "a chunk below the watermark is dropped entirely",
			watermark: 100,
			chunk:     runLiveLogChunk{Offset: 0, Len: 50, Data: []byte(sample(50))},
			wantOK:    false,
			wantWater: 100,
		},
		{
			name:      "a chunk exactly at the watermark is dropped entirely",
			watermark: 50,
			chunk:     runLiveLogChunk{Offset: 0, Len: 50, Data: []byte(sample(50))},
			wantOK:    false,
			wantWater: 50,
		},
		{
			name:      "a straddling chunk is trimmed to the bytes after the watermark",
			watermark: 30,
			chunk:     runLiveLogChunk{Offset: 20, Len: 20, Data: []byte(sample(20))},
			wantOK:    true,
			wantData:  sample(20)[10:],
			wantWater: 40,
		},
		{
			name:      "a chunk entirely after the watermark is kept whole",
			watermark: 10,
			chunk:     runLiveLogChunk{Offset: 10, Len: 5, Data: []byte("abcde")},
			wantOK:    true,
			wantData:  "abcde",
			wantWater: 15,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, newWater, ok := applyLiveChunkWatermark(tc.watermark, tc.chunk)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
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
	if lines := a.feed(stale); len(lines) != 0 {
		t.Fatalf("expected no lines from a stale chunk, got %+v", lines)
	}

	// A straddling chunk: bytes 6-16 overlap the watermark at 11 and add
	// "gamma\n" (11-17). Only the bytes at or after 11 should surface.
	straddle := runLiveLogChunk{Offset: 6, Len: 11, Data: []byte("beta\ngamma\n"), StageID: "s1", TsMs: 42}
	lines := a.feed(straddle)
	if len(lines) != 1 {
		t.Fatalf("expected exactly one new line, got %+v", lines)
	}
	if lines[0].N != 3 || lines[0].Off != 11 || lines[0].Text != "gamma" {
		t.Fatalf("unexpected line: %+v", lines[0])
	}

	// A chunk arriving whole after that: no overlap, no repeat.
	next := runLiveLogChunk{Offset: 17, Len: 6, Data: []byte("delta\n"), StageID: "s1"}
	lines = a.feed(next)
	if len(lines) != 1 || lines[0].N != 4 || lines[0].Text != "delta" {
		t.Fatalf("unexpected line: %+v", lines)
	}
}

// TestRunLogLiveAssembler_PartialLineAcrossChunks proves a line split
// across two chunks by a write-buffer boundary still yields one line.
func TestRunLogLiveAssembler_PartialLineAcrossChunks(t *testing.T) {
	a := newRunLogLiveAssembler(0, 1)

	first := a.feed(runLiveLogChunk{Offset: 0, Len: 5, Data: []byte("hello")})
	if len(first) != 0 {
		t.Fatalf("expected no line yet (no newline seen), got %+v", first)
	}

	second := a.feed(runLiveLogChunk{Offset: 5, Len: 8, Data: []byte(" world\nx")})
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
	got := a.feed(runLiveLogChunk{Offset: 13, Len: 3, Data: []byte("lo\n")})
	if len(got) != 1 || got[0].Text != "hello" || got[0].Off != 10 || got[0].N != 4 {
		t.Fatalf("lines = %+v, want one line \"hello\" at offset 10 numbered 4", got)
	}
}
