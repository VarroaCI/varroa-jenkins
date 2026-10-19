package mcp

import (
	"net/http"

	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/api"
)

// NewServer builds the MCP server that exposes Varroa's full CRUD surface as
// MCP tools. The HTTP endpoint and the in-process tool host both serve it.
func NewServer(deps *api.Dependencies) *server.MCPServer {
	mcpServer := server.NewMCPServer("varroa", "0.0.0-dev",
		server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(),
	)

	registerAllTools(mcpServer, deps)
	return mcpServer
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
