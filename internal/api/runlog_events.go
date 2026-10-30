// The run log stream's server-sent-event shapes (design D5;
// run-log-viewer), and the one function that writes any of them to the
// wire. Every field name here is the wire name the delta spec fixes.
package api

import (
	"encoding/json"
	"fmt"
	"io"
)

// runLogMetaEvent is the "meta" event: the computed manifest plus the
// first error line, sent once, first.
type runLogMetaEvent struct {
	RunID          string  `json:"runId"`
	ChunkCount     int64   `json:"chunkCount"`
	ByteCount      int64   `json:"byteCount"`
	DroppedBytes   int64   `json:"droppedBytes"`
	TruncatedAt    *int64  `json:"truncatedAt,omitempty"`
	MissingSeq     []int64 `json:"missingSeq,omitempty"`
	Complete       bool    `json:"complete"`
	Status         string  `json:"status"`
	FirstErrorLine int64   `json:"firstErrorLine"`
}

// runLogStageEvent is the "stage" event: one stage boundary.
type runLogStageEvent struct {
	StageID     string `json:"stageId"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	StartOffset int64  `json:"startOffset"`
	StartLine   int64  `json:"startLine"`
}

// runLogLineEvent is the "line" event: one console line.
type runLogLineEvent struct {
	N       int64  `json:"n"`
	Off     int64  `json:"off"`
	TsMs    int64  `json:"ts"`
	StageID string `json:"stageId,omitempty"`
	NodeID  string `json:"nodeId,omitempty"`
	Text    string `json:"text"`
}

// runLogGapEvent is the "gap" event: a sequence the manifest reports as
// missing.
type runLogGapEvent struct {
	Seq int64 `json:"seq"`
	Off int64 `json:"off"`
}

// runLogLiveEvent is the "live" event: history ended and the live tail
// began (or did not, for a run that was already terminal at connect).
type runLogLiveEvent struct {
	Active bool `json:"active"`
}

// runLogEndEvent is the "end" event: the run stopped and its log is
// finalized.
type runLogEndEvent struct {
	Result           string `json:"result,omitempty"`
	LogComplete      bool   `json:"logComplete"`
	IncompleteReason string `json:"incompleteReason,omitempty"`
}

// writeRunLogEvent writes one named SSE event carrying payload as its
// JSON data line, and flushes it. It is the only place this stream
// writes to the wire, so every event this handler emits has the same
// shape.
func writeRunLogEvent(w io.Writer, flusher interface{ Flush() }, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// runLogLineToEvent converts a computed line into its wire event. The
// wire type carries no note payload, so the fields are copied one by one.
func runLogLineToEvent(ln runLogLine) runLogLineEvent {
	return runLogLineEvent{
		N: ln.N, Off: ln.Off, TsMs: ln.TsMs,
		StageID: ln.StageID, NodeID: ln.NodeID, Text: ln.Text,
	}
}
