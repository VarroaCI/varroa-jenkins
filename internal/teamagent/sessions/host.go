// Package sessions runs the Team agent sessions: the HTTP tool host, the write
// decision, the worker pool, the resume scan, and the kill switch.
package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/token"
)

// MCPPath is the BFF path of the MCP endpoint.
const MCPPath = accept.MCPPath

// remintBefore is the time left below which the host mints a new token.
const remintBefore = 5 * time.Minute

// HTTPHost is a loop.ToolHost that calls the BFF MCP endpoint as the Team
// agent. The bearer token travels only in the Authorization header.
type HTTPHost struct {
	url    string
	client *http.Client
	mint   func() (token.Value, error)
	now    func() time.Time
	// outputs names the Team agent output tools that are switched on.
	outputs []string

	mu  sync.Mutex
	tok token.Value
	id  int
	// classes maps each tool name to its class, as List reported it.
	classes map[string]agent.ToolClass
}

// NewHTTPHost returns a host that mints tokens for principal and session sid
// with key. outputs holds the output kinds that the TeamAgent turns on.
func NewHTTPHost(bffURL string, key []byte, principal string, uid types.UID, sid uuid.UUID, outputs []string, now func() time.Time) *HTTPHost {
	if now == nil {
		now = time.Now
	}
	return &HTTPHost{
		url:     strings.TrimSuffix(bffURL, "/") + MCPPath,
		client:  &http.Client{Timeout: 2 * time.Minute},
		now:     now,
		outputs: outputs,
		mint: func() (token.Value, error) {
			return token.Mint(key, principal, uid, sid, now())
		},
	}
}

// rpcRequest and rpcResponse are the JSON-RPC envelopes of the MCP endpoint.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// token returns the current token, minting a new one when little time is left.
func (h *HTTPHost) token() (token.Value, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tok.ExpiresAt().IsZero() || h.tok.ExpiresAt().Sub(h.now()) < remintBefore {
		t, err := h.mint()
		if err != nil {
			return token.Value{}, err
		}
		h.tok = t
	}
	return h.tok, nil
}

// remint forces a new token, after a 401.
func (h *HTTPHost) remint() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, err := h.mint()
	if err != nil {
		return err
	}
	h.tok = t
	return nil
}

// post sends one JSON-RPC request. It retries once with a fresh token on 401.
func (h *HTTPHost) post(ctx context.Context, method string, params any) (json.RawMessage, error) {
	h.mu.Lock()
	h.id++
	id := h.id
	h.mu.Unlock()
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encode rpc: %w", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := h.token()
		if err != nil {
			return nil, fmt.Errorf("mint token: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		tok.SetHeader(req.Header)
		resp, err := h.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("call mcp: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = resp.Body.Close()
			if err := h.remint(); err != nil {
				return nil, fmt.Errorf("remint token: %w", err)
			}
			continue
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("mcp answered HTTP %d", resp.StatusCode)
		}
		raw, err := readRPC(resp.Body, resp.Header.Get("Content-Type"))
		if err != nil {
			return nil, err
		}
		var out rpcResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode rpc reply: %w", err)
		}
		if out.Error != nil {
			return nil, errors.New(out.Error.Message)
		}
		return out.Result, nil
	}
	return nil, errors.New("mcp refused the token twice")
}

// readRPC reads one JSON-RPC reply from a JSON body or from the first data
// line of an event stream.
func readRPC(r io.Reader, contentType string) ([]byte, error) {
	if !strings.HasPrefix(contentType, "text/event-stream") {
		return io.ReadAll(io.LimitReader(r, 16<<20))
	}
	sc := bufio.NewScanner(io.LimitReader(r, 16<<20))
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
			return []byte(strings.TrimSpace(line)), nil
		}
	}
	return nil, errors.New("mcp reply held no data line")
}

// List returns the tools the session may call. An output tool that the
// TeamAgent does not turn on is dropped, so the model never sees it.
func (h *HTTPHost) List(ctx context.Context) ([]agent.ToolDef, error) {
	raw, err := h.post(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
			Annotations struct {
				ReadOnlyHint  *bool `json:"readOnlyHint"`
				OpenWorldHint *bool `json:"openWorldHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode tool list: %w", err)
	}
	enabled := accept.OutputToolNames(h.outputs)
	var out []agent.ToolDef
	for _, t := range list.Tools {
		if isOutputTool(t.Name) && !contains(enabled, t.Name) {
			continue
		}
		class, hidden := policy.ClassFromHints(t.Annotations.ReadOnlyHint, t.Annotations.OpenWorldHint)
		out = append(out, agent.ToolDef{Name: t.Name, Description: t.Description, Schema: t.InputSchema, Class: class, Hidden: hidden})
		h.mu.Lock()
		if h.classes == nil {
			h.classes = map[string]agent.ToolClass{}
		}
		h.classes[t.Name] = class
		h.mu.Unlock()
	}
	return out, nil
}

// isOutputTool reports whether name is one of the Team agent output tools.
func isOutputTool(name string) bool {
	return name == "post_note" || name == "open_issue" || name == "send_webhook"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Call runs one tool and returns its text content.
func (h *HTTPHost) Call(ctx context.Context, name string, args json.RawMessage) (agent.ToolResult, error) {
	res, err := h.call(ctx, name, args)
	outcome := "ok"
	switch {
	case err != nil:
		outcome = "transport_error"
	case res.IsError:
		outcome = "tool_error"
	}
	h.mu.Lock()
	class := string(h.classes[name])
	h.mu.Unlock()
	if class == "" {
		class = "unknown"
	}
	ToolCalls.WithLabelValues(class, outcome).Inc()
	return res, err
}

// call runs one tool call without the metric.
func (h *HTTPHost) call(ctx context.Context, name string, args json.RawMessage) (agent.ToolResult, error) {
	var arguments map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return agent.ToolResult{Content: "arguments must be a JSON object", IsError: true}, nil //nolint:nilerr // a bad argument is a tool error, not a Go error
		}
	}
	raw, err := h.post(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return agent.ToolResult{}, err
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode tool result: %w", err)
	}
	var b strings.Builder
	for _, c := range out.Content {
		b.WriteString(c.Text)
	}
	return agent.ToolResult{Content: b.String(), IsError: out.IsError}, nil
}

// compile-time check that HTTPHost satisfies the loop's tool host.
var _ loop.ToolHost = (*HTTPHost)(nil)
