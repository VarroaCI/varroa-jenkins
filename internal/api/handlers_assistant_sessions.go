package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/prompts"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// assistantStartRequest is the body of POST /assistant/sessions.
type assistantStartRequest struct {
	Context  json.RawMessage `json:"context"`
	Message  string          `json:"message"`
	Prompt   string          `json:"prompt"`
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
}

// assistantFail is a refusal of a start request, with its HTTP status and code.
type assistantFail struct {
	status int
	code   string
	msg    string
}

func (f *assistantFail) Error() string { return f.msg }

func failWith(status int, code, msg string) error {
	return &assistantFail{status: status, code: code, msg: msg}
}

// assistantIAT is the login session time that binds a sealed user key.
func assistantIAT(claims *auth.Claims) int64 {
	if claims == nil || claims.IssuedAt.IsZero() {
		return 0
	}
	return claims.IssuedAt.Unix()
}

// assistantCanUse reports whether the caller may use the assistant.
func (s *Server) assistantCanUse(claims *auth.Claims) bool {
	return s.deps.Authorizer != nil && s.deps.Authorizer.CanUseAssistant(claims)
}

// assistantIsAdmin reports whether the caller may read every session and
// change the assistant's global settings.
func (s *Server) assistantIsAdmin(claims *auth.Claims) bool {
	return s.deps.Authorizer != nil && s.deps.Authorizer.CanAdminAssistant(claims)
}

// sessionStart is a validated session start: the context values, the first
// user text, and the requested provider and model.
type sessionStart struct {
	Values   map[string]any
	First    string
	Provider string
	Model    string
}

// startAssistantSession creates a session for the caller. It answers a
// *assistantFail for every refusal. A skipped session is stored and not run.
// A running session starts its loop after its creation event.
func (s *Server) startAssistantSession(ctx context.Context, rt *AssistantRuntime, claims *auth.Claims, req assistantStartRequest, sealed string) (agent.Session, error) {
	if (req.Message == "") == (req.Prompt == "") {
		return agent.Session{}, failWith(http.StatusBadRequest, "", "send either a message or a prompt")
	}
	values, err := agent.ParseClientContext(req.Context)
	if err != nil {
		return agent.Session{}, failWith(http.StatusBadRequest, "", err.Error())
	}
	first := req.Message
	if req.Prompt != "" {
		p, ok := prompts.Get(req.Prompt)
		if !ok {
			return agent.Session{}, failWith(http.StatusBadRequest, "", "unknown prompt")
		}
		if !p.Startable {
			return agent.Session{}, failWith(http.StatusForbidden, assistantErrPromptNoStart, "this prompt cannot start a session")
		}
		first = p.Text
		values[agent.CtxPrompt] = p.ID
	}
	return s.createAssistantSession(ctx, rt, claims, sessionStart{Values: values, First: first, Provider: req.Provider, Model: req.Model}, sealed)
}

