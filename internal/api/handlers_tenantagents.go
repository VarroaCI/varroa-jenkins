package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/accept"
)

// tenantAgentRunRequested is the activity type of a manual Tenant agent run. The
// runner's trigger consumer reads it as the manual trigger.
const tenantAgentRunRequested = "tenantagent.run_requested"

// Refusal codes of the Tenant agent routes. Each answers a 409 body with a
// code field.
const (
	tenantAgentNotAccepted  = "not_accepted"
	tenantAgentDisabled     = "disabled"
	tenantAgentNoManual     = "manual_not_enabled"
	tenantAgentNothingGrant = "nothing_to_grant"
)

// tenantAgentJSON is the Tenant agent object of GET /pipelines/{tenant}/agent.
type tenantAgentJSON struct {
	Namespace        string                     `json:"namespace"`
	Name             string                     `json:"name"`
	Tenant           string                     `json:"tenant"`
	Cluster          string                     `json:"cluster"`
	Spec             v1alpha1.TenantAgentSpec   `json:"spec"`
	Status           v1alpha1.TenantAgentStatus `json:"status"`
	Accepted         bool                       `json:"accepted"`
	PendingApprovals int                        `json:"pendingApprovals"`
	SpendToday       int64                      `json:"spendToday"`
	DayLimit         int64                      `json:"dayLimit"`
}

// tenantAgentSessionJSON is one session of the Tenant agent sessions list. Each
// item carries its pending approvals.
type tenantAgentSessionJSON struct {
	assistantSessionJSON
	PendingApprovals []assistantApprovalJSON `json:"pendingApprovals"`
}

// capabilityJSON names one tool of a capability group.
type capabilityJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
}

// capabilitiesJSON holds the four groups of the capabilities tab.
type capabilitiesJSON struct {
	Reads            []capabilityJSON `json:"reads"`
	WritesByApproval []capabilityJSON `json:"writesByApproval"`
	Unattended       []capabilityJSON `json:"unattended"`
	Never            []capabilityJSON `json:"never"`
}

// pausedJSON names why a Tenant agent is paused.
type pausedJSON struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// agentsItemJSON is one row of GET /agents.
type agentsItemJSON struct {
	Namespace           string             `json:"namespace"`
	Name                string             `json:"name"`
	Tenant              string             `json:"tenant"`
	Cluster             string             `json:"cluster"`
	Enabled             bool               `json:"enabled"`
	Paused              *pausedJSON        `json:"paused"`
	SessionsToday       int                `json:"sessionsToday"`
	SpendToday          int64              `json:"spendToday"`
	FailedToday         int                `json:"failedToday"`
	Conditions          []metav1.Condition `json:"conditions"`
	UnattendedRequested []string           `json:"unattendedRequested"`
	UnattendedGranted   []string           `json:"unattendedGranted"`
}

// tenantAgentRunBody is the optional body of POST /pipelines/{tenant}/agent/run.
type tenantAgentRunBody struct {
	Job string `json:"job"`
	Run string `json:"run"`
}

// tenantAgentAcceptedFor returns the accepted TenantAgent of tenant. A BFF without
// the runner has no source, and it has no accepted agent.
func (s *Server) tenantAgentAcceptedFor(tenant string) (*v1alpha1.TenantAgent, bool) {
	if s.deps.TenantAgents == nil {
		return nil, false
	}
	return accept.AcceptedFor(s.deps.TenantAgents.AcceptInput(), tenant)
}

// tenantAgentWritableRef returns the first of refs the caller may write.
func (s *Server) tenantAgentWritableRef(claims *auth.Claims, refs []TenantRef) (TenantRef, bool) {
	for _, ref := range refs {
		if s.canWritePipeline(claims, ref, "") {
			return ref, true
		}
	}
	return TenantRef{}, false
}

