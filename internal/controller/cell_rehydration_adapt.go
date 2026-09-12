package controller

import (
	"encoding/json"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/rehydrate"
)

// rehydrationChangeWire is the JSON shape a build record's Changes
// field carries (internal/mite/build_events.go's changeEntry, the same
// shape runs.changes stores). Only the fields rehydrate.Change needs
// are decoded.
type rehydrationChangeWire struct {
	CommitID  string    `json:"commit_id"`
	AuthorID  string    `json:"author_id"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// adaptChanges decodes a build record's raw Changes JSON into the
// rehydrate package's own Change shape. An empty or unparsable value
// answers nil, matching a run that recorded no changeset.
func adaptChanges(raw []byte) []rehydrate.Change {
	if len(raw) == 0 {
		return nil
	}
	var wire []rehydrationChangeWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil
	}
	if len(wire) == 0 {
		return nil
	}
	out := make([]rehydrate.Change, len(wire))
	for i, c := range wire {
		out[i] = rehydrate.Change{CommitID: c.CommitID, AuthorID: c.AuthorID, Message: c.Message, Timestamp: c.Timestamp}
	}
	return out
}

// adaptParams decodes a build record's raw Params JSON object into the
// rehydrate package's own map[string]string shape.
func adaptParams(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var params map[string]string
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil
	}
	return params
}

// adaptRecord adapts one cells.BuildRecord (the tenant.sync wire shape)
// into rehydrate.Record.
func adaptRecord(r cells.BuildRecord) rehydrate.Record {
	return rehydrate.Record{
		BuildNumber:          r.BuildNumber,
		QueuedAt:             r.QueuedAt,
		StartedAt:            r.StartedAt,
		DurationMs:           r.DurationMs,
		Result:               r.Result,
		CauseDescription:     r.CauseDescription,
		Params:               adaptParams(r.Params),
		MaskedParameterNames: r.MaskedParameterNames,
		Changes:              adaptChanges(r.Changes),
	}
}

// adaptJobRecords adapts one cells.JobRecords into rehydrate.JobRecords.
func adaptJobRecords(jr cells.JobRecords) rehydrate.JobRecords {
	records := make([]rehydrate.Record, len(jr.Records))
	for i, r := range jr.Records {
		records[i] = adaptRecord(r)
	}
	return rehydrate.JobRecords{JobPath: jr.JobPath, NextBuildNumber: jr.NextBuildNumber, Records: records}
}