// createAssistantSession applies the start rules to a validated start: the
// user switch, the busy cap, the context, the provider, the data level, and
// the skip decision. It stores the session and starts its loop.
func (s *Server) createAssistantSession(ctx context.Context, rt *AssistantRuntime, claims *auth.Claims, st sessionStart, sealed string) (agent.Session, error) {
	sub := claims.Subject
	user, err := rt.Store.GetSettings(ctx, agent.UserScope(sub))
	if err != nil {
		return agent.Session{}, err
	}
	if !user.Enabled {
		return agent.Session{}, failWith(http.StatusConflict, assistantErrOff, "the assistant is off for this user")
	}
	running, err := rt.Store.CountRunning(ctx, sub)
	if err != nil {
		return agent.Session{}, err
	}
	if running >= agent.MaxRunningSessions {
		return agent.Session{}, failWith(http.StatusTooManyRequests, assistantErrBusy, "too many sessions are running")
	}

	tenant, cluster, serr := s.assistantContext(ctx, claims, st.Values)
	if serr != nil {
		return agent.Session{}, serr
	}

	global, err := rt.Store.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		return agent.Session{}, err
	}
	name, model, entry, err := rt.chooseProvider(st.Provider, st.Model, user, global)
	if err != nil {
		return agent.Session{}, err
	}

	var key provider.Key
	keySource := agent.KeySourceChart
	available := false
	if entry != nil {
		if sealed != "" && rt.Sealer != nil {
			if pn, k, ok := rt.Sealer.Open(sealed, sub, assistantIAT(claims)); ok && pn == name {
				key, keySource, available = k, agent.KeySourceUser, true
			}
		}
		if !available && rt.Registry.Available(name) {
			if k, err := rt.Registry.LoadKey(name); err == nil {
				key, available = k, true
			}
		}
	}

	level := rt.sessionLevel(user, global, tenant, entry)

	day, err := rt.Store.DaySpend(ctx, sub)
	if err != nil {
		return agent.Session{}, err
	}
	skip, start := agent.DecideCreate(agent.CreateInput{
		Paused: global.Paused, DayTotal: day, DayLimit: rt.Config.Budgets.UserDay,
		DataLevel: level, ProviderAvailable: available,
	})

	id, err := agent.NewID()
	if err != nil {
		return agent.Session{}, err
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindAssistant, Principal: sub, Tenant: tenant, Cluster: cluster,
		Trigger: agent.TriggerManual, Context: st.Values, Provider: name, Model: model,
		KeySource: keySource, DataSharing: level, StartedAt: rt.now(),
	}
	userEv, err := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: st.First})
	if err != nil {
		return agent.Session{}, err
	}
	if !start {
		sess.State = agent.StateSkipped
		sess.SkipReason = skip
		done, err := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateSkipped})
		if err != nil {
			return agent.Session{}, err
		}
		created, _, err := rt.Store.CreateSession(ctx, sess, []agent.EventInput{userEv, done})
		return created, err
	}

	sess.State = agent.StateRunning
	sess.LeaseOwner = rt.Owner
	created, _, err := rt.Store.CreateSession(ctx, sess, []agent.EventInput{userEv})
	if err != nil {
		return agent.Session{}, err
	}
	rt.startRun(rt.RunConfig(claims, created, key, assistantTenantLookup{s: s, claims: claims}), loop.TriggerStart)
	return created, nil
}

// sessionLevel is the effective data level of a session of the user, for the
// tenant and the provider entry (nil when no provider is configured). The
// route and the session start share this rule.
func (rt *AssistantRuntime) sessionLevel(user, global agent.Settings, tenant string, entry *provider.Entry) agent.DataSharing {
	userLevel := user.DataSharing
	if !userLevel.Valid() {
		userLevel = agent.DataLogs
	}
	var trust agent.ProviderTrust
	if entry != nil {
		trust = entry.Endpoint.Trust
	}
	return datashare.Effective(datashare.Inputs{
		User: userLevel, Tenant: tenant, TenantLevels: global.TenantDataSharing,
		Trust: trust, ExternalCeiling: rt.Config.ExternalDataSharing,
	})
}

// assistantContext resolves the tenant and the cluster of a session from its
// run, tenant, or job context. A run the caller cannot read answers 404 with no
// detail, so the answer does not reveal the run.
func (s *Server) assistantContext(ctx context.Context, claims *auth.Claims, values map[string]any) (string, string, error) {
	rt := s.deps.Assistant
	str := func(k string) string {
		v, _ := values[k].(string)
		return v
	}
	if runID := str(agent.CtxRun); runID != "" {
		if s.deps.RecordStore == nil {
			return "", "", failWith(http.StatusServiceUnavailable, assistantErrDisabled, "the record store is not configured")
		}
		run, _, err := s.deps.RecordStore.GetRun(ctx, runID)
		if err != nil || !s.canReadRun(claims, run.Tenant, run.JobPath) {
			return "", "", failWith(http.StatusNotFound, "", "not found")
		}
		return run.Tenant, run.Cluster, nil
	}
	if tenant := str(agent.CtxTenant); tenant != "" {
		refs, serr := s.resolveTenantTenants(ctx, claims, tenant)
		if serr != nil {
			return "", "", failWith(serr.Status, "", serr.Message)
		}
		var ref TenantRef
		ok := false
		if job := str(agent.CtxJob); job != "" {
			ref, ok = s.tenantForJob(claims, refs, job)
		} else if len(refs) > 0 {
			ref, ok = refs[0], true
		}
		if !ok {
			return "", "", failWith(http.StatusNotFound, "", "not found")
		}
		return ref.Tenant, ref.Cluster, nil
	}
	if str(agent.CtxJob) != "" {
		return "", "", failWith(http.StatusBadRequest, "", "a job context needs a tenant or a run")
	}
	return "", rt.Cluster, nil
}

