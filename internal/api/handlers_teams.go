package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// teamEntry is the JSON shape for GET /teams entries. Pipelines carries
// spec.pipelines; Placement carries status.placement; Rehydration carries
// status.rehydration. Neither status.pipelines.webhookToken nor
// status.pipelines.webhookSecret is ever mapped here: both are credentials
// (specs/tenant-model/spec.md). Controller.status.cell.rehydration is never
// mapped here either: it is the controller reconciler's own per-cell record,
// and TeamReconciler's copy onto status.rehydration is the projection this
// handler serves.
type teamEntry struct {
	Name                string                        `json:"name"`
	DisplayName         string                        `json:"displayName,omitempty"`
	Members             []string                      `json:"members,omitempty"`
	Subjects            []v1alpha1.SubjectRef         `json:"subjects,omitempty"`
	Namespaces          []string                      `json:"namespaces"`
	RoleRef             string                        `json:"roleRef,omitempty"`
	ProvisionNamespaces bool                          `json:"provisionNamespaces,omitempty"`
	Pipelines           *teamPipelinesEntry           `json:"pipelines,omitempty"`
	ObservedGeneration  int64                         `json:"observedGeneration,omitempty"`
	GroupRef            string                        `json:"groupRef,omitempty"`
	BindingRef          string                        `json:"bindingRef,omitempty"`
	NamespaceStates     []v1alpha1.TeamNamespaceState `json:"namespaceStates,omitempty"`
	Conditions          []v1alpha1.TeamCondition      `json:"conditions,omitempty"`
	Placement           *teamPlacementEntry           `json:"placement,omitempty"`
	Rehydration         *teamRehydrationEntry         `json:"rehydration,omitempty"`
}

// teamPipelinesEntry is the JSON shape of spec.pipelines, carried on both
// the request and the response. RehydrateDeadline and CronGrace keep the
// *metav1.Duration type: it marshals to and parses from a plain duration
// string on its own, so no hand-written conversion is needed here.
type teamPipelinesEntry struct {
	Profile           string                      `json:"profile"`
	CellPool          string                      `json:"cellPool,omitempty"`
	AgentNamespace    string                      `json:"agentNamespace"`
	ComposedBundleRef *teamComposedBundleRefEntry `json:"composedBundleRef,omitempty"`

	RehydrateBuilds   *int32           `json:"rehydrateBuilds,omitempty"`
	RehydrateDeadline *metav1.Duration `json:"rehydrateDeadline,omitempty"`
	InterruptedRetry  string           `json:"interruptedRetry,omitempty"`
	Concurrency       string           `json:"concurrency,omitempty"`
	CronBackstop      *bool            `json:"cronBackstop,omitempty"`
	CronCatchUp       string           `json:"cronCatchUp,omitempty"`
	CronGrace         *metav1.Duration `json:"cronGrace,omitempty"`

	// Members and Standby carry the tenant's slot counts: the number of
	// active slots it runs, and the number of warm members it keeps.
	Members *int32 `json:"members,omitempty"`
	Standby *int32 `json:"standby,omitempty"`

	// Exports carries spec.pipelines.exports: the grants this tenant makes
	// of its own jobs and locks to other tenants. Reading it needs
	// CanReadPipeline on the owning tenant; writing it needs the tenant's
	// pipelines:admin capability. Neither check is controller-scoped,
	// because a grant belongs to a tenant, not to a cell.
	Exports []teamExportEntry `json:"exports,omitempty"`

	// Retention carries spec.pipelines.retention: the record-store windows
	// for this tenant's runs. Only an operator may write it, so the
	// tenant-admin path rejects a change to it the same way it rejects any
	// other change outside the exports.
	Retention *v1alpha1.TeamPipelinesRetention `json:"retention,omitempty"`

	// ConcurrencyLimits carries spec.pipelines.concurrencyLimits. A tenant
	// pipelines admin may write it, the same as exports.
	ConcurrencyLimits []v1alpha1.TeamConcurrencyLimit `json:"concurrencyLimits,omitempty"`

	// Notifications carries spec.pipelines.notifications. Changing it needs
	// the operator's own write, not the tenant's pipelines:admin grant.
	Notifications *teamNotificationsEntry `json:"notifications,omitempty"`
}

