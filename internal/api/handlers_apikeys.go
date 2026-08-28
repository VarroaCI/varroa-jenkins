package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/apikey"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/identity"
)

// handleMeApiKeys dispatches to the appropriate handler based on method.
func (s *Server) handleMeApiKeys(w http.ResponseWriter, r *http.Request) {
	if s.deps.KeyVerifier == nil {
		s.writeJSONError(w, http.StatusNotFound, "api keys not available")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleListOwnKeys(w, r)
	case http.MethodPost:
		s.handleCreateKey(w, r)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleMeApiKeyDispatch dispatches to revoke/rotate by prefix.
func (s *Server) handleMeApiKeyDispatch(w http.ResponseWriter, r *http.Request) {
	if s.deps.KeyVerifier == nil {
		s.writeJSONError(w, http.StatusNotFound, "api keys not available")
		return
	}
	// Path: /me/apikeys/{prefix} or /me/apikeys/{prefix}/rotate
	prefix, action := parseApiKeyPath(r.URL.Path, "/me/apikeys/")
	if prefix == "" {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodDelete:
		s.handleRevokeOwnKey(w, r, prefix)
	case action == "/rotate" && r.Method == http.MethodPost:
		s.handleRotateOwnKey(w, r, prefix)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleUsersApiKeysDispatch handles admin listing of a user's keys.
func (s *Server) handleUsersApiKeysDispatch(w http.ResponseWriter, r *http.Request) {
	if s.deps.KeyVerifier == nil {
		s.writeJSONError(w, http.StatusNotFound, "api keys not available")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	// Path: /users/{name}/apikeys or /users/{name}/apikeys/{prefix}
	user, prefix := parseUsersApiKeyPath(r.URL.Path, "/users/")
	if user == "" {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	switch {
	case prefix == "" && r.Method == http.MethodGet:
		s.handleAdminListKeys(w, r, user)
	case prefix != "" && r.Method == http.MethodDelete:
		s.handleAdminRevokeKey(w, r, user, prefix)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// --- Self-service handlers ---

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	var req struct {
		Name      string `json:"name,omitempty"`
		ExpiresIn string `json:"expiresIn,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !isJSONEmpty(err) {
		s.writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if len(req.Name) > 128 {
		s.writeJSONError(w, http.StatusBadRequest, "name too long (max 128)")
		return
	}

	var expiresIn time.Duration
	if req.ExpiresIn != "" {
		var err error
		expiresIn, err = time.ParseDuration(req.ExpiresIn)
		if err != nil || expiresIn <= 0 {
			s.writeJSONError(w, http.StatusBadRequest, "invalid expiresIn")
			return
		}
	}

	userRef := identity.UserResourceName(claims, auth.AuthMode(s.deps.IdentityConfig.Mode))
	_, _, token, err := s.deps.KeyVerifier.Generate(r.Context(), claims, expiresIn, userRef, req.Name)
	if err != nil {
		s.deps.Logger.Error("create api key", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to create key")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"token":   token,
		"warning": "this token will not be shown again",
	})
}

func (s *Server) handleListOwnKeys(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	user := claims.PreferredUsername
	if user == "" {
		user = claims.Subject
	}
	keys, err := s.deps.KeyVerifier.ListByUser(r.Context(), user)
	if err != nil {
		s.deps.Logger.Error("list api keys", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	writeJSON(w, http.StatusOK, itemsEnvelope(keys))
}

func (s *Server) handleRevokeOwnKey(w http.ResponseWriter, r *http.Request, prefix string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	// Verify ownership — list own keys and check the prefix belongs to this user.
	user := claims.PreferredUsername
	if user == "" {
		user = claims.Subject
	}
	if !s.ownsKey(r.Context(), user, prefix) {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.deps.KeyVerifier.Revoke(r.Context(), prefix); err != nil {
		s.deps.Logger.Error("revoke api key", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to revoke key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRotateOwnKey(w http.ResponseWriter, r *http.Request, prefix string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	user := claims.PreferredUsername
	if user == "" {
		user = claims.Subject
	}
	if !s.ownsKey(r.Context(), user, prefix) {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}

	var req struct {
		Name      string `json:"name,omitempty"`
		ExpiresIn string `json:"expiresIn,omitempty"`
	}
	// Accept an empty body (no expiry); reject other malformed JSON, like create.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !isJSONEmpty(err) {
		s.writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if len(req.Name) > 128 {
		s.writeJSONError(w, http.StatusBadRequest, "name too long (max 128)")
		return
	}
	var expiresIn time.Duration
	if req.ExpiresIn != "" {
		var parseErr error
		expiresIn, parseErr = time.ParseDuration(req.ExpiresIn)
		if parseErr != nil || expiresIn <= 0 {
			s.writeJSONError(w, http.StatusBadRequest, "invalid expiresIn")
			return
		}
	}

	userRef := identity.UserResourceName(claims, auth.AuthMode(s.deps.IdentityConfig.Mode))
	_, _, newToken, err := s.deps.KeyVerifier.Rotate(r.Context(), claims, prefix, expiresIn, userRef, req.Name)
	if err != nil {
		var rotateErr *apikey.RotateError
		if errors.As(err, &rotateErr) {
			// Partial failure — new key created but old key could not be deleted.
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error":    "rotation partially complete: new key created but old key could not be revoked",
				"newToken": rotateErr.NewToken,
			})
			return
		}
		s.deps.Logger.Error("rotate api key", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to rotate key")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"token":   newToken,
		"warning": "this token will not be shown again",
	})
}

// --- Admin handlers ---

func (s *Server) handleAdminListKeys(w http.ResponseWriter, r *http.Request, user string) {
	keys, err := s.deps.KeyVerifier.ListByUser(r.Context(), user)
	if err != nil {
		s.deps.Logger.Error("admin list api keys", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	writeJSON(w, http.StatusOK, itemsEnvelope(keys))
}

func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request, user, prefix string) {
	// Confirm the key belongs to the user named in the path so the action
	// cannot silently revoke a different user's key than the URL implies.
	if !s.ownsKey(r.Context(), user, prefix) {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.deps.KeyVerifier.Revoke(r.Context(), prefix); err != nil {
		s.deps.Logger.Error("admin revoke api key", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to revoke key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Helpers ---

func (s *Server) ownsKey(ctx context.Context, user, prefix string) bool {
	keys, err := s.deps.KeyVerifier.ListByUser(ctx, user)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if k.Prefix == prefix {
			return true
		}
	}
	return false
}

// parseApiKeyPath extracts the prefix and optional action (e.g., "/rotate") from paths like:
// /me/apikeys/{prefix} or /me/apikeys/{prefix}/rotate
func parseApiKeyPath(path, prefixPath string) (prefix, action string) {
	rest := strings.TrimPrefix(path, prefixPath)
	if rest == "" {
		return "", ""
	}
	parts := strings.SplitN(rest, "/", 2)
	prefix = parts[0]
	if len(parts) > 1 {
		action = "/" + parts[1]
	}
	return prefix, action
}

// parseUsersApiKeyPath parses /users/{name}/apikeys[/{prefix}]
func parseUsersApiKeyPath(path, prefixPath string) (user, prefix string) {
	rest := strings.TrimPrefix(path, prefixPath)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 1 || parts[0] == "" {
		return "", ""
	}
	user = parts[0]
	if len(parts) < 3 || parts[1] != "apikeys" {
		return user, ""
	}
	if parts[2] == "" {
		return user, ""
	}
	return user, parts[2]
}

func isJSONEmpty(err error) bool {
	// An empty request body decodes to io.EOF — a valid "no fields" request.
	if errors.Is(err, io.EOF) {
		return true
	}
	var syntaxErr *json.SyntaxError
	return errors.As(err, &syntaxErr) && syntaxErr.Offset == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
