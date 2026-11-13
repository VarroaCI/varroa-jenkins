package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/varroaci/varroa-jenkins/internal/agent/guide"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestHTTPHandlerServesBothProtocolEras pins that one endpoint serves the
// stateless 2026-07-28 protocol and the earlier handshake protocol. Both eras
// return the operator guide, publish strict schemas, and refuse an undeclared
// argument with the same text.
func TestHTTPHandlerServesBothProtocolEras(t *testing.T) {
	h := HTTPHandler(NewServer(adminDeps()))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(auth.ContextWithClaims(r.Context(), mcpAdminClaims)))
	}))
	t.Cleanup(srv.Close)

	cases := []struct {
		name string
		opts []client.ClientOption
		want string
	}{
		{name: "stateless", want: mcpgo.LATEST_PROTOCOL_VERSION},
		{name: "handshake", opts: []client.ClientOption{client.WithProtocolVersion(mcpgo.ProtocolVersion20251125)}, want: mcpgo.ProtocolVersion20251125},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tr, err := transport.NewStreamableHTTP(srv.URL)
			if err != nil {
				t.Fatalf("transport: %v", err)
			}
			c := client.NewClient(tr, tc.opts...)
			if err := c.Start(ctx); err != nil {
				t.Fatalf("start: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })

			res, err := c.Initialize(ctx, mcpgo.InitializeRequest{
				Params: mcpgo.InitializeParams{
					ProtocolVersion: mcpgo.LATEST_PROTOCOL_VERSION,
					ClientInfo:      mcpgo.Implementation{Name: "era-test", Version: "1"},
				},
			})
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			if c.ProtocolVersion() != tc.want {
				t.Errorf("protocol version = %q, want %q", c.ProtocolVersion(), tc.want)
			}
			if res.Instructions != guide.Text {
				t.Errorf("instructions do not match the operator guide")
			}

			list, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
			if err != nil {
				t.Fatalf("list tools: %v", err)
			}
			if len(list.Tools) == 0 {
				t.Fatal("no tools listed")
			}
			for _, tool := range list.Tools {
				if tool.InputSchema.AdditionalProperties != false {
					t.Errorf("%s additionalProperties = %v, want false", tool.Name, tool.InputSchema.AdditionalProperties)
				}
			}

			call, err := c.CallTool(ctx, mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
				Name:      "get_me",
				Arguments: map[string]interface{}{undeclaredArg: 1},
			}})
			if err != nil {
				t.Fatalf("call tool: %v", err)
			}
			if !call.IsError {
				t.Fatalf("get_me accepted the undeclared argument: %v", call.Content)
			}
			var sb strings.Builder
			for _, content := range call.Content {
				if txt, ok := content.(mcpgo.TextContent); ok {
					sb.WriteString(txt.Text)
				}
			}
			if !strings.Contains(sb.String(), notAllowedText(undeclaredArg)) {
				t.Errorf("text = %q, want it to contain %q", sb.String(), notAllowedText(undeclaredArg))
			}
		})
	}
}
