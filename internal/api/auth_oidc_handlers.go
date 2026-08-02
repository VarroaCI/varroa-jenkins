package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/userdirectory"
)

// stateCookieMaxAge is the lifetime of the signed oidc_state cookie (5 minutes).
const stateCookieMaxAge = 300

// stateCookieName is the name of the signed state cookie.
const stateCookieName = "oidc_state"

// interactiveCookieName is the marker cookie for interactive login after logout.
const interactiveCookieName = "interactive_login"

// statePayloadJSON is the signed state cookie content, serialized as base64url JSON.
type statePayloadJSON struct {
	Nonce  string `json:"n"`
	Return string `json:"r"`
	Expiry int64  `json:"e"`
}

// normalizeReturn validates and normalizes a return-path query parameter.
// It rejects scheme/host, double slashes, dot-path traversal, and ensures
// exactly one leading slash. Returns "/" on invalid input.
func normalizeReturn(raw string) string {
	// Empty or unset defaults to "/".
	if raw == "" {
		return "/"
	}
	// Reject schemes and hosts (absolute URLs).
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	// Ensure one leading slash.
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	// Reject dot-path traversal (e.g. /../, /./).
	cleaned := strings.TrimRight(raw, "/")
	if cleaned == "" {
		return "/"
	}
	parts := strings.Split(cleaned, "/")
	for _, p := range parts {
		if p == ".." || p == "." {
			return "/"
		}
	}
	return cleaned
}

// signStatePayload creates an HMAC-SHA256 signature for the state payload.
func signStatePayload(secret []byte, nonce, returnPath string, expiry int64) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(nonce))
	mac.Write([]byte("\x00"))
	mac.Write([]byte(returnPath))
	mac.Write([]byte("\x00"))
	fmt.Fprintf(mac, "%d", expiry)
	return hex.EncodeToString(mac.Sum(nil))
}

// newStateNonce generates a cryptographically random hex nonce.
func newStateNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("state nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// encodeStateCookie creates a base64url-encoded signed state cookie value.
// Format: base64url(json_payload).hex(signature)
func encodeStateCookie(secret []byte, nonce, returnPath string, expiry int64) (string, error) {
	p := statePayloadJSON{Nonce: nonce, Return: returnPath, Expiry: expiry}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	sig := signStatePayload(secret, nonce, returnPath, expiry)
	return encoded + "." + sig, nil
}

// decodeStateCookie parses and validates a base64url-encoded signed state cookie.
func decodeStateCookie(secret []byte, cookieValue string) (*statePayloadJSON, error) {
	dot := strings.LastIndexByte(cookieValue, '.')
	if dot < 0 {
		return nil, fmt.Errorf("invalid cookie format")
	}
	encoded := cookieValue[:dot]
	sig := cookieValue[dot+1:]

	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}

	var p statePayloadJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("json decode: %w", err)
	}

	expectedSig := signStatePayload(secret, p.Nonce, p.Return, p.Expiry)
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return nil, fmt.Errorf("signature mismatch")
	}

	return &p, nil
}

