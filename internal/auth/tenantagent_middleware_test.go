package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubTenantAgent answers matched for the token "agent-token" only.
type stubTenantAgent struct {
	err error
}

func (s stubTenantAgent) Verify(_ *http.Request, token string) (*Claims, string, bool, error) {
	if token != "agent-token" {
		return nil, "", false, nil
	}
	if s.err != nil {
		return nil, "", true, s.err
	}
	return &Claims{Subject: "agent:ns/team/a", PreferredUsername: "agent:ns/team/a", Name: "agent:ns/team/a"}, "sid-1", true, nil
}

// TestAuthMiddleware_TenantAgentToken checks that a matched Tenant agent token
// sets the claims and the session id, and that a refused one answers 401.
func TestAuthMiddleware_TenantAgentToken(t *testing.T) {
	logger := slog.Default()
	mp := &mockProvider{mode: AuthModeLocal}

	var gotSID string
	var gotSubject string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSID, _ = AgentSessionFrom(r.Context())
		gotSubject = ClaimsFromContext(r.Context()).Subject
	})
	mw := AuthMiddleware(mp, nil, nil, nil, stubTenantAgent{}, next, logger)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp", nil)
	req.Header.Set("Authorization", "Bearer agent-token")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gotSID != "sid-1" || gotSubject != "agent:ns/team/a" {
		t.Fatalf("code=%d sid=%q subject=%q", rec.Code, gotSID, gotSubject)
	}

	refused := AuthMiddleware(mp, nil, nil, nil, stubTenantAgent{err: errors.New("bad")}, next, logger)
	rec = httptest.NewRecorder()
	refused.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refused token code = %d, want 401", rec.Code)
	}
}

// reservedProvider returns a human identity that carries the agent prefix.
type reservedProvider struct{ mockProvider }

func (r *reservedProvider) Verify(_ context.Context, _ string) (*Claims, error) {
	return &Claims{Subject: "alice", PreferredUsername: "agent:ns/team/a"}, nil
}

// TestAuthMiddleware_ReservedSubject_Provider refuses a human identity that
// carries the reserved prefix in a user claim.
func TestAuthMiddleware_ReservedSubject_Provider(t *testing.T) {
	rp := &reservedProvider{mockProvider{mode: AuthModeLocal}}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a reserved subject reached the handler")
	})
	mw := AuthMiddleware(rp, nil, nil, nil, nil, next, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	req.Header.Set("Authorization", "Bearer any")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// TestReservedSubject covers each identity field and the nil case.
func TestReservedSubject(t *testing.T) {
	cases := map[string]struct {
		c    *Claims
		want bool
	}{
		"nil":              {nil, false},
		"human":            {&Claims{Subject: "alice"}, false},
		"subject":          {&Claims{Subject: "agent:a/b/c"}, true},
		"preferred name":   {&Claims{Subject: "alice", PreferredUsername: "agent:a/b/c"}, true},
		"email":            {&Claims{Subject: "alice", Email: "agent:a/b/c"}, true},
		"prefix not start": {&Claims{Subject: "x agent:a"}, false},
	}
	for name, c := range cases {
		if got := ReservedSubject(c.c); got != c.want {
			t.Errorf("%s: ReservedSubject = %v, want %v", name, got, c.want)
		}
	}
}

// TestAgentSessionContext round-trips the session id through the context.
func TestAgentSessionContext(t *testing.T) {
	if _, ok := AgentSessionFrom(context.Background()); ok {
		t.Fatal("an empty context must name no session")
	}
	ctx := ContextWithAgentSession(context.Background(), "sid-9")
	if sid, ok := AgentSessionFrom(ctx); !ok || sid != "sid-9" {
		t.Fatalf("AgentSessionFrom = %q, %v", sid, ok)
	}
}
