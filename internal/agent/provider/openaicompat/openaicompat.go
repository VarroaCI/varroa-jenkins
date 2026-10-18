// Package openaicompat is the adapter for the Chat Completions stream. An
// endpoint that serves the same stream, with or without a key, uses this
// adapter.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent/budget"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
)

// Option tunes an adapter.
type Option func(*Adapter)

// WithTimeouts sets the deadline of one call and the idle limit between two
// stream lines.
func WithTimeouts(deadline, idle time.Duration) Option {
	return func(a *Adapter) {
		a.deadline = deadline
		a.idle = idle
	}
}

// Adapter streams Chat Completions calls.
type Adapter struct {
	base     string
	client   *http.Client
	deadline time.Duration
	idle     time.Duration
}

var _ provider.Provider = (*Adapter)(nil)

// New returns an adapter for the base URL. A nil client means a default client.
func New(baseURL string, client *http.Client, opts ...Option) *Adapter {
	if client == nil {
		client = &http.Client{}
	}
	a := &Adapter{
		base:     strings.TrimRight(baseURL, "/"),
		client:   client,
		deadline: provider.CallDeadline,
		idle:     provider.IdleLimit,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Factory builds the adapter for an endpoint.
func Factory(e provider.Endpoint) (provider.Provider, error) {
	return New(e.BaseURL, nil), nil
}

// Stream sends one call and returns its deltas. The channel closes after the
// final delta.
func (a *Adapter) Stream(ctx context.Context, key provider.Key, req provider.Request) (<-chan provider.Delta, error) {
	body, err := buildBody(req)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.deadline)
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, a.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openaicompat: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	key.SetHeader(httpReq.Header, "Authorization", "Bearer ")

	resp, err := a.client.Do(httpReq) //nolint:bodyclose // the body closes on the status path or in the stream goroutine
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openaicompat: send: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("openaicompat: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	ch := make(chan provider.Delta, 16)
	var idleFired atomic.Bool
	idle := time.AfterFunc(a.idle, func() {
		idleFired.Store(true)
		cancel()
	})
	go func() {
		defer close(ch)
		defer cancel()
		defer idle.Stop()
		defer func() { _ = resp.Body.Close() }()
		st := &streamState{requestBytes: len(body)}
		err := provider.ScanSSE(resp.Body, func() { idle.Reset(a.idle) }, func(_, data string) error {
			return st.handle(callCtx, data, ch)
		})
		if st.finished {
			return
		}
		var cause error
		switch {
		case idleFired.Load():
			cause = provider.ErrIdle
		case callCtx.Err() == context.DeadlineExceeded:
			cause = provider.ErrDeadline
		case err != nil:
			cause = fmt.Errorf("%w: %v", provider.ErrBrokenStream, err) //nolint:errorlint // the sentinel is the caller-visible error
		default:
			cause = provider.ErrBrokenStream
		}
		provider.SendFinal(ctx, ch, provider.Delta{Stop: provider.StopError, Err: cause})
	}()
	return ch, nil
}

// streamState holds what one stream reads across its chunks.
type streamState struct {
	requestBytes int
	text         strings.Builder
	argBytes     int
	usage        *provider.Usage
	stop         provider.StopReason
	finished     bool
}

// handle reads one data line. The [DONE] line ends the stream and sends the
// final delta.
func (st *streamState) handle(ctx context.Context, data string, ch chan<- provider.Delta) error {
	emit := func(d provider.Delta) error { return provider.Send(ctx, ch, d) }
	if st.finished {
		return nil
	}
	if data == "[DONE]" {
		st.finished = true
		var u provider.Usage
		if st.usage != nil {
			u = *st.usage
		} else {
			u = provider.Usage{
				PromptTokens:     int64(st.requestBytes / 4),
				CompletionTokens: int64((st.text.Len() + st.argBytes) / 4),
				Estimated:        true,
			}
		}
		stop := st.stop
		if stop == "" {
			stop = provider.StopEndTurn
		}
		return emit(provider.Delta{Usage: &u, Stop: stop})
	}
	var c struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return nil //nolint:nilerr // a malformed event is skipped
	}
	for _, ch0 := range c.Choices {
		if ch0.Delta.Content != "" {
			st.text.WriteString(ch0.Delta.Content)
			if err := emit(provider.Delta{Text: ch0.Delta.Content}); err != nil {
				return err
			}
		}
		for _, tc := range ch0.Delta.ToolCalls {
			st.argBytes += len(tc.Function.Arguments)
			if err := emit(provider.Delta{ToolCall: &provider.ToolCallDelta{
				Index: tc.Index, ID: tc.ID, Name: tc.Function.Name, ArgumentsFragment: tc.Function.Arguments,
			}}); err != nil {
				return err
			}
		}
		if ch0.FinishReason != nil {
			st.stop = finishMapping(*ch0.FinishReason)
		}
	}
	if c.Usage != nil {
		st.usage = &provider.Usage{PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens}
	}
	return nil
}

// finishMapping maps a Chat Completions finish reason to the contract's stop
// reason.
func finishMapping(s string) provider.StopReason {
	switch s {
	case "tool_calls", "function_call":
		return provider.StopToolUse
	case "length":
		return provider.StopMaxTokens
	}
	return provider.StopEndTurn
}

// buildBody builds the request document of one call.
func buildBody(req provider.Request) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = budget.MaxTokensPerCall
	}
	msgs := []map[string]any{}
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case provider.RoleUser:
			msgs = append(msgs, map[string]any{"role": "user", "content": m.Text})
		case provider.RoleAssistant:
			msg := map[string]any{"role": "assistant", "content": nil}
			if m.Text != "" {
				msg["content"] = m.Text
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, c := range m.ToolCalls {
					args := string(c.Arguments)
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{
						"id": c.ID, "type": "function",
						"function": map[string]any{"name": c.Name, "arguments": args},
					})
				}
				msg["tool_calls"] = calls
			}
			msgs = append(msgs, msg)
		case provider.RoleTool:
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": m.ToolCallID, "content": m.Text})
		}
	}
	doc := map[string]any{
		"model":          req.Model,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"max_tokens":     maxTokens,
		"messages":       msgs,
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Schema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params},
			})
		}
		doc["tools"] = tools
	}
	return json.Marshal(doc)
}
