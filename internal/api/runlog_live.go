// The live-path decode, watermark, and de-duplication (design D5;
// log-live-tail: "The watermark SHALL be the end offset of the last line
// the handler emitted... drop a live chunk that lies at or below the
// watermark... drop the leading bytes of a chunk that straddles it").
package api

import "bytes"

// runLiveLogChunk is the live-chunk relay payload the gateway publishes
// on the run's log subject. internal/mite/tail.go's liveLogChunkPayload
// is the same wire shape, declared independently here: internal/mite is
// the gateway's package, this is the BFF's own copy of the contract.
type runLiveLogChunk struct {
	RunID   string `json:"runId"`
	Seq     int64  `json:"seq"`
	NodeID  string `json:"nodeId,omitempty"`
	StageID string `json:"stageId,omitempty"`
	TsMs    int64  `json:"tsMs,omitempty"`
	Offset  int64  `json:"offset"`
	Len     int64  `json:"len"`
	Dropped int64  `json:"dropped,omitempty"`
	Final   bool   `json:"final,omitempty"`
	Data    []byte `json:"data"`
}

// applyLiveChunkWatermark trims chunk to the bytes at or after
// watermark, the end offset of the last line already emitted. ok is
// false when the whole chunk lies at or below the watermark, so the
// caller drops it and emits no line for it.
func applyLiveChunkWatermark(watermark int64, chunk runLiveLogChunk) (trimmed []byte, newWatermark int64, ok bool) {
	end := chunk.Offset + chunk.Len
	if end <= watermark {
		return nil, watermark, false
	}
	skip := watermark - chunk.Offset
	if skip < 0 {
		skip = 0
	}
	if skip > int64(len(chunk.Data)) {
		skip = int64(len(chunk.Data))
	}
	return chunk.Data[skip:], end, true
}

// runLogLiveAssembler buffers a live tail's not-yet-newline-terminated
// tail across chunks, so a write() call that splits one console line
// across two chunks still yields one line, and tracks the running line
// number and watermark the whole live path shares.
type runLogLiveAssembler struct {
	watermark  int64
	nextLine   int64
	partial    []byte
	partialOff int64
}

// newRunLogLiveAssembler starts an assembler at watermark (the end
// offset of the last line history replay emitted) and nextLine (one past
// the last line number replay emitted).
func newRunLogLiveAssembler(watermark, nextLine int64) *runLogLiveAssembler {
	return &runLogLiveAssembler{watermark: watermark, nextLine: nextLine, partialOff: watermark}
}

// seedPartial hands the assembler the unterminated fragment that closed
// the history snapshot, so the first live chunk completes that line
// instead of starting a new one. off is the fragment's byte offset in the
// console stream; the watermark stays at the end of the snapshot.
func (a *runLogLiveAssembler) seedPartial(fragment []byte, off int64) {
	a.partial = append(a.partial[:0], fragment...)
	a.partialOff = off
}

// feed applies chunk's watermark trim, appends the surviving bytes to
// any buffered partial line, and returns every complete line the
// combined bytes now yield. A trailing run with no newline is
// re-buffered for the next chunk, not emitted. Every line this call
// yields is attributed to chunk's own stageId, nodeId, and ts — an
// approximation for a line whose earlier bytes arrived in a prior chunk,
// accepted because a console line only ever splits across a write-buffer
// boundary within the same stage.
func (a *runLogLiveAssembler) feed(chunk runLiveLogChunk) []runLogLine {
	trimmed, newWatermark, ok := applyLiveChunkWatermark(a.watermark, chunk)
	if !ok {
		return nil
	}
	a.watermark = newWatermark
	a.partial = append(a.partial, trimmed...)

	var out []runLogLine
	for {
		idx := bytes.IndexByte(a.partial, '\n')
		if idx < 0 {
			break
		}
		text := string(stripConsoleNotes(a.partial[:idx]))
		out = append(out, runLogLine{
			N: a.nextLine, Off: a.partialOff, TsMs: chunk.TsMs,
			StageID: chunk.StageID, NodeID: chunk.NodeID, Text: text,
		})
		a.nextLine++
		a.partialOff += int64(idx) + 1
		a.partial = a.partial[idx+1:]
	}
	return out
}

// flushPartial returns the still-buffered, not-yet-newline-terminated
// tail as one final line, and clears it, so a caller that is about to
// end the stream emits it instead of dropping it silently. ok is false
// when there is nothing buffered.
func (a *runLogLiveAssembler) flushPartial() (line runLogLine, ok bool) {
	if len(a.partial) == 0 {
		return runLogLine{}, false
	}
	line = runLogLine{N: a.nextLine, Off: a.partialOff, Text: string(stripConsoleNotes(a.partial))}
	a.nextLine++
	a.partial = nil
	return line, true
}
