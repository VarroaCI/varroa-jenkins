package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/identity"
	"github.com/varroaci/varroa-jenkins/internal/auth/ldap"
	"github.com/varroaci/varroa-jenkins/internal/auth/local"
)

// loginRequest is the JSON body for POST /api/v1/login.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse is the JSON body returned on successful login.
type loginResponse struct {
	IDToken   string `json:"id_token"`
	ExpiresIn int    `json:"expires_in"`
}

// changePasswordRequest is the JSON body for PUT /api/v1/me/password.
type changePasswordRequest struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

// setPasswordRequest is the JSON body for PUT /api/v1/users/{name}/password.
type setPasswordRequest struct {
	NewPassword string `json:"newPassword"`
}

// authConfigResponse is the JSON body of GET /api/v1/auth-config.
type authConfigResponse struct {
	Mode          string `json:"mode"`
	LocalAccounts bool   `json:"localAccounts"`
}

// HandleAuthConfig returns the configured auth mode and whether declared
// local accounts sign in beside OIDC.
// GET /api/v1/auth-config (unauthenticated).
func (s *Server) HandleAuthConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	mode := "oidc"
	if s.deps.Auth != nil {
		mode = string(s.deps.Auth.Mode())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(authConfigResponse{
		Mode:          mode,
		LocalAccounts: s.deps.IdentityConfig.LocalAccounts,
	})
}

// localAccountsOn reports whether local accounts exist on this release:
// local mode, or local accounts beside OIDC.
func (s *Server) localAccountsOn() bool {
	return s.deps.IdentityConfig.Mode == string(auth.AuthModeLocal) || s.deps.IdentityConfig.LocalAccounts
}

// auditLocalLogin publishes the audit event for a local sign-in beside
// OIDC. A break-glass account is audited at warning severity. A failed
// attempt is audited only for a declared account, so the event never
// confirms that an unknown name exists.
func (s *Server) auditLocalLogin(username string, purpose v1alpha1.UserPurpose, failed bool) {
	if !s.deps.IdentityConfig.LocalAccounts || purpose == "" {
		return
	}
	actor := auth.LocalSubjectPrefix + username
	e := activity.Event{
		Type:     "auth.local.login",
		Source:   "user",
		Severity: "info",
		Actor:    actor,
		Reason:   string(purpose),
		Message:  fmt.Sprintf("%s signed in with a local %s account", actor, purpose),
	}
	if purpose == v1alpha1.UserPurposeBreakGlass {
		e.Severity = "warning"
	}
	if failed {
		e.Type = "auth.local.login_failed"
		e.Severity = "warning"
		e.Message = fmt.Sprintf("failed sign-in for the local %s account %s", purpose, actor)
	}
	s.notifyActivity(e)
}

