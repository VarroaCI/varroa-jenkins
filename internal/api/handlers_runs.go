// The run-scoped read paths (design D1a, D2, D6): GET /runs/{run} and its
// artifacts, tests, changes, and download sub-resources. Every handler
// here authorizes with the run read check on the record's own tenant and
// job path (internal/api/handlers_pipelines_auth.go's canReadRun), never
// on a controller, so a run whose cell was deleted still resolves for a
// permitted caller. Cancel and rerun live in handlers_pipelines_write.go,
// which authorizes them with the tenant write check instead. The log
// stream stub at the bottom answers 501 until section 5 implements it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// --- Response shapes (api/openapi/components/schemas.yaml) ---

type runStageJSON struct {
	StageID    string     `json:"stageId"`
	ParentID   string     `json:"parentId,omitempty"`
	Name       string     `json:"name"`
	NodeType   string     `json:"nodeType,omitempty"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
	DurationMs int64      `json:"durationMs,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// runSourceLibraryJSON is one shared library the run was defined with.
type runSourceLibraryJSON struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// runSourcePodTemplateJSON is one pod template the run allocated.
type runSourcePodTemplateJSON struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	Image string `json:"image,omitempty"`
}

// runSourceJSON describes how the run was defined. The recorder stores these
// facts under snake_case keys, so buildRunDetail maps the stored shape into
// this type field by field.
type runSourceJSON struct {
	DefinitionPath string                     `json:"definitionPath,omitempty"`
	Libraries      []runSourceLibraryJSON     `json:"libraries,omitempty"`
	PodTemplates   []runSourcePodTemplateJSON `json:"podTemplates,omitempty"`
}

// runInputParameterJSON is one parameter definition of a pending input step.
// It carries the response's camelCase key names, never the stored column's
// snake_case key names.
type runInputParameterJSON struct {
	Name         string   `json:"name"`
	Type         string   `json:"type,omitempty"`
	Description  string   `json:"description,omitempty"`
	DefaultValue string   `json:"defaultValue,omitempty"`
	Choices      []string `json:"choices,omitempty"`
}

// runInputJSON is one pending input step of a run. The recorder stores these
// facts under snake_case keys, so buildRunInputs maps the stored shape into
// this type field by field.
type runInputJSON struct {
	ID                 string                  `json:"id"`
	Message            string                  `json:"message,omitempty"`
	OK                 string                  `json:"ok,omitempty"`
	Submitter          string                  `json:"submitter,omitempty"`
	SubmitterParameter string                  `json:"submitterParameter,omitempty"`
	Parameters         []runInputParameterJSON `json:"parameters,omitempty"`
}

type runDetailJSON struct {
	RunID             string             `json:"runId"`
	JobPath           string             `json:"jobPath"`
	TenantFolder      string             `json:"tenantFolder"`
	Tenant            string             `json:"tenant"`
	BuildNumber       int64              `json:"buildNumber,omitempty"`
	Branch            string             `json:"branch,omitempty"`
	SCMRevision       string             `json:"scmRevision,omitempty"`
	Status            string             `json:"status"`
	Result            string             `json:"result,omitempty"`
	QueuedAt          *time.Time         `json:"queuedAt,omitempty"`
	StartedAt         *time.Time         `json:"startedAt,omitempty"`
	EndedAt           *time.Time         `json:"endedAt,omitempty"`
	DurationMs        int64              `json:"durationMs,omitempty"`
	Actor             string             `json:"actor,omitempty"`
	Cause             string             `json:"cause,omitempty"`
	SCMURL            string             `json:"scmUrl,omitempty"`
	TriggerSource     string             `json:"triggerSource,omitempty"`
	ChangeRequest     *changeRequestJSON `json:"changeRequest,omitempty"`
	Params            map[string]string  `json:"params,omitempty"`
	TestsTotal        int32              `json:"testsTotal,omitempty"`
	TestsFailed       int32              `json:"testsFailed,omitempty"`
	TestsSkipped      int32              `json:"testsSkipped,omitempty"`
	LogFinalized      bool               `json:"logFinalized,omitempty"`
	LogComplete       bool               `json:"logComplete,omitempty"`
	InterruptedReason string             `json:"interruptedReason,omitempty"`
	TriggerError      string             `json:"triggerError,omitempty"`
	TriggerReasonCode string             `json:"triggerReasonCode,omitempty"`
	RetryRunID        string             `json:"retryRunId,omitempty"`
	Stages            []runStageJSON     `json:"stages,omitempty"`
	Source            *runSourceJSON     `json:"source,omitempty"`
	Summary           string             `json:"summary,omitempty"`
	Digest            *runDigestJSON     `json:"digest,omitempty"`
	Inputs            []runInputJSON     `json:"inputs,omitempty"`
	// TestReport is absent when the run holds no stored report. Cases is
	// absent when the run holds no case row, unlike the tests read's own
	// cases array, which is always present.
	TestReport *runTestReportDetailJSON `json:"testReport,omitempty"`
	Cases      []runTestCaseJSON        `json:"cases,omitempty"`
	// ReplayOf names the run a replayed build copied its script from. It
	// is absent for a build that is not a replay.
	ReplayOf string `json:"replayOf,omitempty"`
	// CommandRefusal is the newest cancel or input answer the plugin
	// refused for this run. It is absent when no such refusal exists. A
	// later successful command does not clear it.
	CommandRefusal *runCommandRefusalJSON `json:"commandRefusal,omitempty"`
	// Upstream names the run that started this run. Downstream holds the
	// first page of the runs this run started, and GET /runs/{run}/downstream
	// pages the rest. Both are absent when no link exists.
	Upstream   *RunLink     `json:"upstream,omitempty"`
	Downstream *RunLinkPage `json:"downstream,omitempty"`
}

