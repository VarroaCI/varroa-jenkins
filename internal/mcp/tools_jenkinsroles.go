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
// JenkinsRole tools (5)
// =============================================================================

func registerJenkinsRoleTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listJR := mcp.NewTool("list_jenkins_roles",
		mcp.WithDescription("List Jenkins roles managed by Varroa. Returns a compact "+
			"summary per role (name, roleType, description, permissionCount) \u2014 enough to "+
			"survey the permission sets defined for the fleet. Use get_jenkins_role for "+
			"the full resource."),
		mcp.WithBoolean("verbose", mcp.Description("Return full JenkinsRole resources instead "+
			"of summaries. Expensive: full resources carry spec.permissions, so a fleet-sized "+
			"listing can exhaust the context window. Prefer get_jenkins_role for detail on "+
			"specific roles.")),
		withListOutput(jenkinsRoleListOutputSchema),
	)
	addTool(mcpServer, kindRead, listJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		roles, err := crdstore.List[v1alpha1.JenkinsRole](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list Jenkins roles: %v", err)), nil
		}
		if verbose, _ := args["verbose"].(bool); verbose {
			return resultJSON(roles)
		}
		summaries := make([]jenkinsRoleSummary, 0, len(roles))
		for _, r := range roles {
			summaries = append(summaries, summarizeJenkinsRole(r))
		}
		return resultJSON(summaries)
	})

	getJR := mcp.NewTool("get_jenkins_role",
		mcp.WithDescription("Get a specific Jenkins role by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
	)
	addTool(mcpServer, kindRead, getJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strArg(getArgs(req), "name")
		role, err := crdstore.Get[v1alpha1.JenkinsRole](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Jenkins role not found: %v", err)), nil
		}
		return resultJSON(role)
	})

	createJR := mcp.NewTool("create_jenkins_role",
		mcp.WithDescription("Create a new Jenkins role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name (DNS-1123)")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithString("roleType", mcp.Description("Role type: Global, Item, Agent; default Global")),
		mcp.WithArray("permissions", mcp.Required(), mcp.Description("Permission IDs")),
	)
	addTool(mcpServer, kindCreate, createJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanCreateJenkinsRole(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsroles:create permission"), nil
		}
		args := getArgs(req)
		role := &v1alpha1.JenkinsRole{
			ObjectMeta: metav1.ObjectMeta{Name: strArg(args, "name")},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    strArg(args, "roleType"),
				Permissions: firstSlice(strSliceArg(args, "permissions")),
				Description: strArg(args, "description"),
			},
		}
		if err := crdstore.Apply[v1alpha1.JenkinsRole](ctx, deps.Store, role); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create Jenkins role: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:    "jenkinsrole.created",
			Message: "JenkinsRole " + strArg(args, "name") + " created",
		})
		return resultJSON(role)
	})

	updateJR := mcp.NewTool("update_jenkins_role",
		mcp.WithDescription("Update an existing Jenkins role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithString("roleType", mcp.Description("Role type: Global, Item, Agent")),
		mcp.WithArray("permissions", mcp.Description("Permission IDs")),
	)
	addTool(mcpServer, kindUpdate, updateJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		if !deps.Authorizer.CanUpdateJenkinsRole(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsroles:update permission"), nil
		}
		existing, err := crdstore.Get[v1alpha1.JenkinsRole](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Jenkins role not found: %v", err)), nil
		}
		if v := strArg(args, "description"); v != "" {
			existing.Spec.Description = v
		}
		if v := strArg(args, "roleType"); v != "" {
			existing.Spec.RoleType = v
		}
		if v, ok := strSliceArg(args, "permissions"); ok {
			existing.Spec.Permissions = v
		}
		if err := crdstore.Apply[v1alpha1.JenkinsRole](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update Jenkins role: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:    "jenkinsrole.updated",
			Message: "JenkinsRole " + name + " updated",
		})
		return resultJSON(existing)
	})

	deleteJR := mcp.NewTool("delete_jenkins_role",
		mcp.WithDescription("Delete a Jenkins role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
	)
	addTool(mcpServer, kindDelete, deleteJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		name := strArg(getArgs(req), "name")
		if !deps.Authorizer.CanDeleteJenkinsRole(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsroles:delete permission"), nil
		}
		if err := crdstore.Delete[v1alpha1.JenkinsRole](ctx, deps.Store, name, ""); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete Jenkins role: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:    "jenkinsrole.deleted",
			Message: "JenkinsRole " + name + " deleted",
		})
		return mcp.NewToolResultText("Jenkins role " + name + " deleted"), nil
	})
}

// jenkinsRoleSummary is the default projection for list_jenkins_roles.
//
// spec.permissions[] is the cost — up to 11 permission IDs per role — and the
// count carries the same operational signal ("how much does this role grant")
// while the detail is one get_jenkins_role away. JenkinsRole is cluster-scoped
// (kubebuilder scope=Cluster), so the summary carries no namespace.
type jenkinsRoleSummary struct {
	Name            string `json:"name"`
	RoleType        string `json:"roleType,omitempty"`
	Description     string `json:"description,omitempty"`
	PermissionCount int    `json:"permissionCount"`
}

func summarizeJenkinsRole(r *v1alpha1.JenkinsRole) jenkinsRoleSummary {
	if r == nil {
		return jenkinsRoleSummary{}
	}
	return jenkinsRoleSummary{
		Name:            r.Name,
		RoleType:        r.Spec.RoleType,
		Description:     r.Spec.Description,
		PermissionCount: len(r.Spec.Permissions),
	}
}

var jenkinsRoleListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "JenkinsRole name."},
	{Name: "roleType", Type: "string", Desc: "Role type: Global, Item, or Agent."},
	{Name: "description", Type: "string", Desc: "Human-readable description."},
	{Name: "permissionCount", Type: "integer", Desc: "Number of permission IDs; get_jenkins_role for the list."},
})
