package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// runnerEntry is the JSON shape for GET /runners entries, GET
// /runners/{name}, and the POST /runners request body's spec. Field names
// match v1alpha1.RunnerSpec/RunnerStatus's own JSON tags exactly: the
// varroa-cli merge-patch machinery (design D12) diffs a fetched runner
// against a locally edited one and sends only the difference through the
// generated client, so the wire shape and the CRD shape must never
// diverge.
type runnerEntry struct {
	Name   string            `json:"name"`
	Spec   runnerSpecEntry   `json:"spec"`
	Status runnerStatusEntry `json:"status"`
}

// runnerSpecEntry mirrors v1alpha1.RunnerSpec.
type runnerSpecEntry struct {
	Tenant        string   `json:"tenant"`
	Launcher      string   `json:"launcher"`
	Labels        []string `json:"labels,omitempty"`
	RemoteFS      string   `json:"remoteFS"`
	NumExecutors  int      `json:"numExecutors,omitempty"`
	Host          string   `json:"host,omitempty"`
	Port          *int     `json:"port,omitempty"`
	CredentialRef string   `json:"credentialRef,omitempty"`
	JavaPath      string   `json:"javaPath,omitempty"`
}

// runnerStatusEntry mirrors v1alpha1.RunnerStatus. Token carries the live
// door credential; handlers redact it before a response reaches a caller
// without runners:create (design D12, "The BFF redacts status.token").
type runnerStatusEntry struct {
	NodeName           string                 `json:"nodeName,omitempty"`
	Cell               string                 `json:"cell,omitempty"`
	CellNamespace      string                 `json:"cellNamespace,omitempty"`
	Token              string                 `json:"token,omitempty"`
	Connected          bool                   `json:"connected"`
	LastSeen           *metav1.Time           `json:"lastSeen,omitempty"`
	ObservedGeneration int64                  `json:"observedGeneration,omitempty"`
	Conditions         []runnerConditionEntry `json:"conditions,omitempty"`
}

// runnerConditionEntry mirrors v1alpha1.RunnerCondition.
type runnerConditionEntry struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
}

// createRunnerRequest is the JSON body for POST /runners.
type createRunnerRequest struct {
	Name string          `json:"name"`
	Spec runnerSpecEntry `json:"spec"`
}

func runnerSpecEntryFromSpec(s v1alpha1.RunnerSpec) runnerSpecEntry {
	return runnerSpecEntry{
		Tenant:        s.Tenant,
		Launcher:      s.Launcher,
		Labels:        s.Labels,
		RemoteFS:      s.RemoteFS,
		NumExecutors:  s.NumExecutors,
		Host:          s.Host,
		Port:          s.Port,
		CredentialRef: s.CredentialRef,
		JavaPath:      s.JavaPath,
	}
}

func runnerSpecFromEntry(e runnerSpecEntry) v1alpha1.RunnerSpec {
	return v1alpha1.RunnerSpec{
		Tenant:        e.Tenant,
		Launcher:      e.Launcher,
		Labels:        e.Labels,
		RemoteFS:      e.RemoteFS,
		NumExecutors:  e.NumExecutors,
		Host:          e.Host,
		Port:          e.Port,
		CredentialRef: e.CredentialRef,
		JavaPath:      e.JavaPath,
	}
}

// runnerStatusEntryFromStatus maps status.* onto the wire shape. redact
// clears Token, per CanCreateRunners (design D12).
func runnerStatusEntryFromStatus(s v1alpha1.RunnerStatus, redact bool) runnerStatusEntry {
	out := runnerStatusEntry{
		NodeName:           s.NodeName,
		Cell:               s.Cell,
		CellNamespace:      s.CellNamespace,
		Token:              s.Token,
		Connected:          s.Connected,
		LastSeen:           s.LastSeen,
		ObservedGeneration: s.ObservedGeneration,
	}
	if redact {
		out.Token = ""
	}
	for _, c := range s.Conditions {
		out.Conditions = append(out.Conditions, runnerConditionEntry{
			Type:               c.Type,
			Status:             string(c.Status),
			LastTransitionTime: c.LastTransitionTime,
			Reason:             c.Reason,
			Message:            c.Message,
		})
	}
	return out
}