// teamNotificationsEntry is the JSON shape of spec.pipelines.notifications.
type teamNotificationsEntry struct {
	Webhook *teamWebhookEntry `json:"webhook,omitempty"`
}

// teamWebhookEntry is the JSON shape of a completion webhook.
type teamWebhookEntry struct {
	URL       string                 `json:"url"`
	SecretRef teamWebhookSecretEntry `json:"secretRef"`
}

// teamWebhookSecretEntry names the Secret and the data key that hold the
// webhook signing key.
type teamWebhookSecretEntry struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// teamExportEntry is the JSON shape of one spec.pipelines.exports entry.
// Exactly one of Job or Locks is set; the CRD's CEL rules enforce it.
type teamExportEntry struct {
	Job     string   `json:"job,omitempty"`
	Locks   []string `json:"locks,omitempty"`
	Callers []string `json:"callers,omitempty"`
}

// teamComposedBundleRefEntry is the JSON shape of
// spec.pipelines.composedBundleRef. A Team is cluster-scoped, so it
// carries both name and namespace with no namespace to default to.
type teamComposedBundleRefEntry struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// teamPlacementEntry is the JSON shape of status.placement, response-only:
// the placement pass is its only writer.
type teamPlacementEntry struct {
	Cluster       string `json:"cluster,omitempty"`
	Cell          string `json:"cell,omitempty"`
	CellNamespace string `json:"cellNamespace,omitempty"`
	Generation    int64  `json:"generation,omitempty"`
	// Members reports every member of the tenant. The scalar fields above
	// keep describing slot 0.
	Members []teamMemberEntry `json:"members,omitempty"`
}

// teamMemberEntry is the JSON shape of one status.placement.members
// entry, response-only.
type teamMemberEntry struct {
	Cell          string `json:"cell,omitempty"`
	CellNamespace string `json:"cellNamespace,omitempty"`
	Role          string `json:"role,omitempty"`
	Slot          *int32 `json:"slot,omitempty"`
	Applied       bool   `json:"applied,omitempty"`
}

// teamRehydrationEntry is the JSON shape of status.rehydration,
// response-only: TeamReconciler is its only writer.
type teamRehydrationEntry struct {
	Generation   int64        `json:"generation,omitempty"`
	CellInstance string       `json:"cellInstance,omitempty"`
	Records      int32        `json:"records,omitempty"`
	Truncated    bool         `json:"truncated,omitempty"`
	UnseededJobs int32        `json:"unseededJobs,omitempty"`
	Deferred     []string     `json:"deferred,omitempty"`
	Attempts     int32        `json:"attempts,omitempty"`
	LastAttempt  *metav1.Time `json:"lastAttempt,omitempty"`
}

// teamPipelinesEntryFromSpec maps spec.pipelines onto the wire shape. It
// returns nil for a Team that is not a pipelines tenant.
func teamPipelinesEntryFromSpec(p *v1alpha1.TeamPipelinesSpec) *teamPipelinesEntry {
	if p == nil {
		return nil
	}
	e := &teamPipelinesEntry{
		Profile:           p.Profile,
		CellPool:          p.CellPool,
		AgentNamespace:    p.AgentNamespace,
		RehydrateBuilds:   p.RehydrateBuilds,
		RehydrateDeadline: p.RehydrateDeadline,
		InterruptedRetry:  p.InterruptedRetry,
		Concurrency:       p.Concurrency,
		CronBackstop:      p.CronBackstop,
		CronCatchUp:       p.CronCatchUp,
		CronGrace:         p.CronGrace,
		Members:           p.Members,
		Standby:           p.Standby,
	}
	if p.ComposedBundleRef != nil {
		e.ComposedBundleRef = &teamComposedBundleRefEntry{
			Name:      p.ComposedBundleRef.Name,
			Namespace: p.ComposedBundleRef.Namespace,
		}
	}
	for _, exp := range p.Exports {
		e.Exports = append(e.Exports, teamExportEntry{
			Job:     exp.Job,
			Locks:   exp.Locks,
			Callers: exp.Callers,
		})
	}
	if n := p.Notifications; n != nil && n.Webhook != nil {
		e.Notifications = &teamNotificationsEntry{Webhook: &teamWebhookEntry{
			URL: n.Webhook.URL,
			SecretRef: teamWebhookSecretEntry{
				Name: n.Webhook.SecretRef.Name,
				Key:  n.Webhook.SecretRef.Key,
			},
		}}
	}
	e.Retention = p.Retention
	e.ConcurrencyLimits = p.ConcurrencyLimits
	return e
}