// tenantAgentDayLimit returns the smaller of the chart day budget and the
// TenantAgent's own, as the status ticker computes it.
func (s *Server) tenantAgentDayLimit(ta *v1alpha1.TenantAgent) int64 {
	var limit int64
	if rt := s.deps.Assistant; rt != nil {
		limit = rt.Config.Budgets.TenantDay
	}
	if ta.Spec.Budgets != nil && ta.Spec.Budgets.Day != nil && (limit == 0 || *ta.Spec.Budgets.Day < limit) {
		limit = *ta.Spec.Budgets.Day
	}
	return limit
}

// tenantAgentSpendToday reads the rollup row of tenant for the current UTC day.
func (s *Server) tenantAgentSpendToday(r *http.Request, tenant string) (int64, error) {
	if s.deps.RecordStore == nil {
		return 0, nil
	}
	row, err := s.deps.RecordStore.TenantAgentSpend(r.Context(), tenant, utcDay(time.Now()))
	return row.Tokens, err
}

// utcDay returns the UTC midnight of t.
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// tenantAgentPendingCount counts the pending approvals of the sessions of one
// Tenant agent principal.
func (s *Server) tenantAgentPendingCount(r *http.Request, principal string) (int, error) {
	rt := s.deps.Assistant
	if rt == nil {
		return 0, nil
	}
	page, err := rt.Store.ListApprovals(r.Context(), store.ApprovalQuery{
		Principal: principal, State: agent.ApprovalPending, Limit: 200,
	})
	return len(page.Items), err
}

// handleGetTenantAgent serves GET /pipelines/{tenant}/agent.
func (s *Server) handleGetTenantAgent(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	tenant := r.PathValue("tenant")
	refs, serr := s.resolveTenantTenants(r.Context(), claims, tenant)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	ref, ok := s.tenantForJob(claims, refs, "")
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	ta, ok := s.tenantAgentAcceptedFor(tenant)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "no accepted Tenant agent")
		return
	}
	pending, err := s.tenantAgentPendingCount(r, accept.Principal(ta))
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	spend, err := s.tenantAgentSpendToday(r, tenant)
	if err != nil {
		s.deps.Logger.Error("tenant agent spend", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "tenant agent spend failed")
		return
	}
	s.writeJSON(w, http.StatusOK, tenantAgentJSON{
		Namespace: ta.Namespace, Name: ta.Name, Tenant: tenant, Cluster: ref.Cluster,
		Spec: ta.Spec, Status: ta.Status, Accepted: true,
		PendingApprovals: pending, SpendToday: spend, DayLimit: s.tenantAgentDayLimit(ta),
	})
}

