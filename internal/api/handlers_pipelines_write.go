// Package api: the pipelines write paths (design D3, D3a): POST
// /pipelines/{team}/jobs/{job}/runs, POST /runs/{run}/cancel, and POST
// /runs/{run}/rerun. The BFF mints or validates the run identity, then
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
// read-admitted by TenantResolver.Resolve) to the tenants whose folder
// prefixes jobPath, because a job path starts with its tenant folder.
// One surviving tenant is used. Two or more break on an explicit
// cluster value; a remaining tie answers 409 naming every candidate,
// and no surviving tenant answers 404. The first candidate is never
// taken silently.
func selectWriteTenant(refs []TenantRef, jobPath, cluster string) (TenantRef, *ServiceError) {
	var survivors []TenantRef
	for _, ref := range refs {
		if withinFolder(ref.Folder, jobPath) {
			survivors = append(survivors, ref)
		}
	}
	if len(survivors) == 0 {
		return TenantRef{}, &ServiceError{Status: http.StatusNotFound, Message: "no tenant hosts job path " + jobPath}
	}
	if len(survivors) == 1 {
		return survivors[0], nil
	}
	if cluster != "" {
		var selected []TenantRef
		for _, ref := range survivors {
			if ref.Cluster == cluster {
				selected = append(selected, ref)
			}
		}
		if len(selected) == 1 {
			return selected[0], nil
		}
	}
	names := make([]string, len(survivors))
	for i, ref := range survivors {
		names[i] = ref.Cluster + "/" + ref.Tenant
	}
	return TenantRef{}, &ServiceError{
		Status:  http.StatusConflict,
		Message: fmt.Sprintf("job path %q is ambiguous across tenants %s; name a cluster to select one", jobPath, strings.Join(names, ", ")),
	}
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

// resolveRunForWrite reads runID and authorizes it with the tenant
// write check on the run's own TenantRef (pipelines-api's authorization
// table: cancel and rerun authorize by tenant and job path, not by the
// run read check). An unknown run answers 404. A run the caller may not
// write answers 403 and publishes no command.
func (s *Server) resolveRunForWrite(w http.ResponseWriter, r *http.Request, claims *auth.Claims, runID string) (recordstore.Run, TenantRef, bool) {
	if s.deps.TenantResolver == nil {
		s.pipelinesUnavailable(w)
		return recordstore.Run{}, TenantRef{}, false
	}
	ctx := r.Context()
	run, _, err := s.deps.RecordStore.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, recordstore.ErrRunNotFound) {
			s.writeJSONError(w, http.StatusNotFound, "run not found")
			return recordstore.Run{}, TenantRef{}, false
		}
		s.handleReaderError(w, err, "get run")
		return recordstore.Run{}, TenantRef{}, false
	}
	ref, err := s.deps.TenantResolver.ResolveRun(ctx, claims, run)
	if err != nil {
		s.deps.Logger.Error("resolve run tenant", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "resolve run tenant failed")
		return recordstore.Run{}, TenantRef{}, false
	}
	if !s.canWritePipeline(claims, ref, run.JobPath) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
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
	ref, serr = selectWriteTenant(refs, jobPath, cluster)
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
