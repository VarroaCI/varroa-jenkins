package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// RunRestart is the restart answer of one run: whether a declarative
// build can restart from a stage, the reason it cannot, and the stages it
// can restart from. Stages is never nil, so the JSON array is always
// present.
type RunRestart struct {
	Restartable bool     `json:"restartable"`
	Reason      string   `json:"reason,omitempty"`
	Stages      []string `json:"stages"`
}

// The reasons a run does not restart. The plugin refusals pass through
// unchanged, so the first four below match the plugin's own strings.
const (
	restartReasonNotTerminal     = "not-terminal"
	restartReasonTemplateDefined = "template-defined"
	restartReasonNotRecorded     = "not-recorded"
	restartReasonNotDeclarative  = "not-declarative"
	restartReasonDisabled        = "disabled"
	restartReasonUnavailable     = "unavailable"
	restartReasonNoStage         = "no-restartable-stage"
	restartReasonHomeMoved       = "home-moved"
)

// restartRunMaxBodyBytes bounds the restart body. A stage name is short,
// so the bound stays small.
const restartRunMaxBodyBytes = 4096

// restartRunRequest is POST /runs/{run}/restart's body.
type restartRunRequest struct {
	Stage string `json:"stage"`
}

// restartCompletedEnvelope reads the restart answer the plugin records on
// the run.completed event. The recorder stores the whole envelope under
// payload->'run.completed', so the answer sits under the envelope's own
// payload key.
type restartCompletedEnvelope struct {
	RunCompleted *struct {
		Payload struct {
			RestartStages  []string `json:"restart_stages"`
			RestartRefusal string   `json:"restart_refusal"`
		} `json:"payload"`
	} `json:"run.completed"`
}

// runRestartSource decides whether a run restarts from a stage. It reads
// the run row and the restart answer the plugin recorded when the run
// completed. It never calls Jenkins.
//
// The rules run in a fixed order, and the first failure names itself. A
// stage name is restartable only while the job's home is still the cell
// that ran the build, because the source build exists only on that cell.
//
// It answers an error only for a home read that failed.
func (s *Server) runRestartSource(ctx context.Context, run recordstore.Run) (RunRestart, *ServiceError) {
	refused := func(reason string) (RunRestart, *ServiceError) {
		return RunRestart{Stages: []string{}, Reason: reason}, nil
	}
	if run.Status != recordstore.StatusCompleted && run.Status != recordstore.StatusDuplicate {
		return refused(restartReasonNotTerminal)
	}
	if definitionKind(run) == definitionKindTemplate {
		return refused(restartReasonTemplateDefined)
	}
	var doc restartCompletedEnvelope
	if len(run.Payload) == 0 || json.Unmarshal(run.Payload, &doc) != nil || doc.RunCompleted == nil {
		return refused(restartReasonNotRecorded)
	}
	if refusal := doc.RunCompleted.Payload.RestartRefusal; refusal != "" {
		return refused(refusal)
	}
	stages := doc.RunCompleted.Payload.RestartStages
	if len(stages) == 0 {
		return refused(restartReasonNoStage)
	}
	if s.deps.RecordStore != nil {
		home, err := s.deps.RecordStore.JobHome(ctx, run.Cluster, run.Tenant, run.JobPath)
		if err != nil {
			s.deps.Logger.Error("read the job home for a restart",
				"runId", run.RunID, "error", err)
			return RunRestart{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "resolve the job home member failed"}
		}
		if home.Placed && home.Cell != run.Cell {
			return refused(restartReasonHomeMoved)
		}
	}
	return RunRestart{Restartable: true, Stages: append([]string{}, stages...)}, nil
}

