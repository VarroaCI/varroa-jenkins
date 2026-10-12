package recordstore

import (
	"encoding/json"
	"time"
)

// Ingest kind constants. The payload on builds.<cluster>.<ns>.<cell>
// carries exactly one of the kinds below in its "kind" field: the
// run.* kinds the recorder accepts, and the cell-scoped job.schedules
// kind C11's gateway publishes. C2 maps its protobuf BuildEvent onto
// the run.* kinds that carry build state. The gateway publishes
// run.trigger_failed from a failed TRIGGER_BUILD result,
// run.trigger_accepted from a successful one, run.state from each
// run-state push outcome, run.resume_failed from a failed RESUME_BUILD
// result, and run.command_refused from a refused cancel or input answer,
// instead.
const (
	KindRunStarted   = "run.started"
	KindRunStage     = "run.stage"
	KindRunCompleted = "run.completed"
	KindRunTests     = "run.tests"
	KindRunArtifacts = "run.artifacts"
	KindRunChanges   = "run.changes"
	KindRunLog       = "run.log"
	KindRunHeartbeat = "run.heartbeat"
	// KindRunSource records the source facts of a run: its definition
	// path, its libraries, and its pod templates. The merge writes one
	// StoredRunSource value into the runs.source column.
	KindRunSource = "run.source"
	// KindRunInput records the pending input steps of a run. The write
	// replaces the whole runs.inputs value, because the event carries
	// the run's whole pending set and never a delta. A paused build
	// always holds an intent row, so the recorder routes it after the
	// intent read, beside run.source.
	KindRunInput = "run.input"
	// KindRunTriggerFailed records a build trigger Jenkins refused. Such a
	// run holds no intent row, so the recorder routes it before the intent
	// read.
	KindRunTriggerFailed = "run.trigger_failed"
	// KindRunTriggerAccepted records a build trigger Jenkins accepted onto
	// its queue. No intent needs to exist for it: an accepted trigger can
	// arrive before any claim, so the recorder routes it before the
	// intent read, beside run.trigger_failed.
	KindRunTriggerAccepted = "run.trigger_accepted"
	// KindRunResumeFailed records a RESUME_BUILD command the target could
	// not apply. The run already holds its own record, and the write
	// touches that record alone, so the recorder routes it before the
	// intent read beside the two trigger kinds.
	KindRunResumeFailed = "run.resume_failed"
	// KindRunCommandRefused records a cancel or an input answer the plugin
	// refused. The write merges one notice into the run's own payload and
	// needs no intent, so the recorder routes it before the intent read
	// beside run.resume_failed.
	KindRunCommandRefused = "run.command_refused"
	// KindRunState records one run-state push outcome. The gateway
	// publishes it after it stores a file, and after it refuses a push
	// because the run passed its byte cap.
	KindRunState     = "run.state"
	KindJobSchedules = "job.schedules"
	// KindJobBranches records one cell's multibranch inventory. Like
	// job.schedules it carries no run_id.
	KindJobBranches = "job.branches"
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
	KindRunSource:          true,
	KindRunInput:           true,
	KindRunTriggerFailed:   true,
	KindRunTriggerAccepted: true,
	KindRunResumeFailed:    true,
	KindRunCommandRefused:  true,
	KindRunState:           true,
	KindJobSchedules:       true,
	KindJobBranches:        true,
}