// runnerFromCRD builds a runnerEntry from a Runner CRD, redacting
// status.token for a caller who does not hold runners:create.
func (s *Server) runnerFromCRD(claims *auth.Claims, r *v1alpha1.Runner) runnerEntry {
	redact := s.deps.Authorizer == nil || !s.deps.Authorizer.CanCreateRunners(claims)
	return runnerEntry{
		Name:   r.Name,
		Spec:   runnerSpecEntryFromSpec(r.Spec),
		Status: runnerStatusEntryFromStatus(r.Status, redact),
	}
}

// canReadRunner reports whether claims may read a runner belonging to
// tenant: the runners:read verb, and CanReadPipeline on the runner's own
// tenant with an empty job path — a runner is tenant-wide and belongs to
// no job (specs/runner-registration).
func (s *Server) canReadRunner(claims *auth.Claims, tenant string) bool {
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadRunners(claims) {
		return false
	}
	ref, err := TenantRefFromTenant(tenant)
	if err != nil {
		return false
	}
	return s.deps.Authorizer.CanReadPipeline(claims, ref, "")
}

// runnerTenantWriteGrant reports whether claims hold CanWritePipeline on
// tenant, with an empty job path. Shared by the three write-verb checks
// below, each of which also requires its own runners verb.
func (s *Server) runnerTenantWriteGrant(claims *auth.Claims, tenant string) bool {
	ref, err := TenantRefFromTenant(tenant)
	if err != nil {
		return false
	}
	return s.deps.Authorizer.CanWritePipeline(claims, ref, "")
}

// canCreateRunner reports whether claims may create a runner belonging to
// tenant: the runners:create verb and the tenant write grant.
func (s *Server) canCreateRunner(claims *auth.Claims, tenant string) bool {
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanCreateRunners(claims) {
		return false
	}
	return s.runnerTenantWriteGrant(claims, tenant)
}

// canUpdateRunner reports whether claims may update a runner belonging to
// tenant: the runners:update verb and the tenant write grant. tenant is the
// stored object's spec.tenant, which is immutable.
func (s *Server) canUpdateRunner(claims *auth.Claims, tenant string) bool {
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateRunners(claims) {
		return false
	}
	return s.runnerTenantWriteGrant(claims, tenant)
}

// canDeleteRunner reports whether claims may delete a runner belonging to
// tenant: the runners:delete verb and the tenant write grant. tenant is the
// stored object's spec.tenant, which is immutable.
func (s *Server) canDeleteRunner(claims *auth.Claims, tenant string) bool {
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanDeleteRunners(claims) {
		return false
	}
	return s.runnerTenantWriteGrant(claims, tenant)
}

// HandleRunners dispatches /runners requests:
//
//	GET  /runners — list runners the caller may read (filtered, never refused)
//	POST /runners — register a runner
func (s *Server) HandleRunners(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListRunners(w, r)
	case http.MethodPost:
		s.handleCreateRunner(w, r)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleListRunners applies the read check to each runner and omits one
// the caller cannot read, rather than refusing the whole request
// (specs/runner-registration). An absent or misconfigured Authorizer
// therefore answers an empty list, not an error.
func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())

	ctx := r.Context()
	runners, err := crdstore.List[v1alpha1.Runner](ctx, s.deps.Store, "", "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list runners")
		return
	}

	tenant := r.URL.Query().Get("tenant")
	result := make([]runnerEntry, 0, len(runners))
	for _, rn := range runners {
		if tenant != "" && rn.Spec.Tenant != tenant {
			continue
		}
		if !s.canReadRunner(claims, rn.Spec.Tenant) {
			continue
		}
		result = append(result, s.runnerFromCRD(claims, rn))
	}
	s.writeJSON(w, http.StatusOK, itemsEnvelope(result))
}

