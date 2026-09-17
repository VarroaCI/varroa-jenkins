package recordstore

import (
	"encoding/json"
	"time"
)

// Ingest kind constants. The payload on builds.<cluster>.<ns>.<cell>
// carries exactly one of these twelve values in its "kind" field:
// eleven run.* kinds, and the cell-scoped job.schedules kind C11's
// gateway publishes. C2 maps its protobuf BuildEvent onto eight of the
// run.* kinds; the gateway publishes run.trigger_failed from a failed
// TRIGGER_BUILD result, run.trigger_accepted from a successful one, and
// run.state from each run-state push outcome, instead.
const (
	KindRunStarted   = "run.started"
	KindRunStage     = "run.stage"
	KindRunCompleted = "run.completed"
	KindRunTests     = "run.tests"
	KindRunArtifacts = "run.artifacts"
	KindRunChanges   = "run.changes"
	KindRunLog       = "run.log"
	KindRunHeartbeat = "run.heartbeat"
	// KindRunTriggerFailed records a build trigger Jenkins refused. Such a
	// run holds no intent row, so the recorder routes it before the intent
	// read.
	KindRunTriggerFailed = "run.trigger_failed"
	// KindRunTriggerAccepted records a build trigger Jenkins accepted onto
	// its queue. No intent needs to exist for it: an accepted trigger can
	// arrive before any claim, so the recorder routes it before the
	// intent read, beside run.trigger_failed.
	KindRunTriggerAccepted = "run.trigger_accepted"
	// KindRunState records one run-state push outcome. The gateway
	// publishes it after it stores a file, and after it refuses a push
	// because the run passed its byte cap.
	KindRunState     = "run.state"
	KindJobSchedules = "job.schedules"
)

// ingestKinds lists every kind the recorder accepts. A kind outside this
// set is unknown.
var ingestKinds = map[string]bool{
	KindRunStarted:         true,
	KindRunStage:           true,
	KindRunCompleted:       true,
	KindRunTests:           true,
	KindRunArtifacts:       true,
	KindRunChanges:         true,
	KindRunLog:             true,
	KindRunHeartbeat:       true,
	KindRunTriggerFailed:   true,
	KindRunTriggerAccepted: true,
	KindRunState:           true,
	KindJobSchedules:       true,
}

// IsKnownKind reports whether kind is one of the twelve ingest kinds.
func IsKnownKind(kind string) bool {
	return ingestKinds[kind]
}

// minSchemaVersion and maxSchemaVersion bound IngestEnvelope.SchemaVersion.
// A version below the minimum can never become readable and is dropped.
// A version above the maximum belongs to a newer recorder in a rolling
// upgrade and is retried, never dropped.
const (
	minSchemaVersion = 1
	maxSchemaVersion = 1
)

// IngestEnvelope is the versioned JSON payload on
// builds.<cluster>.<ns>.<cell>. C2 maps its protobuf BuildEvent onto
// these exact field names; the gateway stamps CellInstance from the
// mite's registered instance, so a cell cannot claim another identity.
//
// Payload carries one of the Run*Payload structs below, chosen by Kind.
// A field C2 sends that no typed column maps to still round-trips: the
// recorder stores Payload beside the typed columns it writes.
type IngestEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Kind          string          `json:"kind"`
	RunID         string          `json:"run_id"`
	IntentKey     string          `json:"intent_key"`
	Cluster       string          `json:"cluster"`
	Namespace     string          `json:"namespace"`
	Cell          string          `json:"cell"`
	CellInstance  string          `json:"cell_instance"`
	Tenant        string          `json:"tenant"`
	JobPath       string          `json:"job_path"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

// RunStartedPayload is the payload of a run.started envelope. A pointer
// field is nil when C2 sends no value for it, so the recorder's merge
// leaves the stored column untouched rather than clearing it.
type RunStartedPayload struct {
	BuildNumber *int64          `json:"build_number,omitempty"`
	Branch      *string         `json:"branch,omitempty"`
	SCMRevision *string         `json:"scm_revision,omitempty"`
	QueuedAt    *time.Time      `json:"queued_at,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	Cause       json.RawMessage `json:"cause,omitempty"`
	Actor       *string         `json:"actor,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
}

// RunCompletedPayload is the payload of a run.completed envelope.
type RunCompletedPayload struct {
	Result     *string    `json:"result,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMs *int64     `json:"duration_ms,omitempty"`
}

