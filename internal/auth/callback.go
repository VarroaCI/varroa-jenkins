package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth/userdirectory"
)

// callbackData holds values for the OIDC callback HTML page.
type callbackData struct {
	IDToken    string
	Email      string
	RedirectTo string
}

// callbackTemplate renders the OIDC callback page. html/template auto-escapes
// all values for their context (JS string literals, HTML text, URL).
var callbackTemplate = template.Must(template.New("callback").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Varroa — Authenticated</title>
<script>
localStorage.setItem('varroa_id_token', {{.IDToken}});
localStorage.setItem('varroa_user', {{.Email}});
window.location.replace({{.RedirectTo}});
</script>
</head><body><p>Authenticated as {{.Email}}. Redirecting...</p></body></html>`))

// CallbackHandler handles the OIDC callback from Dex.
// It exchanges the authorization code for tokens, stores the ID token
// in the browser via an HTML page, and redirects to the app.
// If actStore is provided, login events are emitted to the activity feed.
// If store is provided and namespace is non-empty, the user directory entry
// is created or refreshed on every login.
func CallbackHandler(v *Validator, actStore *activity.Store, store userdirectory.UserStore, namespace string, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code parameter", http.StatusBadRequest)
			return
		}

		state := r.URL.Query().Get("state")
		redirectTo := "/"
		if state != "" {
			redirectTo = state
		}

		oauth2Token, err := v.Exchange(r.Context(), code)
		if err != nil {
			logger.Error("token exchange failed", "error", err)
			http.Error(w, "token exchange failed", http.StatusInternalServerError)
			return
		}

		rawIDToken, ok := oauth2Token.Extra("id_token").(string)
		if !ok {
			logger.Error("no id_token in OIDC response")
			http.Error(w, "no id_token in response", http.StatusInternalServerError)
			return
		}

		// Validate the token we just got
		claims, err := v.VerifyToken(r.Context(), rawIDToken)
		if err != nil {
			logger.Error("token validation failed", "error", err)
			http.Error(w, "token validation failed", http.StatusInternalServerError)
			return
		}

		if len(claims.Groups) == 0 {
			logger.Warn("user has zero groups in ID token, RBAC group-based permissions will not apply",
				"email", claims.Email,
				"subject", claims.Subject,
				"hint", "Add a 'claims' mapping to your Dex connector config. See: https://dexidp.io/docs/connectors/#claims")
		} else {
			logger.Info("user authenticated",
				"email", claims.Email,
				"subject", claims.Subject,
				"groups", claims.Groups)
		}

		// Emit login event to activity feed.
		if actStore != nil {
			actStore.Notify(activity.Event{
				Type:    "login",
				Source:  "user",
				Actor:   claims.Email,
				Message: claims.Email + " logged in",
			})
		}

		// Record the user in the directory (create-if-absent, refresh on
		// every login). Never modifies a local-managed user.
		if store != nil && namespace != "" {
			// Derive the deterministic OIDC user name (never email).
			h := sha256.Sum256([]byte(claims.Subject))
			userName := "oidc-" + hex.EncodeToString(h[:])[:32]
			if err := userdirectory.RecordLogin(context.Background(), store, userName,
				claims.Email, claims.Name, claims.Groups,
				claims.Subject, claims.PreferredUsername, namespace); err != nil {
				logger.Error("failed to record user in directory", "error", err, "subject", claims.Subject)
				// Non-fatal: the login itself succeeded.
			}
		}

		// Set domain cookie so all controllers see the token.
		http.SetCookie(w, &http.Cookie{
			Name:     "varroa_token",
			Value:    rawIDToken,
			Domain:   v.cookieDomain,
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   86400,
		})

		// Render the callback page to a buffer first so a template failure
		// can be surfaced as a 500 instead of a partial 200 HTML page.
		var buf bytes.Buffer
		if err := callbackTemplate.Execute(&buf, callbackData{
			IDToken:    rawIDToken,
			Email:      claims.Email,
			RedirectTo: redirectTo,
		}); err != nil {
			logger.Error("template execution failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := buf.WriteTo(w); err != nil {
			logger.Error("writing response failed", "error", err)
		}
	}
}
