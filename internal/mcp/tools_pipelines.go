//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// =============================================================================
// Pipeline tools (8): run_pipeline, list_runs, get_run, get_run_log,
// list_fleet_locks, proceed_run_input, abort_run_input, replay_run
//
// Design D1a: these tools call the same internal services the
// pipelines HTTP handlers call (internal/api's RunPipeline, ListRuns,
// GetRun, GetRunLog, ListFleetLocks, ProceedRunInput, AbortRunInput,
// ReplayRun), so
// each tool authorizes and reads exactly as its HTTP counterpart does.
// No tool here calls a controller-scoped check; the authorization table
// lives in specs/pipelines-api.
// =============================================================================

// defaultListRunsLimit and maxListRunsLimit match list_activity's clamp
// (mcp-jenkins-proxy: "list_runs SHALL take limit with a default of 50
// and a maximum of 200 ... matching the activity tool").
const (
	defaultListRunsLimit = 50
	maxListRunsLimit     = 200

	defaultGetRunLogTailLines = 100
	minGetRunLogTailLines     = 1
	maxGetRunLogTailLines     = 500
)

func registerPipelinesTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	registerRunPipelineTool(mcpServer, deps)
	registerListRunsTool(mcpServer, deps)
	registerGetRunTool(mcpServer, deps)
	registerGetRunLogTool(mcpServer, deps)
	registerListFleetLocksTool(mcpServer, deps)
	registerProceedRunInputTool(mcpServer, deps)
	registerAbortRunInputTool(mcpServer, deps)
	registerReplayRunTool(mcpServer, deps)
}

// registerReplayRunTool registers replay_run: it runs a completed build
// again with an edited Groovy script as the calling user, publishes one
// REPLAY_BUILD command, and reports no outcome. No read tool answers a
// run's script, because a script can reach 256 KiB.
func registerReplayRunTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	replayTool := mcp.NewTool("replay_run",
		mcp.WithDescription("Run a completed build again with an edited Groovy script, as the "+
			"calling user. The BFF mints a fresh run identifier and publishes a REPLAY_BUILD "+
			"command; it does not wait for the build to start. Use get_run to follow it."),
		mcp.WithString("run", mcp.Required(), mcp.Description("Run identifier of the build to replay")),
		mcp.WithString("script", mcp.Required(), mcp.Description("The Groovy script the replay runs")),
	)
	addTool(mcpServer, kindAction, replayTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, toolErr := requirePipelineCaller(ctx, deps)
		if toolErr != nil {
			return toolErr, nil
		}
		args := getArgs(req)
		run := strArg(args, "run")
		if run == "" {
			return mcp.NewToolResultError("run is required"), nil
		}
		runID, intentKey, targetCluster, serr := api.NewServer(deps).ReplayRun(ctx, claims, run, strArg(args, "script"))
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		// One activity event records the request, on the cluster the
		// command reached, which can differ from the cluster this BFF serves.
		emitActivityForCluster(deps, claims, targetCluster, activity.Event{
			Type:    "pipeline.run_replay_requested",
			Message: fmt.Sprintf("Replay of run %s requested (run %s)", run, runID),
		})
		return resultJSON(map[string]any{"runId": runID, "intentKey": intentKey, "state": "requested"})
	})
}

// requirePipelineCaller guards a nil authorizer and requires claims, the
// one check every pipeline tool performs before it reads or writes
// anything (mcp-jenkins-proxy: "A nil authorizer SHALL deny every tool
// with an error, never bypass the gate").
func requirePipelineCaller(ctx context.Context, deps *api.Dependencies) (*auth.Claims, *mcp.CallToolResult) {
	if deps.Authorizer == nil {
		return nil, mcp.NewToolResultError("authorizer not configured")
	}
	claims, err := requireClaims(ctx)
	if err != nil {
		return nil, mcp.NewToolResultError("authentication required")
	}
	return claims, nil
}

func registerRunPipelineTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	runPipelineTool := mcp.NewTool("run_pipeline",
		mcp.WithDescription("Start a build of a pipeline job. The BFF mints a fresh run "+
			"identifier for every call and publishes a TRIGGER_BUILD command; it does not "+
			"wait for the build to start. Use get_run or get_run_log to follow it."),
		mcp.WithString("team", mcp.Required(), mcp.Description("Team, or the controller name for a single-controller tenant")),
		mcp.WithString("job", mcp.Required(), mcp.Description("Full job path, e.g. checkout-api/main")),
		mcp.WithObject("parameters", mcp.Description("Build parameters, as name to string value")),
		mcp.WithString("cluster", mcp.Description("Cluster to select, when team and job resolve to more than one tenant")),
	)
	addTool(mcpServer, kindAction, runPipelineTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, toolErr := requirePipelineCaller(ctx, deps)
		if toolErr != nil {
			return toolErr, nil
		}
		args := getArgs(req)
		team := strArg(args, "team")
		job := strArg(args, "job")
		if team == "" || job == "" {
			return mcp.NewToolResultError("team and job are required"), nil
		}
		var parameters map[string]string
		if raw := args["parameters"]; raw != nil {
			if err := marshalUnmarshal(raw, &parameters); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid parameters: %v", err)), nil
			}
		}
		runID, intentKey, targetCluster, serr := api.NewServer(deps).RunPipeline(ctx, claims, team, job, strArg(args, "cluster"), parameters)
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		emitActivityForCluster(deps, claims, targetCluster, activity.Event{
			Type:    "pipeline.run_requested",
			Message: fmt.Sprintf("Pipeline run requested for %s (run %s)", job, runID),
		})
		return resultJSON(map[string]any{"runId": runID, "intentKey": intentKey})
	})
}

// registerProceedRunInputTool registers proceed_run_input: it answers one
// paused input step of a run as the calling user, publishes one
// PROCEED_INPUT command, and reports no outcome. The caller reads the run
// again to learn the result.
func registerProceedRunInputTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	proceedTool := mcp.NewTool("proceed_run_input",
		mcp.WithDescription("Answer a paused input step of a run, as the calling user. "+
			"The BFF publishes a PROCEED_INPUT command and does not wait for Jenkins. "+
			"Use get_run to read the run's remaining inputs."),
		mcp.WithString("run", mcp.Required(), mcp.Description("Run identifier")),
		mcp.WithString("input", mcp.Required(), mcp.Description("Input identifier, from get_run's inputs list")),
		mcp.WithObject("parameters", mcp.Description("Input parameters, as name to string value")),
	)
	addTool(mcpServer, kindAction, proceedTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, toolErr := requirePipelineCaller(ctx, deps)
		if toolErr != nil {
			return toolErr, nil
		}
		args := getArgs(req)
		run := strArg(args, "run")
		input := strArg(args, "input")
		if run == "" || input == "" {
			return mcp.NewToolResultError("run and input are required"), nil
		}
		var parameters map[string]string
		if raw := args["parameters"]; raw != nil {
			if err := marshalUnmarshal(raw, &parameters); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid parameters: %v", err)), nil
			}
		}
		targetCluster, serr := api.NewServer(deps).ProceedRunInput(ctx, claims, run, input, parameters)
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		// One activity event records the request, on the run's own
		// cluster, which can differ from the cluster this BFF serves.
		emitActivityForCluster(deps, claims, targetCluster, activity.Event{
			Type:    "pipeline.run_input_proceeded",
			Message: fmt.Sprintf("Run %s input %s proceed requested", run, input),
		})
		return resultJSON(map[string]any{"state": "requested"})
	})
}

// registerAbortRunInputTool registers abort_run_input: it aborts one
// paused input step of a run as the calling user, publishes one
// ABORT_INPUT command, and reports no outcome.
func registerAbortRunInputTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	abortTool := mcp.NewTool("abort_run_input",
		mcp.WithDescription("Abort a paused input step of a run, as the calling user. "+
			"The BFF publishes an ABORT_INPUT command and does not wait for Jenkins. "+
			"Use get_run to read the run's remaining inputs."),
		mcp.WithString("run", mcp.Required(), mcp.Description("Run identifier")),
		mcp.WithString("input", mcp.Required(), mcp.Description("Input identifier, from get_run's inputs list")),
	)
	addTool(mcpServer, kindAction, abortTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, toolErr := requirePipelineCaller(ctx, deps)
		if toolErr != nil {
			return toolErr, nil
		}
		args := getArgs(req)
		run := strArg(args, "run")
		input := strArg(args, "input")
		if run == "" || input == "" {
			return mcp.NewToolResultError("run and input are required"), nil
		}
		targetCluster, serr := api.NewServer(deps).AbortRunInput(ctx, claims, run, input)
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		emitActivityForCluster(deps, claims, targetCluster, activity.Event{
			Type:    "pipeline.run_input_aborted",
			Message: fmt.Sprintf("Run %s input %s abort requested", run, input),
		})
		return resultJSON(map[string]any{"state": "requested"})
	})
}

func registerListRunsTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listRunsTool := mcp.NewTool("list_runs",
		mcp.WithDescription("List runs of one pipeline job, newest first, merged across every "+
			"tenant the job path resolves to. Capped at limit (default 50, max 200)."),
		mcp.WithString("team", mcp.Required(), mcp.Description("Team, or the controller name for a single-controller tenant")),
		mcp.WithString("job", mcp.Required(), mcp.Description("Full job path, e.g. checkout-api/main")),
		mcp.WithString("branch", mcp.Description("Filter by branch")),
		mcp.WithString("result", mcp.Description("Filter by result: SUCCESS, FAILURE, UNSTABLE, or ABORTED")),
		mcp.WithString("actor", mcp.Description("Filter by the user who started the run")),
		mcp.WithString("cause", mcp.Description("Filter by cause text")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of runs to return (default 50, max 200)")),
		mcp.WithString("cursor", mcp.Description("Opaque cursor from a previous call's nextCursor")),
		withListOutput(runListOutputSchema),
	)
	addTool(mcpServer, kindRead, listRunsTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, toolErr := requirePipelineCaller(ctx, deps)
		if toolErr != nil {
			return toolErr, nil
		}
		args := getArgs(req)
		team := strArg(args, "team")
		job := strArg(args, "job")
		if team == "" || job == "" {
			return mcp.NewToolResultError("team and job are required"), nil
		}
		limit := defaultListRunsLimit
		if n, ok := args["limit"].(float64); ok {
			limit = int(n)
			if limit < 1 {
				limit = 1
			}
			if limit > maxListRunsLimit {
				limit = maxListRunsLimit
			}
		}
		page, serr, err := api.NewServer(deps).ListRuns(ctx, claims, team, job,
			strArg(args, "branch"), strArg(args, "result"), strArg(args, "actor"), strArg(args, "cause"),
			limit, strArg(args, "cursor"))
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		if err != nil {
			return mcp.NewToolResultError("list runs failed: " + err.Error()), nil
		}
		items := page.Items
		if items == nil {
			items = []api.RunSummary{}
		}
		return resultJSON(map[string]any{
			"items":      items,
			"count":      len(items),
			"nextCursor": page.NextCursor,
		})
	})
}

func registerGetRunTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	getRunTool := mcp.NewTool("get_run",
		mcp.WithDescription("Get one run's detail: status, timing, params, test counts, and stages."),
		mcp.WithString("run", mcp.Required(), mcp.Description("Run id (UUIDv7)")),
	)
	addTool(mcpServer, kindRead, getRunTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, toolErr := requirePipelineCaller(ctx, deps)
		if toolErr != nil {
			return toolErr, nil
		}
		run := strArg(getArgs(req), "run")
		if run == "" {
			return mcp.NewToolResultError("run is required"), nil
		}
		detail, serr := api.NewServer(deps).GetRun(ctx, claims, run)
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		return resultJSON(detail)
	})
}

func registerGetRunLogTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	getRunLogTool := mcp.NewTool("get_run_log",
		mcp.WithDescription("Get the tail of a run's console log, with Jenkins console notes "+
			"removed. Returns a bounded amount of text, never a stream; an agent that needs "+
			"live output polls. tailLines defaults to 100 and clamps between 1 and 500."),
		mcp.WithString("run", mcp.Required(), mcp.Description("Run id (UUIDv7)")),
		mcp.WithNumber("tailLines", mcp.Description("Number of trailing log lines to return (default 100, max 500)")),
	)
	addTool(mcpServer, kindRead, getRunLogTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, toolErr := requirePipelineCaller(ctx, deps)
		if toolErr != nil {
			return toolErr, nil
		}
		args := getArgs(req)
		run := strArg(args, "run")
		if run == "" {
			return mcp.NewToolResultError("run is required"), nil
		}
		tailLines := defaultGetRunLogTailLines
		if n, ok := args["tailLines"].(float64); ok {
			tailLines = int(n)
			if tailLines < minGetRunLogTailLines {
				tailLines = minGetRunLogTailLines
			}
			if tailLines > maxGetRunLogTailLines {
				tailLines = maxGetRunLogTailLines
			}
		}
		text, totalLines, serr := api.NewServer(deps).GetRunLog(ctx, claims, run, tailLines)
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		return resultJSON(map[string]any{"text": text, "totalLines": totalLines})
	})
}

// runListOutputSchema describes list_runs' items: api.RunSummary
// serializes directly.
var runListOutputSchema = listOutputSchema("Run summary.", []schemaField{
	{Name: "runId", Type: "string", Desc: "Run id (UUIDv7)."},
	{Name: "buildNumber", Type: "integer", Desc: "Jenkins build number, once assigned."},
	{Name: "jobPath", Type: "string", Desc: "Full job path."},
	{Name: "status", Type: "string", Desc: "QUEUED, RUNNING, or COMPLETED."},
	{Name: "result", Type: "string", Desc: "SUCCESS, FAILURE, UNSTABLE, or ABORTED, once known."},
	{Name: "branch", Type: "string", Desc: "SCM branch, if any."},
	{Name: "scmRevision", Type: "string", Desc: "SCM revision, if any."},
	{Name: "actor", Type: "string", Desc: "User who started the run, if any."},
	{Name: "cause", Type: "string", Desc: "Short cause description."},
	{Name: "queuedAt", Type: "string", Desc: "Queue time, RFC3339."},
	{Name: "startedAt", Type: "string", Desc: "Start time, RFC3339."},
	{Name: "endedAt", Type: "string", Desc: "End time, RFC3339."},
	{Name: "durationMs", Type: "integer", Desc: "Duration in milliseconds, once ended."},
})

func registerListFleetLocksTool(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listFleetLocksTool := mcp.NewTool("list_fleet_locks",
		mcp.WithDescription("List the fleet locks a team's tenants own, hold, or wait on. "+
			"A lock under another tenant's prefix appears when one of the team's own runs "+
			"holds or waits on it."),
		mcp.WithString("team", mcp.Required(), mcp.Description("Team, or the controller name for a single-controller tenant")),
		withListOutput(fleetLockListOutputSchema),
	)
	addTool(mcpServer, kindRead, listFleetLocksTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, toolErr := requirePipelineCaller(ctx, deps)
		if toolErr != nil {
			return toolErr, nil
		}
		team := strArg(getArgs(req), "team")
		if team == "" {
			return mcp.NewToolResultError("team is required"), nil
		}
		items, serr := api.NewServer(deps).ListFleetLocks(ctx, claims, team)
		if serr != nil {
			return mcp.NewToolResultError(serr.Error()), nil
		}
		return resultJSON(map[string]any{"items": items, "count": len(items)})
	})
}

// fleetLockListOutputSchema describes list_fleet_locks' items:
// api.FleetLockRow serializes directly.
var fleetLockListOutputSchema = listOutputSchema("Fleet lock.", []schemaField{
	{Name: "name", Type: "string", Desc: "Qualified lock name, <tenant>/<name>."},
	{Name: "tenant", Type: "string", Desc: "Tenant whose run holds or waits on the lock."},
	{Name: "jobPath", Type: "string", Desc: "Full job path of the holder's or waiter's run."},
	{Name: "state", Type: "string", Desc: "waiting, held, or releasing."},
	{Name: "runId", Type: "string", Desc: "Run that holds or waits on the lock."},
	{Name: "runLink", Type: "string", Desc: "Run link, present only when the caller may read that run."},
	{Name: "waitingSince", Type: "string", Desc: "Time the row began to wait, RFC3339."},
	{Name: "acquiredAt", Type: "string", Desc: "Time the lock was acquired, once held."},
})
