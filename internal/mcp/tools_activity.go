//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// =============================================================================
// Activity, Search, Me/Meta tools (4)
// =============================================================================

func registerActivitySearchMetaTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	activityTool := mcp.NewTool("list_activity",
		mcp.WithDescription("List recent Varroa activity events, capped at limit "+
			"(default 50, max 200). The activity stream is unbounded by design, "+
			"so only the most recent limit events are returned — narrow with "+
			"the controller filter instead of raising the cap. Events are already "+
			"flat; every field is returned."),
		mcp.WithString("controller", mcp.Description("Optional controller name filter")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of events to return (default 50, max 200)")),
		withListOutput(activityListOutputSchema),
	)
	addTool(mcpServer, kindRead, activityTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		controller, _ := args["controller"].(string)
		limit := 50
		if n, ok := args["limit"].(float64); ok {
			limit = int(n)
			if limit < 1 {
				limit = 1
			}
			if limit > 200 {
				limit = 200
			}
		}
		// Read through deps.Backfill — the same source the REST /activity
		// handler uses. In stream mode that is the JetStream backfill; with
		// retention off it is the ring wrapper. deps.ActivityStore must NOT be
		// read here: the ring is only fed in retention-off mode, so reading it
		// directly returns an eternally empty feed on any stream-mode brood.
		if deps.Backfill == nil {
			return resultJSON([]activity.Event{})
		}
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		query := activity.Query{Controller: controller, Limit: limit}
		query.Authorize = func(e activity.Event) bool {
			return deps.Authorizer.CanReadActivityEvent(claims, e)
		}
		page, qerr := deps.Backfill.Query(ctx, query)
		if qerr != nil {
			return mcp.NewToolResultError("failed to load activity: " + qerr.Error()), nil
		}
		return resultJSON(page.Items)
	})

	searchTool := mcp.NewTool("search",
		mcp.WithDescription("Search Jenkins controllers across the brood by case-insensitive "+
			"substring match on name and namespace. Returns the same compact summaries as "+
			"list_controllers, capped at 50 hits."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query string")),
		mcp.WithString("namespace", mcp.Description("Optional namespace filter")),
		withListOutput(controllerListOutputSchema),
	)
	addTool(mcpServer, kindRead, searchTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := getArgs(req)["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("query parameter is required"), nil
		}
		needle := strings.ToLower(query)
		ns := namespaceOrDefault(getArgs(req))
		const maxHits = 50

		// When an Authorizer is wired, claims are mandatory — treating a
		// missing identity as "no filter" would let an unauthenticated caller
		// enumerate controller names across the brood.
		var claims *auth.Claims
		if deps.Authorizer != nil {
			var err error
			claims, err = requireClaims(ctx)
			if err != nil {
				return mcp.NewToolResultError("authentication required"), nil
			}
		}

		matches := func(cluster string, cr *v1alpha1.Controller) *controllerSummary {
			if deps.Authorizer != nil &&
				!deps.Authorizer.CanReadController(claims, cr.Namespace, cr.Name) {
				return nil
			}
			if !strings.Contains(strings.ToLower(cr.Name), needle) &&
				!strings.Contains(strings.ToLower(cr.Namespace), needle) {
				return nil
			}
			s := summarizeController(cluster, cr)
			return &s
		}

		hits := make([]controllerSummary, 0, 16)
		if deps.Brood != nil {
			cc, _, err := deps.Brood.ListAll(ctx, ns, "")
			if err != nil {
				return mcp.NewToolResultError("search failed: " + err.Error()), nil
			}
			for _, c := range cc {
				if s := matches(c.Cluster, c.CR); s != nil {
					hits = append(hits, *s)
					if len(hits) == maxHits {
						break
					}
				}
			}
			return resultJSON(hits)
		}
		controllers, err := crdstore.List[v1alpha1.Controller](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError("search failed: " + err.Error()), nil
		}
		for _, cr := range controllers {
			// Cluster is left empty: without a brood there is no authoritative
			// cluster name to report (same rule as list_controllers).
			if s := matches("", cr); s != nil {
				hits = append(hits, *s)
				if len(hits) == maxHits {
					break
				}
			}
		}
		return resultJSON(hits)
	})

	meTool := mcp.NewTool("get_me",
		mcp.WithDescription("Get the current authenticated user's profile"),
	)
	addTool(mcpServer, kindRead, meTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		return resultJSON(map[string]interface{}{
			"subject":            claims.Subject,
			"preferred_username": claims.PreferredUsername,
			"name":               claims.Name,
			"email":              claims.Email,
			"groups":             claims.Groups,
		})
	})

	permsTool := mcp.NewTool("get_my_permissions",
		mcp.WithDescription("Get the current user's effective RBAC permissions"),
		mcp.WithString("namespace", mcp.Description("Optional namespace scope")),
		mcp.WithString("controllerName", mcp.Description("Optional controller name scope")),
	)
	addTool(mcpServer, kindRead, permsTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer != nil {
			caps := deps.Authorizer.EffectivePermissions(claims)
			return resultJSON(caps)
		}
		return resultJSON(map[string]interface{}{})
	})
}

// activityListOutputSchema describes the result of list_activity.
//
// The event shape is already flat (activity.Event serializes directly), so
// there is no summary struct: every field is operationally useful and the
// unbounded item count is what needed a cap, not a projection. buildNumber is
// an int64, hence "integer"; everything else is a string.
var activityListOutputSchema = listOutputSchema("Activity event.", []schemaField{
	{Name: "timestamp", Type: "string", Desc: "Event timestamp, RFC3339."},
	{Name: "type", Type: "string", Desc: "Event type."},
	{Name: "source", Type: "string", Desc: "Producer: mite, operator, user, api, jenkins, or mcp."},
	{Name: "actor", Type: "string", Desc: "Acting user email, operator, or empty."},
	{Name: "controller", Type: "string", Desc: "Controller the event concerns."},
	{Name: "namespace", Type: "string", Desc: "Controller namespace."},
	{Name: "cluster", Type: "string", Desc: "Cluster the controller lives on."},
	{Name: "message", Type: "string", Desc: "Human-readable event message."},
	{Name: "phase", Type: "string", Desc: "Lifecycle phase at the time of the event."},
	{Name: "reason", Type: "string", Desc: "Machine-readable reason."},
	{Name: "severity", Type: "string", Desc: "info, warning, or error."},
	{Name: "itemPath", Type: "string", Desc: "Catalog item path, if any."},
	{Name: "buildNumber", Type: "integer", Desc: "Jenkins build number, if any."},
	{Name: "result", Type: "string", Desc: "Build result, if any."},
	{Name: "url", Type: "string", Desc: "Link to the build, if any."},
})
