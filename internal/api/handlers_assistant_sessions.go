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

// startAssistantSession creates a session for the caller. It answers a
// *assistantFail for every refusal. A skipped session is stored and not run.
// A running session starts its loop after its creation event.
func (s *Server) startAssistantSession(ctx context.Context, rt *AssistantRuntime, claims *auth.Claims, req assistantStartRequest, sealed string) (agent.Session, error) {
	sub := claims.Subject
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

	team, cluster, serr := s.assistantContext(ctx, claims, values)
	if serr != nil {
		return agent.Session{}, serr
	}

	global, err := rt.Store.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		return agent.Session{}, err
	}
	name, model, entry, err := rt.chooseProvider(req, user, global)
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

	userLevel := user.DataSharing
	if !userLevel.Valid() {
		userLevel = agent.DataLogs
	}
	var trust agent.ProviderTrust
	if entry != nil {
		trust = entry.Endpoint.Trust
	}
	level := datashare.Effective(datashare.Inputs{
		User: userLevel, Team: team, TenantLevels: global.TenantDataSharing,
		Trust: trust, ExternalCeiling: rt.Config.ExternalDataSharing,
	})

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
		ID: id, Kind: agent.KindAssistant, Principal: sub, Team: team, Cluster: cluster,
		Trigger: agent.TriggerManual, Context: values, Provider: name, Model: model,
		KeySource: keySource, DataSharing: level, StartedAt: rt.now(),
	}
	userEv, err := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: first})
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

// assistantContext resolves the team and the cluster of a session from its
// run, team, or job context. A run the caller cannot read answers 404 with no
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
	if team := str(agent.CtxTeam); team != "" {
		refs, serr := s.resolveTeamTenants(ctx, claims, team)
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
		return "", "", failWith(http.StatusBadRequest, "", "a job context needs a team or a run")
	}
	return "", rt.Cluster, nil
}

// chooseProvider picks the provider and model of a new session. A provider
// named in the request must exist. Otherwise the user setting, the global
// default, then the registry default apply, and the entry may be nil.
func (rt *AssistantRuntime) chooseProvider(req assistantStartRequest, user, global agent.Settings) (string, string, *provider.Entry, error) {
	if rt.Registry == nil {
		return "", "", nil, nil
	}
	var entry provider.Entry
	var ok bool
	switch {
	case req.Provider != "":
		entry, ok = rt.Registry.Get(req.Provider)
		if !ok {
			return "", "", nil, failWith(http.StatusBadRequest, "", "unknown provider")
		}
	case user.Provider != "":
		entry, ok = rt.Registry.Get(user.Provider)
	case global.DefaultProvider != "":
		entry, ok = rt.Registry.Get(global.DefaultProvider)
	}
	if !ok {
		entry, ok = rt.Registry.Default()
		if !ok {
			return "", "", nil, nil
		}
	}
	name := entry.Endpoint.Name
	model := req.Model
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
		Team:      q.Get("team"),
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

// visibleSession loads a session the caller may read: its owner, or an admin.
// Any other caller gets not found.
func (s *Server) visibleSession(w http.ResponseWriter, r *http.Request, rt *AssistantRuntime, claims *auth.Claims) (agent.Session, bool) {
	id, ok := sessionUUID(r.PathValue("id"))
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return agent.Session{}, false
	}
	sess, err := rt.Store.GetSession(r.Context(), id)
	if err != nil {
		s.assistantStoreError(w, err)
		return agent.Session{}, false
	}
	if sess.Principal != claims.Subject && !s.assistantIsAdmin(claims) {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return agent.Session{}, false
	}
	return sess, true
}

// handleGetAssistantSession serves GET /assistant/sessions/{id}.
func (s *Server) handleGetAssistantSession(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	sess, ok := s.visibleSession(w, r, rt, claims)
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
	approvals := make([]assistantApprovalJSON, 0, len(page.Items))
	for _, a := range page.Items {
		var result *string
		if c, ok := results[a.CallID]; ok {
			result = &c
		}
		approvals = append(approvals, toAssistantApprovalJSON(a, result))
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
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
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
	sess, err := rt.Store.GetSession(r.Context(), id)
	if err != nil || sess.Principal != claims.Subject {
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
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	sess, ok := s.visibleSession(w, r, rt, claims)
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
		State:  agent.StateCancelled,
		Events: []agent.EventInput{done},
	}); err != nil {
		s.assistantStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
