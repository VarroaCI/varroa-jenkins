package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// sessionVerb names what a route does with one session. The access table of
// design section 12.2 keys its rules on it.
type sessionVerb int

const (
	// sessionView reads a session, its events, or its approvals.
	sessionView sessionVerb = iota
	// sessionDecide approves or denies an approval.
	sessionDecide
	// sessionCancel ends a session.
	sessionCancel
)

// assistantGate refuses an assistant session to a caller without
// assistant:use. A Team agent session needs no assistant grant, because its
// tenant table decides. It writes the refusal and returns false.
func (s *Server) assistantGate(w http.ResponseWriter, claims *auth.Claims, sess agent.Session) bool {
	if sess.Kind == agent.KindTeamAgent || s.assistantCanUse(claims) {
		return true
	}
	s.writeJSONError(w, http.StatusForbidden, "forbidden")
	return false
}

// sessionAllowed applies the rule of verb to sess. An assistant session keeps
// the A1 owner rules. A Team agent session follows the tenant table: a
// reader views it, a writer decides or cancels it, and an admin views and
// cancels every session.
func (s *Server) sessionAllowed(ctx context.Context, claims *auth.Claims, sess agent.Session, verb sessionVerb) bool {
	if sess.Kind == agent.KindTeamAgent {
		return s.teamAgentSessionAllowed(ctx, claims, sess, verb)
	}
	if verb == sessionDecide {
		return sess.Principal == claims.Subject
	}
	return sess.Principal == claims.Subject || s.assistantIsAdmin(claims)
}

// teamAgentSessionAllowed applies the Team agent rows of the access table.
// A Team agent subject never decides an approval, so an agent cannot approve
// its own write.
func (s *Server) teamAgentSessionAllowed(ctx context.Context, claims *auth.Claims, sess agent.Session, verb sessionVerb) bool {
	if verb == sessionDecide && accept.IsPrincipal(claims.Subject) {
		return false
	}
	if verb != sessionDecide && s.assistantIsAdmin(claims) {
		return true
	}
	return s.teamAgentTenantAllows(ctx, claims, sess, verb)
}

// teamAgentTenantAllows applies the tenant rows of the access table to a Team
// agent session. A view needs a tenant read grant, and any other verb needs a
// tenant write grant on one of the Team's tenants.
func (s *Server) teamAgentTenantAllows(ctx context.Context, claims *auth.Claims, sess agent.Session, verb sessionVerb) bool {
	refs, serr := s.resolveTeamTenants(ctx, claims, sess.Team)
	if serr != nil {
		return false
	}
	for _, ref := range refs {
		if verb == sessionView && s.canReadPipeline(claims, ref, "") {
			return true
		}
		if verb != sessionView && s.canWritePipeline(claims, ref, "") {
			return true
		}
	}
	return false
}

// transcriptFull reports whether claims reads tool result content of sess.
// A tool result holds data that the owner's grants fetched, so only the
// principal, or a tenant reader of a Team agent session, reads it.
func (s *Server) transcriptFull(ctx context.Context, claims *auth.Claims, sess agent.Session) bool {
	if claims == nil {
		return false
	}
	if sess.Principal == claims.Subject {
		return true
	}
	if sess.Kind == agent.KindTeamAgent {
		return s.teamAgentTenantAllows(ctx, claims, sess, sessionView)
	}
	return false
}

// sessionFor loads the session id and applies the rule of verb. A refusal
// answers 404, as the A1 visibility rule does. The assistant gate answers
// 403. It writes the refusal and returns false.
func (s *Server) sessionFor(w http.ResponseWriter, r *http.Request, rt *AssistantRuntime, claims *auth.Claims, id uuid.UUID, verb sessionVerb) (agent.Session, bool) {
	sess, err := rt.Store.GetSession(r.Context(), id)
	if err != nil {
		s.assistantStoreError(w, err)
		return agent.Session{}, false
	}
	if !s.assistantGate(w, claims, sess) {
		return agent.Session{}, false
	}
	if !s.sessionAllowed(r.Context(), claims, sess, verb) {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return agent.Session{}, false
	}
	return sess, true
}

// hostForSession returns the tool host that acts as claims for sess. A Team
// agent approval runs with the team_agent origin, so its activity is stamped
// as the Team agent's.
func (rt *AssistantRuntime) hostForSession(claims *auth.Claims, sess agent.Session) loop.ToolHost {
	if sess.Kind == agent.KindTeamAgent && rt.HostForOrigin != nil {
		return rt.HostForOrigin(claims, agent.ActivitySourceTeamAgent)
	}
	return rt.HostFor(claims)
}
