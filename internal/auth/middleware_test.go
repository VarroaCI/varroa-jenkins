package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type mockProvider struct {
	mode AuthMode
}

func (m *mockProvider) Mode() AuthMode { return m.mode }
func (m *mockProvider) Verify(_ context.Context, token string) (*Claims, error) {
	if token == "valid-jwt" {
		return &Claims{Subject: "jwt-user", Email: "jwt@test.com", Groups: []string{"users"}}, nil
	}
	return nil, http.ErrAbortHandler
}
func (m *mockProvider) CookieDomain() string              { return "" }
func (m *mockProvider) Discovery() ([]byte, []byte, bool) { return nil, nil, false }

type mockKeyVerifier struct{}

func (m *mockKeyVerifier) Verify(_ context.Context, token string) (*Claims, error) {
	if token == "vk_prefix.secret" {
		return &Claims{Subject: "apikey-user", Email: "apikey@test.com", Groups: []string{"developers"}}, nil
	}
	return nil, http.ErrAbortHandler
}

func TestAuthMiddleware_SSETicketAndQueryTokenRejection(t *testing.T) {
	mp := &mockProvider{}
	mkv := &mockKeyVerifier{}
	logger := slog.Default()
	signer := newTestSigner(t)
	iss := NewTicketIssuer(signer, "iss", 30*time.Second)
	tv := NewTicketVerifier(signer.PublicKey(), "iss")

	t.Run("valid ticket on SSE route authenticates", func(t *testing.T) {
		ticket, _, _ := iss.Mint(&Claims{Subject: "alice"}, "brood")
		var claims *Claims
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { claims = ClaimsFromContext(r.Context()) })
		mw := AuthMiddleware(mp, mkv, tv, nil, next, logger)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/brood?ticket="+ticket, nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if claims == nil || claims.Subject != "alice" {
			t.Errorf("ticket claims not injected: %+v", claims)
		}
	})

	t.Run("session token in query string is NOT honored", func(t *testing.T) {
		// The old ?token= leak path must be gone: a valid session JWT in the query
		// must not authenticate, on any route (SSE or not).
		for _, path := range []string{"/api/v1/settings?token=valid-jwt", "/api/v1/stream/brood?token=valid-jwt"} {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			mw := AuthMiddleware(mp, mkv, tv, nil, next, logger)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: expected 401 (query token rejected), got %d", path, rec.Code)
			}
		}
	})
}

func TestAuthMiddlewareRoutesApiKeys(t *testing.T) {
	mp := &mockProvider{}
	mkv := &mockKeyVerifier{}
	logger := slog.Default()

	t.Run("routes vk_ token to key verifier", func(t *testing.T) {
		var claims *Claims
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			claims = ClaimsFromContext(r.Context())
		})
		mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)

		req := httptest.NewRequest("GET", "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer vk_prefix.secret")
		mw.ServeHTTP(httptest.NewRecorder(), req)

		if claims == nil || claims.Subject != "apikey-user" {
			t.Errorf("expected apikey-user claims, got %v", claims)
		}
	})

	t.Run("routes JWT token to provider", func(t *testing.T) {
		var claims *Claims
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			claims = ClaimsFromContext(r.Context())
		})
		mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)

		req := httptest.NewRequest("GET", "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer valid-jwt")
		mw.ServeHTTP(httptest.NewRecorder(), req)

		if claims == nil || claims.Subject != "jwt-user" {
			t.Errorf("expected jwt-user claims, got %v", claims)
		}
	})

	t.Run("rejects bad vk_ token with 401", func(t *testing.T) {
		next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
		mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)

		req := httptest.NewRequest("GET", "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer vk_bad.bad")
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("non-API routes bypass auth", func(t *testing.T) {
		var called bool
		next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			called = true
		})
		mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)

		req := httptest.NewRequest("GET", "/health", nil)
		mw.ServeHTTP(httptest.NewRecorder(), req)

		if !called {
			t.Error("expected non-API route to be handled")
		}
	})
}

func TestAuthMiddleware_OpenAPIDocsExemptions(t *testing.T) {
	mp := &mockProvider{}
	mkv := &mockKeyVerifier{}
	logger := slog.Default()

	t.Run("GET /api/v1/openapi.json unauthenticated", func(t *testing.T) {
		var called bool
		next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			called = true
		})
		mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
		if !called {
			t.Error("expected handler to be called")
		}
	})

	t.Run("GET /api/v1/docs unauthenticated", func(t *testing.T) {
		var called bool
		next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			called = true
		})
		mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/docs", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
		if !called {
			t.Error("expected handler to be called")
		}
	})

	t.Run("GET /api/v1/docs/rapidoc-min.js unauthenticated", func(t *testing.T) {
		var called bool
		next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			called = true
		})
		mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/docs/rapidoc-min.js", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
		if !called {
			t.Error("expected handler to be called")
		}
	})

	t.Run("POST /api/v1/docs still requires auth", func(t *testing.T) {
		next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
		mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/docs", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})
}

func TestAuthMiddleware_OIDCAuthEndpoints(t *testing.T) {
	mp := &mockProvider{}
	mkv := &mockKeyVerifier{}
	logger := slog.Default()

	for _, tc := range []struct {
		name   string
		method string
		path   string
		allow  bool
	}{
		{"GET /api/v1/auth/login allowed", http.MethodGet, "/api/v1/auth/login", true},
		{"POST /api/v1/auth/login blocked", http.MethodPost, "/api/v1/auth/login", false},
		{"GET /api/v1/callback allowed", http.MethodGet, "/api/v1/callback", true},
		{"POST /api/v1/callback blocked", http.MethodPost, "/api/v1/callback", false},
		{"GET /api/v1/auth-config allowed", http.MethodGet, "/api/v1/auth-config", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				called = true
			})
			mw := AuthMiddleware(mp, mkv, nil, nil, next, logger)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			if tc.allow {
				if !called {
					t.Errorf("expected handler to be called for %s %s", tc.method, tc.path)
				}
			} else {
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("expected 401 for %s %s, got %d", tc.method, tc.path, rec.Code)
				}
			}
		})
	}
}