// chooseProvider picks the provider and model of a new session. A provider
// named in the request must exist. Otherwise the first known of the user
// setting, the global default, and the registry default applies. The entry
// may be nil.
func (rt *AssistantRuntime) chooseProvider(reqProvider, reqModel string, user, global agent.Settings) (string, string, *provider.Entry, error) {
	if rt.Registry == nil {
		return "", "", nil, nil
	}
	var entry provider.Entry
	var ok bool
	if reqProvider != "" {
		entry, ok = rt.Registry.Get(reqProvider)
		if !ok {
			return "", "", nil, failWith(http.StatusBadRequest, "", "unknown provider")
		}
	}
	if !ok && user.Provider != "" {
		entry, ok = rt.Registry.Get(user.Provider)
	}
	if !ok && global.DefaultProvider != "" {
		entry, ok = rt.Registry.Get(global.DefaultProvider)
	}
	if !ok {
		entry, ok = rt.Registry.Default()
		if !ok {
			return "", "", nil, nil
		}
	}
	name := entry.Endpoint.Name
	model := reqModel
	if model == "" {
		switch {
		case user.Provider == name && user.Model != "":
			model = user.Model
		case global.DefaultProvider == name && global.DefaultModel != "":
			model = global.DefaultModel
		default:
			model = entry.Endpoint.DefaultModel()
		}
	}
	if !assistantContains(entry.Endpoint.Models, model) {
		return "", "", nil, failWith(http.StatusBadRequest, "", "unknown model for this provider")
	}
	return name, model, &entry, nil
}

func assistantContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// handleCreateAssistantSession serves POST /assistant/sessions.
func (s *Server) handleCreateAssistantSession(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req assistantStartRequest
	r.Body = http.MaxBytesReader(w, r.Body, assistantMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sealed := ""
	if c, err := r.Cookie(AssistantKeyCookie); err == nil {
		sealed = c.Value
	}
	sess, err := s.startAssistantSession(r.Context(), rt, claims, req, sealed)
	if err != nil {
		s.writeAssistantStartError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, toAssistantSessionJSON(sess))
}

// writeAssistantStartError writes a refusal of a start request.
func (s *Server) writeAssistantStartError(w http.ResponseWriter, err error) {
	if f, ok := err.(*assistantFail); ok {
		if f.code != "" {
			writeAssistantError(s, w, f.status, f.code, f.msg)
			return
		}
		s.writeJSONError(w, f.status, f.msg)
		return
	}
	s.assistantStoreError(w, err)
}

// handleListAssistantSessions serves GET /assistant/sessions.
func (s *Server) handleListAssistantSessions(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	q := r.URL.Query()
	query := store.SessionQuery{
		Principal: claims.Subject,
		Kind:      agent.SessionKind(q.Get("kind")),
		Prompt:    q.Get("prompt"),
		Tenant:    q.Get("tenant"),
		State:     agent.SessionState(q.Get("state")),
		Cursor:    q.Get("cursor"),
	}
	if q.Get("scope") == "all" {
		if !s.assistantIsAdmin(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		query.Principal = ""
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
	items := make([]assistantSessionJSON, 0, len(page.Items))
	for _, sess := range page.Items {
		items = append(items, toAssistantSessionJSON(sess))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": page.NextCursor})
}

// visibleSession loads the session in the path and applies the rule of verb.
// A refusal answers 404, and an assistant session without assistant:use
// answers 403.
func (s *Server) visibleSession(w http.ResponseWriter, r *http.Request, rt *AssistantRuntime, claims *auth.Claims, verb sessionVerb) (agent.Session, bool) {
	id, ok := sessionUUID(r.PathValue("id"))
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return agent.Session{}, false
	}
	return s.sessionFor(w, r, rt, claims, id, verb)
}

// handleGetAssistantSession serves GET /assistant/sessions/{id}.
func (s *Server) handleGetAssistantSession(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	sess, ok := s.visibleSession(w, r, rt, claims, sessionView)
	if !ok {
		return
	}
	page, err := rt.Store.ListApprovals(r.Context(), store.ApprovalQuery{Principal: sess.Principal, SessionID: sess.ID, Limit: 200})
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	results, err := s.toolResults(r.Context(), rt, sess.ID)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	full := s.transcriptFull(r.Context(), claims, sess)
	approvals := make([]assistantApprovalJSON, 0, len(page.Items))
	for _, a := range page.Items {
		var result *string
		withheld := false
		if c, ok := results[a.CallID]; ok {
			if full {
				result = &c
			} else {
				withheld = true
			}
		}
		item := toAssistantApprovalJSON(a, result)
		item.ResultWithheld = withheld
		approvals = append(approvals, item)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"session":   toAssistantSessionJSON(sess),
		"approvals": approvals,
	})
}

// toolResults maps each tool call id of a session to its result content.
func (s *Server) toolResults(ctx context.Context, rt *AssistantRuntime, id uuid.UUID) (map[string]string, error) {
	out := map[string]string{}
	after := 0
	for {
		page, err := rt.Store.Events(ctx, id, after, 500)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return out, nil
		}
		for _, e := range page {
			after = e.Seq
			if e.Type != agent.EventToolResult {
				continue
			}
			var p agent.ToolResultPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				out[p.CallID] = p.Content
			}
		}
	}
}

