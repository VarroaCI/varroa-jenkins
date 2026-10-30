package api

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// testNewNodeNoteLine builds one console line that starts with a
// NewNodeConsoleNote, with no MAC, as Jenkins writes it. An empty
// argument for a field encodes Java null.
func testNewNodeNoteLine(enclosing, id, start, text string) runLogLine {
	var body bytes.Buffer
	body.Write(newNodeNotePrefix)
	writeVal := func(s string) {
		if s == "" {
			body.WriteByte(tcNull)
			return
		}
		body.WriteByte(tcString)
		_ = binary.Write(&body, binary.BigEndian, uint16(len(s)))
		body.WriteString(s)
	}
	writeVal(enclosing)
	writeVal(id)
	writeVal(start)

	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(body.Bytes())
	_ = zw.Close()

	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, int32(zipped.Len()))
	frame.Write(zipped.Bytes())

	payload := base64.StdEncoding.EncodeToString(frame.Bytes())
	raw := consoleNotePreamble + payload + consoleNotePostamble + text
	return runLogLine{Text: string(stripConsoleNotes([]byte(raw))), notePayload: payload}
}

func TestDecodeNewNodeNote_RealPayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    newNodeNote
	}{
		{
			name:    "close of a block with no enclosing link",
			payload: "////4NZabIF2zxycQ94dFqiJg5m7Hxo21TIBZEO7PZSDQbsYAAAApB+LCAAAAAAAAP9tjTEOwjAQBDdBFLSUPMIRokFCVLRWGl5gEmOcWHfBvpBUvIiv8QciIlGx1c4083pjmSKOHJ1qLLWeUuVVF3o3PTVwbK+BB9XwRVVMiYNVpR1Kru1pxpLFYl6WY6GxslQFTp6cYK0b8zBFMOSKs8TJHTRyX9/xRKanspgoXxgjNre+Tky/zt8Axq4T5NudINt/AH4dU5y9AAAA",
			want:    newNodeNote{Enclosing: "", ID: "13", Start: "8"},
		},
		{
			name:    "block start with an enclosing link",
			payload: "////4PUphzLWw8gYdE6y0VV8AGh52ouOBff5KMo2wbjbkQReAAAApx+LCAAAAAAAAP9tjTEOwjAUQ3+LOrAycohUTCAhpq5RFk4QmhDSRv+Xn5R24kRcjTvQUokJD5ZtyXqvNxSR4UTsRGOx9RhrL7rQuymJgbi9BhpEQxdRE0YKVig7KDK2WqqiZGFRlsNKwtpiHSh6dAk2stEPXQaNrjwnnrajhNybOzwhkxM5aU7fMjJsb72JhD/OXwCMXYJ8t5/tMD+LD0rlhNLCAAAA",
			want:    newNodeNote{Enclosing: "17", ID: "18", Start: "18"},
		},
		{
			name:    "step inside a block",
			payload: "////4LaCXzYvJsxrLoN16f4EVmCgxI6XeNeIWYD5vre9hcBxAAAApB+LCAAAAAAAAP9tjTEOwjAQBC9BFLSUPOIiOhCiSmu54QUmNsaJdWdsh6TiRXyNPxCIRMUWq51t5vmCZYpw5GixNdQ5So3D4Hs7LRw4dhfPA7Z8xoYpsTcozSBZm3pGydnAnKKEhYCVocZzcmQzrEWr7qryimx1ynH6DgJKp2/wgEJM5qxi/sIYYXPtdWL6ef4KYAwZyu3uU/vwBr2eB0O+AAAA",
			want:    newNodeNote{Enclosing: "18", ID: "19", Start: ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeNewNodeNote(tc.payload)
			if !ok {
				t.Fatalf("decodeNewNodeNote ok = false, want true")
			}
			if got != tc.want {
				t.Fatalf("decodeNewNodeNote = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDecodeNewNodeNote_Rejects(t *testing.T) {
	valid := testNewNodeNoteLine("17", "18", "18", "").notePayload
	raw, _ := base64.StdEncoding.DecodeString(valid)

	truncated := valid[:len(valid)-8]

	oversize := append([]byte(nil), raw...)
	binary.BigEndian.PutUint32(oversize[0:4], uint32(len(raw)*10))
	oversizeB64 := base64.StdEncoding.EncodeToString(oversize)

	notGzip := make([]byte, 0, 8)
	notGzip = append(notGzip, 0, 0, 0, 4)
	notGzip = append(notGzip, 'x', 'y', 'z', 'w')
	notGzipB64 := base64.StdEncoding.EncodeToString(notGzip)

	// A gzip body with a different class header must be refused.
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte("aced0005737200ff"))
	_ = zw.Close()
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, int32(zipped.Len()))
	frame.Write(zipped.Bytes())
	wrongClassB64 := base64.StdEncoding.EncodeToString(frame.Bytes())

	danglingHandle := func() string {
		var body bytes.Buffer
		body.Write(newNodeNotePrefix)
		body.WriteByte(tcNull)
		body.WriteByte(tcReference)
		_ = binary.Write(&body, binary.BigEndian, uint32(0x7e0009))
		body.WriteByte(tcNull)
		var zipped bytes.Buffer
		zw := gzip.NewWriter(&zipped)
		_, _ = zw.Write(body.Bytes())
		_ = zw.Close()
		var frame bytes.Buffer
		_ = binary.Write(&frame, binary.BigEndian, int32(zipped.Len()))
		frame.Write(zipped.Bytes())
		return base64.StdEncoding.EncodeToString(frame.Bytes())
	}()

	cases := map[string]string{
		"truncated base64": truncated,
		"oversize length":  oversizeB64,
		"non-gzip body":    notGzipB64,
		"wrong class":      wrongClassB64,
		"dangling handle":  danglingHandle,
		"not base64":       "%%%",
	}
	for name, payload := range cases {
		if _, ok := decodeNewNodeNote(payload); ok {
			t.Errorf("%s: decodeNewNodeNote ok = true, want false", name)
		}
	}
}

func TestConsoleNotePayloadAtLineStart_RequiresLineStart(t *testing.T) {
	line := testNewNodeNoteLine("17", "18", "18", "")
	if got := consoleNotePayloadAtLineStart([]byte("xx" + consoleNotePreamble + line.notePayload)); got != "" {
		t.Fatalf("payload after text = %q, want empty", got)
	}
	if got := consoleNotePayloadAtLineStart([]byte(consoleNotePreamble + line.notePayload)); got != line.notePayload {
		t.Fatalf("payload at start = %q, want %q", got, line.notePayload)
	}
	long := consoleNotePreamble + strings.Repeat("A", maxNoteBase64Bytes+1)
	if got := consoleNotePayloadAtLineStart([]byte(long)); got != "" {
		t.Fatalf("oversize run returned %d bytes, want empty", len(got))
	}
}

// attributeLines runs the attributor over lines in order, the way the
// history replay does.
func attributeLines(a *runLogStageAttributor, lines []runLogLine) []runLogLine {
	for i := range lines {
		a.attribute(&lines[i])
	}
	return lines
}

func TestRunLogStageAttributor_WalksToLabelledBlock(t *testing.T) {
	a := newRunLogStageAttributor(nil)
	lines := attributeLines(a, []runLogLine{
		testNewNodeNoteLine("4", "17", "17", "[Pipeline] stage"),
		testNewNodeNoteLine("17", "18", "18", "[Pipeline] { (boom)"),
		testNewNodeNoteLine("18", "19", "", "[Pipeline] error"),
		testNewNodeNoteLine("", "20", "18", "[Pipeline] }"),
		testNewNodeNoteLine("", "21", "17", "[Pipeline] // stage"),
	})
	want := []string{"", "18", "18", "18", ""}
	for i, ln := range lines {
		if ln.StageID != want[i] {
			t.Errorf("line %d (%q) StageID = %q, want %q", i, ln.Text, ln.StageID, want[i])
		}
	}
}

func TestRunLogStageAttributor_KeepsChunkStage(t *testing.T) {
	a := newRunLogStageAttributor(nil)
	line := testNewNodeNoteLine("17", "18", "18", "[Pipeline] { (boom)")
	line.StageID = "chunk-stage"
	a.attribute(&line)
	if line.StageID != "chunk-stage" {
		t.Fatalf("StageID = %q, want chunk-stage", line.StageID)
	}
}

func TestRunLogStageAttributor_UnknownEnclosingIsEmpty(t *testing.T) {
	a := newRunLogStageAttributor([]recordstore.Stage{{StageID: "5"}})
	line := testNewNodeNoteLine("99", "100", "100", "step text")
	a.attribute(&line)
	if line.StageID != "" {
		t.Fatalf("StageID = %q, want empty", line.StageID)
	}
}

func TestRunLogStageAttributor_NilAndNoPayloadAreNoOps(t *testing.T) {
	var nilAttr *runLogStageAttributor
	line := runLogLine{Text: "plain"}
	nilAttr.attribute(&line)
	if line.StageID != "" {
		t.Fatalf("nil attributor set StageID %q", line.StageID)
	}
	a := newRunLogStageAttributor(nil)
	a.attribute(&line)
	if line.StageID != "" {
		t.Fatalf("line without payload set StageID %q", line.StageID)
	}
}

// testNoteRawBytes returns the console bytes of a note line, without its
// newline.
func testNoteRawBytes(ln runLogLine) []byte {
	return []byte(consoleNotePreamble + ln.notePayload + consoleNotePostamble + ln.Text)
}
