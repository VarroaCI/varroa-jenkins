package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

func TestSliceAtRune(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		offset     int64
		length     int64
		want       string
		wantCutOff bool
	}{
		{"whole short text", "abc", 0, 10, "abc", false},
		{"cut at length", "abcdef", 0, 3, "abc", true},
		{"offset past end", "abc", 5, 3, "", true},
		{"offset into a multibyte rune moves back", "aéb", 2, 10, "éb", true},
		{"length into a multibyte rune moves back", "éé", 0, 3, "é", true},
		{"empty text", "", 0, 3, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, cut := sliceAtRune(c.text, c.offset, c.length)
			if got != c.want || cut != c.wantCutOff {
				t.Fatalf("sliceAtRune(%q, %d, %d) = %q, %v; want %q, %v",
					c.text, c.offset, c.length, got, cut, c.want, c.wantCutOff)
			}
		})
	}
}

func TestIsJSONObjectBody(t *testing.T) {
	cases := map[string]bool{
		`{"a":1}`:        true,
		`{}`:             true,
		`[1,2]`:          false,
		`null`:           false,
		`"text"`:         false,
		`{"a":1} {}`:     false,
		`{"a":`:          false,
		``:               false,
		`{"a":1}   `:     true,
		`{"a":{"b":[]}}`: true,
	}
	for in, want := range cases {
		if got := isJSONObjectBody(json.RawMessage(in)); got != want {
			t.Errorf("isJSONObjectBody(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCallerAgent_MatchesOnlyTheOwnPrincipal(t *testing.T) {
	ta := v1alpha1.TeamAgent{}
	ta.Name = "triage"
	ta.Namespace = "tenant-a"
	ta.Spec.TeamRef.Name = "alpha"
	in := accept.Input{TeamAgents: []v1alpha1.TeamAgent{ta}}

	principal := accept.Principal(&ta)
	if got, ok := callerAgent(&auth.Claims{Subject: principal}, in); !ok || got.Name != "triage" {
		t.Fatalf("own principal: callerAgent = %v, %v", got, ok)
	}
	if _, ok := callerAgent(&auth.Claims{Subject: "agent:tenant-a/alpha/other"}, in); ok {
		t.Fatal("a different agent principal matched")
	}
	if _, ok := callerAgent(&auth.Claims{Subject: "alice"}, in); ok {
		t.Fatal("a human subject matched a team agent")
	}
	if _, ok := callerAgent(nil, in); ok {
		t.Fatal("nil claims matched")
	}
}

func TestTeamAgentSession_ReportsAMissingSessionAsAnError(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory(store.Options{})
	srv := &Server{deps: &Dependencies{Assistant: &AssistantRuntime{Store: st}}}

	if _, ok, err := srv.teamAgentSession(ctx); ok || err != nil {
		t.Fatalf("no session: got ok=%v err=%v", ok, err)
	}

	missing := agent.WithSessionContext(ctx, agent.SessionContext{SessionID: uuid.New()})
	if _, ok, err := srv.teamAgentSession(missing); ok || err == nil {
		t.Fatalf("missing session: got ok=%v err=%v, want an error", ok, err)
	}

	off := &Server{deps: &Dependencies{}}
	if _, ok, err := off.teamAgentSession(missing); ok || err != nil {
		t.Fatalf("assistant off: got ok=%v err=%v", ok, err)
	}
}
