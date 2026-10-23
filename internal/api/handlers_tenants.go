package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// tenantEntry is the JSON shape for GET /tenants entries. Pipelines carries
// spec.pipelines; Placement carries status.placement; Rehydration carries
// status.rehydration. Neither status.pipelines.webhookToken nor
// status.pipelines.webhookSecret is ever mapped here: both are credentials
// (specs/tenant-model/spec.md). Controller.status.cell.rehydration is never
// mapped here either: it is the controller reconciler's own per-cell record,
// and TenantReconciler's copy onto status.rehydration is the projection this
// handler serves.
type tenantEntry struct {
	Name                string                          `json:"name"`
	DisplayName         string                          `json:"displayName,omitempty"`
	Members             []string                        `json:"members,omitempty"`
	Subjects            []v1alpha1.SubjectRef           `json:"subjects,omitempty"`
	Namespaces          []string                        `json:"namespaces"`
	RoleRef             string                          `json:"roleRef,omitempty"`
	ProvisionNamespaces bool                            `json:"provisionNamespaces,omitempty"`
	Pipelines           *tenantPipelinesEntry           `json:"pipelines,omitempty"`
	ObservedGeneration  int64                           `json:"observedGeneration,omitempty"`
	GroupRef            string                          `json:"groupRef,omitempty"`
	BindingRef          string                          `json:"bindingRef,omitempty"`
	NamespaceStates     []v1alpha1.TenantNamespaceState `json:"namespaceStates,omitempty"`
	Conditions          []v1alpha1.TenantCondition      `json:"conditions,omitempty"`
	Placement           *tenantPlacementEntry           `json:"placement,omitempty"`
	Rehydration         *tenantRehydrationEntry         `json:"rehydration,omitempty"`
}

// tenantPipelinesEntry is the JSON shape of spec.pipelines, carried on both
// the request and the response. RehydrateDeadline and CronGrace keep the
// *metav1.Duration type: it marshals to and parses from a plain duration
// string on its own, so no hand-written conversion is needed here.
type tenantPipelinesEntry struct {
	Profile           string                        `json:"profile"`
	CellPool          string                        `json:"cellPool,omitempty"`
	AgentNamespace    string                        `json:"agentNamespace"`
	ComposedBundleRef *tenantComposedBundleRefEntry `json:"composedBundleRef,omitempty"`

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
	Exports []tenantExportEntry `json:"exports,omitempty"`

	// Retention carries spec.pipelines.retention: the record-store windows
	// for this tenant's runs. Only an operator may write it, so the
	// tenant-admin path rejects a change to it the same way it rejects any
	// other change outside the exports.
	Retention *v1alpha1.TenantPipelinesRetention `json:"retention,omitempty"`

	// ConcurrencyLimits carries spec.pipelines.concurrencyLimits. A tenant
	// pipelines admin may write it, the same as exports.
	ConcurrencyLimits []v1alpha1.TenantConcurrencyLimit `json:"concurrencyLimits,omitempty"`

	// Notifications carries spec.pipelines.notifications. Changing it needs
	// the operator's own write, not the tenant's pipelines:admin grant.
	Notifications *tenantNotificationsEntry `json:"notifications,omitempty"`
}

// tenantNotificationsEntry is the JSON shape of spec.pipelines.notifications.
type tenantNotificationsEntry struct {
	Webhook *tenantWebhookEntry `json:"webhook,omitempty"`
}

// tenantWebhookEntry is the JSON shape of a completion webhook.
type tenantWebhookEntry struct {
	URL       string                   `json:"url"`
	SecretRef tenantWebhookSecretEntry `json:"secretRef"`
}

// tenantWebhookSecretEntry names the Secret and the data key that hold the
// webhook signing key.
type tenantWebhookSecretEntry struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// tenantExportEntry is the JSON shape of one spec.pipelines.exports entry.
// Exactly one of Job or Locks is set; the CRD's CEL rules enforce it.
type tenantExportEntry struct {
	Job     string   `json:"job,omitempty"`
	Locks   []string `json:"locks,omitempty"`
	Callers []string `json:"callers,omitempty"`
}

// tenantComposedBundleRefEntry is the JSON shape of
// spec.pipelines.composedBundleRef. A Tenant is cluster-scoped, so it
// carries both name and namespace with no namespace to default to.
type tenantComposedBundleRefEntry struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// tenantPlacementEntry is the JSON shape of status.placement, response-only:
// the placement pass is its only writer.
type tenantPlacementEntry struct {
	Cluster       string `json:"cluster,omitempty"`
	Cell          string `json:"cell,omitempty"`
	CellNamespace string `json:"cellNamespace,omitempty"`
	Generation    int64  `json:"generation,omitempty"`
	// Members reports every member of the tenant. The scalar fields above
	// keep describing slot 0.
	Members []tenantMemberEntry `json:"members,omitempty"`
}