// teamPipelinesSpecFromEntry maps the wire shape back onto spec.pipelines.
// It returns nil when the request carries no pipelines object, so a Team
// with no spec.pipelines round-trips as no spec.pipelines, not an empty
// one.
func teamPipelinesSpecFromEntry(e *teamPipelinesEntry) *v1alpha1.TeamPipelinesSpec {
	if e == nil {
		return nil
	}
	spec := &v1alpha1.TeamPipelinesSpec{
		Profile:           strings.TrimSpace(e.Profile),
		CellPool:          e.CellPool,
		AgentNamespace:    strings.TrimSpace(e.AgentNamespace),
		RehydrateBuilds:   e.RehydrateBuilds,
		RehydrateDeadline: e.RehydrateDeadline,
		InterruptedRetry:  e.InterruptedRetry,
		Concurrency:       e.Concurrency,
		CronBackstop:      e.CronBackstop,
		CronCatchUp:       e.CronCatchUp,
		CronGrace:         e.CronGrace,
		Members:           e.Members,
		Standby:           e.Standby,
	}
	if e.ComposedBundleRef != nil {
		spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{
			Name:      e.ComposedBundleRef.Name,
			Namespace: e.ComposedBundleRef.Namespace,
		}
	}
	for _, exp := range e.Exports {
		spec.Exports = append(spec.Exports, v1alpha1.TeamExport{
			Job:     exp.Job,
			Locks:   exp.Locks,
			Callers: exp.Callers,
		})
	}
	if n := e.Notifications; n != nil && n.Webhook != nil {
		spec.Notifications = &v1alpha1.TeamNotificationsSpec{
			Webhook: &v1alpha1.TeamWebhookSpec{
				URL: strings.TrimSpace(n.Webhook.URL),
				SecretRef: v1alpha1.TeamWebhookSecretRef{
					Name: strings.TrimSpace(n.Webhook.SecretRef.Name),
					Key:  strings.TrimSpace(n.Webhook.SecretRef.Key),
				},
			},
		}
	}
	spec.Retention = e.Retention
	spec.ConcurrencyLimits = e.ConcurrencyLimits
	return spec
}

// validateTeamPipelines checks the spec.pipelines rules that a caller breaks
// by omission. The apiserver enforces the same rules. This check names the
// field in a 400 before the write. It returns "" for a valid or absent block.
func validateTeamPipelines(p *teamPipelinesEntry, namespaces []string) string {
	if p == nil {
		return ""
	}
	if strings.TrimSpace(p.Profile) == "" {
		return "pipelines.profile is required"
	}
	if strings.TrimSpace(p.AgentNamespace) == "" {
		return "pipelines.agentNamespace is required"
	}
	if !slices.Contains(namespaces, strings.TrimSpace(p.AgentNamespace)) {
		return "pipelines.agentNamespace must be one of namespaces"
	}
	for i, exp := range p.Exports {
		if len(exp.Callers) == 0 {
			return fmt.Sprintf("pipelines.exports[%d].callers is required", i)
		}
	}
	if n := p.Notifications; n != nil && n.Webhook != nil {
		if strings.TrimSpace(n.Webhook.URL) == "" {
			return "pipelines.notifications.webhook.url is required"
		}
		if strings.TrimSpace(n.Webhook.SecretRef.Name) == "" {
			return "pipelines.notifications.webhook.secretRef.name is required"
		}
	}
	if msg := validateConcurrencyLimits(p.ConcurrencyLimits); msg != "" {
		return msg
	}
	if r := p.Retention; r != nil {
		if _, err := recordstore.ParseRetention(r.Runs); r.Runs != "" && err != nil {
			return "pipelines.retention.runs is invalid"
		}
		for i, job := range r.Jobs {
			if strings.TrimSpace(job.Job) == "" || strings.HasPrefix(job.Job, "/") {
				return fmt.Sprintf("pipelines.retention.jobs[%d].job is invalid", i)
			}
			if _, err := recordstore.ParseRetention(job.Runs); err != nil {
				return fmt.Sprintf("pipelines.retention.jobs[%d].runs is invalid", i)
			}
		}
	}
	return ""
}