// IsKnownKind reports whether kind is one of the kinds ingestKinds
// lists.
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
	// ReplayOf names the run a replayed build copied its script from. The
	// gateway writes the key only for a non-empty value, so nil binds NULL.
	ReplayOf *string `json:"replay_of,omitempty"`
	// RestartOf and RestartStage name the source run and the stage of a
	// restarted build. The gateway writes the keys only for a non-empty value.
	RestartOf    *string `json:"restart_of,omitempty"`
	RestartStage *string `json:"restart_stage,omitempty"`
	// GrantedBuildNumber is the grant the run applied in Jenkins. The
	// recorder only compares it with BuildNumber; no column stores it.
	GrantedBuildNumber *int64 `json:"granted_build_number,omitempty"`
	// UpstreamJobPath and UpstreamBuildNumber name the run that started this
	// run through a Jenkins upstream cause. deriveUpstream reads them when
	// the intent key names no upstream run.
	UpstreamJobPath     *string `json:"upstream_job_path,omitempty"`
	UpstreamBuildNumber *int64  `json:"upstream_build_number,omitempty"`
}

// RunCompletedPayload is the payload of a run.completed envelope.
//
// Summary stays nil when the payload carries no summary value, and that
// nil is the gate that leaves both the summary column and the payload's
// summary_seq key untouched. Seq orders the summary raise against the
// stored watermark.
type RunCompletedPayload struct {
	Result     *string    `json:"result,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMs *int64     `json:"duration_ms,omitempty"`
	// SCMRevision carries the newest changelog entry's revision. A
	// run.completed write fills an absent runs.scm_revision from it,
	// and never replaces a stored value.
	SCMRevision *string `json:"scm_revision,omitempty"`
	Summary     *string `json:"summary,omitempty"`
	Seq         int64   `json:"seq"`
}

// RunStatePayload is the payload of a run.state envelope. It takes one
// of two shapes. A stored file carries RelPath, Bytes, Seq, StateGen,
// and PusherEpoch, where Bytes is the whole stored object's inflated
// size. A refused push carries RejectedReason alone, because the gateway
// wrote no object for it.
type RunStatePayload struct {
	RelPath  string `json:"relpath,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Seq      int64  `json:"seq,omitempty"`
	StateGen int64  `json:"state_gen,omitempty"`
	// PusherEpoch names the Jenkins process that pushed the file. The
	// object key carries it, so the record cannot name the object
	// without it.
	PusherEpoch    int64  `json:"pusher_epoch,omitempty"`
	RejectedReason string `json:"rejected_reason,omitempty"`
}

// RunTriggerFailedPayload is the payload of a run.trigger_failed
// envelope. The gateway maps the Jenkins HTTP status onto ReasonCode,
// which holds exactly one of JOB_NOT_FOUND, JOB_NOT_BUILDABLE,
// TRIGGER_REJECTED, and TRIGGER_UNREACHABLE. The status 0 answers
// TRIGGER_UNREACHABLE: the trigger reached no Jenkins ready to build it.
type RunTriggerFailedPayload struct {
	Error      string `json:"error"`
	ReasonCode string `json:"reason_code"`
	HTTPStatus int    `json:"http_status"`
	Actor      string `json:"actor,omitempty"`
}

// RunResumeFailedPayload is the payload of a run.resume_failed
// envelope. Instance names the target the recorder dispatched to, and
// the write changes nothing when the stored record names another
// instance. ReasonCode decides whether the run returns to the armed
// state: the gateway alone reads the target's error text, and the
// recorder applies the one value it answers.
type RunResumeFailedPayload struct {
	Instance   string `json:"instance"`
	Error      string `json:"error"`
	ReasonCode string `json:"reason_code"`
}

// RunCommandRefusedPayload is the payload of a run.command_refused
// envelope. Command is one of the names refusedCommands lists. The
// gateway caps Error.
type RunCommandRefusedPayload struct {
	Command    string `json:"command"`
	Actor      string `json:"actor"`
	HTTPStatus int32  `json:"http_status"`
	Error      string `json:"error"`
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
}

