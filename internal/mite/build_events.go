package mite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// Build event phases a mite reports. These match the literal strings the
// Jenkins plugin sends on BuildEvent.phase.
const (
	phaseStarted      = "STARTED"
	phaseStageStarted = "STAGE_STARTED"
	phaseStage        = "STAGE"
	phaseHeartbeat    = "HEARTBEAT"
	phaseCompleted    = "COMPLETED"
	phaseRunSource    = "run.source"
	phaseRunInput     = "run.input"
)

// Ingest kinds the recorder accepts on builds.<cluster>.<ns>.<cell>. The
// literal values must match internal/recordstore's ingest kind constants.
const (
	kindRunStarted   = "run.started"
	kindRunStage     = "run.stage"
	kindRunCompleted = "run.completed"
	kindRunTests     = "run.tests"
	kindRunArtifacts = "run.artifacts"
	kindRunChanges   = "run.changes"
	kindRunLog       = "run.log"
	kindRunHeartbeat = "run.heartbeat"
	kindRunSource    = "run.source"
	kindRunInput     = "run.input"
	// kindRunTriggerFailed records a build trigger Jenkins refused. The
	// gateway publishes it from a failed TRIGGER_BUILD result, not from a
	// BuildEvent.
	kindRunTriggerFailed = "run.trigger_failed"
	// kindRunTriggerAccepted records a build trigger Jenkins accepted onto
	// its queue. The gateway publishes it from a successful TRIGGER_BUILD
	// result, not from a BuildEvent.
	kindRunTriggerAccepted = "run.trigger_accepted"
	// kindRunState records one run-state push outcome. The gateway
	// publishes it after it stores a file, and after it refuses a push
	// because the run passed its byte cap.
	kindRunState = "run.state"
	// kindRunResumeFailed records a RESUME_BUILD command the target could
	// not apply. The gateway publishes it from a failed RESUME_BUILD
	// result. A successful result publishes nothing, because a resumed
	// build proves itself by its own heartbeat.
	kindRunResumeFailed = "run.resume_failed"
	// kindRunCommandRefused records a cancel or an input answer the plugin
	// refused. The gateway publishes it from a failed CANCEL_BUILD,
	// PROCEED_INPUT, or ABORT_INPUT result, so the run's own reader sees
	// the refusal.
	kindRunCommandRefused = "run.command_refused"
)

// Command names a run.command_refused envelope carries. The literal values
// must match internal/recordstore's own command refusal names.
const (
	refusedCommandCancel  = "cancel"
	refusedCommandProceed = "proceed_input"
	refusedCommandAbort   = "abort_input"
)

// Reason codes a run.trigger_failed envelope carries. The set is complete:
// every status maps onto exactly one of them. reasonTriggerUnreachable is
// the one retryable code: the call reached no Jenkins ready to build it,
// so a later fire can still succeed. The other three are permanent.
const (
	reasonJobNotFound        = "JOB_NOT_FOUND"
	reasonJobNotBuildable    = "JOB_NOT_BUILDABLE"
	reasonTriggerRejected    = "TRIGGER_REJECTED"
	reasonTriggerUnreachable = "TRIGGER_UNREACHABLE"
)

// Reason codes a run.resume_failed envelope carries. The mite names five
// failures with a leading token, and every other failure answers
// reasonResumeFailed. The first two codes prove the target's own Jenkins
// process reached the build load, so the recorder keeps the dispatch for
// them and a second target never adopts the same build.
const (
	reasonResumeLoadFailed       = "RESUME_LOAD_FAILED"
	reasonResumeNotBuilding      = "RESUME_NOT_BUILDING"
	reasonResumeExpired          = "RESUME_EXPIRED"
	reasonResumeInstanceMismatch = "RESUME_INSTANCE_MISMATCH"
	reasonResumeProbeFailed      = "RESUME_PROBE_FAILED"
	reasonResumeFailed           = "RESUME_FAILED"
)

// miteInstanceMismatchToken is the token the mite writes for an instance
// mismatch. It carries no RESUME_ prefix, so the envelope's own code and
// the mite's token differ for this one failure alone.
const miteInstanceMismatchToken = "INSTANCE_MISMATCH"

// resumeReasonOrder is the order reasonCodeForResumeError tests the
// tokens in. The two codes that prove a loaded build come first, so a
// deadline that wraps a load failure still answers the load failure and
// the run keeps its dispatch. A wrong answer in the other direction
// would hand a loaded build to a second target.
var resumeReasonOrder = []struct{ token, code string }{
	{reasonResumeLoadFailed, reasonResumeLoadFailed},
	{reasonResumeNotBuilding, reasonResumeNotBuilding},
	{reasonResumeExpired, reasonResumeExpired},
	{miteInstanceMismatchToken, reasonResumeInstanceMismatch},
	{reasonResumeProbeFailed, reasonResumeProbeFailed},
}

// hasResumeToken reports whether text names token as a failure reason.
//
// The test is a bounded form, never a plain substring. The mite writes
// every named reason as "TOKEN: ", and it wraps an inner error with
// ": ". A Jenkins folder may legally be named RESUME_LOAD_FAILED_backup,
// and the mite writes a job path into the text of an unnamed failure, so
// a plain substring test would read that folder as a loaded build.
func hasResumeToken(text, token string) bool {
	return strings.Contains(text, token+": ")
}

// reasonCodeForResumeError maps a failed resume's error text onto one
// reason code. A text that names no token answers reasonResumeFailed,
// because every step the mite leaves unnamed runs before the build load.
func reasonCodeForResumeError(text string) string {
	for _, entry := range resumeReasonOrder {
		if hasResumeToken(text, entry.token) {
			return entry.code
		}
	}
	return reasonResumeFailed
}