// teamPlacementEntryFromStatus maps status.placement onto the wire shape.
func teamPlacementEntryFromStatus(p *v1alpha1.TeamPlacementStatus) *teamPlacementEntry {
	if p == nil {
		return nil
	}
	out := &teamPlacementEntry{
		Cluster:       p.Cluster,
		Cell:          p.Cell,
		CellNamespace: p.CellNamespace,
		Generation:    p.Generation,
	}
	for _, m := range p.Members {
		out.Members = append(out.Members, teamMemberEntry{
			Cell: m.Cell, CellNamespace: m.CellNamespace, Role: m.Role,
			Slot: m.Slot, Applied: m.Applied,
		})
	}
	return out
}

// teamRehydrationEntryFromStatus maps status.rehydration onto the wire
// shape.
func teamRehydrationEntryFromStatus(r *v1alpha1.TeamRehydrationStatus) *teamRehydrationEntry {
	if r == nil {
		return nil
	}
	return &teamRehydrationEntry{
		Generation:   r.Generation,
		CellInstance: r.CellInstance,
		Records:      r.Records,
		Truncated:    r.Truncated,
		UnseededJobs: r.UnseededJobs,
		Deferred:     r.Deferred,
		Attempts:     r.Attempts,
		LastAttempt:  r.LastAttempt,
	}
}

// teamFromCRD builds a teamEntry from a Team CRD.
func teamFromCRD(t *v1alpha1.Team) teamEntry {
	return teamEntry{
		Name:                t.Name,
		DisplayName:         t.Spec.DisplayName,
		Members:             t.Spec.Members,
		Subjects:            t.Spec.Subjects,
		Namespaces:          t.Spec.Namespaces,
		RoleRef:             t.Spec.RoleRef,
		ProvisionNamespaces: t.Spec.ProvisionNamespaces,
		Pipelines:           teamPipelinesEntryFromSpec(t.Spec.Pipelines),
		ObservedGeneration:  t.Status.ObservedGeneration,
		GroupRef:            t.Status.GroupRef,
		BindingRef:          t.Status.BindingRef,
		NamespaceStates:     t.Status.NamespaceStates,
		Conditions:          t.Status.Conditions,
		Placement:           teamPlacementEntryFromStatus(t.Status.Placement),
		Rehydration:         teamRehydrationEntryFromStatus(t.Status.Rehydration),
	}
}

// HandleTeams dispatches /teams requests:
//
//	GET    /teams          — list teams (admin)
//	POST   /teams          — create team (admin)
func (s *Server) HandleTeams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListTeams(w, r)
	case http.MethodPost:
		s.handleCreateTeam(w, r)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	teams, err := crdstore.List[v1alpha1.Team](ctx, s.deps.Store, "", "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list teams")
		return
	}

	result := make([]teamEntry, 0, len(teams))
	for _, t := range teams {
		result = append(result, teamFromCRD(t))
	}
	s.writeJSON(w, http.StatusOK, itemsEnvelope(result))
}