// tenantMemberEntry is the JSON shape of one status.placement.members
// entry, response-only.
type tenantMemberEntry struct {
	Cell          string `json:"cell,omitempty"`
	CellNamespace string `json:"cellNamespace,omitempty"`
	Role          string `json:"role,omitempty"`
	Slot          *int32 `json:"slot,omitempty"`
	Applied       bool   `json:"applied,omitempty"`
}

// tenantRehydrationEntry is the JSON shape of status.rehydration,
// response-only: TenantReconciler is its only writer.
type tenantRehydrationEntry struct {
	Generation   int64        `json:"generation,omitempty"`
	CellInstance string       `json:"cellInstance,omitempty"`
	Records      int32        `json:"records,omitempty"`
	Truncated    bool         `json:"truncated,omitempty"`
	UnseededJobs int32        `json:"unseededJobs,omitempty"`
	Deferred     []string     `json:"deferred,omitempty"`
	Attempts     int32        `json:"attempts,omitempty"`
	LastAttempt  *metav1.Time `json:"lastAttempt,omitempty"`
}

// tenantPipelinesEntryFromSpec maps spec.pipelines onto the wire shape. It
// returns nil for a Tenant that is not a pipelines tenant.
func tenantPipelinesEntryFromSpec(p *v1alpha1.TenantPipelinesSpec) *tenantPipelinesEntry {
	if p == nil {
		return nil
	}
	e := &tenantPipelinesEntry{
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
		e.ComposedBundleRef = &tenantComposedBundleRefEntry{
			Name:      p.ComposedBundleRef.Name,
			Namespace: p.ComposedBundleRef.Namespace,
		}
	}
	for _, exp := range p.Exports {
		e.Exports = append(e.Exports, tenantExportEntry{
			Job:     exp.Job,
			Locks:   exp.Locks,
			Callers: exp.Callers,
		})
	}
	if n := p.Notifications; n != nil && n.Webhook != nil {
		e.Notifications = &tenantNotificationsEntry{Webhook: &tenantWebhookEntry{
			URL: n.Webhook.URL,
			SecretRef: tenantWebhookSecretEntry{
				Name: n.Webhook.SecretRef.Name,
				Key:  n.Webhook.SecretRef.Key,
			},
		}}
	}
	e.Retention = p.Retention
	e.ConcurrencyLimits = p.ConcurrencyLimits
	return e
}

