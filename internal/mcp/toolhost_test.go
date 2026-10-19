package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

func TestInProcessHost_ListClasses(t *testing.T) {
	h := NewInProcessHost(NewServer(newTestDeps()), &auth.Claims{Subject: "s"})
	tools, err := h.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]agent.ToolDef{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	if tl, ok := byName["get_run"]; !ok || tl.Class != agent.ClassRead || !policy.Visible(tl) {
		t.Errorf("get_run = %+v, want a visible read tool", tl)
	}
	if tl, ok := byName["call_jenkins_tool"]; ok && policy.Visible(tl) {
		t.Errorf("call_jenkins_tool is visible, want hidden")
	}
}

func TestInProcessHost_ValidationMatchesHTTP(t *testing.T) {
	deps := newTestDeps()
	claims := &auth.Claims{PreferredUsername: "alice", Subject: "uuid-1"}
	h := NewInProcessHost(NewServer(deps), claims)
	got, err := h.Call(context.Background(), "get_run", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsError {
		t.Fatalf("get_run with no arguments: IsError = false, want true")
	}
	resp := mcpRequest(t, HTTPHandler(NewServer(deps)), "tools/call", map[string]any{
		"name": "get_run", "arguments": map[string]any{},
	}, claims)
	var httpResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &httpResult); err != nil {
		t.Fatal(err)
	}
	if len(httpResult.Content) == 0 || !strings.Contains(got.Content, httpResult.Content[0].Text) {
		t.Errorf("in-process %q does not match HTTP %+v", got.Content, httpResult)
	}
}

func TestInProcessHost_CarriesClaims(t *testing.T) {
	deps := newTestDeps()
	h := NewInProcessHost(NewServer(deps), &auth.Claims{PreferredUsername: "carol", Subject: "uuid-3"})
	got, err := h.Call(context.Background(), "get_me", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Content, "carol") {
		t.Errorf("get_me = %q, want the caller carol", got.Content)
	}
}

func TestSessionFrom(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	ctx := agent.WithSessionContext(context.Background(), agent.SessionContext{SessionID: id})
	if got, ok := SessionFrom(ctx); !ok || got != id.String() {
		t.Errorf("loop context: SessionFrom = %q, %v", got, ok)
	}
	if _, ok := SessionFrom(context.Background()); ok {
		t.Errorf("plain call: SessionFrom reports a session")
	}
	vctx := context.WithValue(context.Background(), sessionIDKey{}, "sess-1")
	if got, ok := SessionFrom(vctx); !ok || got != "sess-1" {
		t.Errorf("verifier key: SessionFrom = %q, %v", got, ok)
	}
}

func TestEmitActivity_AssistantOrigin(t *testing.T) {
	sink := &recordingSink{}
	deps := &api.Dependencies{ActivityPublisher: sink}
	ctx := WithOrigin(context.Background(), OriginAssistant)
	emitActivity(ctx, deps, &auth.Claims{Subject: "s"}, activity.Event{Type: "x.y"})
	if got := sink.events[0].Source; got != "assistant" {
		t.Errorf("Source = %q, want assistant", got)
	}
	emitActivity(context.Background(), deps, &auth.Claims{Subject: "s"}, activity.Event{Type: "x.y"})
	if got := sink.events[1].Source; got != "mcp" {
		t.Errorf("Source = %q, want mcp", got)
	}
}