// runCommandRefusalJSON is the response shape of a stored refusal notice.
type runCommandRefusalJSON struct {
	Command    string    `json:"command"`
	Actor      string    `json:"actor,omitempty"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
	Error      string    `json:"error,omitempty"`
	RefusedAt  time.Time `json:"refusedAt"`
}

type runArtifactJSON struct {
	Path         string `json:"path"`
	SizeBytes    int64  `json:"sizeBytes"`
	Digest       string `json:"digest,omitempty"`
	ContentType  string `json:"contentType,omitempty"`
	Downloadable bool   `json:"downloadable"`
}

type runArtifactListResponse struct {
	Items []runArtifactJSON `json:"items"`
}

type runTestSuiteJSON struct {
	Suite      string `json:"suite"`
	Total      int32  `json:"total"`
	Failed     int32  `json:"failed"`
	Skipped    int32  `json:"skipped"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

// testReportPath is the archived path of the per-case test report, the
// one the record store names.
const testReportPath = recordstore.TestReportPath

// runTestCaseJSON is one stored test case of a run. CaseKey is the opaque
// key the case history read takes back unchanged.
type runTestCaseJSON struct {
	Suite        string `json:"suite"`
	ClassName    string `json:"className"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	DurationMs   int64  `json:"durationMs,omitempty"`
	ErrorDetails string `json:"errorDetails,omitempty"`
	CaseKey      string `json:"caseKey"`
}

// runTestReportJSON is the tests read's report object. SizeBytes and
// CaseCount carry no omitempty, because a zero case count is a real
// answer. Downloadable is false when the store holds no blob key.
type runTestReportJSON struct {
	Path         string `json:"path"`
	SizeBytes    int64  `json:"sizeBytes"`
	CaseCount    int32  `json:"caseCount"`
	Downloadable bool   `json:"downloadable"`
}

// runTestReportDetailJSON is the run detail's testReport object. BlobKey
// is empty for a report that a controller stores locally.
type runTestReportDetailJSON struct {
	BlobKey   string `json:"blobKey"`
	SizeBytes int64  `json:"sizeBytes"`
	CaseCount int32  `json:"caseCount"`
}

// runTestListResponse is the tests read. Cases is never null: an empty
// array is its value for a run with no case row.
type runTestListResponse struct {
	Items  []runTestSuiteJSON `json:"items"`
	Cases  []runTestCaseJSON  `json:"cases"`
	Report *runTestReportJSON `json:"report,omitempty"`
}

// TestCaseKey mints the opaque case key the tests read returns and the
// case history read takes back. url.PathEscape escapes a "/" as "%2F", so
// neither half holds the separator and the split back is unambiguous.
func TestCaseKey(className, name string) string {
	return url.PathEscape(className) + "/" + url.PathEscape(name)
}

// ParseTestCaseKey splits key on the first "/" and unescapes each half. A
// key with no "/", or a half that does not unescape, answers an error.
func ParseTestCaseKey(key string) (className, name string, err error) {
	rawClass, rawName, ok := strings.Cut(key, "/")
	if !ok {
		return "", "", errors.New("test case key holds no separator")
	}
	if className, err = url.PathUnescape(rawClass); err != nil {
		return "", "", fmt.Errorf("test case key class name: %w", err)
	}
	if name, err = url.PathUnescape(rawName); err != nil {
		return "", "", fmt.Errorf("test case key test name: %w", err)
	}
	return className, name, nil
}

// buildRunTestCases maps the stored case rows onto the response shape, in
// the reader's own order. It never answers nil.
func buildRunTestCases(cases []recordstore.TestCase) []runTestCaseJSON {
	out := make([]runTestCaseJSON, 0, len(cases))
	for _, c := range cases {
		entry := runTestCaseJSON{
			Suite:        c.Suite,
			ClassName:    c.ClassName,
			Name:         c.Name,
			Status:       c.Status,
			ErrorDetails: c.ErrorDetails,
			CaseKey:      TestCaseKey(c.ClassName, c.Name),
		}
		if c.DurationMs != nil {
			entry.DurationMs = *c.DurationMs
		}
		out = append(out, entry)
	}
	return out
}

// runTestReportPayload reads the test_report object the recorder merges
// into Run.Payload. A nil TestReport means the run holds no stored report.
type runTestReportPayload struct {
	TestReport *struct {
		SizeBytes int64 `json:"size_bytes"`
		CaseCount int32 `json:"case_count"`
	} `json:"test_report"`
}

// storedTestReport answers the run's stored report: the size, the case
// count, and the blob key. found is false when runs.payload holds no
// test_report object, or when the column does not decode.
func storedTestReport(run recordstore.Run) (sizeBytes int64, caseCount int32, blobKey string, found bool) {
	if len(run.Payload) == 0 {
		return 0, 0, "", false
	}
	var payload runTestReportPayload
	if json.Unmarshal(run.Payload, &payload) != nil || payload.TestReport == nil {
		return 0, 0, "", false
	}
	if run.TestReportKey != nil {
		blobKey = *run.TestReportKey
	}
	return payload.TestReport.SizeBytes, payload.TestReport.CaseCount, blobKey, true
}

// --- Shared resolution ---

// runByID reads runID and authorizes it with the run read check,
// reporting a *ServiceError instead of writing an HTTP response.
// resolveRunOrNotFound is its HTTP wrapper; the run-scoped MCP tools
// (get_run, get_run_log) call runByID directly, so both surfaces share
// the one lookup.
func (s *Server) runByID(ctx context.Context, claims *auth.Claims, runID string) (recordstore.Run, []recordstore.Stage, *ServiceError) {
	run, stages, err := s.deps.RecordStore.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, recordstore.ErrRunNotFound) {
			return recordstore.Run{}, nil, &ServiceError{Status: http.StatusNotFound, Message: "run not found"}
		}
		return recordstore.Run{}, nil, s.readerServiceError(err, "get run")
	}
	if !s.canReadRun(claims, run.Tenant, run.JobPath) {
		return recordstore.Run{}, nil, &ServiceError{Status: http.StatusNotFound, Message: "run not found"}
	}
	return run, stages, nil
}

// resolveRunOrNotFound reads runID and authorizes it with the run read
// check. It writes the response and returns ok=false for every failure:
// a missing run and an unreadable run both answer 404, so the surface
// never confirms that a record the caller may not read exists.
func (s *Server) resolveRunOrNotFound(w http.ResponseWriter, r *http.Request, claims *auth.Claims, runID string) (recordstore.Run, []recordstore.Stage, bool) {
	run, stages, serr := s.runByID(r.Context(), claims, runID)
	if serr != nil {
		s.writeServiceError(w, serr)
		return recordstore.Run{}, nil, false
	}
	return run, stages, true
}

// --- GET /runs/{run} ---

func buildRunStage(st recordstore.Stage) runStageJSON {
	out := runStageJSON{StageID: st.StageID, Name: st.Name, NodeType: st.NodeType, Status: st.Status}
	if st.ParentID != nil {
		out.ParentID = *st.ParentID
	}
	out.StartedAt = st.StartedAt
	out.EndedAt = st.EndedAt
	if st.DurationMs != nil {
		out.DurationMs = *st.DurationMs
	}
	if st.Error != nil {
		out.Error = *st.Error
	}
	return out
}

// runLogSummaryPayload reads the two log-finality fields C3 writes into
// Run.Payload's log_summary object. recordstore.Run carries no typed
// LogSummary field: C1's schema keeps it as free-form JSON.
type runLogSummaryPayload struct {
	LogSummary struct {
		Finalized  bool `json:"finalized"`
		Incomplete bool `json:"incomplete"`
	} `json:"log_summary"`
}

// runTriggerFailedPayload reads the two reason fields the recorder writes
// into Run.Payload for a refused build trigger. The stored
// trigger_http_status reaches no response field: it is a diagnostic value
// for an operator who reads the row directly.
type runTriggerFailedPayload struct {
	TriggerError      string `json:"trigger_error"`
	TriggerReasonCode string `json:"trigger_reason_code"`
}

// runInterruptedPayload reads the reason field the interruption sweep
// writes into Run.Payload (internal/recordstore's markInterrupted) when
// it marks a run INTERRUPTED.
type runInterruptedPayload struct {
	InterruptedReason string `json:"interrupted_reason"`
}

// runCommandRefusalPayload reads the refusal notice the recorder writes
// into Run.Payload for a refused cancel or input answer
// (internal/recordstore's applyCommandRefused).
type runCommandRefusalPayload struct {
	CommandRefusal *struct {
		Command    string    `json:"command"`
		Actor      string    `json:"actor"`
		HTTPStatus int       `json:"http_status"`
		Error      string    `json:"error"`
		RefusedAt  time.Time `json:"refused_at"`
	} `json:"command_refusal"`
}

// buildRunSource maps the stored column shape into the response shape, field
// by field. It answers nil when the column carries none of the three facts.
func buildRunSource(stored recordstore.StoredRunSource) *runSourceJSON {
	if stored.DefinitionPath == "" && len(stored.Libraries) == 0 && len(stored.PodTemplates) == 0 {
		return nil
	}
	out := &runSourceJSON{DefinitionPath: stored.DefinitionPath}
	for _, lib := range stored.Libraries {
		out.Libraries = append(out.Libraries, runSourceLibraryJSON{
			Name:     lib.Name,
			Version:  lib.Version,
			Revision: lib.Revision,
		})
	}
	for _, pod := range stored.PodTemplates {
		out.PodTemplates = append(out.PodTemplates, runSourcePodTemplateJSON{
			Name:  pod.Name,
			Label: pod.Label,
			Image: pod.Image,
		})
	}
	return out
}

// buildRunInputs maps the stored column shape into the response shape, field
// by field. It answers nil when the column carries no input.
func buildRunInputs(stored []recordstore.StoredRunInput) []runInputJSON {
	if len(stored) == 0 {
		return nil
	}
	out := make([]runInputJSON, 0, len(stored))
	for _, in := range stored {
		entry := runInputJSON{
			ID:                 in.ID,
			Message:            in.Message,
			OK:                 in.OK,
			Submitter:          in.Submitter,
			SubmitterParameter: in.SubmitterParameter,
		}
		for _, p := range in.Parameters {
			entry.Parameters = append(entry.Parameters, runInputParameterJSON{
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

func buildRunDetail(run recordstore.Run, stages []recordstore.Stage, retryRunID, tenantFolder string) runDetailJSON {
	d := runDetailJSON{RunID: run.RunID, JobPath: run.JobPath, TenantFolder: tenantFolder, Tenant: run.Tenant, Status: run.Status, RetryRunID: retryRunID}
	if run.BuildNumber != nil {
		d.BuildNumber = *run.BuildNumber
	}
	if run.Branch != nil {
		d.Branch = *run.Branch
	}
	if run.SCMRevision != nil {
		d.SCMRevision = *run.SCMRevision
	}
	if run.Result != nil {
		d.Result = *run.Result
	}
	d.QueuedAt = run.QueuedAt
	d.StartedAt = run.StartedAt
	d.EndedAt = run.EndedAt
	if run.DurationMs != nil {
		d.DurationMs = *run.DurationMs
	}
	if run.Actor != nil {
		d.Actor = *run.Actor
	}
	d.Cause = causeSummary(run.Cause)
	d.SCMURL, d.TriggerSource = runOriginOf(run.Payload)
	d.ChangeRequest = changeRequestOf(run.Payload)
	// The column holds snake_case keys, so decode it into the store shape.
	// buildRunSource then maps the three fields one by one. A NULL column
	// and a malformed column both leave the pointer nil.
	if len(run.Source) > 0 {
		var stored recordstore.StoredRunSource
		if json.Unmarshal(run.Source, &stored) == nil {
			d.Source = buildRunSource(stored)
		}
	}
	// The column holds snake_case keys, so decode it into the store shape.
	// buildRunInputs then maps the fields one by one. A NULL column, a
	// malformed column, and an empty array all leave the slice nil. The
	// block tests no status: a terminal row holds no input on the write
	// side, except a provisional heartbeat-lost mark, whose owner can
	// still wait on that input. The read hides no live input.
	if len(run.Inputs) > 0 {
		var stored []recordstore.StoredRunInput
		if json.Unmarshal(run.Inputs, &stored) == nil {
			d.Inputs = buildRunInputs(stored)
		}
	}
	// A NULL summary column leaves the response key absent. The digest
	// stays the zero value here: it needs the blob store, so Server.GetRun
	// sets it after this function returns.
	if run.Summary != nil {
		d.Summary = *run.Summary
	}
	if run.ReplayOf != nil {
		d.ReplayOf = run.ReplayOf.String()
	}
	if len(run.Params) > 0 {
		var params map[string]string
		if json.Unmarshal(run.Params, &params) == nil {
			d.Params = params
		}
	}
	if run.TestsTotal != nil {
		d.TestsTotal = *run.TestsTotal
	}
	if run.TestsFailed != nil {
		d.TestsFailed = *run.TestsFailed
	}
	if run.TestsSkipped != nil {
		d.TestsSkipped = *run.TestsSkipped
	}
	if len(run.Payload) > 0 {
		var payload runLogSummaryPayload
		if json.Unmarshal(run.Payload, &payload) == nil {
			d.LogFinalized = payload.LogSummary.Finalized
			d.LogComplete = !payload.LogSummary.Incomplete
		}
		var trigger runTriggerFailedPayload
		if json.Unmarshal(run.Payload, &trigger) == nil {
			d.TriggerError = trigger.TriggerError
			d.TriggerReasonCode = trigger.TriggerReasonCode
		}
		// A real terminal event that supersedes an interruption keeps the
		// stored reason, so the result gates the field.
		var interrupted runInterruptedPayload
		if d.Result == "INTERRUPTED" && json.Unmarshal(run.Payload, &interrupted) == nil {
			d.InterruptedReason = interrupted.InterruptedReason
		}
		var refusal runCommandRefusalPayload
		if json.Unmarshal(run.Payload, &refusal) == nil && refusal.CommandRefusal != nil && refusal.CommandRefusal.Command != "" {
			c := refusal.CommandRefusal
			d.CommandRefusal = &runCommandRefusalJSON{
				Command: c.Command, Actor: c.Actor, HTTPStatus: c.HTTPStatus,
				Error: c.Error, RefusedAt: c.RefusedAt,
			}
		}
	}
	if size, count, key, found := storedTestReport(run); found {
		d.TestReport = &runTestReportDetailJSON{BlobKey: key, SizeBytes: size, CaseCount: count}
	}
	for _, st := range stages {
		d.Stages = append(d.Stages, buildRunStage(st))
	}
	return d
}

// handleGetRun serves GET /runs/{run} from Server.GetRun, the one read
// path the get_run MCP tool also calls, so the REST and the MCP surfaces
// answer the same bytes. It keeps its own early 503 on a nil record
// store, which is the response this surface gives a nil store for every
// run-scoped route.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	detail, serr := s.GetRun(ctx, claims, r.PathValue("run"))
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, detail)
}

// --- GET /runs/{run}/artifacts ---

func (s *Server) handleListRunArtifacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	run, _, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}
	artifacts, err := s.deps.RecordStore.ListArtifacts(ctx, run.RunID)
	if s.handleReaderError(w, err, "list artifacts") {
		return
	}
	out := make([]runArtifactJSON, len(artifacts))
	for i, a := range artifacts {
		out[i] = runArtifactJSON{Path: a.Path, SizeBytes: a.SizeBytes, Downloadable: a.BlobKey != ""}
		if a.Digest != nil {
			out[i].Digest = *a.Digest
		}
		if a.ContentType != nil {
			out[i].ContentType = *a.ContentType
		}
	}
	s.writeJSON(w, http.StatusOK, runArtifactListResponse{Items: out})
}

// --- GET /runs/{run}/tests ---

func (s *Server) handleListRunTests(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	run, _, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}
	suites, err := s.deps.RecordStore.ListTests(ctx, run.RunID)
	if s.handleReaderError(w, err, "list tests") {
		return
	}
	cases, err := s.deps.RecordStore.ListTestCases(ctx, run.RunID)
	if s.handleReaderError(w, err, "list test cases") {
		return
	}
	out := make([]runTestSuiteJSON, len(suites))
	for i, ts := range suites {
		out[i] = runTestSuiteJSON{Suite: ts.Suite, Total: ts.Total, Failed: ts.Failed, Skipped: ts.Skipped}
		if ts.DurationMs != nil {
			out[i].DurationMs = *ts.DurationMs
		}
	}
	resp := runTestListResponse{Items: out, Cases: buildRunTestCases(cases)}
	if size, count, key, found := storedTestReport(run); found {
		resp.Report = &runTestReportJSON{Path: testReportPath, SizeBytes: size, CaseCount: count, Downloadable: key != ""}
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// --- GET /runs/{run}/changes ---

// parseRunChanges reads runs.changes, a free-form JSON array or object
// C2 writes from the Jenkins ChangeLogSet, and returns one entry per
// change. It never returns nil, so the items envelope stays non-null.
func parseRunChanges(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return []json.RawMessage{}
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		if arr == nil {
			arr = []json.RawMessage{}
		}
		return arr
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil && len(obj) > 0 {
		return []json.RawMessage{raw}
	}
	return []json.RawMessage{}
}

func (s *Server) handleListRunChanges(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	run, _, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": parseRunChanges(run.Changes)})
}

// --- GET /runs/{run}/artifacts/{path...} ---

// parseByteRange reads a single-range "bytes=start-end" or "bytes=start-"
// Range header value. It reports ok=false for a suffix range
// ("bytes=-N") or a malformed header, either of which falls back to a
// full read: a byte range is a resume optimization, not a requirement.
func parseByteRange(header string) (offset, length int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, -1, false
	}
	spec := strings.SplitN(strings.TrimPrefix(header, prefix), ",", 2)[0]
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		return 0, -1, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 {
		return 0, -1, false
	}
	if parts[1] == "" {
		return start, -1, true
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start {
		return 0, -1, false
	}
	return start, end - start + 1, true
}

func artifactFilename(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// handleGetRunScript answers GET /runs/{run}/script: the run's own Groovy
// script, its loaded scripts, and whether it replays. It applies the run
// read check, so a run the caller may not read answers 404.
func (s *Server) handleGetRunScript(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil || s.deps.BlobStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	out, serr := s.GetRunScript(r.Context(), claims, r.PathValue("run"))
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDownloadRunArtifact(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	if s.deps.BlobStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	run, _, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}
	path := r.PathValue("path")

	// GetArtifact reads an internal key too. The tests response gives the
	// report's internal path, and this route serves that download.
	artifact, found, err := s.deps.RecordStore.GetArtifact(ctx, run.RunID, path)
	if s.handleReaderError(w, err, "get artifact") {
		return
	}
	if !found {
		s.writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if artifact.BlobKey == "" {
		s.writeJSONError(w, http.StatusConflict, "artifact was stored by Jenkins locally; set recordStore.blobs.mode to a blob-backed mode to enable download")
		return
	}

	offset, length, ranged := int64(0), int64(-1), false
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		if o, l, headerOK := parseByteRange(rangeHeader); headerOK {
			offset, length, ranged = o, l, true
		}
	}

	// downloadRunArtifact reads the recorded blob_key with the
	// native-object read: an artifact is one object the artifact plugin
	// wrote, never a chunked sequence, so the chunked range reader (logs
	// only) must not be used here.
	body, total, err := s.deps.BlobStore.NewObjectReader(ctx, artifact.BlobKey, offset, length)
	if err != nil {
		if errors.Is(err, blob.ErrObjectNotFound) {
			s.writeJSONError(w, http.StatusNotFound, "artifact not found")
			return
		}
		s.deps.Logger.Error("open artifact", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "open artifact failed")
		return
	}
	defer func() { _ = body.Close() }()

	contentType := "application/octet-stream"
	if artifact.ContentType != nil && *artifact.ContentType != "" {
		contentType = *artifact.ContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", artifactFilename(path)))
	w.Header().Set("Accept-Ranges", "bytes")

	served := length
	if served < 0 {
		served = total - offset
	}
	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+served-1, total))
		w.Header().Set("Content-Length", strconv.FormatInt(served, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	}
	_, _ = io.Copy(w, body)
}
