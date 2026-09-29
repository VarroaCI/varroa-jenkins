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
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
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
	RunID             string                   `json:"runId"`
	JobPath           string                   `json:"jobPath"`
	TenantFolder      string                   `json:"tenantFolder"`
	Tenant            string                   `json:"tenant"`
	BuildNumber       int64                    `json:"buildNumber,omitempty"`
	Branch            string                   `json:"branch,omitempty"`
	SCMRevision       string                   `json:"scmRevision,omitempty"`
	Status            string                   `json:"status"`
	Result            string                   `json:"result,omitempty"`
	QueuedAt          *time.Time               `json:"queuedAt,omitempty"`
	StartedAt         *time.Time               `json:"startedAt,omitempty"`
	EndedAt           *time.Time               `json:"endedAt,omitempty"`
	DurationMs        int64                    `json:"durationMs,omitempty"`
	Actor             string                   `json:"actor,omitempty"`
	Cause             string                   `json:"cause,omitempty"`
	Params            map[string]string        `json:"params,omitempty"`
	TestsTotal        int32                    `json:"testsTotal,omitempty"`
	TestsFailed       int32                    `json:"testsFailed,omitempty"`
	TestsSkipped      int32                    `json:"testsSkipped,omitempty"`
	LogFinalized      bool                     `json:"logFinalized,omitempty"`
	LogComplete       bool                     `json:"logComplete,omitempty"`
	InterruptedReason string                   `json:"interruptedReason,omitempty"`
	TriggerError      string                   `json:"triggerError,omitempty"`
	TriggerReasonCode string                   `json:"triggerReasonCode,omitempty"`
	RetryRunID        string                   `json:"retryRunId,omitempty"`
	Stages            []RunStage               `json:"stages,omitempty"`
	Source            *runSourceJSON           `json:"source,omitempty"`
	Summary           string                   `json:"summary,omitempty"`
	Digest            *runDigestJSON           `json:"digest,omitempty"`
	Inputs            []runInputJSON           `json:"inputs,omitempty"`
	TestReport        *runTestReportDetailJSON `json:"testReport,omitempty"`
	Cases             []runTestCaseJSON        `json:"cases,omitempty"`
	ReplayOf          string                   `json:"replayOf,omitempty"`
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

// GetRunScript is GET /runs/{run}/script's service entry. It authorizes
// with the run read check, so a run the caller may not read answers 404,
// and it answers the same shape the HTTP path serializes.
func (s *Server) GetRunScript(ctx context.Context, claims *auth.Claims, runID string) (RunScript, *ServiceError) {
	if s.deps.RecordStore == nil || s.deps.BlobStore == nil {
		return RunScript{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	run, _, serr := s.runByID(ctx, claims, runID)
	if serr != nil {
		return RunScript{}, serr
	}
	out, err := s.runScriptSource(ctx, run)
	if err != nil {
		s.deps.Logger.Error("read run script", "runId", run.RunID, "error", err)
		return RunScript{}, &ServiceError{Status: http.StatusInternalServerError, Message: "read run script failed"}
	}
	return out, nil
}

// ReplayRun is the replay_run MCP tool's entry into POST
// /runs/{run}/replay. It authorizes with the tenant write check on the
// run's own tenant, exactly as the HTTP path does, and reports no outcome:
// the caller reads the new run to learn it. targetCluster is the cluster
// of the home member the command reached, for the caller's own activity
// audit; it is empty whenever serr is set.
func (s *Server) ReplayRun(ctx context.Context, claims *auth.Claims, runID, script string) (newRunID, intentKey, targetCluster string, serr *ServiceError) {
	return s.replayRun(ctx, claims, runID, script)
}

// ProceedRunInput is the proceed_run_input MCP tool's entry into the
// proceed route. It authorizes with the tenant write check on the run's
// own tenant, exactly as POST /runs/{run}/inputs/{input}/proceed does,
// and reports no outcome: the caller reads the run again to learn it. It
// answers the cluster the command went to, for the tool's audit event.
func (s *Server) ProceedRunInput(ctx context.Context, claims *auth.Claims, runID, inputID string, parameters map[string]string) (targetCluster string, serr *ServiceError) {
	return s.answerRunInput(ctx, claims, runID, inputID, mitev1.CommandTypeProceedInput, parameters)
}

// AbortRunInput is the abort_run_input MCP tool's entry into the abort
// route. It authorizes the same way ProceedRunInput does, and carries no
// parameters.
func (s *Server) AbortRunInput(ctx context.Context, claims *auth.Claims, runID, inputID string) (targetCluster string, serr *ServiceError) {
	return s.answerRunInput(ctx, claims, runID, inputID, mitev1.CommandTypeAbortInput, nil)
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
	detail := buildRunDetail(run, stages, retryRunID, tenantFolder)
	// A failed case read degrades the two case lists alone: the run read
	// and the rest of the digest still answer.
	cases, casesErr := s.deps.RecordStore.ListTestCases(ctx, run.RunID)
	if casesErr != nil {
		s.deps.Logger.Warn("read run test cases failed", "runId", run.RunID, "error", casesErr)
	} else if len(cases) > 0 {
		detail.Cases = buildRunTestCases(cases)
	}
	detail.Digest = computeRunDigest(ctx, s.deps.BlobStore, run, stages, runTestCaseRead{cases: cases, err: casesErr})
	out, cerr := convertJSON[RunDetail](detail)
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

// FleetLockRow is one row of a fleet lock list: the same shape both lock
// routes serialize (components/schemas.yaml#FleetLock). The response
// carries no holder field and no queue position, so a consumer that
// wants either groups the rows it already has by name.
type FleetLockRow struct {
	Name         string     `json:"name"`
	Tenant       string     `json:"tenant"`
	JobPath      string     `json:"jobPath"`
	State        string     `json:"state"`
	RunID        string     `json:"runId"`
	RunLink      *string    `json:"runLink,omitempty"`
	WaitingSince *time.Time `json:"waitingSince,omitempty"`
	AcquiredAt   *time.Time `json:"acquiredAt,omitempty"`
}

// ListFleetLocks is the list_fleet_locks MCP tool's entry into the team
// lock read. It is the same read GET /pipelines/{team}/locks serves, so
// the tool and the route authorize a caller and shape a row
// identically. The tenant read check answers for every resolved tenant,
// and a team the caller may read on no tenant answers not found.
func (s *Server) ListFleetLocks(ctx context.Context, claims *auth.Claims, team string) ([]FleetLockRow, *ServiceError) {
	if s.deps.RecordStore == nil {
		return nil, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	refs, serr := s.resolveTeamTenants(ctx, claims, team)
	if serr != nil {
		return nil, serr
	}
	var admitted []TenantRef
	for _, ref := range refs {
		if s.canReadPipeline(claims, ref, "") {
			admitted = append(admitted, ref)
		}
	}
	if len(admitted) == 0 {
		return nil, &ServiceError{Status: http.StatusNotFound, Message: "team not found"}
	}

	rows, err := s.listTeamFleetLocks(ctx, claims, admitted)
	if serr := s.readerServiceError(err, "list fleet locks"); serr != nil {
		return nil, serr
	}
	out, cerr := convertJSON[[]FleetLockRow](rows)
	if cerr != nil {
		return nil, &ServiceError{Status: http.StatusInternalServerError, Message: "encode fleet locks failed"}
	}
	if out == nil {
		out = []FleetLockRow{}
	}
	return out, nil
}