// tenantPipelinesSpecFromEntry maps the wire shape back onto spec.pipelines.
// It returns nil when the request carries no pipelines object, so a Tenant
// with no spec.pipelines round-trips as no spec.pipelines, not an empty
// one.
func tenantPipelinesSpecFromEntry(e *tenantPipelinesEntry) *v1alpha1.TenantPipelinesSpec {
	if e == nil {
		return nil
	}
	spec := &v1alpha1.TenantPipelinesSpec{
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
		spec.Exports = append(spec.Exports, v1alpha1.TenantExport{
			Job:     exp.Job,
			Locks:   exp.Locks,
			Callers: exp.Callers,
		})
	}
	if n := e.Notifications; n != nil && n.Webhook != nil {
		spec.Notifications = &v1alpha1.TenantNotificationsSpec{
			Webhook: &v1alpha1.TenantWebhookSpec{
				URL: strings.TrimSpace(n.Webhook.URL),
				SecretRef: v1alpha1.TenantWebhookSecretRef{
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

// validateTenantPipelines checks the spec.pipelines rules that a caller breaks
// by omission. The apiserver enforces the same rules. This check names the
// field in a 400 before the write. It returns "" for a valid or absent block.
func validateTenantPipelines(p *tenantPipelinesEntry, namespaces []string) string {
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
	if p.Members != nil && (*p.Members < 1 || *p.Members > 16) {
		return "pipelines.members must be between 1 and 16"
	}
	if p.Standby != nil && (*p.Standby < 0 || *p.Standby > 8) {
		return "pipelines.standby must be between 0 and 8"
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

// tenantPlacementEntryFromStatus maps status.placement onto the wire shape.
func tenantPlacementEntryFromStatus(p *v1alpha1.TenantPlacementStatus) *tenantPlacementEntry {
	if p == nil {
		return nil
	}
	out := &tenantPlacementEntry{
		Cluster:       p.Cluster,
		Cell:          p.Cell,
		CellNamespace: p.CellNamespace,
		Generation:    p.Generation,
	}
	for _, m := range p.Members {
		out.Members = append(out.Members, tenantMemberEntry{
			Cell: m.Cell, CellNamespace: m.CellNamespace, Role: m.Role,
			Slot: m.Slot, Applied: m.Applied,
		})
	}
	return out
}

// tenantRehydrationEntryFromStatus maps status.rehydration onto the wire
// shape.
func tenantRehydrationEntryFromStatus(r *v1alpha1.TenantRehydrationStatus) *tenantRehydrationEntry {
	if r == nil {
		return nil
	}
	return &tenantRehydrationEntry{
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

// tenantFromCRD builds a tenantEntry from a Tenant CRD.
func tenantFromCRD(t *v1alpha1.Tenant) tenantEntry {
	return tenantEntry{
		Name:                t.Name,
		DisplayName:         t.Spec.DisplayName,
		Members:             t.Spec.Members,
		Subjects:            t.Spec.Subjects,
		Namespaces:          t.Spec.Namespaces,
		RoleRef:             t.Spec.RoleRef,
		ProvisionNamespaces: t.Spec.ProvisionNamespaces,
		Pipelines:           tenantPipelinesEntryFromSpec(t.Spec.Pipelines),
		ObservedGeneration:  t.Status.ObservedGeneration,
		GroupRef:            t.Status.GroupRef,
		BindingRef:          t.Status.BindingRef,
		NamespaceStates:     t.Status.NamespaceStates,
		Conditions:          t.Status.Conditions,
		Placement:           tenantPlacementEntryFromStatus(t.Status.Placement),
		Rehydration:         tenantRehydrationEntryFromStatus(t.Status.Rehydration),
	}
}

// HandleTenants dispatches /tenants requests:
//
//	GET    /tenants          — list tenants (admin)
//	POST   /tenants          — create tenant (admin)
func (s *Server) HandleTenants(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListTenants(w, r)
	case http.MethodPost:
		s.handleCreateTenant(w, r)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	tenants, err := crdstore.List[v1alpha1.Tenant](ctx, s.deps.Store, "", "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list tenants")
		return
	}

	result := make([]tenantEntry, 0, len(tenants))
	for _, t := range tenants {
		result = append(result, tenantFromCRD(t))
	}
	s.writeJSON(w, http.StatusOK, itemsEnvelope(result))
}

// createTenantRequest is the JSON body for POST /tenants and PUT /tenants/{name}.
type createTenantRequest struct {
	Name                string                `json:"name"`
	DisplayName         string                `json:"displayName,omitempty"`
	Members             []string              `json:"members,omitempty"`
	Subjects            []v1alpha1.SubjectRef `json:"subjects,omitempty"`
	Namespaces          []string              `json:"namespaces"`
	RoleRef             string                `json:"roleRef,omitempty"`
	ProvisionNamespaces bool                  `json:"provisionNamespaces,omitempty"`
	Pipelines           *tenantPipelinesEntry `json:"pipelines,omitempty"`
}

func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createTenantRequest
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
		s.writeJSONError(w, http.StatusBadRequest, "roleRef 'admin' is not permitted on a Tenant")
		return
	}
	if msg := validateTenantPipelines(req.Pipelines, req.Namespaces); msg != "" {
		s.writeJSONError(w, http.StatusBadRequest, msg)
		return
	}

	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name,
		},
		Spec: v1alpha1.TenantSpec{
			DisplayName:         req.DisplayName,
			Members:             req.Members,
			Subjects:            req.Subjects,
			Namespaces:          req.Namespaces,
			RoleRef:             req.RoleRef,
			ProvisionNamespaces: req.ProvisionNamespaces,
			Pipelines:           tenantPipelinesSpecFromEntry(req.Pipelines),
		},
	}

	ctx := r.Context()
	if err := crdstore.Create[v1alpha1.Tenant](ctx, s.deps.Store, tenant); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.writeJSONError(w, http.StatusConflict, "tenant already exists")
			return
		}
		if k8sErrorToHTTP(err) == http.StatusBadRequest {
			s.writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, "failed to create tenant")
		return
	}

	s.writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

// HandleTenantDispatch dispatches /tenants/{name} requests:
//
//	GET    /tenants/{name} — get tenant detail (admin, or CanReadPipeline on the tenant)
//	PUT    /tenants/{name} — update tenant (admin, or the tenant's pipelines:admin capability)
//	DELETE /tenants/{name} — delete tenant (admin)
func (s *Server) HandleTenantDispatch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/tenants/")
	if name == "" {
		s.writeJSONError(w, http.StatusBadRequest, "tenant name required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetTenant(w, r, name)
	case http.MethodPut:
		s.handleUpdateTenant(w, r, name)
	case http.MethodDelete:
		s.handleDeleteTenant(w, r, name)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// canReadTenant reports whether claims may read the Tenant named name: full
// administrative access, or CanReadPipeline on the Tenant's own pipelines
// tenant, with an empty job path (specs/tenant-model). A Tenant name never
// contains "/", so TenantRefFromTenant always classifies it as a Tenant
// tenant.
func (s *Server) canReadTenant(claims *auth.Claims, name string) bool {
	if s.deps.Authorizer.IsAdmin(claims) {
		return true
	}
	ref, err := TenantRefFromTenant(name)
	if err != nil {
		return false
	}
	return s.deps.Authorizer.CanReadPipeline(claims, ref, "")
}

// canWriteTenant reports whether claims may write the Tenant named name: full
// administrative access, or the tenant's pipelines:admin capability, which
// add-tenant-authorization defines as a tenant-owned write of
// Tenant.spec.pipelines.exports (specs/tenant-model). Neither check is
// controller-scoped, because a grant belongs to a tenant, not to a cell.
func (s *Server) canWriteTenant(claims *auth.Claims, name string) bool {
	if s.deps.Authorizer.IsAdmin(claims) {
		return true
	}
	ref, err := TenantRefFromTenant(name)
	if err != nil {
		return false
	}
	return s.deps.Authorizer.canAccessPipeline(claims, ref.Tenant, ref.Folder, "", "admin")
}

func (s *Server) handleGetTenant(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.canReadTenant(claims, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	tenant, err := crdstore.Get[v1alpha1.Tenant](ctx, s.deps.Store, name, "")
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "tenant not found")
		return
	}

	s.writeJSON(w, http.StatusOK, tenantFromCRD(tenant))
}

func (s *Server) handleUpdateTenant(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.canWriteTenant(claims, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createTenantRequest
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
		s.writeJSONError(w, http.StatusBadRequest, "roleRef 'admin' is not permitted on a Tenant")
		return
	}
	if msg := validateTenantPipelines(req.Pipelines, req.Namespaces); msg != "" {
		s.writeJSONError(w, http.StatusBadRequest, msg)
		return
	}

	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.TenantSpec{
			DisplayName:         req.DisplayName,
			Members:             req.Members,
			Subjects:            req.Subjects,
			Namespaces:          req.Namespaces,
			RoleRef:             req.RoleRef,
			ProvisionNamespaces: req.ProvisionNamespaces,
			Pipelines:           tenantPipelinesSpecFromEntry(req.Pipelines),
		},
	}

	ctx := r.Context()
	if !s.deps.Authorizer.IsAdmin(claims) {
		// A tenant pipelines admin may change only spec.pipelines.exports.
		// Every other field of the Tenant stays as stored, or the write is
		// refused, so the tenant right never reaches members, roleRef,
		// namespaces, or placement.
		stored, err := crdstore.Get[v1alpha1.Tenant](ctx, s.deps.Store, name, "")
		if err != nil || stored == nil {
			s.writeJSONError(w, http.StatusNotFound, "tenant not found")
			return
		}
		if !tenantSpecEqualExceptTenantOwned(stored.Spec, tenant.Spec) {
			s.writeJSONError(w, http.StatusForbidden, "a tenant pipelines admin may change only exports and concurrency limits")
			return
		}
	}
	if err := crdstore.Apply[v1alpha1.Tenant](ctx, s.deps.Store, tenant); err != nil {
		if k8sErrorToHTTP(err) == http.StatusBadRequest {
			s.writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeJSONError(w, http.StatusInternalServerError, "failed to update tenant")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleDeleteTenant(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ctx := r.Context()
	if err := crdstore.Delete[v1alpha1.Tenant](ctx, s.deps.Store, name, ""); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to delete tenant")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// tenantSpecEqualExceptTenantOwned reports whether a and b differ only in
// spec.pipelines.exports or spec.pipelines.concurrencyLimits. Both specs are
// copied before those fields are cleared, so neither argument changes.
func tenantSpecEqualExceptTenantOwned(a, b v1alpha1.TenantSpec) bool {
	strip := func(in v1alpha1.TenantSpec) v1alpha1.TenantSpec {
		var out v1alpha1.TenantSpec
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
func validateConcurrencyLimits(limits []v1alpha1.TenantConcurrencyLimit) string {
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
