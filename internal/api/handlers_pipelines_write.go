// Package api: the pipelines write paths (design D3, D3a): POST
// /pipelines/{team}/jobs/{job}/runs, POST /runs/{run}/cancel, POST
// /runs/{run}/rerun, and the two run input routes POST
// /runs/{run}/inputs/{input}/proceed and .../abort. The BFF mints or
// validates the run identity, then
// publishes one imperative command through the mite transport. It never
// claims the build-record ledger: the ledger row records the claiming
// cell's instance and its placement generation, and the BFF holds
// neither value.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// clientTokenMaxAge is design D3a's bound on a supplied clientToken's
// embedded time: the record store only holds partitions for a bounded
// window, so an older token would place a run where no partition
// exists.
const clientTokenMaxAge = 24 * time.Hour

// runActionResponse is the 202 response for a run request or a rerun
// (components/schemas.yaml#RunActionResponse).
type runActionResponse struct {
	RunID     string `json:"runId"`
	IntentKey string `json:"intentKey"`
	State     string `json:"state"`
}

// createRunRequest is POST /pipelines/{team}/jobs/{job}/runs' body
// (components/schemas.yaml#CreateRunRequest). Every field is optional.
type createRunRequest struct {
	ClientToken string            `json:"clientToken,omitempty"`
	Parameters  map[string]string `json:"parameters,omitempty"`
	Cluster     string            `json:"cluster,omitempty"`
}

