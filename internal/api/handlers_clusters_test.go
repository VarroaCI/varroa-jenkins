package api

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

func TestActorFrom_ResolutionOrder(t *testing.T) {
	tests := []struct {
		name   string
		claims *auth.Claims
		want   string
	}{
		{"nil claims", nil, ""},
		{"prefers username", &auth.Claims{PreferredUsername: "alice", Email: "a@x.io", Subject: "uuid-1"}, "alice"},
		{"falls back to email", &auth.Claims{Email: "a@x.io", Subject: "uuid-1"}, "a@x.io"},
		{"falls back to subject", &auth.Claims{Subject: "uuid-1"}, "uuid-1"},
		{"all empty", &auth.Claims{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ActorFrom(tc.claims); got != tc.want {
				t.Errorf("ActorFrom() = %q, want %q", got, tc.want)
			}
		})
	}
}
