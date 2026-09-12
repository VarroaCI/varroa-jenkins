// Package api: the non-HTTP entry points the pipeline MCP tools call
// (design D1a: "the four MCP tools call the same internal services the
// handlers call, so each tool calls the seam itself"). Every method
// here is a thin wrapper over the unexported core the matching HTTP
// handler already uses — handlers_pipelines.go's listRunsMerged and
// resolveTeamTenants, handlers_pipelines_write.go's createRun, and
// handlers_runs.go's runByID — so the MCP surface and the REST surface
// share one implementation of every rule. The exported result types
// below (RunSummary, RunListResult, RunDetail, RunStage) restate the
// JSON shape of the HTTP layer's own unexported response types: revive's
// unexported-return rule forbids an exported method from returning an
// unexported type, so a shape-only mirror is unavoidable here, even
// though every field on it comes from converting the HTTP layer's own
// value.
//
// This file exists separately from handlers_pipelines.go because that
// file is under concurrent edit elsewhere in the same change; every
// function this file calls is unexported and already defined there or
// in a sibling handlers_*.go file.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// RunSummary is one row of a run list: the same shape the pipelines
// REST paths serialize (components/schemas.yaml#RunSummary).
type RunSummary struct {
	RunID       string     `json:"runId"`
	BuildNumber int64      `json:"buildNumber,omitempty"`
	JobPath     string     `json:"jobPath,omitempty"`
	Status      string     `json:"status"`
	Result      string     `json:"result,omitempty"`
	Branch      string     `json:"branch,omitempty"`
	SCMRevision string     `json:"scmRevision,omitempty"`
	Actor       string     `json:"actor,omitempty"`
	Cause       string     `json:"cause,omitempty"`
	QueuedAt    *time.Time `json:"queuedAt,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	DurationMs  int64      `json:"durationMs,omitempty"`
}

// RunListResult is ListRuns' result: RunSummary rows in the same order
// GET .../runs returns, plus the cross-tenant cursor for the next page
// when one exists.
type RunListResult struct {
	Items      []RunSummary `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// RunStage is one stage row of a RunDetail.
type RunStage struct {
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

// RunDetail is GetRun's result: the same shape GET /runs/{run}
// serializes (components/schemas.yaml#RunDetail).
type RunDetail struct {
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
	Stages            []RunStage        `json:"stages,omitempty"`
}

// convertJSON re-shapes in into a T by round-tripping through JSON. Every
// caller here converts between two types whose json tags already agree
// field for field, so the round trip is a pure re-typing, never a
// projection: it exists only to give an exported method an exported
// return type without hand-copying each field.
func convertJSON[T any](in any) (T, error) {
	var out T
	b, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, err
	}
	return out, nil
}

// readerServiceError maps a recordstore.Reader failure onto a
// *ServiceError: nil stays nil, ErrSchemaTooOld is 503, and every other
// error is 500. It is handleReaderError's non-HTTP counterpart, for a
// caller that reports through a *ServiceError instead of writing an
// HTTP response.
func (s *Server) readerServiceError(err error, action string) *ServiceError {
	if err == nil {
		return nil
	}
	if errors.Is(err, recordstore.ErrSchemaTooOld) {
		return &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	s.deps.Logger.Error(action, "error", err)
	return &ServiceError{Status: http.StatusInternalServerError, Message: action + " failed"}
}

// RunPipeline is the run_pipeline MCP tool's entry into design D3's
// write path. The MCP tool carries no client token, so the BFF mints a
// fresh run identifier for every call (design D3a: "a request that
// carries no clientToken gets one the BFF mints as a UUIDv7"). It
// authorizes with the tenant write check, exactly as
// POST /pipelines/{team}/jobs/{job}/runs does. targetCluster is the
// cluster the command actually reached, for the caller's own activity
// audit; it is empty whenever serr is set.
func (s *Server) RunPipeline(ctx context.Context, claims *auth.Claims, team, jobPath, cluster string, parameters map[string]string) (runID, intentKey, targetCluster string, serr *ServiceError) {
	runID, intentKey, ref, serr := s.createRun(ctx, claims, team, jobPath, "", cluster, parameters)
	return runID, intentKey, ref.Cluster, serr
}

// ListRuns is the list_runs MCP tool's entry into design D1's
// cross-tenant run list. limit is the caller's already-clamped value:
// the MCP tool clamps to 200 rather than answering 400 on an
// out-of-range value, matching list_activity, so the clamp happens in
// the tool, not here. The four filters are separate strings, not
// handlers_pipelines.go's unexported listRunsFilters, because that type
// cannot appear in another package's call to an exported method.
func (s *Server) ListRuns(ctx context.Context, claims *auth.Claims, team, jobPath, branch, result, actor, cause string, limit int, cursor string) (RunListResult, *ServiceError, error) {
	if s.deps.RecordStore == nil {
		return RunListResult{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}, nil
	}
	refs, serr := s.resolveTeamTenants(ctx, claims, team)
	if serr != nil {
		return RunListResult{}, serr, nil
	}
	var admitted []TenantRef
	for _, ref := range refs {
		if s.canReadPipeline(claims, ref, jobPath) {
			admitted = append(admitted, ref)
		}
	}
	if len(admitted) == 0 {
		return RunListResult{}, &ServiceError{Status: http.StatusNotFound, Message: "job not found"}, nil
	}
	filters := listRunsFilters{branch: branch, result: result, actor: actor, cause: cause}
	resp, serr, err := s.listRunsMerged(ctx, admitted, jobPath, filters, limit, cursor)
	if serr != nil || err != nil {
		return RunListResult{}, serr, err
	}
	out, cerr := convertJSON[RunListResult](resp)
	if cerr != nil {
		return RunListResult{}, nil, cerr
	}
	if out.Items == nil {
		out.Items = []RunSummary{}
	}
	return out, nil, nil
}

// GetRun is the get_run MCP tool's entry into design D1a's run read
// path: it authorizes with the same run read check as GET /runs/{run}
// and returns the same detail shape.
func (s *Server) GetRun(ctx context.Context, claims *auth.Claims, runID string) (RunDetail, *ServiceError) {
	if s.deps.RecordStore == nil {
		return RunDetail{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	run, stages, serr := s.runByID(ctx, claims, runID)
	if serr != nil {
		return RunDetail{}, serr
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
	out, cerr := convertJSON[RunDetail](buildRunDetail(run, stages, retryRunID, tenantFolder))
	if cerr != nil {
		return RunDetail{}, &ServiceError{Status: http.StatusInternalServerError, Message: "encode run detail failed"}
	}
	return out, nil
}

// GetRunLog is the get_run_log MCP tool's entry into design D5's log
// history reader, bounded to a tail instead of streamed (mcp-jenkins-proxy,
// "get_run_log SHALL return a bounded text result"). It authorizes with
// the run read check, reads the run's console object, splits it into
// lines with console notes removed by the same rule the SSE path uses
// (splitRunLogLines), and returns the last tailLines of them plus the
// total line count. It starts no live tail: it never touches the tail
// registry the SSE handler uses, so an agent that needs live output
// must poll.
func (s *Server) GetRunLog(ctx context.Context, claims *auth.Claims, runID string, tailLines int) (text string, totalLines int64, serr *ServiceError) {
	if s.deps.RecordStore == nil {
		return "", 0, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	if s.deps.BlobStore == nil {
		return "", 0, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	run, _, serr := s.runByID(ctx, claims, runID)
	if serr != nil {
		return "", 0, serr
	}

	consoleData, err := readRunLogObject(ctx, s.deps.BlobStore, blob.LogKey(run.RunID))
	if err != nil {
		s.deps.Logger.Error("read run log", "run_id", run.RunID, "error", err)
		return "", 0, &ServiceError{Status: http.StatusInternalServerError, Message: "read run log failed"}
	}

	lines := splitRunLogLines(consoleData, map[int64]runLogIndexEntry{})
	total := int64(len(lines))
	start := total - int64(tailLines)
	if start < 0 {
		start = 0
	}
	texts := make([]string, 0, total-start)
	for _, ln := range lines[start:] {
		texts = append(texts, ln.Text)
	}
	return strings.Join(texts, "\n"), total, nil
}
