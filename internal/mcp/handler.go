package mcp

import (
	"context"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/agent/guide"
	"github.com/varroaci/varroa-jenkins/internal/api"
)

// NewServer builds the MCP server that exposes Varroa's full CRUD surface as
// MCP tools. The HTTP endpoint and the in-process tool host both serve it.
func NewServer(deps *api.Dependencies) *server.MCPServer {
	mcpServer := server.NewMCPServer("varroa", "0.0.0-dev",
		server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(),
		server.WithInstructions(guide.Text),
	)

	registerAllTools(mcpServer, deps)
	registerGuideResource(mcpServer)
	registerUIResources(mcpServer)
	return mcpServer
}

// registerGuideResource serves the operator guide as the resource guide.URI.
// The text is public, so the resource needs no authorization check.
func registerGuideResource(s *server.MCPServer) {
	resource := mcp.NewResource(guide.URI, "Varroa operator guide",
		mcp.WithResourceDescription("How Varroa works and how to investigate a problem with the Varroa tools."),
		mcp.WithMIMEType("text/markdown"),
	)
	s.AddResource(resource, func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{mcp.TextResourceContents{
			URI:      guide.URI,
			MIMEType: "text/markdown",
			Text:     guide.Text,
		}}, nil
	})
}

// HTTPHandler serves a server from NewServer on the MCP Streamable HTTP
// endpoint.
func HTTPHandler(s *server.MCPServer) http.Handler {
	return server.NewStreamableHTTPServer(s,
		server.WithStateLess(true),
	)
}

// NewHandler creates an HTTP handler that serves the MCP (Model Context Protocol)
// Streamable HTTP endpoint. It exposes Varroa's full CRUD surface as MCP tools.
func NewHandler(deps *api.Dependencies) http.Handler {
	return HTTPHandler(NewServer(deps))
}
