package mcp

import (
	"net/http"

	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/api"
)

// NewHandler creates an HTTP handler that serves the MCP (Model Context Protocol)
// Streamable HTTP endpoint. It exposes Varroa's full CRUD surface as MCP tools.
func NewHandler(deps *api.Dependencies) http.Handler {
	mcpServer := server.NewMCPServer("varroa", "0.0.0-dev",
		server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(),
	)

	registerAllTools(mcpServer, deps)

	return server.NewStreamableHTTPServer(mcpServer,
		server.WithStateLess(true),
	)
}