// reasonCodeForStatus maps the Jenkins HTTP status of a failed trigger onto
// one reason code. The status 0 names a call that reached no Jenkins: a
// readiness wait that expired, a refused connection, or a crumb fetch that
// failed before the trigger call.
func reasonCodeForStatus(status int) string {
	switch status {
	case 0:
		return reasonTriggerUnreachable
	case 404:
		return reasonJobNotFound
	case 409:
		return reasonJobNotBuildable
	default:
		return reasonTriggerRejected
	}
}

// EventAck status values.
const (
	eventAckStored   = "STORED"
	eventAckRejected = "REJECTED"
)

// MaskedParamValue replaces the value of a sensitive parameter. The BFF
// reads the same constant to refuse a replay of a masked run.
const MaskedParamValue = "<masked>"

// gatewayEnvelopeBudget is the byte budget for one marshaled envelope,
// matching the plugin-inventory degradation path's headroom below NATS'
// 1 MiB message limit.
const gatewayEnvelopeBudget = 900 * 1024

// errorTruncateChars is the character cap on every error field the gateway
// publishes: a run.stage envelope's error and each command outcome record.
const errorTruncateChars = 512

// ingestEnvelope is the wire shape of one message on
// builds.<cluster>.<ns>.<cell>. Field names and JSON tags mirror
// internal/recordstore's IngestEnvelope exactly; the two packages do not
// share a Go type because the gateway and the recorder are separate
// binaries and communicate over the wire only.
type ingestEnvelope struct {
	SchemaVersion int                    `json:"schema_version"`
	Kind          string                 `json:"kind"`
	RunID         string                 `json:"run_id"`
	IntentKey     string                 `json:"intent_key"`
	Cluster       string                 `json:"cluster"`
	Namespace     string                 `json:"namespace"`
	Cell          string                 `json:"cell"`
	CellInstance  string                 `json:"cell_instance"`
	Tenant        string                 `json:"tenant"`
	JobPath       string                 `json:"job_path"`
	OccurredAt    time.Time              `json:"occurred_at"`
	Payload       map[string]interface{} `json:"payload"`
}

// changeEntry is one entry of a run.changes envelope's payload["changes"]
// array.
type changeEntry struct {
	CommitID          string    `json:"commit_id"`
	AuthorID          string    `json:"author_id"`
	AuthorDisplayName string    `json:"author_display_name"`
	Message           string    `json:"message"`
	Timestamp         time.Time `json:"timestamp"`
	AffectedPathCount int       `json:"affected_path_count"`
}

// libraryEntry is one entry of a run.source envelope's payload["libraries"]
// array.
type libraryEntry struct {
	Name            string `json:"name"`
	Version         string `json:"version,omitempty"`
	Revision        string `json:"revision,omitempty"`
	RepositoryURL   string `json:"repository_url,omitempty"`
	RepositoryForge string `json:"repository_forge,omitempty"`
}

// podTemplateEntry is one entry of a run.source envelope's
// payload["pod_templates"] array.
type podTemplateEntry struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	Image string `json:"image,omitempty"`
}

