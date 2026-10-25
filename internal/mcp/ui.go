package mcp

import (
	"context"
	"embed"
	"fmt"
	"maps"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/api"
)

// The MCP Apps views. Each view is one static HTML document. A host that
// supports MCP Apps renders it beside the text result of its tool.

//go:embed ui/*.html
var uiFS embed.FS

const uiMIMEType = "text/html;profile=mcp-app"

// uiViewTools is the single list of tools that advertise a view. Each name
// has exactly one embedded file ui/<name>.html. A test checks both directions.
var uiViewTools = []string{"get_cell_usage", "get_controller", "get_run", "list_fleet_locks"}

func uiResourceURI(tool string) string { return "ui://varroa/" + tool }

// withUIView sets _meta.ui.resourceUri and keeps every other _meta key.
// It writes only the nested key: the flat "ui/resourceUri" key is deprecated.
func withUIView(tool string) mcp.ToolOption {
	return func(t *mcp.Tool) {
		fields := map[string]any{}
		if t.Meta != nil {
			maps.Copy(fields, t.Meta.AdditionalFields)
		}
		fields["ui"] = map[string]any{"resourceUri": uiResourceURI(tool)}
		t.Meta = mcp.NewMetaFromMap(fields)
	}
}

// registerUIResources serves each view document. The HTML is static and
// public, so the read needs no caller identity. The data reaches the view
// only through the tool result, which keeps its own checks.
func registerUIResources(s *server.MCPServer) {
	for _, tool := range uiViewTools {
		b, err := uiFS.ReadFile("ui/" + tool + ".html")
		if err != nil {
			panic(fmt.Sprintf("mcp: view for %s is not embedded: %v", tool, err))
		}
		uri, text := uiResourceURI(tool), string(b)
		s.AddResource(
			mcp.NewResource(uri, tool+" view",
				mcp.WithResourceDescription("Read-only card for the "+tool+" result."),
				mcp.WithMIMEType(uiMIMEType)),
			func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return []mcp.ResourceContents{mcp.TextResourceContents{URI: uri, MIMEType: uiMIMEType, Text: text}}, nil
			})
	}
}

// dashboardLink joins the dashboard origin and a page path. An empty origin
// gives an empty link, and the view then hides its button.
func dashboardLink(deps *api.Dependencies, path string) string {
	if deps == nil || deps.DashboardURL == "" || path == "" {
		return ""
	}
	return strings.TrimRight(deps.DashboardURL, "/") + path
}

// withViewMeta sets result _meta for a view. resultJSON stays the one
// chokepoint that sanitizes the payload. An error result carries no _meta.
// An empty string value or a nil value is dropped.
func withViewMeta(res *mcp.CallToolResult, err error, fields map[string]any) (*mcp.CallToolResult, error) {
	if err != nil || res == nil || res.IsError {
		return res, err
	}
	out := map[string]any{}
	for k, v := range fields {
		switch x := v.(type) {
		case nil:
			continue
		case string:
			if x == "" {
				continue
			}
		case *api.AttentionJSON:
			if x == nil {
				continue
			}
		}
		out[k] = v
	}
	if len(out) == 0 {
		return res, nil
	}
	res.Meta = mcp.NewMetaFromMap(out)
	return res, nil
}