// handlePostAssistantMessage serves POST /assistant/sessions/{id}/messages.
func (s *Server) handlePostAssistantMessage(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	id, ok := sessionUUID(r.PathValue("id"))
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, assistantMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		s.writeJSONError(w, http.StatusBadRequest, "message is required")
		return
	}
	sess, ok := s.sessionFor(w, r, rt, claims, id, sessionView)
	if !ok {
		return
	}
	if sess.Kind == agent.KindTenantAgent {
		writeAssistantError(s, w, http.StatusConflict, assistantErrTenantAgent, "a Tenant agent session takes no messages")
		return
	}
	if sess.Principal != claims.Subject {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if sess.State != agent.StateCompleted {
		writeAssistantError(s, w, http.StatusConflict, assistantErrBusy, "the session is not waiting for a message")
		return
	}
	if sess.Tokens() >= rt.Config.Budgets.Session {
		writeAssistantError(s, w, http.StatusConflict, assistantErrBusy, "the session token budget is spent")
		return
	}
	key, serr := s.sessionKey(r, rt, sess)
	if serr != nil {
		writeAssistantError(s, w, http.StatusConflict, assistantErrKeyUnavail, "the provider key is not available")
		return
	}
	if _, err := rt.Store.TakeLease(r.Context(), id, rt.Owner, []agent.SessionState{agent.StateCompleted}); err != nil {
		s.assistantStoreError(w, err)
		return
	}
	userEv, err := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: body.Message})
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "encode message failed")
		return
	}
	if _, err := rt.Store.AppendEvent(r.Context(), id, store.Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: rt.Owner}, userEv); err != nil {
		s.assistantStoreError(w, err)
		return
	}
	sess.State = agent.StateRunning
	sess.LeaseOwner = rt.Owner
	rt.startRun(rt.RunConfig(claims, sess, key, assistantTenantLookup{s: s, claims: claims}), loop.TriggerMessage)
	s.writeJSON(w, http.StatusAccepted, map[string]string{"state": string(agent.StateRunning)})
}

// sessionKey returns the provider key a session needs. A user-key session
// reads its sealed cookie, and a chart-key session reads the key file.
func (s *Server) sessionKey(r *http.Request, rt *AssistantRuntime, sess agent.Session) (provider.Key, error) {
	claims := auth.ClaimsFromContext(r.Context())
	if sess.KeySource == agent.KeySourceUser {
		c, err := r.Cookie(AssistantKeyCookie)
		if err != nil || rt.Sealer == nil {
			return provider.Key{}, fmt.Errorf("user key missing")
		}
		name, k, ok := rt.Sealer.Open(c.Value, claims.Subject, assistantIAT(claims))
		if !ok || name != sess.Provider {
			return provider.Key{}, fmt.Errorf("user key does not open")
		}
		return k, nil
	}
	return rt.Registry.LoadKey(sess.Provider)
}

// handleCancelAssistantSession serves POST /assistant/sessions/{id}/cancel.
func (s *Server) handleCancelAssistantSession(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	sess, ok := s.visibleSession(w, r, rt, claims, sessionCancel)
	if !ok {
		return
	}
	done, err := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateCancelled})
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "encode cancel failed")
		return
	}
	if _, err := rt.Store.EndSession(r.Context(), sess.ID, store.EndInput{
		From:   []agent.SessionState{agent.StateRunning, agent.StateWaitingApproval},
		By:     ActorFrom(claims),
		State:  agent.StateCancelled,
		Events: []agent.EventInput{done},
	}); err != nil {
		s.assistantStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