// testCaseEntry is one entry of a run.tests envelope's payload["cases"]
// array. The key names match the ones internal/recordstore's
// RunTestCasePayload decodes.
type testCaseEntry struct {
	Suite        string `json:"suite"`
	ClassName    string `json:"class_name"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	ErrorDetails string `json:"error_details,omitempty"`
}

// inputEntry is one entry of a run.input envelope's payload["inputs"]
// array. The key names match the ones the runs.inputs column holds.
type inputEntry struct {
	ID                 string                `json:"id"`
	Message            string                `json:"message,omitempty"`
	OK                 string                `json:"ok,omitempty"`
	Submitter          string                `json:"submitter,omitempty"`
	SubmitterParameter string                `json:"submitter_parameter,omitempty"`
	Parameters         []inputParameterEntry `json:"parameters,omitempty"`
}

// inputParameterEntry is one parameter definition of a run.input
// envelope's input entry.
type inputParameterEntry struct {
	Name         string   `json:"name"`
	Type         string   `json:"type,omitempty"`
	Description  string   `json:"description,omitempty"`
	DefaultValue string   `json:"default_value,omitempty"`
	Choices      []string `json:"choices,omitempty"`
}

// kindPayload is one mapped envelope's kind and payload, before the
// envelope-level identity fields are stamped on.
type kindPayload struct {
	kind    string
	payload map[string]interface{}
}

// millisToTime converts a millisecond epoch value to a UTC time.Time. A
// zero input has no meaningful conversion; callers check for zero before
// calling this so an absent field stays absent rather than becoming the
// Unix epoch.
func millisToTime(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}

// truncateError cuts an error text to at most errorTruncateChars runes, so
// a truncation never splits a multi-byte UTF-8 character.
func truncateError(s string) string {
	r := []rune(s)
	if len(r) <= errorTruncateChars {
		return s
	}
	return string(r[:errorTruncateChars])
}

// maskParameters copies params, replacing the value of every name in
// masked with the literal <masked>. It returns nil for an empty map, so
// callers can omit the payload key entirely.
func maskParameters(params map[string]string, masked []string) map[string]string {
	if len(params) == 0 {
		return nil
	}
	maskedSet := make(map[string]bool, len(masked))
	for _, name := range masked {
		maskedSet[name] = true
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		if maskedSet[k] {
			out[k] = MaskedParamValue
		} else {
			out[k] = v
		}
	}
	return out
}

// addTemplateFields copies evt's four template-resolution fields onto p,
// omitting any field the event does not carry. A run with no template
// carries none, so p gains no key. This runs only for kinds C1 folds into
// runs.payload (run.started, run.completed): design.md D14 reads the
// resolved template from the job's most recent run, so a stage-level
// envelope needs no copy.
func addTemplateFields(p map[string]interface{}, evt *mitev1.BuildEvent) {
	if evt.TemplateName != "" {
		p["template_name"] = evt.TemplateName
	}
	if evt.TemplateVersion != "" {
		p["template_version"] = evt.TemplateVersion
	}
	if evt.MarkerPath != "" {
		p["marker_path"] = evt.MarkerPath
	}
	if evt.MarkerRevision != "" {
		p["marker_revision"] = evt.MarkerRevision
	}
}

// startedPayload maps a STARTED BuildEvent onto a run.started payload, per
// design.md D9's payload-key table. A field the event does not carry is
// omitted, so the recorder's coalescing merge leaves the stored column
// untouched rather than clearing it.
func startedPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{}
	if evt.BuildNumber != 0 {
		p["build_number"] = evt.BuildNumber
	}
	if evt.Branch != "" {
		p["branch"] = evt.Branch
	}
	if evt.QueuedAtMillis != 0 {
		p["queued_at"] = millisToTime(evt.QueuedAtMillis)
	}
	if evt.StartedAtMillis != 0 {
		p["started_at"] = millisToTime(evt.StartedAtMillis)
	}
	if evt.CauseDescription != "" {
		p["cause"] = evt.CauseDescription
	}
	if evt.Actor != "" {
		p["actor"] = evt.Actor
	}
	if params := maskParameters(evt.Parameters, evt.MaskedParameterNames); params != nil {
		p["params"] = params
	}
	if evt.Seq != 0 {
		p["seq"] = evt.Seq
	}
	if evt.URL != "" {
		p["url"] = evt.URL
	}
	if evt.JobDisplayName != "" {
		p["job_display_name"] = evt.JobDisplayName
	}
	if evt.BuildDisplayName != "" {
		p["build_display_name"] = evt.BuildDisplayName
	}
	if len(evt.CauseTypes) > 0 {
		p["cause_types"] = evt.CauseTypes
	}
	if evt.UpstreamJobPath != "" {
		p["upstream_job_path"] = evt.UpstreamJobPath
	}
	if evt.UpstreamBuildNumber != 0 {
		p["upstream_build_number"] = evt.UpstreamBuildNumber
	}
	if evt.QueueWaitMillis != 0 {
		p["queue_wait_ms"] = evt.QueueWaitMillis
	}
	if evt.EstimatedDurationMillis != 0 {
		p["estimated_duration_ms"] = evt.EstimatedDurationMillis
	}
	if len(evt.MaskedParameterNames) > 0 {
		p["masked_params"] = evt.MaskedParameterNames
	}
	if evt.ScmUrl != "" {
		p["scm_url"] = evt.ScmUrl
	}
	if evt.ScmRevision != "" {
		p["scm_revision"] = evt.ScmRevision
	}
	if evt.DeliveryId != "" {
		p["delivery_id"] = evt.DeliveryId
	}
	if evt.ReplayOf != "" {
		p["replay_of"] = evt.ReplayOf
	}
	if evt.RestartOf != "" {
		p["restart_of"] = evt.RestartOf
	}
	if evt.RestartStage != "" {
		p["restart_stage"] = evt.RestartStage
	}
	if evt.GrantedBuildNumber != 0 {
		p["granted_build_number"] = evt.GrantedBuildNumber
	}
	if cr := changeRequestPayload(evt.ChangeRequest); cr != nil {
		p["change_request"] = cr
	}
	addTemplateFields(p, evt)
	return p
}

// changeRequestPayload maps a change request onto the run.started
// change_request object. It returns nil when the request carries no id,
// and it omits each empty key.
func changeRequestPayload(cr *mitev1.ChangeRequestInfo) map[string]interface{} {
	if cr == nil || cr.Id == "" {
		return nil
	}
	p := map[string]interface{}{"id": cr.Id}
	if cr.Title != "" {
		p["title"] = cr.Title
	}
	if cr.SourceBranch != "" {
		p["source_branch"] = cr.SourceBranch
	}
	if cr.TargetBranch != "" {
		p["target_branch"] = cr.TargetBranch
	}
	if cr.URL != "" {
		p["url"] = cr.URL
	}
	return p
}

// stagePayload maps one BuildStage onto a run.stage payload. It carries
// the typed subset C1 reads plus every other BuildStage field, because
// C1 keeps the whole envelope in run_stages.payload even where it has no
// typed column.
func stagePayload(st *mitev1.BuildStage) map[string]interface{} {
	p := map[string]interface{}{
		"stage_id":  st.StageId,
		"name":      st.Name,
		"node_type": st.NodeType,
		"status":    st.Status,
	}
	if st.ParentId != "" {
		p["parent_id"] = st.ParentId
	}
	if st.StartedAtMillis != 0 {
		p["started_at"] = millisToTime(st.StartedAtMillis)
	}
	if st.EndedAtMillis != 0 {
		p["ended_at"] = millisToTime(st.EndedAtMillis)
	}
	if st.DurationMillis != 0 {
		p["duration_ms"] = st.DurationMillis
	}
	if st.Error != "" {
		p["error"] = st.Error
	}
	if len(st.ParentIds) > 0 {
		p["parent_ids"] = st.ParentIds
	}
	if st.EnclosingId != "" {
		p["enclosing_id"] = st.EnclosingId
	}
	if st.ParallelBranchName != "" {
		p["parallel_branch_name"] = st.ParallelBranchName
	}
	if st.PauseDurationMillis != 0 {
		p["pause_duration_millis"] = st.PauseDurationMillis
	}
	if st.NodeName != "" {
		p["node_name"] = st.NodeName
	}
	if st.AgentLabel != "" {
		p["agent_label"] = st.AgentLabel
	}
	return p
}

// heartbeatPayload maps a HEARTBEAT BuildEvent onto a run.heartbeat
// payload. It writes seq unconditionally, including the value zero: the
// recorder's summary guard reads that value and needs it present on
// every heartbeat. It writes summary only for a non-empty value, so a
// run that sets no summary carries no summary key. It writes the inputs
// key for every event, including an empty set: the recorder reads that
// key to repair a lost clearing event.
func heartbeatPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{"seq": evt.Seq}
	p["inputs"] = inputEntries(evt.Inputs)
	if evt.Summary != "" {
		p["summary"] = evt.Summary
	}
	return p
}

// changesPayload maps a BuildEvent's changes onto a run.changes payload. A
// COMPLETED event and a run.source event both carry the change list.
func changesPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	entries := make([]changeEntry, 0, len(evt.Changes))
	for _, c := range evt.Changes {
		if c == nil {
			continue
		}
		entries = append(entries, changeEntry{
			CommitID:          c.CommitId,
			AuthorID:          c.AuthorId,
			AuthorDisplayName: c.AuthorDisplayName,
			Message:           c.Message,
			Timestamp:         millisToTime(c.TimestampMillis),
			AffectedPathCount: c.AffectedPathCount,
		})
	}
	p := map[string]interface{}{"changes": entries}
	if evt.ChangeCount != 0 {
		p["change_count"] = evt.ChangeCount
	}
	return p
}

// sourcePayload maps a run.source BuildEvent onto a run.source payload. It
// writes a key only for a non-empty value, so an absent value never
// reaches the recorder as an empty string. It writes seq unconditionally,
// including the value zero. A run's first event takes seq zero, and the
// recorder's source guard reads that value.
func sourcePayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{"seq": evt.Seq}
	if evt.Branch != "" {
		p["branch"] = evt.Branch
	}
	if evt.ScmRevision != "" {
		p["scm_revision"] = evt.ScmRevision
	}
	if evt.ScmHead != "" {
		p["scm_head"] = evt.ScmHead
	}
	if evt.ScmUrl != "" {
		p["scm_url"] = evt.ScmUrl
	}
	if evt.DefinitionPath != "" {
		p["definition_path"] = evt.DefinitionPath
	}
	if libs := libraryEntries(evt.Libraries); len(libs) > 0 {
		p["libraries"] = libs
	}
	if pods := podTemplateEntries(evt.PodTemplates); len(pods) > 0 {
		p["pod_templates"] = pods
	}
	return p
}

// libraryEntries maps the event's libraries onto payload entries. It skips
// a nil element, and an absent list answers no entries.
func libraryEntries(in []*mitev1.BuildLibrary) []libraryEntry {
	out := make([]libraryEntry, 0, len(in))
	for _, l := range in {
		if l == nil {
			continue
		}
		out = append(out, libraryEntry{
			Name:            l.Name,
			Version:         l.Version,
			Revision:        l.Revision,
			RepositoryURL:   l.RepositoryUrl,
			RepositoryForge: l.RepositoryForge,
		})
	}
	return out
}

// podTemplateEntries maps the event's pod templates onto payload entries.
// It skips a nil element, and an absent list answers no entries.
func podTemplateEntries(in []*mitev1.BuildPodTemplate) []podTemplateEntry {
	out := make([]podTemplateEntry, 0, len(in))
	for _, t := range in {
		if t == nil {
			continue
		}
		out = append(out, podTemplateEntry{Name: t.Name, Label: t.Label, Image: t.Image})
	}
	return out
}

// testCaseEntries maps the event's test cases onto payload entries. It
// skips a nil element and keeps the event's order, because the plugin
// sends the cases in priority order.
func testCaseEntries(in []*mitev1.BuildTestCase) []testCaseEntry {
	out := make([]testCaseEntry, 0, len(in))
	for _, c := range in {
		if c == nil {
			continue
		}
		out = append(out, testCaseEntry{
			Suite:        c.Suite,
			ClassName:    c.ClassName,
			Name:         c.Name,
			Status:       c.Status,
			DurationMs:   c.DurationMs,
			ErrorDetails: c.ErrorDetails,
		})
	}
	return out
}

// inputEntries maps the event's pending inputs onto payload entries. It
// skips a nil element, and an absent list answers an empty slice. The
// slice is never nil, because the recorder reads the key's presence.
func inputEntries(in []*mitev1.BuildInput) []inputEntry {
	out := make([]inputEntry, 0, len(in))
	for _, i := range in {
		if i == nil {
			continue
		}
		entry := inputEntry{
			ID:                 i.Id,
			Message:            i.Message,
			OK:                 i.Ok,
			Submitter:          i.Submitter,
			SubmitterParameter: i.SubmitterParameter,
		}
		for _, p := range i.Parameters {
			if p == nil {
				continue
			}
			entry.Parameters = append(entry.Parameters, inputParameterEntry{
				Name:         p.Name,
				Type:         p.Type,
				Description:  p.Description,
				DefaultValue: p.DefaultValue,
				Choices:      p.Choices,
			})
		}
		out = append(out, entry)
	}
	return out
}

// inputsPayload maps a run.input BuildEvent onto a run.input payload. It
// writes seq unconditionally, including the value zero, because the
// recorder's inputs guard reads that value. It writes the inputs key for
// every set, empty or not: the recorder replaces its stored set with the
// payload's set, and an empty set clears the stored set.
func inputsPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	return map[string]interface{}{
		"seq":    evt.Seq,
		"inputs": inputEntries(evt.Inputs),
	}
}

// testsPayload maps a COMPLETED BuildEvent's test counters onto the
// aggregate run.tests payload. suite is always the sentinel "_total". The
// cases list spans every suite, because the plugin sends one set per run.
// It writes cases and test_report only when the event carries each.
func testsPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{
		"suite":   "_total",
		"total":   evt.TestTotal,
		"failed":  evt.TestFailed,
		"skipped": evt.TestSkipped,
		"passed":  evt.TestPassed,
	}
	if len(evt.TestCases) > 0 {
		p["cases"] = testCaseEntries(evt.TestCases)
	}
	if evt.TestReport != nil {
		p["test_report"] = map[string]interface{}{
			"blob_key":   evt.TestReport.BlobKey,
			"size_bytes": evt.TestReport.SizeBytes,
			"case_count": evt.TestReport.CaseCount,
		}
	}
	return p
}

// completedPayload maps a COMPLETED BuildEvent onto a run.completed
// payload. It writes seq unconditionally, including the value zero, for
// the same reason heartbeatPayload does. It writes summary only for a
// non-empty value.
func completedPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{"seq": evt.Seq}
	if evt.Result != "" {
		p["result"] = evt.Result
	}
	if evt.CompletedAtMillis != 0 {
		p["ended_at"] = millisToTime(evt.CompletedAtMillis)
	}
	if evt.DurationMillis != 0 {
		p["duration_ms"] = evt.DurationMillis
	}
	if evt.ScmRevision != "" {
		p["scm_revision"] = evt.ScmRevision
	}
	addTemplateFields(p, evt)
	if evt.Summary != "" {
		p["summary"] = evt.Summary
	}
	if len(evt.RestartStages) > 0 {
		p["restart_stages"] = evt.RestartStages
	}
	if evt.RestartRefusal != "" {
		p["restart_refusal"] = evt.RestartRefusal
	}
	if evt.RejectedSignature != "" {
		p["rejected_signature"] = evt.RejectedSignature
	}
	return p
}

// artifactPayload maps one BuildArtifact onto a run.artifacts payload.
// Field names mirror internal/recordstore's RunArtifactsPayload; that
// struct carries no field for BuildArtifact.storage or
// BuildArtifact.display_path, so this mapping carries neither.
func artifactPayload(art *mitev1.BuildArtifact) map[string]interface{} {
	p := map[string]interface{}{
		"path":     art.Path,
		"blob_key": art.BlobKey,
	}
	if art.SizeBytes != 0 {
		p["size_bytes"] = art.SizeBytes
	}
	return p
}

// logSummaryPayload maps a COMPLETED BuildEvent's LogSummary onto a
// run.log payload, merged with the gateway's own per-run incompleteness
// flag by logical OR. finalized always comes from the gateway's own
// knowledge: the plugin's LogSummary never sets it, because only the
// gateway's durable final index entry proves the log is finished.
func (h *BusHandler) logSummaryPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{}
	var incomplete bool
	var reason string
	if ls := evt.LogSummary; ls != nil {
		p["chunk_count"] = ls.ChunkCount
		p["dropped_bytes"] = ls.DroppedBytes
		incomplete = ls.Incomplete
		reason = ls.IncompleteReason
	}
	if gwIncomplete, gwReason := h.runIncompleteness(evt.RunId); gwIncomplete {
		incomplete = true
		if reason == "" {
			reason = gwReason
		}
	}
	p["incomplete"] = incomplete
	if reason != "" {
		p["incomplete_reason"] = reason
	}
	p["finalized"] = h.runFinalized(evt.RunId)
	return p
}

// buildPayloadGroup maps one non-heartbeat BuildEvent onto the ordered
// list of kinds and payloads design.md D9 states. The caller handles
// HEARTBEAT separately, because a heartbeat is droppable and every other
// phase is not. run.artifacts (one envelope per BuildArtifact) and
// run.log publish immediately before run.completed, per design.md A0.
func (h *BusHandler) buildPayloadGroup(evt *mitev1.BuildEvent) ([]kindPayload, error) {
	switch evt.Phase {
	case phaseStarted:
		return []kindPayload{{kindRunStarted, startedPayload(evt)}}, nil

	case phaseStageStarted, phaseStage:
		if len(evt.Stages) != 1 {
			return nil, errors.New("malformed stage event: want exactly one stage")
		}
		payload := stagePayload(evt.Stages[0])
		if evt.Phase == phaseStageStarted {
			// A defensive restamp: the plugin already sets IN_PROGRESS for
			// a start event, but the gateway does not trust the payload
			// for this invariant.
			payload["status"] = "IN_PROGRESS"
		}
		return []kindPayload{{kindRunStage, payload}}, nil

	case phaseRunSource:
		// The run.source envelope goes first: it carries the revision the
		// change list belongs to.
		out := []kindPayload{{kindRunSource, sourcePayload(evt)}}
		if len(evt.Changes) > 0 {
			out = append(out, kindPayload{kindRunChanges, changesPayload(evt)})
		}
		return out, nil

	case phaseRunInput:
		return []kindPayload{{kindRunInput, inputsPayload(evt)}}, nil

	case phaseCompleted:
		out := make([]kindPayload, 0, len(evt.Stages)+len(evt.Artifacts)+4)
		for _, st := range evt.Stages {
			if st == nil {
				continue
			}
			out = append(out, kindPayload{kindRunStage, stagePayload(st)})
		}
		if len(evt.Changes) > 0 {
			out = append(out, kindPayload{kindRunChanges, changesPayload(evt)})
		}
		// A report or a case list publishes run.tests on its own: a report
		// whose case count is zero still reaches the store.
		if evt.TestTotal > 0 || len(evt.TestCases) > 0 || evt.TestReport != nil {
			out = append(out, kindPayload{kindRunTests, testsPayload(evt)})
		}
		for _, art := range evt.Artifacts {
			if art == nil {
				continue
			}
			out = append(out, kindPayload{kindRunArtifacts, artifactPayload(art)})
		}
		out = append(out, kindPayload{kindRunLog, h.logSummaryPayload(evt)})
		out = append(out, kindPayload{kindRunCompleted, completedPayload(evt)})
		return out, nil

	default:
		return nil, fmt.Errorf("unknown phase %q", evt.Phase)
	}
}

// shrinkToBudget applies the size ladder to env until its marshaled size
// fits gatewayEnvelopeBudget. It returns false when env still does not
// fit. The ladder truncates payload["error"] first. It then drops
// payload["cases"] entries from the end, because the list is priority
// ordered and the first entry is the case a reader needs. It then drops
// entries from the oldest, in this order: payload["changes"],
// payload["pod_templates"], payload["libraries"], payload["stages"]. No
// kind this package emits carries a plural "stages" key today, so the
// last step stays defensive.
func shrinkToBudget(env *ingestEnvelope) bool {
	fits := func() bool {
		data, err := json.Marshal(env)
		return err == nil && len(data) <= gatewayEnvelopeBudget
	}
	if fits() {
		return true
	}

	if errVal, ok := env.Payload["error"].(string); ok && errVal != "" {
		env.Payload["error"] = truncateError(errVal)
		if fits() {
			return true
		}
	}

	// A rung that empties the list removes the key. The recorder reads an
	// empty list as an order to clear the run's stored set, and an absent
	// key as no change to it.
	if cases, ok := env.Payload["cases"].([]testCaseEntry); ok {
		for len(cases) > 0 {
			cases = cases[:len(cases)-1]
			if len(cases) == 0 {
				delete(env.Payload, "cases")
			} else {
				env.Payload["cases"] = cases
			}
			if fits() {
				return true
			}
		}
	}

	if changes, ok := env.Payload["changes"].([]changeEntry); ok {
		for len(changes) > 0 {
			changes = changes[1:]
			env.Payload["changes"] = changes
			if fits() {
				return true
			}
		}
	}

	if pods, ok := env.Payload["pod_templates"].([]podTemplateEntry); ok {
		for len(pods) > 0 {
			pods = pods[1:]
			env.Payload["pod_templates"] = pods
			if fits() {
				return true
			}
		}
	}

	if libs, ok := env.Payload["libraries"].([]libraryEntry); ok {
		for len(libs) > 0 {
			libs = libs[1:]
			env.Payload["libraries"] = libs
			if fits() {
				return true
			}
		}
	}

	if stages, ok := env.Payload["stages"].([]interface{}); ok {
		for len(stages) > 0 {
			stages = stages[1:]
			env.Payload["stages"] = stages
			if fits() {
				return true
			}
		}
	}

	return fits()
}

// buildEventItem is one unit the publish worker drains: either the single
// droppable envelope of a HEARTBEAT event, or every ordered envelope of
// one other BuildEvent, acknowledged together once all are durably
// published.
type buildEventItem struct {
	heartbeat bool
	runID     string
	eventSeq  int64
	envelopes []ingestEnvelope
}

// maxQueueEnvelopes bounds the publish worker's queue. It is sized for
// roughly one large COMPLETED event (at most 1000 stages plus a changes,
// a tests, and a completed envelope, per the plugin's own D11 caps).
// enqueue admits one larger item into an empty queue.
const maxQueueEnvelopes = 1024

// buildEventWorker is the per-connection publish worker. One instance
// runs for the life of one CommandStream, so envelopes publish in the
// per-cell order the recorder's monotonic status merge needs, and a slow
// stream never blocks another controller.
type buildEventWorker struct {
	ctx  context.Context
	send Sender

	mu        sync.Mutex
	queue     []buildEventItem
	queueLen  int // sum of len(item.envelopes) across queue
	notify    chan struct{}
	roomFreed chan struct{}
}

func newBuildEventWorker(ctx context.Context, send Sender) *buildEventWorker {
	return &buildEventWorker{
		ctx:       ctx,
		send:      send,
		notify:    make(chan struct{}, 1),
		roomFreed: make(chan struct{}, 1),
	}
}

// tryEnqueueHeartbeat enqueues item without waiting for room. It reports
// false when the queue has no room, so the caller can drop and count the
// heartbeat instead.
func (w *buildEventWorker) tryEnqueueHeartbeat(item buildEventItem) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queueLen+len(item.envelopes) > maxQueueEnvelopes {
		return false
	}
	w.queue = append(w.queue, item)
	w.queueLen += len(item.envelopes)
	w.wakeLocked()
	return true
}

// enqueue waits up to wait for room for the whole item, so a group's
// envelopes are never split across two waits. It reports false when no
// room freed within wait, or the connection ended while waiting.
//
// An item whose own envelope count exceeds maxQueueEnvelopes is admitted
// when the queue is empty. The plugin caps stages but not artifacts, so
// one COMPLETED event can outgrow the bound. Without this exception each
// retry of that event is rejected again forever. The empty-queue rule
// still stops a pile of smaller items.
func (w *buildEventWorker) enqueue(item buildEventItem, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		w.mu.Lock()
		if w.queueLen == 0 || w.queueLen+len(item.envelopes) <= maxQueueEnvelopes {
			w.queue = append(w.queue, item)
			w.queueLen += len(item.envelopes)
			w.wakeLocked()
			w.mu.Unlock()
			return true
		}
		w.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-w.roomFreed:
			timer.Stop()
		case <-timer.C:
			return false
		case <-w.ctx.Done():
			timer.Stop()
			return false
		}
	}
}

// wakeLocked signals the drain goroutine. Caller must hold w.mu.
func (w *buildEventWorker) wakeLocked() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// dequeue removes and returns the oldest queued item, and signals any
// producer waiting for room.
func (w *buildEventWorker) dequeue() (buildEventItem, bool) {
	w.mu.Lock()
	if len(w.queue) == 0 {
		w.mu.Unlock()
		return buildEventItem{}, false
	}
	item := w.queue[0]
	w.queue = w.queue[1:]
	w.queueLen -= len(item.envelopes)
	w.mu.Unlock()
	select {
	case w.roomFreed <- struct{}{}:
	default:
	}
	return item, true
}

// registeredBuildWorker pairs a worker with the connection epoch it was
// created for, so a lookup from a superseded stream's connToken misses.
type registeredBuildWorker struct {
	token  int64
	worker *buildEventWorker
}

// getBuildWorker returns the worker for name/ns, or nil when none is
// registered or connToken names a superseded connection.
func (h *BusHandler) getBuildWorker(ns, name string, connToken int64) *buildEventWorker {
	key := ns + "/" + name
	h.buildMu.Lock()
	defer h.buildMu.Unlock()
	rw, ok := h.buildWorkers[key]
	if !ok || (connToken != 0 && rw.token != connToken) {
		return nil
	}
	return rw.worker
}

// startBuildEventWorker registers and starts the publish worker for one
// new connection. Called from OnConnect with the same ctx and send the
// other per-connection watchers use.
func (h *BusHandler) startBuildEventWorker(ctx context.Context, ns, name string, token int64, send Sender) {
	key := ns + "/" + name
	w := newBuildEventWorker(ctx, send)
	h.buildMu.Lock()
	if h.buildWorkers == nil {
		h.buildWorkers = make(map[string]*registeredBuildWorker)
	}
	h.buildWorkers[key] = &registeredBuildWorker{token: token, worker: w}
	h.buildMu.Unlock()
	go h.runBuildEventWorker(ctx, ns, name, w)
}

// clearBuildWorker removes the worker registered for token, if any. A
// call for a token a newer connection already replaced is a no-op, so a
// superseded stream's late teardown cannot drop the live connection's
// worker.
func (h *BusHandler) clearBuildWorker(ns, name string, token int64) {
	key := ns + "/" + name
	h.buildMu.Lock()
	defer h.buildMu.Unlock()
	if rw, ok := h.buildWorkers[key]; ok && (token == 0 || rw.token == token) {
		delete(h.buildWorkers, key)
	}
}

// runBuildEventWorker drains w until ctx is done. It never publishes on
// the CommandStream read loop: OnBuildEvent only enqueues, and this
// goroutine performs every JetStream publish and every EventAck send.
func (h *BusHandler) runBuildEventWorker(ctx context.Context, ns, name string, w *buildEventWorker) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.notify:
		}
		for {
			if ctx.Err() != nil {
				return
			}
			item, ok := w.dequeue()
			if !ok {
				break
			}
			h.deliverBuildEventItem(ctx, ns, name, w.send, item)
		}
	}
}

// deliverBuildEventItem publishes every envelope of item in order, then
// sends STORED once all publishes are durably acknowledged. A heartbeat
// item publishes asynchronously and best-effort, and never gets an
// EventAck.
func (h *BusHandler) deliverBuildEventItem(ctx context.Context, ns, name string, send Sender, item buildEventItem) {
	subject := bus.BuildSubject(h.cluster, ns, name)

	if item.heartbeat {
		if len(item.envelopes) != 1 {
			return
		}
		h.publishHeartbeatEnvelope(ctx, subject, item.envelopes[0], ns, name)
		return
	}

	for _, env := range item.envelopes {
		data, err := json.Marshal(env)
		if err != nil {
			h.log().Error("marshal build envelope failed",
				"controller", ns+"/"+name, "kind", env.Kind, "error", err)
			return
		}
		if !h.publishWithRetry(ctx, subject, data, env.Kind, ns, name) {
			// ctx ended (disconnect). The mite still retains the event and
			// replays it after reconnect, so abandoning here is safe.
			return
		}
	}
	h.sendEventAck(send, item.runID, item.eventSeq, eventAckStored, "")
}

const (
	defaultPublishRetryDelay = 1 * time.Second
	publishRetryMaxDelay     = 30 * time.Second
)

// publishRetryDelay returns the starting retry delay for a rejected
// build-event publish. buildPublishRetryDelay lets a test shrink it; zero
// means defaultPublishRetryDelay.
func (h *BusHandler) publishRetryDelay() time.Duration {
	if h.buildPublishRetryDelay > 0 {
		return h.buildPublishRetryDelay
	}
	return defaultPublishRetryDelay
}

// publishWithRetry publishes data to subject and waits for the JetStream
// ack, retrying with a growing delay until it succeeds. It returns false
// only when ctx ends first.
func (h *BusHandler) publishWithRetry(ctx context.Context, subject string, data []byte, kind, ns, name string) bool {
	delay := h.publishRetryDelay()
	for {
		_, err := h.publisher.PublishJetStream(subject, data)
		if err == nil {
			return true
		}
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("build event publish rejected, retrying",
			"controller", ns+"/"+name, "kind", kind, "retryIn", delay, "error", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		if delay *= 2; delay > publishRetryMaxDelay {
			delay = publishRetryMaxDelay
		}
	}
}

// publishHeartbeatEnvelope publishes one run.heartbeat envelope
// asynchronously and reads its PubAckFuture. A publish failure is
// counted and never retried: the next heartbeat follows within the
// interval.
func (h *BusHandler) publishHeartbeatEnvelope(ctx context.Context, subject string, env ingestEnvelope, ns, name string) {
	data, err := json.Marshal(env)
	if err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Error("marshal heartbeat envelope failed", "controller", ns+"/"+name, "error", err)
		return
	}
	future, err := h.publisher.PublishJetStreamAsync(subject, data)
	if err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("heartbeat publish failed", "controller", ns+"/"+name, "error", err)
		return
	}
	select {
	case <-future.Ok():
	case pubErr := <-future.Err():
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("heartbeat publish rejected", "controller", ns+"/"+name, "error", pubErr)
	case <-ctx.Done():
	}
}

// sendEventAck sends one OperatorMessage_EventAck to the mite.
func (h *BusHandler) sendEventAck(send Sender, runID string, eventSeq int64, status, reason string) {
	msg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_EventAck{
			EventAck: &mitev1.EventAck{
				RunId:    runID,
				EventSeq: eventSeq,
				Status:   status,
				Reason:   reason,
			},
		},
	}
	if err := send(msg); err != nil {
		h.log().Error("send event ack failed",
			"run_id", runID, "event_seq", eventSeq, "status", status, "error", err)
	}
}

// buildEnvelope stamps the identity fields of one ingest envelope. cell
// and cell_instance never come from the event payload: cell/namespace
// come from the mTLS stream identity (the name/ns parameters), and
// cell_instance from the mite's registered pod unique identifier. tenant
// is resolved once per event by the caller, through CellTenantResolver.
func (h *BusHandler) buildEnvelope(ns, name string, evt *mitev1.BuildEvent, kind string, payload map[string]interface{}, tenant string) ingestEnvelope {
	return ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kind,
		RunID:         evt.RunId,
		IntentKey:     evt.IntentKey,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		Tenant:        tenant,
		JobPath:       evt.JobPath,
		OccurredAt:    millisToTime(evt.EmittedAtMillis),
		Payload:       payload,
	}
}

// RecordCellInstance stores the pod unique identifier a mite presented at
// registration, keyed by controller. Server.Register calls this so a
// later build event on this connection — possibly a different
// CommandStream than the one Register arrived on — can stamp the same
// value the mite also sends on a ClaimRequest.
func (h *BusHandler) RecordCellInstance(name, ns, cellInstance string) {
	h.mu.Lock()
	if h.cellInstances == nil {
		h.cellInstances = make(map[string]string)
	}
	h.cellInstances[ns+"/"+name] = cellInstance
	h.mu.Unlock()
}

func (h *BusHandler) lookupCellInstance(ns, name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cellInstances[ns+"/"+name]
}

const defaultEventPublishWait = 10 * time.Second

// eventPublishWait reads VARROA_EVENT_PUBLISH_WAIT, the back-pressure
// budget OnBuildEvent waits for room in the publish worker's queue
// before it ends the stream.
func eventPublishWait() time.Duration {
	v := os.Getenv("VARROA_EVENT_PUBLISH_WAIT")
	if v == "" {
		return defaultEventPublishWait
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultEventPublishWait
	}
	return d
}

// OnBuildEvent maps evt onto one or more ingest envelopes and enqueues
// them on this connection's publish worker. It never publishes to
// JetStream itself: a synchronous publish of a large COMPLETED event
// would stall this controller's heartbeats on the CommandStream read
// loop, which is where the caller runs this method from.
func (h *BusHandler) OnBuildEvent(name, ns string, connToken int64, evt *mitev1.BuildEvent) error {
	w := h.getBuildWorker(ns, name, connToken)
	if w == nil {
		// The connection already ended. The read loop's IsCurrentConnection
		// gate should already have stopped this call from reaching here.
		return nil
	}

	// Resolved once per event, from this event's own job path: one cell
	// hosts many tenants, so a build event's tenant is not fixed per
	// connection the way cluster/namespace/cell are (design.md D14).
	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, evt.JobPath)
	if tenant == "" {
		// A cell must not write a record under a tenant it does not
		// host. This is an admission refusal, not the back-pressure case
		// below: the mite must release the event, not replay it forever.
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		if evt.Phase != phaseHeartbeat {
			h.sendEventAck(w.send, evt.RunId, evt.Seq, eventAckRejected, "unhosted tenant")
		}
		return nil
	}

	if evt.Phase == phaseHeartbeat {
		env := h.buildEnvelope(ns, name, evt, kindRunHeartbeat, heartbeatPayload(evt), tenant)
		item := buildEventItem{heartbeat: true, envelopes: []ingestEnvelope{env}}
		if !w.tryEnqueueHeartbeat(item) {
			h.metrics.buildEventsDropped.Add(context.Background(), 1)
		}
		return nil
	}

	if evt.Phase == phaseCompleted && evt.RunId != "" {
		// The log writer's finalize deadline anchors on this moment, so
		// it must be recorded before the ordered envelope group below
		// reads the gateway's own incompleteness and finalized knowledge.
		h.markRunTerminal(ns, name, evt.RunId, evt.JobPath, millisToTime(evt.CompletedAtMillis))
	}

	kps, err := h.buildPayloadGroup(evt)
	if err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.sendEventAck(w.send, evt.RunId, evt.Seq, eventAckRejected, err.Error())
		return nil
	}

	envs := make([]ingestEnvelope, 0, len(kps))
	for _, kp := range kps {
		env := h.buildEnvelope(ns, name, evt, kp.kind, kp.payload, tenant)
		if !shrinkToBudget(&env) {
			h.metrics.buildEventsDropped.Add(context.Background(), 1)
			h.sendEventAck(w.send, evt.RunId, evt.Seq, eventAckRejected, "OVERSIZED")
			return nil
		}
		envs = append(envs, env)
	}

	item := buildEventItem{runID: evt.RunId, eventSeq: evt.Seq, envelopes: envs}
	if !w.enqueue(item, eventPublishWait()) {
		return fmt.Errorf("build event queue full for %s/%s: no room within the publish wait", ns, name)
	}
	return nil
}