// createTeamRequest is the JSON body for POST /teams and PUT /teams/{name}.
type createTeamRequest struct {
	Name                string                `json:"name"`
	DisplayName         string                `json:"displayName,omitempty"`
	Members             []string              `json:"members,omitempty"`
	Subjects            []v1alpha1.SubjectRef `json:"subjects,omitempty"`
	Namespaces          []string              `json:"namespaces"`
	RoleRef             string                `json:"roleRef,omitempty"`
	ProvisionNamespaces bool                  `json:"provisionNamespaces,omitempty"`
	Pipelines           *teamPipelinesEntry   `json:"pipelines,omitempty"`
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createTeamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Validate: at least one of members or subjects.
	if len(req.Members) == 0 && len(req.Subjects) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "at least one of members or subjects must be set")
		return
	}

	// Validate: at least one namespace.
	if len(req.Namespaces) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "at least one namespace is required")
		return
	}

	// Validate: roleRef must not be "admin".
	if req.RoleRef == "admin" {
		s.writeJSONError(w, http.StatusBadRequest, "roleRef 'admin' is not permitted on a Team")
		return
	}
	if msg := validateTeamPipelines(req.Pipelines, req.Namespaces); msg != "" {
		s.writeJSONError(w, http.StatusBadRequest, msg)
		return
	}

	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name,
		},
		Spec: v1alpha1.TeamSpec{
			DisplayName:         req.DisplayName,
			Members:             req.Members,
			Subjects:            req.Subjects,
			Namespaces:          req.Namespaces,
			RoleRef:             req.RoleRef,
			ProvisionNamespaces: req.ProvisionNamespaces,
			Pipelines:           teamPipelinesSpecFromEntry(req.Pipelines),
		},
	}

	ctx := r.Context()
	if err := crdstore.Apply[v1alpha1.Team](ctx, s.deps.Store, team); err != nil {
		if k8sErrorToHTTP(err) == http.StatusBadRequest {
			s.writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, "failed to create team")
		return
	}

	s.writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

// HandleTeamDispatch dispatches /teams/{name} requests:
//
//	GET    /teams/{name} — get team detail (admin, or CanReadPipeline on the tenant)
//	PUT    /teams/{name} — update team (admin, or the tenant's pipelines:admin capability)
//	DELETE /teams/{name} — delete team (admin)
func (s *Server) HandleTeamDispatch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/teams/")
	if name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "team name required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetTeam(w, r, name)
	case http.MethodPut:
		s.handleUpdateTeam(w, r, name)
	case http.MethodDelete:
		s.handleDeleteTeam(w, r, name)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// canReadTeam reports whether claims may read the Team named name: full
// administrative access, or CanReadPipeline on the Team's own pipelines
// tenant, with an empty job path (specs/tenant-model). A Team name never
// contains "/", so TenantRefFromTenant always classifies it as a Team
// tenant.
func (s *Server) canReadTeam(claims *auth.Claims, name string) bool {
	if s.deps.Authorizer.IsAdmin(claims) {
		return true
	}
	ref, err := TenantRefFromTenant(name)
	if err != nil {
		return false
	}
	return s.deps.Authorizer.CanReadPipeline(claims, ref, "")
}

// canWriteTeam reports whether claims may write the Team named name: full
// administrative access, or the tenant's pipelines:admin capability, which
// add-tenant-authorization defines as a tenant-owned write of
// Team.spec.pipelines.exports (specs/tenant-model). Neither check is
// controller-scoped, because a grant belongs to a tenant, not to a cell.
func (s *Server) canWriteTeam(claims *auth.Claims, name string) bool {
	if s.deps.Authorizer.IsAdmin(claims) {
		return true
	}
	ref, err := TenantRefFromTenant(name)
	if err != nil {
		return false
	}
	return s.deps.Authorizer.canAccessPipeline(claims, ref.Tenant, ref.Folder, "", "admin")
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.canReadTeam(claims, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	team, err := crdstore.Get[v1alpha1.Team](ctx, s.deps.Store, name, "")
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "team not found")
		return
	}

	s.writeJSON(w, http.StatusOK, teamFromCRD(team))
}

