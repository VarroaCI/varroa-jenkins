package store

import (
	"context"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// testSetInstructionsSHA proves the revision and its event land together, and
// that a session the caller does not lease is refused.
func testSetInstructionsSHA(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()
	sess := startRunning(t, s)

	if err := s.SetInstructionsSHA(ctx, sess.ID, testOwner, "abc123", userEvent("read")); err != nil {
		t.Fatalf("SetInstructionsSHA: %v", err)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.InstructionsSHA == nil || *got.InstructionsSHA != "abc123" {
		t.Fatalf("instructions_sha = %v, want abc123", got.InstructionsSHA)
	}
	evs, err := s.Events(ctx, sess.ID, 0, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2 (seed and the new event)", len(evs))
	}

	if err := s.SetInstructionsSHA(ctx, sess.ID, "someone-else", "zzz", userEvent("x")); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-owner error = %v, want ErrConflict", err)
	}
	got, err = s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.InstructionsSHA == nil || *got.InstructionsSHA != "abc123" {
		t.Fatalf("refused write changed instructions_sha to %v", got.InstructionsSHA)
	}
}

// testPutTenantAgentUnattended proves a grant is stored under its key, a second
// key does not replace it, and a nil grant removes only its own key.
func testPutTenantAgentUnattended(t *testing.T, f factory) {
	clock := newTestClock()
	s := f(t, Options{Now: clock.Now})
	ctx := context.Background()

	first := agent.UnattendedGrant{UID: "uid-1", Tools: []string{"run_pipeline"}, GrantedBy: "admin", GrantedAt: clock.Now()}
	if err := s.PutTenantAgentUnattended(ctx, "ns/alpha", &first, "admin"); err != nil {
		t.Fatalf("put alpha: %v", err)
	}
	second := agent.UnattendedGrant{UID: "uid-2", Tools: []string{"restart_run"}, GrantedBy: "admin", GrantedAt: clock.Now()}
	if err := s.PutTenantAgentUnattended(ctx, "ns/beta", &second, "admin"); err != nil {
		t.Fatalf("put beta: %v", err)
	}
	st, err := s.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got := st.TenantAgentUnattended["ns/alpha"]; got.UID != "uid-1" || len(got.Tools) != 1 || got.Tools[0] != "run_pipeline" {
		t.Fatalf("alpha grant = %+v", got)
	}
	if got := st.TenantAgentUnattended["ns/beta"]; got.UID != "uid-2" {
		t.Fatalf("beta grant = %+v", got)
	}

	if err := s.PutTenantAgentUnattended(ctx, "ns/alpha", nil, "admin"); err != nil {
		t.Fatalf("revoke alpha: %v", err)
	}
	st, err = s.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if _, ok := st.TenantAgentUnattended["ns/alpha"]; ok {
		t.Fatalf("alpha grant survived revoke")
	}
	if _, ok := st.TenantAgentUnattended["ns/beta"]; !ok {
		t.Fatalf("revoking alpha removed beta")
	}
}