func (s *Server) handleCreateRunner(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createRunnerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Spec.Tenant == "" {
		s.writeJSONError(w, http.StatusBadRequest, "spec.tenant is required")
		return
	}
	if req.Spec.Launcher == "" {
		s.writeJSONError(w, http.StatusBadRequest, "spec.launcher is required")
		return
	}
	if req.Spec.RemoteFS == "" {
		s.writeJSONError(w, http.StatusBadRequest, "spec.remoteFS is required")
		return
	}

	// The tenant write grant is checked before the Tenant lookup: a caller
	// with no grant on the named tenant learns only "forbidden", never
	// whether the tenant exists (specs/runner-registration, "a fleet verb
	// alone does not reach another tenant's runner").
	if !s.canCreateRunner(claims, req.Spec.Tenant) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	if _, err := crdstore.Get[v1alpha1.Tenant](ctx, s.deps.Store, req.Spec.Tenant, ""); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "spec.tenant names no Tenant")
		return
	}

	runner := &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       runnerSpecFromEntry(req.Spec),
	}
	if err := crdstore.Create[v1alpha1.Runner](ctx, s.deps.Store, runner); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeJSONError(w, http.StatusConflict, "runner already exists")
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, "failed to create runner")
		return
	}

	s.writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

// HandleRunnerDispatch dispatches /runners/{name} requests:
//
//	GET    /runners/{name} — get runner detail
//	PATCH  /runners/{name} — merge-patch spec.* (tenant and launcher are immutable)
//	DELETE /runners/{name} — delete a runner
func (s *Server) HandleRunnerDispatch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/runners/")
	if name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "runner name required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetRunner(w, r, name)
	case http.MethodPatch:
		s.handlePatchRunner(w, r, name)
	case http.MethodDelete:
		s.handleDeleteRunner(w, r, name)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleGetRunner(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	runner, err := crdstore.Get[v1alpha1.Runner](ctx, s.deps.Store, name, "")
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "runner not found")
		return
	}
	if !s.canReadRunner(claims, runner.Spec.Tenant) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	s.writeJSON(w, http.StatusOK, s.runnerFromCRD(claims, runner))
}

// handlePatchRunner applies an RFC 7386 JSON merge patch to spec.* only.
// The patch is rejected with 400 for any top-level key other than "spec",
// and for a merged spec that changes tenant or launcher: both are immutable
// (types.go's CEL rules enforce the same constraint against a real
// apiserver; this handler enforces it against crdstore.Fake too, and
// gives the caller a clearer message than a raw CEL rejection).
func (s *Server) handlePatchRunner(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	patchBody, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var patchTop map[string]json.RawMessage
	if err := json.Unmarshal(patchBody, &patchTop); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid patch JSON")
		return
	}
	for k := range patchTop {
		if k != "spec" {
			s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("patch may only contain spec; got %q", k))
			return
		}
	}

	ctx := r.Context()
	existing, err := crdstore.Get[v1alpha1.Runner](ctx, s.deps.Store, name, "")
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "runner not found")
		return
	}
	if !s.canUpdateRunner(claims, existing.Spec.Tenant) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	specPatch, ok := patchTop["spec"]
	if !ok {
		// No spec key: nothing to change.
		s.writeJSON(w, http.StatusOK, map[string]string{"name": name})
		return
	}

	existingSpecJSON, err := json.Marshal(runnerSpecEntryFromSpec(existing.Spec))
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "json marshal failed")
		return
	}
	mergedJSON, err := jsonpatch.MergePatch(existingSpecJSON, specPatch)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid patch JSON")
		return
	}
	var mergedSpec runnerSpecEntry
	if err := json.Unmarshal(mergedJSON, &mergedSpec); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid patch JSON")
		return
	}
	if mergedSpec.Tenant != existing.Spec.Tenant {
		s.writeJSONError(w, http.StatusBadRequest, "spec.tenant is immutable")
		return
	}
	if mergedSpec.Launcher != existing.Spec.Launcher {
		s.writeJSONError(w, http.StatusBadRequest, "spec.launcher is immutable")
		return
	}

	// Deep-copy the fetched object rather than build a fresh one: status
	// is a subresource on a real apiserver (a plain update ignores it),
	// but crdstore.Fake's UpdateObject replaces the whole stored object,
	// so carrying status forward here keeps both backends in agreement.
	updated := existing.DeepCopy()
	updated.Spec = runnerSpecFromEntry(mergedSpec)
	if err := crdstore.Update[v1alpha1.Runner](ctx, s.deps.Store, updated); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to update runner")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleDeleteRunner(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	runner, err := crdstore.Get[v1alpha1.Runner](ctx, s.deps.Store, name, "")
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "runner not found")
		return
	}
	if !s.canDeleteRunner(claims, runner.Spec.Tenant) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	if err := crdstore.Delete[v1alpha1.Runner](ctx, s.deps.Store, name, ""); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to delete runner")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