// decodeJSONBody reads body into v. An empty body decodes as the zero
// value, since every field of createRunRequest is optional.
func decodeJSONBody(body io.Reader, v any) error {
	if body == nil {
		return nil
	}
	if err := json.NewDecoder(body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// --- Task 4.1: tenant narrowing for a write ---

// selectWriteTenant implements design D3's step 1: narrow refs (already
// read-admitted by TenantResolver.Resolve) to the tenant that hosts
// jobPath, and return the folder-qualified job path to use from that
// point on. A jobPath that already starts with a candidate's folder is
// the long form. A jobPath that omits it is the short form: team names
// the tenant already, so the one candidate whose folder equals team
// hosts jobPath under that folder. One surviving tenant is used. Two or
// more break on an explicit cluster value; a remaining tie answers 409
// naming every candidate, and no surviving tenant answers 404 naming
// the expected shape. The first candidate is never taken silently.
func selectWriteTenant(refs []TenantRef, team, jobPath, cluster string) (TenantRef, string, *ServiceError) {
	survivors, paths := matchingWriteTenants(refs, team, jobPath)
	if len(survivors) == 0 {
		return TenantRef{}, "", &ServiceError{
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("no tenant hosts job path %q; expected <team>/<job path below the tenant folder>", jobPath),
		}
	}
	if len(survivors) == 1 {
		return survivors[0], paths[0], nil
	}
	if cluster != "" {
		var selectedRef TenantRef
		var selectedPath string
		matches := 0
		for i, ref := range survivors {
			if ref.Cluster == cluster {
				selectedRef, selectedPath = ref, paths[i]
				matches++
			}
		}
		if matches == 1 {
			return selectedRef, selectedPath, nil
		}
	}
	names := make([]string, len(survivors))
	for i, ref := range survivors {
		names[i] = ref.Cluster + "/" + ref.Tenant
	}
	return TenantRef{}, "", &ServiceError{
		Status:  http.StatusConflict,
		Message: fmt.Sprintf("job path %q is ambiguous across tenants %s; name a cluster to select one", jobPath, strings.Join(names, ", ")),
	}
}

// matchingWriteTenants returns every ref of refs that hosts jobPath,
// paired with the folder-qualified job path to use for that ref. It
// tries the long form first: jobPath already inside a ref's folder. A
// direct match wins outright, since an explicit folder-qualified path
// names its tenant without guessing. Only when no ref matches directly
// does it try the short form: jobPath alone, prefixed with the one
// ref whose folder equals team. team already names the tenant, so a
// folder that disagrees with it is never a short-form candidate.
func matchingWriteTenants(refs []TenantRef, team, jobPath string) ([]TenantRef, []string) {
	var direct []TenantRef
	var directPaths []string
	for _, ref := range refs {
		if withinFolder(ref.Folder, jobPath) {
			direct = append(direct, ref)
			directPaths = append(directPaths, jobPath)
		}
	}
	if len(direct) > 0 {
		return direct, directPaths
	}
	for _, ref := range refs {
		if ref.Folder != "" && ref.Folder == team {
			return []TenantRef{ref}, []string{ref.Folder + "/" + jobPath}
		}
	}
	return nil, nil
}

// --- Task 4.2: the client-token validator (design D3a) ---

// parseClientToken validates a client-supplied run identifier: it must
// be a UUIDv7 whose embedded time is neither ahead of now nor more than
// clientTokenMaxAge behind it. It returns the token's normalized string
// form on success.
func parseClientToken(token string, now time.Time) (string, error) {
	id, err := uuid.Parse(token)
	if err != nil {
		return "", errors.New("clientToken is not a valid UUID")
	}
	if id.Version() != 7 {
		return "", errors.New("clientToken is not a UUIDv7")
	}
	ts := time.UnixMilli(uuidv7Millis(id)).UTC()
	if ts.After(now) {
		return "", errors.New("clientToken time is in the future")
	}
	if now.Sub(ts) > clientTokenMaxAge {
		return "", errors.New("clientToken is more than 24 hours old")
	}
	return id.String(), nil
}

// uuidv7Millis reads a UUIDv7's embedded 48-bit millisecond timestamp
// from its first six bytes. google/uuid's own Time() decodes the
// version-1 Gregorian layout, which a version-7 UUID does not carry.
func uuidv7Millis(id uuid.UUID) int64 {
	return int64(id[0])<<40 | int64(id[1])<<32 | int64(id[2])<<24 |
		int64(id[3])<<16 | int64(id[4])<<8 | int64(id[5])
}

// resolveManualRunID implements D3a's manual-run scheme: a supplied
// clientToken is validated and used as run_id; an absent one mints a
// fresh UUIDv7. The intent key is manual:<run_id> either way, so the
// key and the identifier are the same value for all time.
func resolveManualRunID(clientToken string) (runID, intentKey string, err error) {
	if clientToken == "" {
		id, mintErr := uuid.NewV7()
		if mintErr != nil {
			return "", "", mintErr
		}
		runID = id.String()
	} else {
		runID, err = parseClientToken(clientToken, time.Now())
		if err != nil {
			return "", "", err
		}
	}
	return runID, "manual:" + runID, nil
}

// --- Shared write-path helpers ---

// publishImperative sends cmd to ref's controller through the mite
// transport (design D3's Transport.SendImperative), never on the
// ledger's claim subject: the BFF has no claim to make, per D3a.
func (s *Server) publishImperative(ctx context.Context, ref TenantRef, cmd *mitev1.ImperativeCommand) error {
	if s.deps.MiteRegistry == nil {
		return errors.New("mite transport not configured")
	}
	return s.deps.MiteRegistry.SendImperative(ctx, ref.Namespace, ref.Controller, cmd)
}

// homeRef answers the tenant reference of the member that holds jobPath's
// home slot. A job homed on slot 1 therefore reaches slot 1's holder, and
// not slot 0's, which the TenantRef alone names.
//
// The resolution never guesses. A trigger published to a member that is
// not the job's home answers the permanent NOT_HOME, and the plugin then
// cancels a build this API already reported accepted. A failed read and
// an unbound home slot therefore both answer a retryable 503, and the
// caller repeats the request.
//
// A tenant that holds no placement row keeps the caller's own reference.
// That is a controller with no cell role, which holds one Jenkins and no
// member list.
func (s *Server) homeRef(ctx context.Context, ref TenantRef, jobPath string) (TenantRef, *ServiceError) {
	if s.deps.RecordStore == nil || ref.Tenant == "" {
		return ref, nil
	}
	home, err := s.deps.RecordStore.JobHome(ctx, ref.Cluster, ref.Tenant, jobPath)
	if err != nil {
		s.deps.Logger.Error("resolve the job home member",
			"tenant", ref.Tenant, "job_path", jobPath, "error", err)
		return TenantRef{}, &ServiceError{
			Status:  http.StatusServiceUnavailable,
			Message: "resolve the job home member failed",
		}
	}
	if !home.Placed {
		return ref, nil
	}
	if home.Cell == "" {
		return TenantRef{}, &ServiceError{
			Status:  http.StatusServiceUnavailable,
			Message: "the home slot of job " + jobPath + " holds no cell yet",
		}
	}
	out := ref
	out.Namespace = home.Namespace
	out.Controller = home.Cell
	return out, nil
}

// runForWrite reads runID and authorizes it with the tenant write check
// on the run's own TenantRef (pipelines-api's authorization table:
// cancel, rerun, and the two input routes authorize by tenant and job
// path, not by the run read check). It reports a *ServiceError instead
// of writing a response, so the MCP tools and the HTTP handlers share
// one lookup. An unknown run answers 404. A run the caller may not
// write answers 403, and no command is published.
func (s *Server) runForWrite(ctx context.Context, claims *auth.Claims, runID string) (recordstore.Run, TenantRef, *ServiceError) {
	if s.deps.TenantResolver == nil {
		return recordstore.Run{}, TenantRef{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	run, _, err := s.deps.RecordStore.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, recordstore.ErrRunNotFound) {
			return recordstore.Run{}, TenantRef{}, &ServiceError{Status: http.StatusNotFound, Message: "run not found"}
		}
		if errors.Is(err, recordstore.ErrSchemaTooOld) {
			return recordstore.Run{}, TenantRef{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
		}
		s.deps.Logger.Error("get run", "error", err)
		return recordstore.Run{}, TenantRef{}, &ServiceError{Status: http.StatusInternalServerError, Message: "get run failed"}
	}
	ref, err := s.deps.TenantResolver.ResolveRun(ctx, claims, run)
	if err != nil {
		s.deps.Logger.Error("resolve run tenant", "error", err)
		return recordstore.Run{}, TenantRef{}, &ServiceError{Status: http.StatusInternalServerError, Message: "resolve run tenant failed"}
	}
	if !s.canWritePipeline(claims, ref, run.JobPath) {
		return recordstore.Run{}, TenantRef{}, &ServiceError{Status: http.StatusForbidden, Message: "forbidden"}
	}
	return run, ref, nil
}

// resolveRunForWrite is the HTTP wrapper over runForWrite. It writes the
// service error and reports whether the caller continues.
func (s *Server) resolveRunForWrite(w http.ResponseWriter, r *http.Request, claims *auth.Claims, runID string) (recordstore.Run, TenantRef, bool) {
	run, ref, serr := s.runForWrite(r.Context(), claims, runID)
	if serr != nil {
		s.writeServiceError(w, serr)
		return recordstore.Run{}, TenantRef{}, false
	}
	return run, ref, true
}

// --- Task 4.3: POST /pipelines/{team}/jobs/{job}/runs ---

// createRun implements design D3's write path: resolve the team to one
// write-admitted tenant, mint or validate the run identity, and publish
// one TRIGGER_BUILD command. It is shared by handleCreateRun and the
// run_pipeline MCP tool, which supplies an empty clientToken (design
// D3a: an MCP call carries no client token, so the BFF mints a fresh
// run identifier every time).
func (s *Server) createRun(ctx context.Context, claims *auth.Claims, team, jobPath, clientToken, cluster string, parameters map[string]string) (runID, intentKey string, ref TenantRef, serr *ServiceError) {
	if s.deps.RecordStore == nil {
		return "", "", TenantRef{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	refs, serr := s.resolveTeamTenants(ctx, claims, team)
	if serr != nil {
		return "", "", TenantRef{}, serr
	}
	ref, jobPath, serr = selectWriteTenant(refs, team, jobPath, cluster)
	if serr != nil {
		return "", "", TenantRef{}, serr
	}
	if !s.canWritePipeline(claims, ref, jobPath) {
		return "", "", TenantRef{}, &ServiceError{Status: http.StatusForbidden, Message: "forbidden"}
	}

	runID, intentKey, verr := resolveManualRunID(clientToken)
	if verr != nil {
		return "", "", TenantRef{}, &ServiceError{Status: http.StatusBadRequest, Message: verr.Error()}
	}

	cmd := &mitev1.ImperativeCommand{
		CommandId: "trigger-build-" + runID,
		Type:      mitev1.CommandTypeTriggerBuild,
		TriggerBuild: &mitev1.TriggerBuildPayload{
			RunId:      runID,
			IntentKey:  intentKey,
			JobPath:    jobPath,
			Parameters: parameters,
			Actor:      ActorFrom(claims),
		},
	}
	target, homeErr := s.homeRef(ctx, ref, jobPath)
	if homeErr != nil {
		return "", "", TenantRef{}, homeErr
	}
	if err := s.publishImperative(ctx, target, cmd); err != nil {
		s.deps.Logger.Error("publish trigger build", "error", err)
		return "", "", TenantRef{}, &ServiceError{Status: http.StatusInternalServerError, Message: "publish trigger build failed"}
	}

	return runID, intentKey, ref, nil
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	team := r.PathValue("team")
	jobPath := r.PathValue("job")

	var req createRunRequest
	if err := decodeJSONBody(r.Body, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	runID, intentKey, _, serr := s.createRun(ctx, claims, team, jobPath, req.ClientToken, req.Cluster, req.Parameters)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}

	s.writeJSON(w, http.StatusAccepted, runActionResponse{RunID: runID, IntentKey: intentKey, State: "requested"})
}

// --- Task 4.5: POST /runs/{run}/cancel and POST /runs/{run}/rerun ---

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	run, ref, ok := s.resolveRunForWrite(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}

	var buildNumber int64
	if run.BuildNumber != nil {
		buildNumber = *run.BuildNumber
	}
	cmd := &mitev1.ImperativeCommand{
		CommandId: "cancel-build-" + run.RunID,
		Type:      mitev1.CommandTypeCancelBuild,
		CancelBuild: &mitev1.CancelBuildPayload{
			RunId:       run.RunID,
			JobPath:     run.JobPath,
			BuildNumber: buildNumber,
		},
	}
	if err := s.publishImperative(r.Context(), ref, cmd); err != nil {
		s.deps.Logger.Error("publish cancel build", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "publish cancel build failed")
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]string{"state": "requested"})
}

// runParams unmarshals a run's stored params column into the map shape
// the trigger command carries. A run with no recorded params, or an
// unparseable column, yields a nil map.
func runParams(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var params map[string]string
	if json.Unmarshal(raw, &params) != nil {
		return nil
	}
	return params
}

func (s *Server) handleRerunRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	sourceRun, ref, ok := s.resolveRunForWrite(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}

	intentKey := "retry:" + sourceRun.RunID
	intent, found, err := s.deps.RecordStore.GetIntentByKey(ctx, intentKey)
	if s.handleReaderError(w, err, "get intent") {
		return
	}
	if found {
		// A retry already exists for this source run: read only, publish
		// nothing, and answer with the existing identifier (D3a's rerun
		// scheme, task 4.5).
		s.writeJSON(w, http.StatusAccepted, runActionResponse{RunID: intent.RunID, IntentKey: intentKey, State: "requested"})
		return
	}

	id, mintErr := uuid.NewV7()
	if mintErr != nil {
		s.deps.Logger.Error("mint rerun run id", "error", mintErr)
		s.writeJSONError(w, http.StatusInternalServerError, "mint run id failed")
		return
	}
	runID := id.String()

	cmd := &mitev1.ImperativeCommand{
		CommandId: "trigger-build-" + runID,
		Type:      mitev1.CommandTypeTriggerBuild,
		TriggerBuild: &mitev1.TriggerBuildPayload{
			RunId:      runID,
			IntentKey:  intentKey,
			JobPath:    sourceRun.JobPath,
			Parameters: runParams(sourceRun.Params),
			Actor:      ActorFrom(claims),
		},
	}
	target, homeErr := s.homeRef(ctx, ref, sourceRun.JobPath)
	if homeErr != nil {
		s.writeServiceError(w, homeErr)
		return
	}
	if err := s.publishImperative(ctx, target, cmd); err != nil {
		s.deps.Logger.Error("publish rerun", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "publish trigger build failed")
		return
	}
	s.writeJSON(w, http.StatusAccepted, runActionResponse{RunID: runID, IntentKey: intentKey, State: "requested"})
}

// --- The two run input routes ---

// proceedRunInputRequest is the proceed route's body: one optional
// parameters object of string values, keyed by parameter name.
type proceedRunInputRequest struct {
	Parameters map[string]string `json:"parameters,omitempty"`
}

// actingUserFrom builds the acting-user value an input command carries,
// beside ActorFrom. A nil claims answers nil, because no caller acts.
// The groups travel with the name, because the plugin rebuilds the
// submitter from them.
func actingUserFrom(claims *auth.Claims) *mitev1.ActingUser {
	if claims == nil {
		return nil
	}
	return &mitev1.ActingUser{
		Name:   ActorFrom(claims),
		Email:  claims.Email,
		Groups: claims.Groups,
	}
}

// answerRunInput resolves the run, authorizes it, builds one imperative
// command, and publishes it. commandType selects PROCEED_INPUT or
// ABORT_INPUT; parameters is nil for an abort, which carries none. The
// command goes to the run's own TenantRef: the run row already names the
// cell the build runs on, so an input answers the build in place. The
// tenant write check runs before the command is built. An empty inputID
// answers 400. A run whose record holds no build number answers 404,
// because an input exists only on a running build. It answers the cluster
// the command went to, so an audit event names the run's own cluster.
func (s *Server) answerRunInput(ctx context.Context, claims *auth.Claims, runID, inputID string, commandType mitev1.CommandType, parameters map[string]string) (string, *ServiceError) {
	if s.deps.RecordStore == nil {
		return "", &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	if inputID == "" {
		return "", &ServiceError{Status: http.StatusBadRequest, Message: "input id is required"}
	}
	run, ref, serr := s.runForWrite(ctx, claims, runID)
	if serr != nil {
		return "", serr
	}
	if run.BuildNumber == nil {
		return "", &ServiceError{Status: http.StatusNotFound, Message: "run holds no build number"}
	}

	acting := actingUserFrom(claims)
	cmd := &mitev1.ImperativeCommand{}
	if commandType == mitev1.CommandTypeAbortInput {
		cmd.CommandId = "abort-input-" + run.RunID + "-" + inputID
		cmd.Type = mitev1.CommandTypeAbortInput
		cmd.AbortInput = &mitev1.AbortInputPayload{
			RunId:       run.RunID,
			JobPath:     run.JobPath,
			BuildNumber: *run.BuildNumber,
			InputId:     inputID,
			ActingUser:  acting,
		}
	} else {
		cmd.CommandId = "proceed-input-" + run.RunID + "-" + inputID
		cmd.Type = mitev1.CommandTypeProceedInput
		cmd.ProceedInput = &mitev1.ProceedInputPayload{
			RunId:       run.RunID,
			JobPath:     run.JobPath,
			BuildNumber: *run.BuildNumber,
			InputId:     inputID,
			Parameters:  parameters,
			ActingUser:  acting,
		}
	}
	if err := s.publishImperative(ctx, ref, cmd); err != nil {
		s.deps.Logger.Error("publish run input", "run", run.RunID, "input", inputID, "error", err)
		return "", &ServiceError{Status: http.StatusInternalServerError, Message: "publish run input failed"}
	}
	return ref.Cluster, nil
}

// handleProceedRunInput answers POST /runs/{run}/inputs/{input}/proceed:
// it decodes the body and publishes one PROCEED_INPUT command.
func (s *Server) handleProceedRunInput(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	var req proceedRunInputRequest
	if err := decodeJSONBody(r.Body, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	_, serr := s.answerRunInput(r.Context(), claims, r.PathValue("run"), r.PathValue("input"), mitev1.CommandTypeProceedInput, req.Parameters)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]string{"state": "requested"})
}

// handleAbortRunInput answers POST /runs/{run}/inputs/{input}/abort. The
// route takes no body, so the handler decodes none.
func (s *Server) handleAbortRunInput(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	_, serr := s.answerRunInput(r.Context(), claims, r.PathValue("run"), r.PathValue("input"), mitev1.CommandTypeAbortInput, nil)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]string{"state": "requested"})
}