// RunTestsPayload is the payload of a run.tests envelope, mapped onto
// run_tests, run_test_cases, and runs. It reports one suite's counters per
// envelope. Cases spans every suite, because the plugin sends one bounded
// set per run. A nil Cases means the key is absent, and the write then
// changes no case row. An empty non-nil Cases clears the run's stored set.
type RunTestsPayload struct {
	Suite      string                `json:"suite"`
	Total      int                   `json:"total"`
	Failed     int                   `json:"failed"`
	Skipped    int                   `json:"skipped"`
	DurationMs *int64                `json:"duration_ms,omitempty"`
	Cases      []RunTestCasePayload  `json:"cases"`
	TestReport *RunTestReportPayload `json:"test_report,omitempty"`
}

// RunTestCasePayload is one entry of a run.tests payload's cases list,
// mapped onto one run_test_cases row.
type RunTestCasePayload struct {
	Suite        string `json:"suite"`
	ClassName    string `json:"class_name"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	DurationMs   *int64 `json:"duration_ms,omitempty"`
	ErrorDetails string `json:"error_details,omitempty"`
}

// RunTestReportPayload is a run.tests payload's test_report object. BlobKey
// is empty for a report that a controller stores locally.
type RunTestReportPayload struct {
	BlobKey   string `json:"blob_key"`
	SizeBytes int64  `json:"size_bytes"`
	CaseCount int32  `json:"case_count"`
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

// RunSourcePayload is the payload of a run.source envelope. A pointer
// field is nil when the plugin resolved no value, so the merge leaves
// the stored column untouched rather than clearing it.
type RunSourcePayload struct {
	Seq            int64               `json:"seq"`
	Branch         *string             `json:"branch,omitempty"`
	SCMRevision    *string             `json:"scm_revision,omitempty"`
	SCMHead        string              `json:"scm_head,omitempty"`
	SCMURL         string              `json:"scm_url,omitempty"`
	DefinitionPath string              `json:"definition_path,omitempty"`
	Libraries      []SourceLibrary     `json:"libraries,omitempty"`
	PodTemplates   []SourcePodTemplate `json:"pod_templates,omitempty"`
}

// SourceLibrary is one shared library a run loaded, as the payload
// carries it.
type SourceLibrary struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// SourcePodTemplate is one pod template a run used, as the payload
// carries it.
type SourcePodTemplate struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	Image string `json:"image,omitempty"`
}

// StoredRunSource is the shape of the runs.source column. The recorder
// marshals it on the write, and the BFF unmarshals it on the read, so
// one type owns the column's key names on both sides. The keys are the
// column's own snake_case names, never the response's camelCase names.
type StoredRunSource struct {
	DefinitionPath string              `json:"definition_path,omitempty"`
	Libraries      []SourceLibrary     `json:"libraries,omitempty"`
	PodTemplates   []SourcePodTemplate `json:"pod_templates,omitempty"`
}

// RunInputPayload is the payload of a run.input envelope. It carries the
// run's whole pending set. A nil Inputs and an empty Inputs both clear
// the stored set, because the event never carries a delta.
type RunInputPayload struct {
	Seq    *int64           `json:"seq"`
	Inputs []StoredRunInput `json:"inputs"`
}

// StoredRunInput is one element of the runs.inputs column. The recorder
// marshals it on the write, and the BFF unmarshals it on the read, so
// one type owns the column's key names on both sides. The keys are the
// column's own snake_case names, never the response's camelCase names.
type StoredRunInput struct {
	ID                 string                    `json:"id"`
	Message            string                    `json:"message,omitempty"`
	OK                 string                    `json:"ok,omitempty"`
	Submitter          string                    `json:"submitter,omitempty"`
	SubmitterParameter string                    `json:"submitter_parameter,omitempty"`
	Parameters         []StoredRunInputParameter `json:"parameters,omitempty"`
}

// StoredRunInputParameter is one parameter definition of a stored pending
// input.
type StoredRunInputParameter struct {
	Name         string   `json:"name"`
	Type         string   `json:"type,omitempty"`
	Description  string   `json:"description,omitempty"`
	DefaultValue string   `json:"default_value,omitempty"`
	Choices      []string `json:"choices,omitempty"`
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