// HandleLogin authenticates a user and returns an id_token.
// POST /api/v1/login (unauthenticated, local auth only).
func (s *Server) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if s.deps.Local == nil && s.deps.LDAP == nil {
		s.writeJSONError(w, http.StatusNotImplemented, "not implemented")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Username == "" || req.Password == "" {
		s.writeJSONError(w, http.StatusBadRequest, "username and password required")
		return
	}

	var tok string
	var expiresIn int
	var err error

	if s.deps.LDAP != nil {
		tok, expiresIn, err = s.deps.LDAP.Login(r.Context(), req.Username, req.Password)
		if err != nil {
			status, msg := http.StatusUnauthorized, "invalid credentials"
			if errors.Is(err, ldap.ErrRateLimited) {
				status, msg = http.StatusTooManyRequests, "too many attempts"
			}
			s.writeJSONError(w, status, msg)
			return
		}
	} else {
		res, lerr := s.deps.Local.Login(r.Context(), req.Username, req.Password)
		if lerr != nil {
			status := http.StatusUnauthorized
			if errors.Is(lerr, local.ErrRateLimited) {
				status = http.StatusTooManyRequests
			}
			s.auditLocalLogin(req.Username, res.Purpose, true)
			s.writeJSONError(w, status, lerr.Error())
			return
		}
		s.auditLocalLogin(req.Username, res.Purpose, false)
		tok, expiresIn = res.IDToken, res.ExpiresIn
	}

	// Set httpOnly cookie for browser access.
	cookieDomain := ""
	if s.deps.LDAP != nil {
		cookieDomain = s.deps.LDAP.CookieDomain()
	} else {
		cookieDomain = s.deps.Local.CookieDomain()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "varroa_token",
		Value:    tok,
		Domain:   cookieDomain,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   expiresIn,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(loginResponse{
		IDToken:   tok,
		ExpiresIn: expiresIn,
	})
}

// HandleChangePassword lets the current user change their own password.
// PUT /api/v1/me/password (authenticated, local auth only).
func (s *Server) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if s.deps.Local == nil {
		s.writeJSONError(w, http.StatusNotImplemented, "not implemented")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.NewPassword) < 8 {
		s.writeJSONError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}

	username := identity.UserResourceName(claims, auth.AuthMode(s.deps.IdentityConfig.Mode))
	if err := s.deps.Local.ChangePassword(r.Context(), username, req.OldPassword, req.NewPassword); err != nil {
		s.writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	s.notifyPasswordSet(claims, username)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleUsersDispatch routes to specific user handlers based on the URL path.
// Supported: PUT /api/v1/users/{name}/password (admin only, local auth only).
// GET /api/v1/users/{name}/apikeys (admin only, list user's keys).
// DELETE /api/v1/users/{name}/apikeys/{prefix} (admin only, revoke user's key).
// DELETE /api/v1/users/{name} (admin, both modes) — handled by HandleDeleteUser.
func (s *Server) handleUsersDispatch(w http.ResponseWriter, r *http.Request) {
	// Path: /users/{name}[/{resource}]
	path := strings.TrimPrefix(r.URL.Path, "/users/")
	parts := strings.Split(path, "/")

	// DELETE /users/{name} — deprovision and delete the user.
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete {
		s.HandleDeleteUser(w, r)
		return
	}

	// PUT /users/{name} — update identity (email, display name).
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodPut {
		s.HandleUpdateUser(w, r, parts[0])
		return
	}

	if len(parts) == 2 && parts[1] == "password" && r.Method == http.MethodPut {
		s.HandleAdminSetPassword(w, r, parts[0])
		return
	}

	if len(parts) >= 2 && parts[1] == "apikeys" {
		s.handleUsersApiKeysDispatch(w, r)
		return
	}

	s.writeJSONError(w, http.StatusNotFound, "not found")
}

// HandleAdminSetPassword lets an admin set a user's password.
// PUT /api/v1/users/{name}/password (authenticated + admin, local auth only).
func (s *Server) HandleAdminSetPassword(w http.ResponseWriter, r *http.Request, username string) {
	if s.deps.Local == nil {
		s.writeJSONError(w, http.StatusNotImplemented, "not implemented")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	// Admin-only: gated on the varroa:admin capability (wildcard */*).
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req setPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.NewPassword) < 8 {
		s.writeJSONError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}

	// Beside OIDC, an IdP user can never sign in locally, so a password on it
	// has no use.
	if s.deps.IdentityConfig.LocalAccounts && identity.IsOIDCName(username) {
		s.writeJSONError(w, http.StatusBadRequest, "only a local account can have a password")
		return
	}

	if err := s.deps.Local.SetPassword(r.Context(), username, req.NewPassword); err != nil {
		s.deps.Logger.Error("failed to set password", "user", username, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	s.notifyPasswordSet(claims, username)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// notifyPasswordSet publishes the user.password_set event for a password
// set on a local account. The handler does not read the account's purpose,
// so the event keeps the info severity.
func (s *Server) notifyPasswordSet(claims *auth.Claims, username string) {
	e := UserPasswordSetEvent(s.deps.OperatorNamespace, username)
	e.Source = "user"
	e.Actor = ActorFrom(claims)
	e.Severity = "info"
	s.notifyActivity(e)
}