// RunStatePayload is the payload of a run.state envelope. It takes one
// of two shapes. A stored file carries RelPath, Bytes, Seq, and
// StateGen, where Bytes is the whole stored object's inflated size. A
// refused push carries RejectedReason alone, because the gateway wrote
// no object for it.
type RunStatePayload struct {
	RelPath        string `json:"relpath,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
	Seq            int64  `json:"seq,omitempty"`
	StateGen       int64  `json:"state_gen,omitempty"`
	RejectedReason string `json:"rejected_reason,omitempty"`
}

// RunTriggerFailedPayload is the payload of a run.trigger_failed
// envelope. The gateway maps the Jenkins HTTP status onto ReasonCode,
// which holds exactly one of JOB_NOT_FOUND, JOB_NOT_BUILDABLE, and
// TRIGGER_REJECTED.
type RunTriggerFailedPayload struct {
	Error      string `json:"error"`
	ReasonCode string `json:"reason_code"`
	HTTPStatus int    `json:"http_status"`
	Actor      string `json:"actor,omitempty"`
}

// RunStagePayload is the payload of a run.stage envelope, mapped onto
// run_stages.
type RunStagePayload struct {
	StageID    string     `json:"stage_id"`
	ParentID   *string    `json:"parent_id,omitempty"`
	Name       string     `json:"name"`
	NodeType   string     `json:"node_type"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMs *int64     `json:"duration_ms,omitempty"`
	Error      *string    `json:"error,omitempty"`
	LogOffset  *int64     `json:"log_offset,omitempty"`
	LogLength  *int64     `json:"log_length,omitempty"`
}

// RunTestsPayload is the payload of a run.tests envelope, mapped onto
// run_tests. It reports one suite per envelope.
type RunTestsPayload struct {
	Suite      string `json:"suite"`
	Total      int    `json:"total"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	DurationMs *int64 `json:"duration_ms,omitempty"`
}

// RunArtifactsPayload is the payload of a run.artifacts envelope, mapped
// onto run_artifacts. It reports one archived path per envelope.
type RunArtifactsPayload struct {
	Path        string  `json:"path"`
	SizeBytes   int64   `json:"size_bytes"`
	Digest      *string `json:"digest,omitempty"`
	ContentType *string `json:"content_type,omitempty"`
	BlobKey     string  `json:"blob_key"`
}

// RunChangesPayload is the payload of a run.changes envelope, mapped onto
// runs.changes.
type RunChangesPayload struct {
	Changes json.RawMessage `json:"changes"`
}

// JobSchedulesPayload is the payload of a job.schedules envelope,
// design.md D8 "The envelope". It carries one row list for the whole
// cell. Every row carries its own tenant and placement generation: one
// envelope describes a whole cell, and a cell hosts many tenants with
// independent generations, so a single envelope-level value cannot
// describe the set.
type JobSchedulesPayload struct {
	Hash      string                `json:"hash"`
	Truncated bool                  `json:"truncated"`
	Total     int                   `json:"total"`
	Rows      []JobScheduleRowInput `json:"rows"`
}

// JobScheduleRowInput is one row of a job.schedules envelope's payload.
// UpstreamTenant, UpstreamJob, and UpstreamResult are empty for every
// row except a varroa-upstream trigger row, design.md D9.
type JobScheduleRowInput struct {
	Tenant              string   `json:"tenant"`
	PlacementGeneration int64    `json:"placement_generation"`
	JobPath             string   `json:"job_path"`
	TriggerType         string   `json:"trigger_type"`
	Spec                string   `json:"spec"`
	Timezone            string   `json:"timezone"`
	Disabled            bool     `json:"disabled"`
	NextFireTimes       []string `json:"next_fire_times"`
	UpstreamTenant      string   `json:"upstream_tenant,omitempty"`
	UpstreamJob         string   `json:"upstream_job,omitempty"`
	UpstreamResult      string   `json:"upstream_result,omitempty"`
}
