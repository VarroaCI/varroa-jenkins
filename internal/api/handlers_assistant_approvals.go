package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// handleListAssistantApprovals serves GET /assistant/approvals. The caller
// sees the approvals of its own sessions. scope=all needs the admin verb.
func (s *Server) handleListAssistantApprovals(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	q := r.URL.Query()
	query := store.ApprovalQuery{
		Principal:  claims.Subject,
		State:      agent.ApprovalState(q.Get("state")),
		TargetKind: agent.TargetKind(q.Get("target_kind")),
		TargetRef:  q.Get("target_ref"),
		Cursor:     q.Get("cursor"),
	}
	if raw := q.Get("session"); raw != "" {
		id, ok := sessionUUID(raw)
		if !ok {
			s.writeJSONError(w, http.StatusBadRequest, "session must be an id")
			return
		}
		// A Team agent session names its approvals by session alone, so a
		// tenant reader sees them without being the Team agent's principal.
		sess, ok := s.sessionFor(w, r, rt, claims, id, sessionView)
		if !ok {
			return
		}
		query.SessionID = id
		if sess.Kind == agent.KindTeamAgent {
			query.Principal = ""
		}
	} else if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
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
	page, err := rt.Store.ListApprovals(r.Context(), query)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	items := make([]assistantApprovalJSON, 0, len(page.Items))
	for _, a := range page.Items {
		items = append(items, toAssistantApprovalJSON(a, nil))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": page.NextCursor})
}

// approvedToolTimeout bounds one approved tool call.
const approvedToolTimeout = 2 * time.Minute

// handleApproveAssistantApproval serves POST /assistant/approvals/{id}/approve.
func (s *Server) handleApproveAssistantApproval(w http.ResponseWriter, r *http.Request) {
	s.resolveAssistantApproval(w, r, true)
}

// handleDenyAssistantApproval serves POST /assistant/approvals/{id}/deny.
func (s *Server) handleDenyAssistantApproval(w http.ResponseWriter, r *http.Request) {
	s.resolveAssistantApproval(w, r, false)
}

// resolveAssistantApproval resolves one pending approval of the caller's own
// session. An approval runs the tool as the approver, under the session
// context. A deny writes an error result and runs nothing. Both resume the
// session once every approval of it has its result.
func (s *Server) resolveAssistantApproval(w http.ResponseWriter, r *http.Request, approve bool) {
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
		Reason string `json:"reason"`
	}
	if !approve {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	ctx := r.Context()
	ap, err := rt.Store.GetApproval(ctx, id)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	sess, err := rt.Store.GetSession(ctx, ap.SessionID)
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if !s.assistantGate(w, claims, sess) {
		return
	}
	if !s.sessionAllowed(ctx, claims, sess, sessionDecide) {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	global, err := rt.Store.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	if approve && global.Paused {
		writeAssistantError(s, w, http.StatusConflict, assistantErrKillSwitch, "the assistant is paused")
		return
	}

	actor := ActorFrom(claims)
	state := agent.ApprovalApproved
	reason := ""
	if !approve {
		state = agent.ApprovalDenied
		reason = body.Reason
	}
	evData := agent.ApprovalResolvedPayload{ApprovalID: id.String(), State: state, ResolvedBy: actor}
	resolvedEv, err := agent.NewEvent(agent.EventApprovalResolved, evData)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "encode approval failed")
		return
	}
	res, err := rt.Store.ResolveApproval(ctx, id, store.Resolution{
		State: state, By: actor, Reason: reason, Owner: sess.Principal,
		Events: []agent.EventInput{resolvedEv},
	})
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}

	content, isError := "", false
	if approve {
		// The write runs to completion even when the client goes away, so its
		// result is always recorded.
		runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), approvedToolTimeout)
		defer cancelRun()
		callCtx := agent.WithSessionContext(runCtx, agent.SessionContext{SessionID: sess.ID, Values: sess.Context})
		out, callErr := rt.hostForSession(claims, sess).Call(callCtx, ap.Tool, ap.Arguments)
		if callErr != nil {
			content, isError = "tool call failed", true
		} else {
			content, isError = out.Content, out.IsError
		}
	} else {
		content, isError = fmt.Sprintf("denied by %s: %s", actor, reason), true
	}
	content = loop.CutResult(content)
	resEv, err := agent.NewEvent(agent.EventToolResult, agent.ToolResultPayload{
		CallID: ap.CallID, Name: ap.Tool, Content: content, IsError: isError, DataLevel: sess.DataSharing,
	})
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "encode result failed")
		return
	}
	if _, err := rt.Store.AppendEvent(context.WithoutCancel(ctx), sess.ID, store.Guard{States: []agent.SessionState{agent.StateWaitingApproval}}, resEv); err != nil {
		s.assistantStoreError(w, err)
		return
	}

	// Every decision checks whether the session can resume. A sibling
	// approver may have written its result after this one counted pending
	// approvals, so the check does not rely on PendingLeft. A Team agent
	// session resumes through the runner's resume scan, not here.
	if sess.Kind != agent.KindTeamAgent {
		s.resumeIfComplete(r, rt, claims, sess)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"approval": toAssistantApprovalJSON(res.Approval, nil),
		"result":   map[string]any{"content": content, "isError": isError},
	})
}

// resumeIfComplete starts the session again when every approval of it has
// its result. A sibling approver may race on the same check, so TakeLease
// lets only one of them resume the session.
func (s *Server) resumeIfComplete(r *http.Request, rt *AssistantRuntime, claims *auth.Claims, sess agent.Session) {
	// The resume runs after the response path may have ended, so it is detached.
	ctx := context.WithoutCancel(r.Context())
	results, err := s.toolResults(ctx, rt, sess.ID)
	if err != nil {
		return
	}
	page, err := rt.Store.ListApprovals(ctx, store.ApprovalQuery{Principal: sess.Principal, SessionID: sess.ID, Limit: 200})
	if err != nil {
		return
	}
	for _, a := range page.Items {
		if a.State == agent.ApprovalPending {
			return
		}
		if _, ok := results[a.CallID]; !ok {
			return
		}
	}
	key, err := s.sessionKey(r, rt, sess)
	if err != nil {
		endEv, _ := agent.NewEvent(agent.EventError, agent.ErrorPayload{Code: agent.CodeSessionKeyMissing, Message: "the provider key is not available"})
		doneEv, _ := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateFailed})
		_, _ = rt.Store.EndSession(ctx, sess.ID, store.EndInput{
			From: []agent.SessionState{agent.StateWaitingApproval}, State: agent.StateFailed,
			Events: []agent.EventInput{endEv, doneEv},
		})
		return
	}
	taken, err := rt.Store.TakeLease(ctx, sess.ID, rt.Owner, []agent.SessionState{agent.StateWaitingApproval})
	if err != nil {
		return
	}
	rt.startRun(rt.RunConfig(claims, taken, key, assistantTenantLookup{s: s, claims: claims}), loop.TriggerResume)
}
