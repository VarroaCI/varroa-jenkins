//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// Team agent tools (4): read_repo_file, post_note, open_issue, send_webhook.
// Each calls the matching api.Server method, so the HTTP surface and the tool
// share one authorization path. A tool serves a Team agent session and a
// person's assistant session alike.
func registerTeamAgentTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	registerReadRepoFileTool(mcpServer, deps)
	registerPostNoteTool(mcpServer, deps)
	registerOpenIssueTool(mcpServer, deps)
	registerSendWebhookTool(mcpServer, deps)
}

func registerReadRepoFileTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	tool := mcp.NewTool("read_repo_file",
		mcp.WithDescription("Read one file or one directory of a team's repository. Inside a session the repository is the session's own, and the revision is the one the session pinned. Outside a session it is the team agent's instructions source. A file reply holds at most 32768 bytes per call; pass offset and length to read on."),
		mcp.WithString("team", mcp.Required(), mcp.Description("The team name")),
		mcp.WithString("path", mcp.Required(), mcp.Description("A path inside the repository, such as .varroa/team-agent.md")),
		mcp.WithString("repo", mcp.Description("A repository URL. Outside a session it needs the tenant write check. Inside a session it must be the session's own repo")),
		mcp.WithNumber("offset", mcp.Description("Byte offset of the first byte to return. Defaults to 0")),
		mcp.WithNumber("length", mcp.Description("Bytes to return, at most 32768. Defaults to 32768")),
	)
	addTool(mcpServer, kindRead, tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		team := strArg(args, "team")
		path := strArg(args, "path")
		if team == "" || path == "" {
			return mcp.NewToolResultError("team and path are required"), nil
		}
		body, serr := api.NewServer(deps).ReadRepoFile(ctx, claims, api.ReadRepoFileArgs{
			Team:   team,
			Path:   path,
			Repo:   strArg(args, "repo"),
			Offset: int64Arg(args, "offset"),
			Length: int64Arg(args, "length"),
		})
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		return resultJSON(body)
	})
}

func registerPostNoteTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	tool := mcp.NewTool("post_note",
		mcp.WithDescription("Write a note on a run, or on a job. A session writes one note; a repeat call answers the stored note with created false. A note holds at most 16384 bytes."),
		mcp.WithString("run", mcp.Description("Run identifier of the run to annotate. Give run, or team and job")),
		mcp.WithString("team", mcp.Description("The team name, for a job note")),
		mcp.WithString("job", mcp.Description("The job path, for a job note")),
		mcp.WithString("body", mcp.Required(), mcp.Description("The note text")),
	)
	addTool(mcpServer, kindCreate, tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		resp, serr := api.NewServer(deps).PostNote(ctx, claims, api.PostNoteArgs{
			Run:  strArg(args, "run"),
			Team: strArg(args, "team"),
			Job:  strArg(args, "job"),
			Body: strArg(args, "body"),
		})
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		emitActivity(ctx, deps, claims, activity.Event{
			Type:    "team_agent.note_posted",
			Message: "Note posted on " + noteTarget(args),
		})
		out := map[string]any{"created": resp.Created}
		if resp.Note != nil {
			out["note"] = resp.Note
		}
		if !resp.Created {
			out["message"] = "this session already posted its note"
		}
		return resultJSON(out)
	})
}

func registerOpenIssueTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	tool := mcp.NewTool("open_issue",
		mcp.WithDescription("Open an issue on the forge of a team. The team's TeamAgent must enable the issues output. A label that the repository lacks refuses the call."),
		mcp.WithString("team", mcp.Required(), mcp.Description("The team name")),
		mcp.WithString("title", mcp.Required(), mcp.Description("The issue title, at most 256 characters")),
		mcp.WithString("body", mcp.Description("The issue body, at most 65536 bytes")),
		mcp.WithArray("labels", mcp.Description("Labels to set on the issue"), mcp.WithStringItems()),
	)
	addTool(mcpServer, kindCreate, tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		reply, serr := api.NewServer(deps).OpenIssue(ctx, claims, api.OpenIssueArgs{
			Team:   strArg(args, "team"),
			Title:  strArg(args, "title"),
			Body:   strArg(args, "body"),
			Labels: stringsArg(args, "labels"),
		})
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		emitActivity(ctx, deps, claims, activity.Event{
			Type:    "team_agent.issue_opened",
			Message: "Issue opened for team " + strArg(args, "team") + ": " + strArg(args, "title"),
		})
		return resultJSON(map[string]any{"number": reply.Number, "url": reply.URL})
	})
}

func registerSendWebhookTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	tool := mcp.NewTool("send_webhook",
		mcp.WithDescription("Queue one signed message to the completion webhook of a team. The team's TeamAgent must enable the webhook output, and the team must set a webhook. The body is a JSON object of at most 16 KiB."),
		mcp.WithString("team", mcp.Required(), mcp.Description("The team name")),
		mcp.WithObject("body", mcp.Required(), mcp.Description("The JSON object to send")),
	)
	addTool(mcpServer, kindAction, tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		raw, err := json.Marshal(args["body"])
		if err != nil {
			return mcp.NewToolResultError("body must be a JSON object"), nil
		}
		deliveryID, serr := api.NewServer(deps).SendWebhook(ctx, claims, api.SendWebhookArgs{
			Team: strArg(args, "team"),
			Body: raw,
		})
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		emitActivity(ctx, deps, claims, activity.Event{
			Type:    "team_agent.webhook_sent",
			Message: "Webhook queued for team " + strArg(args, "team"),
		})
		return resultJSON(map[string]any{"delivery_id": deliveryID})
	})
}

// noteTarget names the run or the job a note is written on, for the activity
// message.
func noteTarget(args map[string]any) string {
	if run := strArg(args, "run"); run != "" {
		return "run " + run
	}
	return "job " + strArg(args, "job") + " of team " + strArg(args, "team")
}

// int64Arg reads a numeric argument. JSON numbers arrive as float64.
func int64Arg(args map[string]any, key string) int64 {
	switch v := args[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// stringsArg reads an array of strings. Non-string items are dropped.
func stringsArg(args map[string]any, key string) []string {
	raw, _ := args[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
