package api

import (
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// HandleIdentitySettings returns the identity/OIDC configuration for admins.
// The OIDC client secret is never included in the response.
func (s *Server) HandleIdentitySettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Identity & OIDC section is gated by admin access.
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	cfg := s.deps.IdentityConfig
	resp := map[string]interface{}{
		"mode":         cfg.Mode,
		"cookieDomain": cfg.CookieDomain,
		"defaultRead":  cfg.DefaultRead,
	}

	if cfg.Mode == string(auth.AuthModeOIDC) {
		resp["issuer"] = cfg.Issuer
		resp["clientId"] = cfg.ClientID
		resp["scopes"] = cfg.Scopes
		// clientSecret is intentionally never included
	}

	s.writeJSON(w, http.StatusOK, resp)
}
