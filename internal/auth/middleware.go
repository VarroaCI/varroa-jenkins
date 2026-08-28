package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

type contextKey string

const claimsKey contextKey = "oidc-claims"

// APIKeyVerifier validates vk_ API key tokens and returns Claims.
type APIKeyVerifier interface {
	Verify(ctx context.Context, token string) (*Claims, error)
}

// AuthMiddleware returns an HTTP middleware that validates tokens on /api/ routes.
// Session JWTs and vk_ API keys are accepted only via the Authorization header or the
// httpOnly cookie — never a query parameter. SSE routes (which EventSource cannot send
// headers to) additionally accept a short-lived, single-purpose stream ticket via
// ?ticket=, verified by tv against the route's canonical scope.
//

// jsonError writes the uniform 401 {"error": msg} envelope (N1) with the
// correct content type; http.Error would ship text/plain.
func jsonError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}

//nolint:revive // AuthMiddleware is the canonical name, stutter is acceptable
func AuthMiddleware(p Provider, kv APIKeyVerifier, tv *TicketVerifier, sv ScheduleVerifier, next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only require auth for /api/ routes.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Unauthenticated endpoints — no token required.
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/login" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/v1/auth-config" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/otel/") {
			next.ServeHTTP(w, r)
			return
		}
		// OpenAPI spec and docs pages: GET is unauthenticated (the spec is public in
		// the git repo and the docs page must load before login). Non-GET methods
		// fall through to the normal 401.
		if r.Method == http.MethodGet && (r.URL.Path == "/api/v1/openapi.json" ||
			r.URL.Path == "/api/v1/docs" || strings.HasPrefix(r.URL.Path, "/api/v1/docs/")) {
			next.ServeHTTP(w, r)
			return
		}

		// OIDC auth endpoints: unauthenticated GET access.
		if r.Method == http.MethodGet && (r.URL.Path == "/api/v1/auth/login" || r.URL.Path == "/api/v1/callback") {
			next.ServeHTTP(w, r)
			return
		}

		// SSE routes: a stream ticket in the query is the header-less auth path for
		// EventSource. It is scoped to exactly this stream and carries the caller's
		// identity. A full session token in the query is never accepted.
		if scope := sseScope(r.URL.Path); scope != "" && tv != nil {
			if ticket := r.URL.Query().Get("ticket"); ticket != "" {
				claims, err := tv.Verify(r.Context(), ticket, scope)
				if err != nil {
					logger.Warn("stream ticket verification failed", "error", err, "scope", scope)
					jsonError(w, "invalid stream ticket")
					return
				}
				ctx := context.WithValue(r.Context(), claimsKey, claims)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		token := extractToken(r)
		if token == "" {
			jsonError(w, "not authenticated")
			return
		}

		var claims *Claims

		// Route vk_ API keys to the key verifier, JWT tokens to the provider.
		if strings.HasPrefix(token, "vk_") && kv != nil {
			var err error
			claims, err = kv.Verify(r.Context(), token)
			if err != nil {
				logger.Warn("api key verification failed", "error", err)
				jsonError(w, "invalid token")
				return
			}
		} else {
			// Check for a BroodSchedule-triggered token before falling through
			// to the provider. The schedule verifier disambiguates on the aud
			// claim, so this is safe even when using local auth mode (same kid).
			if sv != nil {
				if sClaims, matched, verr := sv.Verify(r.Context(), token); matched {
					if verr != nil {
						logger.Warn("schedule token verification failed", "error", verr)
						jsonError(w, "invalid schedule token")
						return
					}
					ctx := context.WithValue(r.Context(), claimsKey, sClaims)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			var err error
			claims, err = p.Verify(r.Context(), token)
			if err != nil {
				logger.Warn("token validation failed", "error", err)
				jsonError(w, "invalid token")
				return
			}
		}

		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sseScope returns the canonical stream scope for an SSE route, or "" if the path is
// not a server-sent-events route. Scopes: "brood", "activity", or
// "controller:{ns}/{name}". A stream ticket is bound to one scope, so a ticket minted
// for one stream cannot open another.
func sseScope(path string) string {
	p := strings.TrimPrefix(path, "/api/v1")
	switch p {
	case "/activity/stream":
		return "activity"
	case "/stream/brood":
		return "brood"
	}
	if strings.HasPrefix(p, "/controllers/") {
		segs := strings.Split(strings.TrimPrefix(p, "/controllers/"), "/")
		if len(segs) >= 3 {
			switch segs[len(segs)-1] {
			case "events", "logs":
				if segs[0] != "" && segs[1] != "" {
					return "controller:" + segs[0] + "/" + segs[1]
				}
			}
		}
	}
	if strings.HasPrefix(p, "/clusters/") {
		rest := strings.TrimPrefix(p, "/clusters/")
		segs := strings.Split(rest, "/")
		// /clusters/{cluster}/controllers/{ns}/{name}/events|logs
		if len(segs) >= 5 && segs[1] == "controllers" {
			switch segs[len(segs)-1] {
			case "events", "logs":
				if segs[0] != "" && segs[2] != "" && segs[3] != "" {
					return "controller:" + segs[0] + "/" + segs[2] + "/" + segs[3]
				}
			}
		}
	}
	if strings.HasPrefix(p, "/brood-operations/") {
		segs := strings.Split(strings.TrimPrefix(p, "/brood-operations/"), "/")
		if len(segs) == 3 && segs[2] == "stream" && segs[0] != "" && segs[1] != "" {
			return "broodop:" + segs[0] + "/" + segs[1]
		}
	}
	return ""
}

// ClaimsFromContext returns the OIDC claims injected by the middleware.
// Returns nil if not authenticated.
func ClaimsFromContext(ctx context.Context) *Claims {
	claims, _ := ctx.Value(claimsKey).(*Claims)
	return claims
}

// ContextWithClaims returns a copy of ctx carrying the given claims, as the
// middleware would inject them. Primarily for handler tests in other packages,
// since the context key is unexported.
func ContextWithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, claimsKey, claims)
}

func extractToken(r *http.Request) string {
	// Primary: Authorization Bearer header (programmatic API access).
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			return auth[7:]
		}
	}
	// Fallback: httpOnly cookie set by OIDC callback (browser requests).
	if cookie, err := r.Cookie("varroa_token"); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	// NOTE: session JWTs / vk_ keys are deliberately NOT read from the query string —
	// that would leak credentials into URLs, logs, and Referer headers. EventSource
	// authenticates via a short-lived stream ticket (?ticket=) handled in AuthMiddleware.
	return ""
}