// HandleOIDCLogin initiates the OIDC authorization flow.
// GET /api/v1/auth/login?return=<encoded-relative-path>
func (s *Server) HandleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	validator, ok := s.validator()
	if !ok {
		s.writeJSONError(w, http.StatusNotImplemented, "OIDC not configured")
		return
	}
	if s.deps.OIDCStateSecret == nil {
		s.writeJSONError(w, http.StatusInternalServerError, "state secret not configured")
		return
	}

	rawReturn := r.URL.Query().Get("return")
	returnPath := normalizeReturn(rawReturn)
	if returnPath != rawReturn && rawReturn != "" {
		s.deps.Logger.Debug("normalized return path", "original", rawReturn, "normalized", returnPath)
	}

	// Generate nonce and signed state cookie.
	nonce, err := newStateNonce()
	if err != nil {
		s.deps.Logger.Error("failed to generate state nonce", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	expiry := time.Now().Unix() + stateCookieMaxAge
	payload, err := encodeStateCookie(s.deps.OIDCStateSecret, nonce, returnPath, expiry)
	if err != nil {
		s.deps.Logger.Error("failed to encode state cookie", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Determine if this is an interactive login (from logout marker).
	authOpts := []oauth2.AuthCodeOption{}
	interactiveMarker := false
	if marker, err := r.Cookie(interactiveCookieName); err == nil && marker.Value == "1" {
		authOpts = append(authOpts, oauth2.SetAuthURLParam("prompt", "login"))
		authOpts = append(authOpts, oauth2.SetAuthURLParam("max_age", "0"))
		interactiveMarker = true
		// Delete the interactive marker cookie.
		s.deleteCookie(w, interactiveCookieName, "/api/v1/auth")
	}

	// Set the signed state cookie.
	s.setCookie(w, &http.Cookie{
		Name:   stateCookieName,
		Value:  payload,
		Path:   "/api/v1",
		MaxAge: stateCookieMaxAge,
	})

	// Redirect to provider with nonce as OAuth state.
	authURL := validator.AuthCodeURLOpts(nonce, authOpts...)
	s.deps.Logger.Debug("oidc login redirect",
		"returnPath", returnPath,
		"interactive", interactiveMarker)
	if interactiveMarker {
		s.deps.Logger.Info("interactive login requested via marker cookie")
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// callbackTemplate renders the OIDC callback progress page.
// The ID token is set as an HttpOnly cookie server-side — no token is
// exposed in the HTML, JS, or localStorage.
var callbackTemplate = template.Must(template.New("callback").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><title>Varroa — Signing you in</title>
<style>
  @media (prefers-reduced-motion: reduce) {
    .spinner { animation: none !important; opacity: 0.6; }
  }
  body { font-family: system-ui, -apple-system, sans-serif; background: #1a1a2e; color: #e0e0e0; display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; }
  .card { text-align: center; padding: 2rem; max-width: 400px; }
  .spinner { width: 40px; height: 40px; margin: 0 auto 1rem; border: 4px solid rgba(255,255,255,0.1); border-top: 4px solid #c2612c; border-radius: 50%; animation: spin 0.8s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }
  h1 { font-size: 1.25rem; font-weight: 500; margin: 0 0 0.5rem; }
  p { font-size: 0.875rem; color: #a0a0a0; margin: 0; }
</style></head><body><div class="card" role="status" aria-live="polite">
<div class="spinner" aria-hidden="true"></div>
<h1>Signing you in</h1>
<p>Completing authentication with your identity provider.</p>
<script>window.location.replace('{{.RedirectTo}}');</script>
<noscript><meta http-equiv="refresh" content="0;url={{.RedirectTo}}"></noscript>
</div></body></html>`))

// callbackData holds values for the OIDC callback progress page.
type callbackData struct {
	RedirectTo string
}

// HandleOIDCCallback processes the OIDC provider callback.
// GET /api/v1/callback?code=<code>&state=<state>
func (s *Server) HandleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	validator, ok := s.validator()
	if !ok {
		http.Error(w, "OIDC not configured", http.StatusNotImplemented)
		return
	}

	// Read and validate the signed state cookie.
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil {
		s.deps.Logger.Warn("callback missing state cookie", "error", err)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Delete the state cookie immediately (single-use).
	s.deleteCookie(w, stateCookieName, "/api/v1")

	// Parse and validate the signed state cookie (base64url JSON + HMAC).
	p, err := decodeStateCookie(s.deps.OIDCStateSecret, stateCookie.Value)
	if err != nil {
		s.deps.Logger.Warn("state cookie validation failed", "error", err)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	nonce, returnPath := p.Nonce, p.Return

	// Validate expiry.
	if time.Now().Unix() > p.Expiry {
		s.deps.Logger.Warn("state cookie expired")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Validate nonce matches OAuth state parameter.
	oauthState := r.URL.Query().Get("state")
	if !hmac.Equal([]byte(nonce), []byte(oauthState)) {
		s.deps.Logger.Warn("state nonce mismatch")
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Check for OIDC error in the callback.
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		errDesc := r.URL.Query().Get("error_description")
		s.deps.Logger.Warn("OIDC provider returned error", "error", errParam, "description", errDesc)
		http.Redirect(w, r, "/login?error="+url.QueryEscape(errParam), http.StatusFound)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	// Exchange code for tokens.
	oauth2Token, err := validator.Exchange(r.Context(), code)
	if err != nil {
		s.deps.Logger.Error("token exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		s.deps.Logger.Error("no id_token in OIDC response")
		http.Error(w, "no id_token in response", http.StatusInternalServerError)
		return
	}

	// Validate the token.
	claims, err := validator.VerifyToken(r.Context(), rawIDToken)
	if err != nil {
		s.deps.Logger.Error("token validation failed", "error", err)
		http.Error(w, "token validation failed", http.StatusInternalServerError)
		return
	}

	if len(claims.Groups) == 0 {
		s.deps.Logger.Warn("user has zero groups in ID token, RBAC group-based permissions will not apply",
			"email", claims.Email,
			"subject", claims.Subject,
			"hint", "Add a 'claims' mapping to your Dex connector config. See: https://dexidp.io/docs/connectors/#claims")
	} else {
		s.deps.Logger.Info("user authenticated",
			"email", claims.Email,
			"subject", claims.Subject,
			"groups", claims.Groups)
	}

	// Emit login event.
	if s.deps.ActivityStore != nil {
		s.deps.ActivityStore.Notify(activity.Event{
			Type:    "login",
			Source:  "user",
			Actor:   claims.Email,
			Message: claims.Email + " logged in",
		})
	}

	// Record the user in the directory.
	if s.deps.Store != nil && s.deps.OperatorNamespace != "" {
		h := sha256.Sum256([]byte(claims.Subject))
		userName := "oidc-" + hex.EncodeToString(h[:])[:32]
		if err := userdirectory.RecordLogin(context.Background(), userStoreAdapter{store: s.deps.Store}, userName,
			claims.Email, claims.Name, claims.Groups,
			claims.Subject, claims.PreferredUsername, s.deps.OperatorNamespace); err != nil {
			s.deps.Logger.Error("failed to record user in directory", "error", err, "subject", claims.Subject)
		}
	}

	// Set domain cookie.
	cookieDomain := ""
	if s.deps.Auth != nil {
		cookieDomain = s.deps.Auth.CookieDomain()
	}
	s.setCookie(w, &http.Cookie{
		Name:   "varroa_token",
		Value:  rawIDToken,
		Domain: cookieDomain,
		Path:   "/",
		MaxAge: 86400,
	})

	// Render progress page.
	redirectTo := normalizeReturn(returnPath)
	var buf bytes.Buffer
	if err := callbackTemplate.Execute(&buf, callbackData{RedirectTo: redirectTo}); err != nil {
		s.deps.Logger.Error("template execution failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := buf.WriteTo(w); err != nil {
		s.deps.Logger.Error("writing response failed", "error", err)
	}
}

// validator returns the OIDCProvider from the auth provider.
func (s *Server) validator() (auth.OIDCProvider, bool) {
	if s.deps.Auth == nil || s.deps.Auth.Mode() != auth.AuthModeOIDC {
		return nil, false
	}
	v, ok := s.deps.Auth.(auth.OIDCProvider)
	return v, ok
}

// setCookie writes an HttpOnly SameSite=Lax cookie using the configured
// Secure flag (disabled only for HTTP local development).
func (s *Server) setCookie(w http.ResponseWriter, c *http.Cookie) {
	c.Secure = s.deps.SecureCookies
	c.HttpOnly = true
	if c.SameSite == 0 {
		c.SameSite = http.SameSiteLaxMode
	}
	http.SetCookie(w, c)
}

// deleteCookie clears an HttpOnly cookie by setting MaxAge=-1.
func (s *Server) deleteCookie(w http.ResponseWriter, name, path string) {
	s.setCookie(w, &http.Cookie{
		Name:   name,
		Path:   path,
		MaxAge: -1,
	})
}
