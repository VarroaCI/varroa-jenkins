// The terminal poll (design D5; log-live-tail, "the run record every 10
// seconds"). This reads the run record's LogSummary from Run.Payload —
// free-form JSON, because recordstore.Run carries no typed field for it
// — independently of runlog_manifest.go's blob-store scan.
package api

import (
	"encoding/json"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// runLogPollInterval is how often the stream re-reads the run record
// while it stays open (run-log-viewer, "read the run record every 10
// seconds").
const runLogPollInterval = 10 * time.Second

// runLogSummaryFields is the log_summary object internal/mite's
// logSummaryPayload writes into Run.Payload: chunk_count, dropped_bytes,
// incomplete, incomplete_reason, and finalized, snake_case. It is
// declared independently of handlers_runs.go's runLogSummaryPayload
// (which reads only finalized and incomplete, for the run detail
// response) because this seam also needs chunk_count, dropped_bytes,
// and incomplete_reason.
type runLogSummaryFields struct {
	ChunkCount       int64  `json:"chunk_count"`
	DroppedBytes     int64  `json:"dropped_bytes"`
	Incomplete       bool   `json:"incomplete"`
	IncompleteReason string `json:"incomplete_reason"`
	Finalized        bool   `json:"finalized"`
}

// parseRunLogSummary reads payload's log_summary key. ok is false when
// the run carries no summary yet — a queued run whose mite has not sent
// a run.log envelope — never on a malformed one, which parses as a zero
// value instead of failing the stream.
func parseRunLogSummary(payload json.RawMessage) (runLogSummaryFields, bool) {
	if len(payload) == 0 {
		return runLogSummaryFields{}, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return runLogSummaryFields{}, false
	}
	raw, ok := top["log_summary"]
	if !ok {
		return runLogSummaryFields{}, false
	}
	var f runLogSummaryFields
	if err := json.Unmarshal(raw, &f); err != nil {
		return runLogSummaryFields{}, false
	}
	return f, true
}

// diffRunLogStages compares a freshly polled stage list against known
// (stageId -> last-emitted status) and returns one stage event for every
// stage that is new or whose status changed since the last poll, plus
// the updated known map. startOffset and startLine are always zero here:
// this poll reads no blob-store index, only the run record, so a
// poll-discovered stage's real log position is left for the client to
// resolve from the line stream it is already receiving.
func diffRunLogStages(known map[string]string, stages []recordstore.Stage) ([]runLogStageEvent, map[string]string) {
	next := make(map[string]string, len(stages))
	var events []runLogStageEvent
	for _, st := range stages {
		next[st.StageID] = st.Status
		if prev, ok := known[st.StageID]; !ok || prev != st.Status {
			events = append(events, runLogStageEvent{StageID: st.StageID, Name: st.Name, Status: st.Status})
		}
	}
	return events, next
}

// initialKnownStages seeds diffRunLogStages' known map from the stage
// list the history replay already read, so the poll does not re-announce
// a stage the client already saw during replay.
func initialKnownStages(stages []recordstore.Stage) map[string]string {
	known := make(map[string]string, len(stages))
	for _, st := range stages {
		known[st.StageID] = st.Status
	}
	return known
}
