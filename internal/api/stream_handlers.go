package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// handleStreamTicket mints a short-lived, single-purpose SSE stream ticket bound to
// the authenticated caller and a requested stream scope. It sits behind AuthMiddleware,
// so the caller is already authenticated via header or cookie. The ticket lets an
// EventSource (which cannot set an Authorization header) open exactly one stream without
// putting a session token in the URL.
func (s *Server) handleStreamTicket(w http.ResponseWriter, r *http.Request) {
	if s.deps.TicketIssuer == nil {
		s.writeJSONError(w, http.StatusNotImplemented, "stream tickets not configured")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		var body struct {
			Scope string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		scope = body.Scope
	}
	if !validStreamScope(scope) {
		s.writeJSONError(w, http.StatusBadRequest, "invalid scope")
		return
	}

	// Mint-time per-scope authorization: fail fast before issuing a ticket
	// that the endpoint-side gate would reject at connect time.
	if s.deps.Authorizer == nil {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	switch {
	case scope == "brood":
		if !s.deps.Authorizer.CanReadGlobalActivity(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
	case scope == "activity":
		// No additional gate here: /activity/stream itself accepts any
		// authenticated caller and enforces visibility per-event via
		// CanReadActivityEvent. Narrowing the mint would block
		// namespace-scoped callers from a stream they are legitimately
		// allowed to open a (filtered) view of.
	case strings.HasPrefix(scope, "controller:"):
		parts := strings.Split(strings.TrimPrefix(scope, "controller:"), "/")
		// len==3 guaranteed by validStreamScope; parts = [cluster, ns, name].
		// cluster is not passed — CanReadController is cluster-agnostic.
		if !s.deps.Authorizer.CanReadController(claims, parts[1], parts[2]) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
	case strings.HasPrefix(scope, "broodop:"):
		ns, _, _ := strings.Cut(strings.TrimPrefix(scope, "broodop:"), "/")
		// Mirrors handleBroodStreamWithPoll's own gate exactly:
		// namespace-level CanReadController with an empty name, since a
		// BroodOperation name is not a Controller name.
		if !s.deps.Authorizer.CanReadController(claims, ns, "") {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
	}

	ticket, ttl, err := s.deps.TicketIssuer.Mint(claims, scope)
	if err != nil {
		s.deps.Logger.Error("mint stream ticket failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to mint ticket")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ticket":           ticket,
		"expiresInSeconds": ttl,
	})
}

// validStreamScope accepts the canonical stream scopes: "brood", "activity",
// "controller:{cluster}/{ns}/{name}", or "broodop:{ns}/{name}". It mirrors sseScope in the auth
// middleware. Three-token form only; two-token form is rejected.
func validStreamScope(scope string) bool {
	switch scope {
	case "brood", "activity":
		return true
	}
	if rest, ok := strings.CutPrefix(scope, "controller:"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) != 3 {
			return false // two-token form not recognized
		}
		return parts[0] != "" && parts[1] != "" && parts[2] != ""
	}
	if rest, ok := strings.CutPrefix(scope, "broodop:"); ok {
		ns, name, found := strings.Cut(rest, "/")
		return found && ns != "" && name != "" && !strings.Contains(name, "/")
	}
	return false
}
