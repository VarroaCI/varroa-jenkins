// Package providertest holds a scripted fake model provider for tests. The
// server answers the Messages and Chat Completions streams with the turns of a
// script, one turn per request. It records every request.
package providertest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// Script is an ordered list of turns. Each request consumes the next turn.
type Script struct {
	Turns []Turn `json:"turns"`
}

// Turn is one scripted answer.
type Turn struct {
	Text      string       `json:"text"`
	ToolCalls []ScriptCall `json:"toolCalls"`
	Usage     *ScriptUsage `json:"usage"`
	// Stop is end_turn, tool_use, or max_tokens. It defaults to tool_use when
	// the turn has calls, and to end_turn otherwise.
	Stop    string       `json:"stop"`
	Error   *ScriptError `json:"error"`
	DelayMs int          `json:"delayMs"`
}

// ScriptCall is one scripted tool call.
type ScriptCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ScriptUsage is the scripted token usage. The Messages shape reports the
// cache fields, and the Chat Completions shape reports Prompt only.
type ScriptUsage struct {
	Prompt        int64 `json:"prompt"`
	Completion    int64 `json:"completion"`
	CacheCreation int64 `json:"cacheCreation"`
	CacheRead     int64 `json:"cacheRead"`
}

// ScriptError is a scripted failure. A failure that is not mid-stream answers
// the status with a JSON body. A mid-stream failure sends the first event and
// then ends the stream with no terminal event.
type ScriptError struct {
	Status    int    `json:"status"`
	Message   string `json:"message"`
	MidStream bool   `json:"midStream"`
}

// Recorded is one request that the server received.
type Recorded struct {
	Path   string      `json:"path"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
}

// Load reads a script file.
func Load(path string) (Script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Script{}, fmt.Errorf("read script: %w", err)
	}
	var s Script
	if err := json.Unmarshal(raw, &s); err != nil {
		return Script{}, fmt.Errorf("decode script: %w", err)
	}
	return s, nil
}

// Server is the fake provider. Its zero value is not usable; use New.
type Server struct {
	mu         sync.Mutex
	turns      []Turn
	next       int
	requests   []Recorded
	recordFile string
}

// New returns a server that answers with the turns of s, in order.
func New(s Script) *Server {
	return &Server{turns: append([]Turn(nil), s.Turns...)}
}

// SetRecordFile appends every later request to path, one JSON line each.
func (s *Server) SetRecordFile(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordFile = path
}

// Requests returns the recorded requests in arrival order.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Recorded, len(s.requests))
	copy(out, s.requests)
	return out
}

// ServeHTTP answers the two stream paths and returns 404 for any other path.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	rec := Recorded{Path: r.URL.Path, Header: r.Header.Clone(), Body: body}

	s.mu.Lock()
	s.requests = append(s.requests, rec)
	if s.recordFile != "" {
		appendLine(s.recordFile, rec)
	}
	if s.next >= len(s.turns) {
		s.mu.Unlock()
		http.Error(w, "no scripted turn left", http.StatusInternalServerError)
		return
	}
	turn := s.turns[s.next]
	s.next++
	s.mu.Unlock()

	messages := r.URL.Path == "/v1/messages"
	if turn.Error != nil && !turn.Error.MidStream {
		status := turn.Error.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": turn.Error.Message}})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if turn.DelayMs > 0 {
		select {
		case <-time.After(time.Duration(turn.DelayMs) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	if messages {
		writeMessages(w, turn)
	} else {
		writeChat(w, turn)
	}
}

// appendLine writes one record as a JSON line. A write error is ignored, so a
// broken record file cannot fail a scripted answer.
func appendLine(path string, rec Recorded) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

// stopOf returns the stop reason of a turn, with its default.
func stopOf(t Turn) string {
	if t.Stop != "" {
		return t.Stop
	}
	if len(t.ToolCalls) > 0 {
		return "tool_use"
	}
	return "end_turn"
}

// splitHalves splits s into two non-empty fragments. A string shorter than two
// bytes gives one fragment.
func splitHalves(s string) []string {
	if len(s) < 2 {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	mid := len(s) / 2
	return []string{s[:mid], s[mid:]}
}

// sendEvent writes one Messages-shape event.
func sendEvent(w http.ResponseWriter, event string, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// sendData writes one Chat Completions chunk.
func sendData(w http.ResponseWriter, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeMessages streams a turn in the Messages shape.
func writeMessages(w http.ResponseWriter, t Turn) {
	var u ScriptUsage
	if t.Usage != nil {
		u = *t.Usage
	}
	sendEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant", "model": "fake",
			"content": []any{},
			"usage": map[string]any{
				"input_tokens":                u.Prompt,
				"cache_creation_input_tokens": u.CacheCreation,
				"cache_read_input_tokens":     u.CacheRead,
				"output_tokens":               0,
			},
		},
	})
	if t.Error != nil && t.Error.MidStream {
		sendEvent(w, "error", map[string]any{
			"type": "error", "error": map[string]any{"type": "api_error", "message": t.Error.Message},
		})
		return
	}
	index := 0
	if t.Text != "" {
		sendEvent(w, "content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		for _, frag := range splitHalves(t.Text) {
			sendEvent(w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "text_delta", "text": frag},
			})
		}
		sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	for _, c := range t.ToolCalls {
		sendEvent(w, "content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": map[string]any{}},
		})
		args := string(c.Arguments)
		if args == "" {
			args = "{}"
		}
		for _, frag := range splitHalves(args) {
			sendEvent(w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
			})
		}
		sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	sendEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopOf(t)},
		"usage": map[string]any{"output_tokens": u.Completion},
	})
	sendEvent(w, "message_stop", map[string]any{"type": "message_stop"})
}

// chatFinish maps a Messages stop reason to a Chat Completions finish reason.
func chatFinish(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	}
	return "stop"
}

// writeChat streams a turn in the Chat Completions shape.
func writeChat(w http.ResponseWriter, t Turn) {
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": "fake",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	if t.Error != nil && t.Error.MidStream {
		sendData(w, chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		return
	}
	for _, frag := range splitHalves(t.Text) {
		sendData(w, chunk(map[string]any{"content": frag}, nil))
	}
	for i, c := range t.ToolCalls {
		sendData(w, chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": ""},
		}}}, nil))
		args := string(c.Arguments)
		if args == "" {
			args = "{}"
		}
		for _, frag := range splitHalves(args) {
			sendData(w, chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": i, "function": map[string]any{"arguments": frag},
			}}}, nil))
		}
	}
	sendData(w, chunk(map[string]any{}, chatFinish(stopOf(t))))
	if t.Usage != nil {
		sendData(w, map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": "fake", "choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     t.Usage.Prompt,
				"completion_tokens": t.Usage.Completion,
			},
		})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
