// Package anthropic is the adapter for the Messages API protocol. The
// provider kind is named after the protocol. Any endpoint that speaks the
// Messages stream can use this adapter.
package anthropic

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

// DefaultBaseURL is the base URL that an endpoint with an empty baseURL uses.
const DefaultBaseURL = "https://api.anthropic.com"

const (
	versionHeader = "anthropic-version"
	// APIVersion is the value of the version header that every call sends.
	APIVersion = "2023-06-01"
	keyHeader  = "x-api-key"
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

// Adapter streams Messages API calls.
type Adapter struct {
	base     string
	client   *http.Client
	deadline time.Duration
	idle     time.Duration
}

var _ provider.Provider = (*Adapter)(nil)

// New returns an adapter for the base URL. An empty base URL means
// DefaultBaseURL, and a nil client means a default client.
func New(baseURL string, client *http.Client, opts ...Option) *Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
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
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, a.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set(versionHeader, APIVersion)
	key.SetHeader(httpReq.Header, keyHeader, "")

	resp, err := a.client.Do(httpReq) //nolint:bodyclose // the body closes on the status path or in the stream goroutine
	if err != nil {
		cancel()
		return nil, fmt.Errorf("anthropic: send: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
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
		st := &streamState{}
		err := provider.ScanSSE(resp.Body, func() { idle.Reset(a.idle) }, func(event, data string) error {
			return st.handle(callCtx, event, data, ch)
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

// streamState holds what one stream reads across its events.
type streamState struct {
	prompt, completion       int64
	cacheCreation, cacheRead int64
	toolIndex                int
	blockTools               map[int]int
	stop                     provider.StopReason
	finished                 bool
}

// handle reads one event. It sends the deltas of the event and reports the
// final delta with finished set.
func (st *streamState) handle(ctx context.Context, event, data string, ch chan<- provider.Delta) error {
	emit := func(d provider.Delta) error { return provider.Send(ctx, ch, d) }
	if st.finished {
		return nil
	}
	switch event {
	case "message_start":
		var m struct {
			Message struct {
				Usage struct {
					Input         int64 `json:"input_tokens"`
					CacheCreation int64 `json:"cache_creation_input_tokens"`
					CacheRead     int64 `json:"cache_read_input_tokens"`
					Output        int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return nil //nolint:nilerr // a malformed event is skipped
		}
		u := m.Message.Usage
		st.prompt = u.Input + u.CacheCreation + u.CacheRead
		st.completion = u.Output
		st.cacheCreation, st.cacheRead = u.CacheCreation, u.CacheRead
	case "content_block_start":
		var b struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(data), &b); err != nil {
			return nil //nolint:nilerr // a malformed event is skipped
		}
		if b.ContentBlock.Type == "tool_use" {
			if st.blockTools == nil {
				st.blockTools = map[int]int{}
			}
			idx := st.toolIndex
			st.toolIndex++
			st.blockTools[b.Index] = idx
			if err := emit(provider.Delta{ToolCall: &provider.ToolCallDelta{Index: idx, ID: b.ContentBlock.ID, Name: b.ContentBlock.Name}}); err != nil {
				return err
			}
		}
	case "content_block_delta":
		var d struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return nil //nolint:nilerr // a malformed event is skipped
		}
		switch d.Delta.Type {
		case "text_delta":
			if d.Delta.Text != "" {
				if err := emit(provider.Delta{Text: d.Delta.Text}); err != nil {
					return err
				}
			}
		case "input_json_delta":
			if idx, ok := st.blockTools[d.Index]; ok {
				if err := emit(provider.Delta{ToolCall: &provider.ToolCallDelta{Index: idx, ArgumentsFragment: d.Delta.PartialJSON}}); err != nil {
					return err
				}
			}
		}
	case "message_delta":
		var m struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Input         int64 `json:"input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				Output        int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return nil //nolint:nilerr // a malformed event is skipped
		}
		if p := m.Usage.Input + m.Usage.CacheCreation + m.Usage.CacheRead; p != 0 {
			st.prompt = p
			st.cacheCreation, st.cacheRead = m.Usage.CacheCreation, m.Usage.CacheRead
		}
		if m.Usage.Output != 0 {
			st.completion = m.Usage.Output
		}
		st.stop = stopMapping(m.Delta.StopReason)
	case "message_stop":
		st.finished = true
		stop := st.stop
		if stop == "" {
			stop = provider.StopEndTurn
		}
		if err := emit(provider.Delta{
			Usage: &provider.Usage{
				PromptTokens:        st.prompt,
				CompletionTokens:    st.completion,
				CacheCreationTokens: st.cacheCreation,
				CacheReadTokens:     st.cacheRead,
			},
			Stop: stop,
		}); err != nil {
			return err
		}
	case "error":
		st.finished = true
		msg := data
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		if err := emit(provider.Delta{Stop: provider.StopError, Err: fmt.Errorf("%w: %s", provider.ErrBrokenStream, msg)}); err != nil {
			return err
		}
	}
	return nil
}

// stopMapping maps a Messages stop reason to the contract's stop reason.
func stopMapping(s string) provider.StopReason {
	switch s {
	case "tool_use":
		return provider.StopToolUse
	case "max_tokens":
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
	doc := map[string]any{
		"model":      req.Model,
		"max_tokens": maxTokens,
		"stream":     true,
	}
	if req.System != "" {
		doc["system"] = []map[string]any{{
			"type":          "text",
			"text":          req.System,
			"cache_control": map[string]any{"type": "ephemeral"},
		}}
	}
	msgs := convertMessages(req.Messages)
	markLastBlock(msgs)
	doc["messages"] = msgs
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for i, t := range req.Tools {
			schema := t.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tool := map[string]any{"name": t.Name, "description": t.Description, "input_schema": schema}
			if i == len(req.Tools)-1 {
				tool["cache_control"] = map[string]any{"type": "ephemeral"}
			}
			tools = append(tools, tool)
		}
		doc["tools"] = tools
	}
	return json.Marshal(doc)
}

// markLastBlock sets a cache breakpoint on the last content block of the last
// message. Only that block is marked, so the request keeps within the provider
// limit of four breakpoints. An empty text block is skipped because the API
// refuses a cache breakpoint on it.
func markLastBlock(msgs []map[string]any) {
	if len(msgs) == 0 {
		return
	}
	content, ok := msgs[len(msgs)-1]["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		return
	}
	last := content[len(content)-1]
	if last["type"] == "text" && last["text"] == "" {
		return
	}
	last["cache_control"] = map[string]any{"type": "ephemeral"}
}

// convertMessages maps the contract messages to Messages API messages. Consecutive
// tool results merge into one user message.
func convertMessages(msgs []provider.Message) []map[string]any {
	var out []map[string]any
	for _, m := range msgs {
		switch m.Role {
		case provider.RoleUser:
			out = append(out, map[string]any{
				"role":    "user",
				"content": []map[string]any{{"type": "text", "text": m.Text}},
			})
		case provider.RoleAssistant:
			var content []map[string]any
			if m.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Text})
			}
			for _, c := range m.ToolCalls {
				input := c.Arguments
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": input})
			}
			out = append(out, map[string]any{"role": "assistant", "content": content})
		case provider.RoleTool:
			block := map[string]any{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Text, "is_error": m.IsError,
			}
			if n := len(out); n > 0 && out[n-1]["role"] == "user" && isToolResults(out[n-1]) {
				content := out[n-1]["content"].([]map[string]any)
				out[n-1]["content"] = append(content, block)
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": []map[string]any{block}})
		}
	}
	return out
}

// isToolResults reports whether a user message holds tool results.
func isToolResults(m map[string]any) bool {
	content, ok := m["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		return false
	}
	t, _ := content[0]["type"].(string)
	return t == "tool_result"
}