func (s *Server) handleUpdateTeam(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.canWriteTeam(claims, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createTeamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate: at least one of members or subjects.
	if len(req.Members) == 0 && len(req.Subjects) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "at least one of members or subjects must be set")
		return
	}

	// Validate: at least one namespace.
	if len(req.Namespaces) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "at least one namespace is required")
		return
	}

	// Validate: roleRef must not be "admin".
	if req.RoleRef == "admin" {
		s.writeJSONError(w, http.StatusBadRequest, "roleRef 'admin' is not permitted on a Team")
		return
	}
	if msg := validateTeamPipelines(req.Pipelines, req.Namespaces); msg != "" {
		s.writeJSONError(w, http.StatusBadRequest, msg)
		return
	}

	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.TeamSpec{
			DisplayName:         req.DisplayName,
			Members:             req.Members,
			Subjects:            req.Subjects,
			Namespaces:          req.Namespaces,
			RoleRef:             req.RoleRef,
			ProvisionNamespaces: req.ProvisionNamespaces,
			Pipelines:           teamPipelinesSpecFromEntry(req.Pipelines),
		},
	}

	ctx := r.Context()
	if !s.deps.Authorizer.IsAdmin(claims) {
		// A tenant pipelines admin may change only spec.pipelines.exports.
		// Every other field of the Team stays as stored, or the write is
		// refused, so the tenant right never reaches members, roleRef,
		// namespaces, or placement.
		stored, err := crdstore.Get[v1alpha1.Team](ctx, s.deps.Store, name, "")
		if err != nil || stored == nil {
			s.writeJSONError(w, http.StatusNotFound, "team not found")
			return
		}
		if !teamSpecEqualExceptTenantOwned(stored.Spec, team.Spec) {
			s.writeJSONError(w, http.StatusForbidden, "a tenant pipelines admin may change only exports and concurrency limits")
			return
		}
	}
	if err := crdstore.Apply[v1alpha1.Team](ctx, s.deps.Store, team); err != nil {
		if k8sErrorToHTTP(err) == http.StatusBadRequest {
			s.writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, "failed to update team")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	if err := crdstore.Delete[v1alpha1.Team](ctx, s.deps.Store, name, ""); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to delete team")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// teamSpecEqualExceptTenantOwned reports whether a and b differ only in
// spec.pipelines.exports or spec.pipelines.concurrencyLimits. Both specs are
// copied before those fields are cleared, so neither argument changes.
func teamSpecEqualExceptTenantOwned(a, b v1alpha1.TeamSpec) bool {
	strip := func(in v1alpha1.TeamSpec) v1alpha1.TeamSpec {
		var out v1alpha1.TeamSpec
		in.DeepCopyInto(&out)
		if out.Pipelines != nil {
			out.Pipelines.Exports = nil
			out.Pipelines.ConcurrencyLimits = nil
		}
		return out
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// concurrencyLimitNamePattern is the DNS label a limit name must match.
var concurrencyLimitNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validateConcurrencyLimits checks each limit of spec.pipelines.concurrencyLimits.
// It returns "" for a valid or absent list.
func validateConcurrencyLimits(limits []v1alpha1.TeamConcurrencyLimit) string {
	seen := map[string]bool{}
	for i, l := range limits {
		if len(l.Name) > 63 || !concurrencyLimitNamePattern.MatchString(l.Name) {
			return fmt.Sprintf("pipelines.concurrencyLimits[%d].name is invalid", i)
		}
		if seen[l.Name] {
			return fmt.Sprintf("pipelines.concurrencyLimits[%d].name is a duplicate", i)
		}
		seen[l.Name] = true
		if len(l.Jobs) == 0 {
			return fmt.Sprintf("pipelines.concurrencyLimits[%d].jobs is required", i)
		}
		for _, g := range l.Jobs {
			if strings.TrimSpace(g) == "" || strings.HasPrefix(g, "/") {
				return fmt.Sprintf("pipelines.concurrencyLimits[%d].jobs holds an invalid glob", i)
			}
		}
		if l.Max < 1 {
			return fmt.Sprintf("pipelines.concurrencyLimits[%d].max must be at least 1", i)
		}
	}
	return ""
}
