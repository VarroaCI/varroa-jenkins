package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/providertest"
)

// fakeServer starts the fake provider with one script and returns the test
// server and the fake.
func fakeServer(t *testing.T, turns ...providertest.Turn) (*httptest.Server, *providertest.Server) {
	t.Helper()
	fake := providertest.New(providertest.Script{Turns: turns})
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	return ts, fake
}

// collect streams one request and reads the response.
func collect(t *testing.T, a *Adapter, key provider.Key, req provider.Request) (provider.Response, error) {
	t.Helper()
	ch, err := a.Stream(context.Background(), key, req)
	if err != nil {
		return provider.Response{}, err
	}
	return provider.Collect(ch)
}

func TestTextAndToolUse(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{
		Text:      "hello world",
		ToolCalls: []providertest.ScriptCall{{ID: "t1", Name: "get_run", Arguments: json.RawMessage(`{"run":"r1"}`)}},
		Usage:     &providertest.ScriptUsage{Prompt: 10, Completion: 5},
	})
	a := New(ts.URL, nil)
	resp, err := collect(t, a, provider.Key{}, provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Text != "hello world" || resp.Stop != provider.StopToolUse {
		t.Fatalf("response = %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "get_run" || string(resp.ToolCalls[0].Arguments) != `{"run":"r1"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestCacheMarkerOnLastToolOnly(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "ok"})
	tools := []agent.ToolDef{
		{Name: "a", Description: "first", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", Description: "second", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	if _, err := collect(t, New(ts.URL, nil), provider.Key{}, provider.Request{Model: "m", Tools: tools,
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(fake.Requests()[0].Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := body.Tools[0]["cache_control"]; ok {
		t.Fatal("the first tool carries a cache marker")
	}
	if body.Tools[1]["cache_control"] == nil {
		t.Fatal("the last tool has no cache marker")
	}
}

func TestCacheMarkerOnSystemText(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "ok"})
	if _, err := collect(t, New(ts.URL, nil), provider.Key{}, provider.Request{Model: "m", System: "You are X.",
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var body struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(fake.Requests()[0].Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.System) != 1 {
		t.Fatalf("system blocks = %d, want 1", len(body.System))
	}
	b := body.System[0]
	if b["type"] != "text" || b["text"] != "You are X." {
		t.Errorf("system block = %v", b)
	}
	cc, _ := b["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" {
		t.Errorf("system cache_control = %v, want ephemeral", b["cache_control"])
	}
}

func TestNoSystemWhenEmpty(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "ok"})
	if _, err := collect(t, New(ts.URL, nil), provider.Key{}, provider.Request{Model: "m",
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(fake.Requests()[0].Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := body["system"]; ok {
		t.Error("an empty system text sends a system key")
	}
}

func TestCachedTokensCountAsPrompt(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{
		Text:  "ok",
		Usage: &providertest.ScriptUsage{Prompt: 3, CacheCreation: 5, CacheRead: 7, Completion: 2},
	})
	resp, err := collect(t, New(ts.URL, nil), provider.Key{}, provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Usage.PromptTokens != 15 {
		t.Fatalf("prompt tokens = %d, want 15 (input, cache creation, cache read)", resp.Usage.PromptTokens)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	t.Parallel()
	if got := New("", nil).base; got != DefaultBaseURL {
		t.Fatalf("base = %q, want %q", got, DefaultBaseURL)
	}
	if got := New("https://gateway.invalid/", nil).base; got != "https://gateway.invalid" {
		t.Fatalf("trailing slash kept: %q", got)
	}
}

func TestKeyHeader(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "ok"})
	key := provider.NewKey([]byte("canary-provider-key-7f3a"))
	if _, err := collect(t, New(ts.URL, nil), key, provider.Request{Model: "m"}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	h := fake.Requests()[0].Header
	if h.Get("x-api-key") != "canary-provider-key-7f3a" {
		t.Fatalf("x-api-key = %q", h.Get("x-api-key"))
	}
	if h.Get("anthropic-version") != APIVersion {
		t.Fatalf("anthropic-version = %q", h.Get("anthropic-version"))
	}
}

func TestIdleLimit(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{Text: "late", DelayMs: 200})
	a := New(ts.URL, nil, WithTimeouts(5*time.Second, 50*time.Millisecond))
	_, err := collect(t, a, provider.Key{}, provider.Request{Model: "m"})
	if !errors.Is(err, provider.ErrIdle) {
		t.Fatalf("error = %v, want ErrIdle", err)
	}
}

func TestHTTPError(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{Error: &providertest.ScriptError{Status: 429, Message: "slow down"}})
	_, err := New(ts.URL, nil).Stream(context.Background(), provider.Key{}, provider.Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("error = %v, want the status 429", err)
	}
}

func TestBrokenStream(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{Text: "cut", Error: &providertest.ScriptError{MidStream: true, Message: "boom"}})
	_, err := collect(t, New(ts.URL, nil), provider.Key{}, provider.Request{Model: "m"})
	if !errors.Is(err, provider.ErrBrokenStream) {
		t.Fatalf("error = %v, want ErrBrokenStream", err)
	}
}

func TestMaxTokensStop(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{Text: "long", Stop: "max_tokens"})
	resp, err := collect(t, New(ts.URL, nil), provider.Key{}, provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Stop != provider.StopMaxTokens {
		t.Fatalf("stop = %s, want max_tokens", resp.Stop)
	}
}

func TestToolResultsMergeIntoUserMessage(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "done"})
	msgs := []provider.Message{
		{Role: provider.RoleUser, Text: "go"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "a", Arguments: json.RawMessage(`{}`)},
			{ID: "c2", Name: "b"},
		}},
		{Role: provider.RoleTool, ToolCallID: "c1", ToolName: "a", Text: "one"},
		{Role: provider.RoleTool, ToolCallID: "c2", ToolName: "b", Text: "bad", IsError: true},
	}
	if _, err := collect(t, New(ts.URL, nil), provider.Key{}, provider.Request{Model: "m", Messages: msgs}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var body struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(fake.Requests()[0].Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (the two tool results merge)", len(body.Messages))
	}
	last := body.Messages[2]
	if last.Role != "user" || len(last.Content) != 2 || last.Content[1]["is_error"] != true {
		t.Fatalf("merged message = %+v", last)
	}
}