// handleListTenantAgentSessions serves GET /pipelines/{tenant}/agent/sessions.
func (s *Server) handleListTenantAgentSessions(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	tenant := r.PathValue("tenant")
	if !s.tenantAgentReadable(w, r, claims, tenant) {
		return
	}
	if _, ok := s.tenantAgentAcceptedFor(tenant); !ok {
		s.writeJSONError(w, http.StatusNotFound, "no accepted Tenant agent")
		return
	}
	q := r.URL.Query()
	query := store.SessionQuery{
		Kind:   agent.KindTenantAgent,
		Tenant: tenant,
		State:  agent.SessionState(q.Get("state")),
		Cursor: q.Get("cursor"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 200 {
			s.writeJSONError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		query.Limit = n
	}
	page, err := rt.Store.ListSessions(r.Context(), query)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	items := make([]tenantAgentSessionJSON, 0, len(page.Items))
	for _, sess := range page.Items {
		item := tenantAgentSessionJSON{assistantSessionJSON: toAssistantSessionJSON(sess), PendingApprovals: []assistantApprovalJSON{}}
		if sess.State == agent.StateWaitingApproval {
			pending, err := rt.Store.ListApprovals(r.Context(), store.ApprovalQuery{
				SessionID: sess.ID, State: agent.ApprovalPending, Limit: 50,
			})
			if err != nil {
				s.assistantStoreError(w, err)
				return
			}
			for _, a := range pending.Items {
				item.PendingApprovals = append(item.PendingApprovals, toAssistantApprovalJSON(a, nil))
			}
		}
		items = append(items, item)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": page.NextCursor})
}

// tenantAgentReadable applies the tenant read check to tenant. It writes the
// refusal and returns false.
func (s *Server) tenantAgentReadable(w http.ResponseWriter, r *http.Request, claims *auth.Claims, tenant string) bool {
	refs, serr := s.resolveTenantTenants(r.Context(), claims, tenant)
	if serr != nil {
		s.writeServiceError(w, serr)
		return false
	}
	if _, ok := s.tenantForJob(claims, refs, ""); !ok {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
}

// handleTenantAgentCapabilities serves GET /pipelines/{tenant}/agent/capabilities.
// The four groups come from the in-process tool list of the agent principal,
// filtered by the data level and the object's grant.
func (s *Server) handleTenantAgentCapabilities(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	tenant := r.PathValue("tenant")
	if !s.tenantAgentReadable(w, r, claims, tenant) {
		return
	}
	ta, ok := s.tenantAgentAcceptedFor(tenant)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "no accepted Tenant agent")
		return
	}
	principal := accept.Principal(ta)
	tools, err := rt.HostFor(&auth.Claims{Subject: principal}).List(r.Context())
	if err != nil {
		s.deps.Logger.Error("tenant agent tools", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "tool list failed")
		return
	}
	settings, err := rt.Store.GetSettings(r.Context(), agent.ScopeGlobal)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	level := s.tenantAgentLevel(rt, ta, settings, tenant)
	visible := []agent.ToolDef{}
	for _, t := range tools {
		if policy.Visible(t) {
			visible = append(visible, t)
		}
	}
	s.writeJSON(w, http.StatusOK, buildCapabilities(ta, settings, datashare.Visible(level, visible)))
}

// tenantAgentLevel returns the effective data level of a Tenant agent session of
// ta on tenant, as the create decision computes it.
func (s *Server) tenantAgentLevel(rt *AssistantRuntime, ta *v1alpha1.TenantAgent, settings agent.Settings, tenant string) agent.DataSharing {
	var trust agent.ProviderTrust
	if rt.Registry != nil {
		entry, ok := rt.Registry.Default()
		if ta.Spec.Provider != "" {
			entry, ok = rt.Registry.Get(ta.Spec.Provider)
		}
		if ok {
			trust = entry.Endpoint.Trust
		}
	}
	return datashare.Effective(datashare.Inputs{
		User:            agent.DataSharing(ta.Spec.DataSharing),
		Tenant:          tenant,
		TenantLevels:    settings.TenantDataSharing,
		Trust:           trust,
		ExternalCeiling: rt.Config.ExternalDataSharing,
	})
}

// buildCapabilities sorts the visible tools into the four groups of design
// section 12.1. A read tool is in reads. A write tool is in unattended when
// the grant or an enabled output names it, in never when it needs an
// approval in every case, and in writesByApproval otherwise.
func buildCapabilities(ta *v1alpha1.TenantAgent, settings agent.Settings, tools []agent.ToolDef) capabilitiesJSON {
	out := capabilitiesJSON{
		Reads: []capabilityJSON{}, WritesByApproval: []capabilityJSON{},
		Unattended: []capabilityJSON{}, Never: []capabilityJSON{},
	}
	outKinds := accept.OutputKinds(ta.Spec.Outputs)
	outTools := accept.OutputToolNames(outKinds)
	var granted []string
	if g, ok := settings.TenantAgentUnattended[ta.Namespace+"/"+ta.Name]; ok && g.UID == string(ta.UID) {
		granted = g.Tools
	}
	kindOf := func(name string) string {
		for _, k := range outKinds {
			if slices.Contains(accept.OutputToolNames([]string{k}), name) {
				return k
			}
		}
		return ""
	}
	for _, t := range tools {
		entry := capabilityJSON{Name: t.Name, Description: t.Description, Kind: string(policy.Class(t))}
		if policy.Class(t) == agent.ClassRead {
			out.Reads = append(out.Reads, entry)
			continue
		}
		if slices.Contains(outTools, t.Name) {
			entry.Kind = kindOf(t.Name)
			out.Unattended = append(out.Unattended, entry)
			continue
		}
		if slices.Contains(granted, t.Name) {
			out.Unattended = append(out.Unattended, entry)
			continue
		}
		if policy.IsNeverUnattended(t, outTools) {
			out.Never = append(out.Never, entry)
			continue
		}
		out.WritesByApproval = append(out.WritesByApproval, entry)
	}
	return out
}

// handleRunTenantAgent serves POST /pipelines/{tenant}/agent/run. It publishes a
// manual run request and answers 202. A kill switch or a spent budget still
// answers 202, because the runner skips the session.
func (s *Server) handleRunTenantAgent(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	tenant := r.PathValue("tenant")
	var body tenantAgentRunBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, assistantMaxBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		s.writeJSONError(w, http.StatusBadRequest, "body must be a JSON object")
		return
	}
	refs, serr := s.resolveTenantTenants(r.Context(), claims, tenant)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	ref, ok := s.tenantAgentWritableRef(claims, refs)
	if !ok {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	ta, ok := s.tenantAgentAcceptedFor(tenant)
	if !ok {
		writeAssistantError(s, w, http.StatusConflict, tenantAgentNotAccepted, "no accepted Tenant agent")
		return
	}
	if !accept.Enabled(ta) {
		writeAssistantError(s, w, http.StatusConflict, tenantAgentDisabled, "the Tenant agent is disabled")
		return
	}
	if !slices.Contains(triggerTypes(ta), v1alpha1.TenantAgentTriggerManual) {
		writeAssistantError(s, w, http.StatusConflict, tenantAgentNoManual, "the manual trigger is not enabled")
		return
	}
	if s.deps.ActivityPublisher == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "activity is not configured")
		return
	}
	cluster := ref.Cluster
	if cluster == "" {
		cluster = s.deps.CoreCluster
	}
	item := tenant
	if body.Job != "" {
		item = tenant + "/" + body.Job
	}
	message := "a manual Tenant agent run was requested"
	if body.Run != "" {
		message += " for run " + body.Run
	}
	s.deps.ActivityPublisher.Publish(activity.Event{
		Timestamp:  time.Now().UTC(),
		Type:       tenantAgentRunRequested,
		Source:     "user",
		Actor:      ActorFrom(claims),
		Controller: ref.Controller,
		Namespace:  ta.Namespace,
		Cluster:    cluster,
		ItemPath:   item,
		Reason:     ta.Namespace + "/" + ta.Name,
		Message:    message,
		Severity:   "info",
	})
	s.writeJSON(w, http.StatusAccepted, map[string]string{"state": "requested"})
}

