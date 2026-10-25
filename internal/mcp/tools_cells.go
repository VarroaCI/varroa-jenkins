//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/api"
)

// =============================================================================
// Cell tool (1): get_cell_usage
//
// The tool calls the same internal service the single-cell HTTP path
// calls, so the two surfaces authorize and read identically. It issues no
// HTTP request to the BFF.
//
// It is a cell read, beside list_controllers, get_controller, and
// get_controller_logs, which call the controller read check today. The
// prohibition on a controller-scoped check binds the four pipeline tools;
// it does not bind this tool. A nil authorizer denies the tool: it is
// stricter than the three controller tools, which skip the check in that
// state.
// =============================================================================

func registerCellTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	registerGetCellUsageTool(mcpServer, deps)
}

func registerGetCellUsageTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	getCellUsageTool := mcp.NewTool("get_cell_usage",
		mcp.WithDescription("Get one cell's load, tenant by tenant: the runs that run now, the "+
			"runs and failures inside a fixed 24-hour window, and the job count. Each tenant row "+
			"also carries its role, its agent namespace, and its placement generation. The response "+
			"also carries the tenant cap, the free slots, and the Jenkins queue length."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("The cell's namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("The cell's name")),
		mcp.WithString("cluster", mcp.Description("Cluster to read (default: the local cluster)")),
		withUIView("get_cell_usage"),
	)
	addTool(mcpServer, kindRead, getCellUsageTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.RecordStore == nil {
			return mcp.NewToolResultError("record store not configured"), nil
		}

		args := getArgs(req)
		namespace := strArg(args, "namespace")
		name := strArg(args, "name")
		if namespace == "" || name == "" {
			return mcp.NewToolResultError("namespace and name are required"), nil
		}
		cluster := strArg(args, "cluster")
		if cluster == "" && deps.Brood != nil {
			cluster = deps.Brood.LocalCluster()
		}

		usage, serr := api.NewServer(deps).CellUsage(ctx, claims, cluster, namespace, name)
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		path := ""
		if usage.Cluster != "" {
			path = api.DashboardControllerPath(usage.Cluster, usage.Namespace, usage.Name) + "?tab=cell"
		}
		res, rerr := resultJSON(usage)
		return withViewMeta(res, rerr, map[string]any{"dashboardLink": dashboardLink(deps, path)})
	})
}
