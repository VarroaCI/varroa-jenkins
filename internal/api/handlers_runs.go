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

type runDetailJSON struct {
	RunID             string            `json:"runId"`
	JobPath           string            `json:"jobPath"`
	TenantFolder      string            `json:"tenantFolder"`
	Tenant            string            `json:"tenant"`
	BuildNumber       int64             `json:"buildNumber,omitempty"`
	Branch            string            `json:"branch,omitempty"`
	SCMRevision       string            `json:"scmRevision,omitempty"`
	Status            string            `json:"status"`
	Result            string            `json:"result,omitempty"`
	QueuedAt          *time.Time        `json:"queuedAt,omitempty"`
	StartedAt         *time.Time        `json:"startedAt,omitempty"`
	EndedAt           *time.Time        `json:"endedAt,omitempty"`
	DurationMs        int64             `json:"durationMs,omitempty"`
	Actor             string            `json:"actor,omitempty"`
	Cause             string            `json:"cause,omitempty"`
	Params            map[string]string `json:"params,omitempty"`
	TestsTotal        int32             `json:"testsTotal,omitempty"`
	TestsFailed       int32             `json:"testsFailed,omitempty"`
	TestsSkipped      int32             `json:"testsSkipped,omitempty"`
	LogFinalized      bool              `json:"logFinalized,omitempty"`
	LogComplete       bool              `json:"logComplete,omitempty"`
	InterruptedReason string            `json:"interruptedReason,omitempty"`
	TriggerError      string            `json:"triggerError,omitempty"`
	TriggerReasonCode string            `json:"triggerReasonCode,omitempty"`
	RetryRunID        string            `json:"retryRunId,omitempty"`
	Stages            []runStageJSON    `json:"stages,omitempty"`
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

type runTestListResponse struct {
	Items []runTestSuiteJSON `json:"items"`
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
	}
	for _, st := range stages {
		d.Stages = append(d.Stages, buildRunStage(st))
	}
	return d
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	run, stages, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}

	retryRunID := ""
	if intent, found, err := s.deps.RecordStore.GetIntentByKey(ctx, "retry:"+run.RunID); err == nil && found {
		retryRunID = intent.RunID
	}

	tenantFolder := ""
	if ref, err := TenantRefFromRow(run.Cluster, run.Namespace, run.Cell, run.Tenant); err == nil {
		tenantFolder = ref.Folder
	} else {
		s.deps.Logger.Warn("resolve run tenant folder failed", "runId", run.RunID, "error", err)
	}

	s.writeJSON(w, http.StatusOK, buildRunDetail(run, stages, retryRunID, tenantFolder))
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
	out := make([]runTestSuiteJSON, len(suites))
	for i, ts := range suites {
		out[i] = runTestSuiteJSON{Suite: ts.Suite, Total: ts.Total, Failed: ts.Failed, Skipped: ts.Skipped}
		if ts.DurationMs != nil {
			out[i].DurationMs = *ts.DurationMs
		}
	}
	s.writeJSON(w, http.StatusOK, runTestListResponse{Items: out})
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

	artifacts, err := s.deps.RecordStore.ListArtifacts(ctx, run.RunID)
	if s.handleReaderError(w, err, "list artifacts") {
		return
	}
	var artifact *recordstore.Artifact
	for i := range artifacts {
		if artifacts[i].Path == path {
			artifact = &artifacts[i]
			break
		}
	}
	if artifact == nil {
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
