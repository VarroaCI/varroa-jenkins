//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// Forge tools (1): open_pull_request. The handler calls api.Server.OpenPullRequest,
// so the operator's forge path is the only one that writes.
func registerForgeTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	registerOpenPullRequestTool(mcpServer, deps)
}

func registerOpenPullRequestTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	tool := mcp.NewTool("open_pull_request",
		mcp.WithDescription("Open one pull request on a team's repository from a new branch with the given files. The branch must not exist. In an assistant session every call waits for a person's approval, and the repo must be the session's repo."),
		mcp.WithString("team", mcp.Required(), mcp.Description("The team name")),
		mcp.WithString("repo", mcp.Required(), mcp.Description("An https repository URL")),
		mcp.WithString("branch", mcp.Required(), mcp.Description("A new branch name, at most 200 characters")),
		mcp.WithString("title", mcp.Required(), mcp.Description("1 to 200 characters, one line")),
		mcp.WithString("body", mcp.Description("At most 8192 bytes")),
		mcp.WithArray("files", mcp.Required(),
			mcp.Description("1 to 10 files; each content at most 65536 bytes, 262144 bytes in total"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"path", "content"},
			})),
	)
	addTool(mcpServer, kindCreate, tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		var files []forgeapi.File
		if err := marshalUnmarshal(args["files"], &files); err != nil {
			return mcp.NewToolResultError("invalid_argument: files: " + err.Error()), nil
		}
		team := strArg(args, "team")
		res, serr := api.NewServer(deps).OpenPullRequest(ctx, claims, api.OpenPullRequestArgs{
			Team:   team,
			Repo:   strArg(args, "repo"),
			Branch: strArg(args, "branch"),
			Title:  strArg(args, "title"),
			Body:   strArg(args, "body"),
			Files:  files,
		})
		if serr != nil {
			return mcp.NewToolResultError(serr.Message), nil
		}
		emitActivityForCluster(ctx, deps, claims, res.Cluster, activity.Event{
			Type:    "pipeline.proposal.opened",
			Message: fmt.Sprintf("Pull request #%d opened for team %s", res.Number, team),
			URL:     res.URL,
		})
		return resultJSON(map[string]any{"url": res.URL, "number": res.Number})
	})
}
