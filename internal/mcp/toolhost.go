package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// InProcessHost is a tool host for the assistant loop. It calls an MCP server
// in the same process through HandleMessage, with the caller's claims. Each
// call runs the same handler code that the HTTP endpoint runs.
type InProcessHost struct {
	srv    *server.MCPServer
	claims *auth.Claims
	origin string
}

// NewInProcessHost returns a host that acts as the caller of claims.
func NewInProcessHost(s *server.MCPServer, claims *auth.Claims) *InProcessHost {
	return NewInProcessHostWithOrigin(s, claims, OriginAssistant)
}

// NewInProcessHostWithOrigin is NewInProcessHost for a caller that stamps its
// own origin, such as a Team agent session.
func NewInProcessHostWithOrigin(s *server.MCPServer, claims *auth.Claims, origin string) *InProcessHost {
	return &InProcessHost{srv: s, claims: claims, origin: origin}
}

// callCtx returns the context of one tool call: the caller's claims and the
// assistant origin.
func (h *InProcessHost) callCtx(ctx context.Context) context.Context {
	return WithOrigin(auth.ContextWithClaims(ctx, h.claims), h.origin)
}

type rpcEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// send runs one JSON-RPC request through the server and returns the decoded
// envelope. The request id is fixed because the call is in process.
func (h *InProcessHost) send(ctx context.Context, method string, params any) (rpcEnvelope, error) {
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return rpcEnvelope{}, err
	}
	resp, err := json.Marshal(h.srv.HandleMessage(h.callCtx(ctx), raw))
	if err != nil {
		return rpcEnvelope{}, err
	}
	var env rpcEnvelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return rpcEnvelope{}, err
	}
	return env, nil
}

// List returns the tools of the server, each with the class and visibility
// that its MCP hints give.
func (h *InProcessHost) List(ctx context.Context) ([]agent.ToolDef, error) {
	return listTools(ctx, h.send)
}

// rpcSender sends one JSON-RPC request and returns its envelope.
type rpcSender func(ctx context.Context, method string, params any) (rpcEnvelope, error)

// listTools maps the tools of tools/list to tool definitions.
func listTools(ctx context.Context, send rpcSender) ([]agent.ToolDef, error) {
	env, err := send(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	if env.Error != nil {
		return nil, fmt.Errorf("tools/list: %s", env.Error.Message)
	}
	var result struct {
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
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	out := make([]agent.ToolDef, 0, len(result.Tools))
	for _, t := range result.Tools {
		class, hidden := policy.ClassFromHints(t.Annotations.ReadOnlyHint, t.Annotations.OpenWorldHint)
		out = append(out, agent.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.InputSchema,
			Class:       class,
			Hidden:      hidden,
		})
	}
	return out, nil
}

// Call runs one tool. A tool error comes back as a ToolResult with IsError
// set. A JSON-RPC error, such as an unknown tool, comes back the same way and
// the returned error stays nil.
func (h *InProcessHost) Call(ctx context.Context, name string, args json.RawMessage) (agent.ToolResult, error) {
	return callTool(ctx, h.send, name, args)
}

// callTool runs one tool through tools/call.
func callTool(ctx context.Context, send rpcSender, name string, args json.RawMessage) (agent.ToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	env, err := send(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	if env.Error != nil {
		return agent.ToolResult{Content: env.Error.Message, IsError: true}, nil
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return agent.ToolResult{}, errors.New("tools/call: malformed result")
	}
	var texts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	return agent.ToolResult{Content: strings.Join(texts, "\n"), IsError: result.IsError}, nil
}
