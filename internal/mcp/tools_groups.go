//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// =============================================================================
// Group tools (3) — admin-only
// =============================================================================

func registerGroupTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listG := mcp.NewTool("list_groups",
		mcp.WithDescription("List all Varroa groups (admin only). Returns a compact "+
			"summary per group (name, displayName, memberCount) \u2014 enough to "+
			"survey who has access without shipping spec.members. Group is "+
			"cluster-scoped, so summaries carry no namespace."),
		mcp.WithBoolean("verbose", mcp.Description("Return full Group resources instead of "+
			"summaries. Expensive at scale; prefer get_group for detail on one object.")),
		withListOutput(groupListOutputSchema),
	)
	addTool(mcpServer, kindRead, listG, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.IsAdmin(claims) {
			return mcp.NewToolResultError("access denied: admin only"), nil
		}
		groups, err := crdstore.List[v1alpha1.Group](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError("failed to list groups: " + err.Error()), nil
		}
		if verbose, _ := getArgs(req)["verbose"].(bool); verbose {
			return resultJSON(groups)
		}
		summaries := make([]groupSummary, 0, len(groups))
		for _, g := range groups {
			summaries = append(summaries, summarizeGroup(g))
		}
		return resultJSON(summaries)
	})

	createG := mcp.NewTool("create_group",
		mcp.WithDescription("Create a new Varroa group (admin only, local auth only)"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Group name (DNS-1123)")),
		mcp.WithArray("members", mcp.Description("Group members")),
	)
	addTool(mcpServer, kindCreate, createG, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.IsAdmin(claims) {
			return mcp.NewToolResultError("access denied: admin only"), nil
		}
		args := getArgs(req)
		group := &v1alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: strArg(args, "name")},
			Spec:       v1alpha1.GroupSpec{Members: firstSlice(strSliceArg(args, "members"))},
		}
		if err := crdstore.Apply[v1alpha1.Group](ctx, deps.Store, group); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create group: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:    "group.created",
			Message: "Group " + strArg(args, "name") + " created",
		})
		return resultJSON(group)
	})

	deleteG := mcp.NewTool("delete_group",
		mcp.WithDescription("Delete a Varroa group (admin only)"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Group name")),
	)
	addTool(mcpServer, kindDelete, deleteG, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.IsAdmin(claims) {
			return mcp.NewToolResultError("access denied: admin only"), nil
		}
		name, _ := getArgs(req)["name"].(string)
		if err := crdstore.Delete[v1alpha1.Group](ctx, deps.Store, name, ""); err != nil {
			return mcp.NewToolResultError("failed to delete group: " + err.Error()), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:    "group.deleted",
			Message: "Group " + name + " deleted",
		})
		return mcp.NewToolResultText("group " + name + " deleted"), nil
	})
}

// groupSummary is the default projection for list_groups.
//
// The cost of a raw Group CR is spec.members: a group's membership list is the
// whole payload, and "how many members" carries the same survey signal as the
// list itself. Group is cluster-scoped, so there is no namespace to project.
type groupSummary struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	MemberCount int    `json:"memberCount"`
}

func summarizeGroup(g *v1alpha1.Group) groupSummary {
	if g == nil {
		return groupSummary{}
	}
	return groupSummary{
		Name:        g.Name,
		DisplayName: g.Spec.DisplayName,
		MemberCount: len(g.Spec.Members),
	}
}

var groupListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "Group name."},
	{Name: "displayName", Type: "string", Desc: "Human-readable display name."},
	{Name: "memberCount", Type: "integer", Desc: "Number of members in the group."},
})
