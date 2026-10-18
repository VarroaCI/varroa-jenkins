package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/providertest"
)

func fakeServer(t *testing.T, turns ...providertest.Turn) (*httptest.Server, *providertest.Server) {
	t.Helper()
	fake := providertest.New(providertest.Script{Turns: turns})
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	return ts, fake
}

func collect(t *testing.T, a *Adapter, key provider.Key, req provider.Request) (provider.Response, error) {
	t.Helper()
	ch, err := a.Stream(context.Background(), key, req)
	if err != nil {
		return provider.Response{}, err
	}
	return provider.Collect(ch)
}

// TestToolCallsJoinedByIndex asserts that fragments with one index form one call.
func TestToolCallsJoinedByIndex(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{
		Text: "checking",
		ToolCalls: []providertest.ScriptCall{
			{ID: "c1", Name: "list_runs", Arguments: json.RawMessage(`{"team":"acme"}`)},
			{ID: "c2", Name: "get_run", Arguments: json.RawMessage(`{"run":"r1"}`)},
		},
	})
	resp, err := collect(t, New(ts.URL+"/v1", nil), provider.Key{}, provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "c1" || resp.ToolCalls[1].Name != "get_run" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if string(resp.ToolCalls[0].Arguments) != `{"team":"acme"}` {
		t.Fatalf("arguments = %s", resp.ToolCalls[0].Arguments)
	}
	if resp.Stop != provider.StopToolUse {
		t.Fatalf("stop = %s, want tool_use", resp.Stop)
	}
}

// TestKeylessHasNoAuthorizationHeader asserts that an empty key sends no header.
func TestKeylessHasNoAuthorizationHeader(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "ok"})
	if _, err := collect(t, New(ts.URL+"/v1", nil), provider.Key{}, provider.Request{Model: "m"}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if fake.Requests()[0].Header.Get("Authorization") != "" {
		t.Fatal("a keyless call sent an Authorization header")
	}
}

// TestBearerHeader asserts that a key is sent as a bearer token.
func TestBearerHeader(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "ok"})
	key := provider.NewKey([]byte("canary-provider-key-7f3a"))
	if _, err := collect(t, New(ts.URL+"/v1", nil), key, provider.Request{Model: "m"}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := fake.Requests()[0].Header.Get("Authorization"); got != "Bearer canary-provider-key-7f3a" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestIncludeUsageRequested asserts that the request asks for the usage chunk.
func TestIncludeUsageRequested(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "ok"})
	if _, err := collect(t, New(ts.URL+"/v1", nil), provider.Key{}, provider.Request{Model: "m"}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var body struct {
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(fake.Requests()[0].Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Stream || !body.StreamOptions.IncludeUsage {
		t.Fatalf("body = %+v, want stream and include_usage", body)
	}
}

// TestUsageEstimatedWhenMissing asserts the size estimate when no usage chunk arrives.
func TestUsageEstimatedWhenMissing(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{Text: "abcdefgh"})
	resp, err := collect(t, New(ts.URL+"/v1", nil), provider.Key{}, provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !resp.Usage.Estimated || resp.Usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v, want an estimate of 2 completion tokens", resp.Usage)
	}
	if resp.Usage.PromptTokens == 0 {
		t.Fatal("prompt estimate is zero")
	}
}

// TestFinishReasonMapping walks the finish reasons.
func TestFinishReasonMapping(t *testing.T) {
	t.Parallel()
	cases := map[string]provider.StopReason{
		"end_turn":   provider.StopEndTurn,
		"tool_use":   provider.StopToolUse,
		"max_tokens": provider.StopMaxTokens,
	}
	for in, want := range cases {
		ts, _ := fakeServer(t, providertest.Turn{Text: "x", Stop: in})
		resp, err := collect(t, New(ts.URL+"/v1", nil), provider.Key{}, provider.Request{Model: "m"})
		if err != nil {
			t.Fatalf("%s: Collect: %v", in, err)
		}
		if resp.Stop != want {
			t.Fatalf("%s: stop = %s, want %s", in, resp.Stop, want)
		}
	}
	if got := finishMapping("function_call"); got != provider.StopToolUse {
		t.Fatalf("function_call = %s, want tool_use", got)
	}
}

// TestToolMessages asserts the shape of an assistant message with calls and a tool message.
func TestToolMessages(t *testing.T) {
	t.Parallel()
	ts, fake := fakeServer(t, providertest.Turn{Text: "ok"})
	msgs := []provider.Message{
		{Role: provider.RoleUser, Text: "go"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "a", Arguments: json.RawMessage(`{}`)}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Text: "result"},
	}
	tools := []agent.ToolDef{{Name: "a", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}}
	if _, err := collect(t, New(ts.URL+"/v1", nil), provider.Key{}, provider.Request{Model: "m", Messages: msgs, Tools: tools}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(fake.Requests()[0].Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Messages[2]["role"] != "tool" || body.Messages[2]["tool_call_id"] != "c1" {
		t.Fatalf("tool message = %v", body.Messages[2])
	}
	if body.Messages[1]["content"] != nil {
		t.Fatalf("empty assistant content = %v, want null", body.Messages[1]["content"])
	}
	if body.Tools[0]["type"] != "function" {
		t.Fatalf("tool = %v", body.Tools[0])
	}
}

// TestBrokenStream asserts that a stream without the DONE line is an error.
func TestBrokenStream(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{Text: "cut", Error: &providertest.ScriptError{MidStream: true, Message: "boom"}})
	_, err := collect(t, New(ts.URL+"/v1", nil), provider.Key{}, provider.Request{Model: "m"})
	if !errors.Is(err, provider.ErrBrokenStream) {
		t.Fatalf("error = %v, want ErrBrokenStream", err)
	}
}

// TestIdleLimitOpenAI asserts the idle limit applies to this adapter too.
func TestIdleLimitOpenAI(t *testing.T) {
	t.Parallel()
	ts, _ := fakeServer(t, providertest.Turn{Text: "late", DelayMs: 200})
	a := New(ts.URL+"/v1", nil, WithTimeouts(5*time.Second, 50*time.Millisecond))
	_, err := collect(t, a, provider.Key{}, provider.Request{Model: "m"})
	if !errors.Is(err, provider.ErrIdle) {
		t.Fatalf("error = %v, want ErrIdle", err)
	}
}