// GetRunRestart is GET /runs/{run}/restart's service entry. It applies the
// run read check, so a run the caller may not read answers 404.
func (s *Server) GetRunRestart(ctx context.Context, claims *auth.Claims, runID string) (RunRestart, *ServiceError) {
	if s.deps.RecordStore == nil {
		return RunRestart{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	run, _, serr := s.runByID(ctx, claims, runID)
	if serr != nil {
		return RunRestart{}, serr
	}
	return s.runRestartSource(ctx, run)
}

// restartRun validates the restart, mints the new identity, and publishes
// one RESTART_BUILD command to the member that holds the job's home slot.
// It answers a ServiceError instead of writing a response, so the MCP tool
// and the HTTP handler share one path.
//
// The key is restart:<source run>:<new run>, and the new run identifier is
// a fresh UUIDv7, never a counter, for the same reason a replay key uses
// one. The command names the source build number, which the plugin checks
// against the source run identifier before it schedules.
func (s *Server) restartRun(ctx context.Context, claims *auth.Claims, runID, stage string) (newRunID, intentKey, targetCluster string, serr *ServiceError) {
	if s.deps.RecordStore == nil {
		return "", "", "", &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	sourceRun, ref, serr := s.runForWrite(ctx, claims, runID)
	if serr != nil {
		return "", "", "", serr
	}
	// The stage keeps its exact spelling, because the plugin matches the
	// recorded name byte for byte. Only an empty value is refused here; a
	// recorded name that holds only spaces is still a valid stage.
	if stage == "" {
		return "", "", "", &ServiceError{Status: http.StatusBadRequest, Message: "stage is required"}
	}
	restart, serr := s.runRestartSource(ctx, sourceRun)
	if serr != nil {
		return "", "", "", serr
	}
	if !restart.Restartable {
		return "", "", "", &ServiceError{Status: http.StatusConflict, Message: "run does not restart: " + restart.Reason}
	}
	if !slices.Contains(restart.Stages, stage) {
		return "", "", "", &ServiceError{Status: http.StatusConflict, Message: "stage " + stage + " is not restartable"}
	}
	if sourceRun.BuildNumber == nil {
		return "", "", "", &ServiceError{Status: http.StatusConflict, Message: "run does not restart: " + restartReasonNotRecorded}
	}

	id, mintErr := uuid.NewV7()
	if mintErr != nil {
		s.deps.Logger.Error("mint restart run id", "error", mintErr)
		return "", "", "", &ServiceError{Status: http.StatusInternalServerError, Message: "mint run id failed"}
	}
	newRunID = id.String()
	intentKey = "restart:" + sourceRun.RunID + ":" + newRunID

	target, homeErr := s.homeRef(ctx, ref, sourceRun.JobPath)
	if homeErr != nil {
		return "", "", "", homeErr
	}
	cmd := &mitev1.ImperativeCommand{
		CommandId: "restart-build-" + newRunID,
		Type:      mitev1.CommandTypeRestartBuild,
		RestartBuild: &mitev1.RestartBuildPayload{
			RunId:             newRunID,
			IntentKey:         intentKey,
			JobPath:           sourceRun.JobPath,
			SourceRunId:       sourceRun.RunID,
			SourceBuildNumber: *sourceRun.BuildNumber,
			Stage:             stage,
			ActingUser:        actingUserFrom(claims),
		},
	}
	if err := s.publishImperative(ctx, target, cmd); err != nil {
		s.deps.Logger.Error("publish restart", "runId", sourceRun.RunID, "error", err)
		return "", "", "", &ServiceError{Status: http.StatusInternalServerError, Message: "publish restart build failed"}
	}
	return newRunID, intentKey, target.Cluster, nil
}

// RestartRun is the restart_run MCP tool's entry into POST
// /runs/{run}/restart. It authorizes with the tenant write check on the
// run's own tenant, exactly as the HTTP path does, and reports no outcome.
// targetCluster is the cluster of the home member the command reached, for
// the caller's own activity audit; it is empty whenever serr is set.
func (s *Server) RestartRun(ctx context.Context, claims *auth.Claims, runID, stage string) (newRunID, intentKey, targetCluster string, serr *ServiceError) {
	return s.restartRun(ctx, claims, runID, stage)
}

// handleGetRunRestart answers GET /runs/{run}/restart.
func (s *Server) handleGetRunRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	out, serr := s.GetRunRestart(r.Context(), claims, r.PathValue("run"))
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleRestartRun answers POST /runs/{run}/restart: it decodes the body
// and publishes one RESTART_BUILD command. It answers 202 with the new run
// identifier and reports no outcome.
func (s *Server) handleRestartRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	var req restartRunRequest
	if err := decodeJSONBody(http.MaxBytesReader(w, r.Body, restartRunMaxBodyBytes), &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	runID, intentKey, _, serr := s.restartRun(r.Context(), claims, r.PathValue("run"), req.Stage)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusAccepted, runActionResponse{RunID: runID, IntentKey: intentKey, State: "requested"})
}