// triggerTypes lists the trigger types of ta.
func triggerTypes(ta *v1alpha1.TenantAgent) []string {
	out := make([]string, 0, len(ta.Spec.Triggers))
	for _, t := range ta.Spec.Triggers {
		out = append(out, t.Type)
	}
	return out
}

// handleListAgents serves GET /agents: every TenantAgent with its state, its
// spend, and its unattended grant.
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadAgentSessions(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	items := []agentsItemJSON{}
	if s.deps.TenantAgents != nil {
		list := s.deps.TenantAgents.List()
		slices.SortFunc(list, func(a, b v1alpha1.TenantAgent) int {
			return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
		})
		for i := range list {
			item, err := s.agentsItem(r, claims, &list[i])
			if err != nil {
				s.deps.Logger.Error("tenant agent list", "error", err)
				s.writeJSONError(w, http.StatusInternalServerError, "tenant agent list failed")
				return
			}
			items = append(items, item)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// agentsItem builds one row of GET /agents.
func (s *Server) agentsItem(r *http.Request, claims *auth.Claims, ta *v1alpha1.TenantAgent) (agentsItemJSON, error) {
	tenant := ta.Spec.TenantRef.Name
	item := agentsItemJSON{
		Namespace: ta.Namespace, Name: ta.Name, Tenant: tenant,
		Enabled:             accept.Enabled(ta),
		UnattendedRequested: append([]string{}, ta.Spec.UnattendedWrites...),
		UnattendedGranted:   []string{},
		Conditions:          []metav1.Condition{},
	}
	if refs, serr := s.resolveTenantTenants(r.Context(), claims, tenant); serr == nil && len(refs) > 0 {
		item.Cluster = refs[0].Cluster
	}
	for _, c := range ta.Status.Conditions {
		if c.Status != metav1.ConditionTrue {
			item.Conditions = append(item.Conditions, c)
		}
		if c.Type == v1alpha1.TenantAgentCondPaused && c.Status == metav1.ConditionTrue {
			item.Paused = &pausedJSON{Reason: c.Reason, Message: c.Message}
		}
	}
	spend, err := s.tenantAgentSpendToday(r, tenant)
	if err != nil {
		return item, err
	}
	item.SpendToday = spend
	rt := s.deps.Assistant
	if rt == nil {
		return item, nil
	}
	settings, err := rt.Store.GetSettings(r.Context(), agent.ScopeGlobal)
	if err != nil {
		return item, err
	}
	if g, ok := settings.TenantAgentUnattended[ta.Namespace+"/"+ta.Name]; ok && g.UID == string(ta.UID) {
		item.UnattendedGranted = append([]string{}, g.Tools...)
	}
	page, err := rt.Store.ListSessions(r.Context(), store.SessionQuery{
		Kind: agent.KindTenantAgent, Principal: accept.Principal(ta), Limit: 200,
	})
	if err != nil {
		return item, err
	}
	today := utcDay(time.Now())
	for _, sess := range page.Items {
		if sess.StartedAt.Before(today) {
			continue
		}
		item.SessionsToday++
		if sess.State == agent.StateFailed {
			item.FailedToday++
		}
	}
	return item, nil
}

// handlePutAgentUnattended serves PUT /agents/{namespace}/{name}/unattended.
// The grant copies the object's unattendedWrites, pinned to its UID. A later
// spec edit does not extend it.
func (s *Server) handlePutAgentUnattended(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanGrantUnattended(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, assistantMaxBody)).Decode(&body); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "body must be {enabled}")
		return
	}
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	ta := s.findTenantAgent(ns, name)
	if ta == nil {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	if body.Enabled && len(ta.Spec.UnattendedWrites) == 0 {
		writeAssistantError(s, w, http.StatusConflict, tenantAgentNothingGrant, "the Tenant agent names no unattended writes")
		return
	}
	actor := ActorFrom(claims)
	now := time.Now().UTC()
	var grant *agent.UnattendedGrant
	if body.Enabled {
		grant = &agent.UnattendedGrant{
			UID: string(ta.UID), Tools: append([]string{}, ta.Spec.UnattendedWrites...),
			GrantedBy: actor, GrantedAt: now,
		}
	}
	key := ns + "/" + name
	if err := rt.Store.PutTenantAgentUnattended(r.Context(), key, grant, actor); err != nil {
		s.assistantStoreError(w, err)
		return
	}
	// The control notice carries the current kill switch state, so the runner
	// reloads the grant at once.
	if settings, err := rt.Store.GetSettings(r.Context(), agent.ScopeGlobal); err == nil {
		if err := publishControl(rt.Publish, settings.Paused, actor, now); err != nil {
			s.deps.Logger.Error("tenant agent control notice", "error", err)
		}
	}
	granted := []string{}
	if grant != nil {
		granted = grant.Tools
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"namespace": ns, "name": name, "enabled": body.Enabled, "unattendedGranted": granted,
	})
}

// findTenantAgent returns the cached TenantAgent named ns/name, or nil.
func (s *Server) findTenantAgent(ns, name string) *v1alpha1.TenantAgent {
	if s.deps.TenantAgents == nil {
		return nil
	}
	for _, ta := range s.deps.TenantAgents.List() {
		if ta.Namespace == ns && ta.Name == name {
			return &ta
		}
	}
	return nil
}
