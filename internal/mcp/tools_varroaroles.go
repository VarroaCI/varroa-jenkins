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
// VarroaRole tools (5)
// =============================================================================

func registerVarroaRoleTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listR := mcp.NewTool("list_varroa_roles",
		mcp.WithDescription("List Varroa RBAC roles. Returns a compact "+
			"summary per role (name, jenkinsRoleRef, apiRuleCount, "+
			"jenkinsPermissionCount) \u2014 enough to survey a fleet and spot roles "+
			"that carry inline permissions instead of a JenkinsRoleRef. Use "+
			"get_varroa_role for the full resource."),
		mcp.WithBoolean("verbose", mcp.Description("Return full VarroaRole resources instead "+
			"of summaries. Expensive: full resources carry spec.apiRules[].resources/verbs "+
			"and spec.jenkinsPermissions[], so a fleet-sized listing can exhaust the context "+
			"window. Prefer get_varroa_role for detail on a specific role.")),
		withListOutput(varroaRoleListOutputSchema),
	)
	addTool(mcpServer, kindRead, listR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		roles, err := crdstore.List[v1alpha1.VarroaRole](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list roles: %v", err)), nil
		}
		if verbose, _ := args["verbose"].(bool); verbose {
			return resultJSON(roles)
		}
		summaries := make([]varroaRoleSummary, 0, len(roles))
		for _, r := range roles {
			summaries = append(summaries, summarizeVarroaRole(r))
		}
		return resultJSON(summaries)
	})

	getR := mcp.NewTool("get_varroa_role",
		mcp.WithDescription("Get a specific Varroa RBAC role by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
	)
	addTool(mcpServer, kindRead, getR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strArg(getArgs(req), "name")
		role, err := crdstore.Get[v1alpha1.VarroaRole](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("role not found: %v", err)), nil
		}
		return resultJSON(role)
	})

	createR := mcp.NewTool("create_varroa_role",
		mcp.WithDescription("Create a new Varroa RBAC role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name (DNS-1123)")),
		mcp.WithString("jenkinsRoleRef", mcp.Description("Reference to a JenkinsRole name")),
		mcp.WithArray("apiRules", mcp.Description("API permission rules")),
		mcp.WithArray("jenkinsPermissions", mcp.Description("Jenkins permissions (legacy inline)")),
	)
	addTool(mcpServer, kindCreate, createR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanCreateRole(claims) {
			return mcp.NewToolResultError("access denied: missing roles:create permission"), nil
		}
		args := getArgs(req)
		role := &v1alpha1.VarroaRole{
			ObjectMeta: metav1.ObjectMeta{Name: strArg(args, "name")},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsRoleRef:     strArg(args, "jenkinsRoleRef"),
				JenkinsPermissions: firstSlice(strSliceArg(args, "jenkinsPermissions")),
			},
		}
		if rules, ok := args["apiRules"].([]interface{}); ok {
			for _, r := range rules {
				if rm, ok := r.(map[string]interface{}); ok {
					rule := v1alpha1.APIRule{
						Resources: strSliceAny(rm["resources"]),
						Verbs:     strSliceAny(rm["verbs"]),
					}
					role.Spec.APIRules = append(role.Spec.APIRules, rule)
				}
			}
		}
		if err := crdstore.Apply[v1alpha1.VarroaRole](ctx, deps.Store, role); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create role: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:    "varroarole.created",
			Message: "VarroaRole " + strArg(args, "name") + " created",
		})
		return resultJSON(role)
	})

	updateR := mcp.NewTool("update_varroa_role",
		mcp.WithDescription("Update an existing Varroa RBAC role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
		mcp.WithString("jenkinsRoleRef", mcp.Description("Reference to a JenkinsRole name")),
		mcp.WithArray("apiRules", mcp.Description("API permission rules")),
		mcp.WithArray("jenkinsPermissions", mcp.Description("Jenkins permissions (legacy inline)")),
	)
	addTool(mcpServer, kindUpdate, updateR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		if !deps.Authorizer.CanUpdateRole(claims) {
			return mcp.NewToolResultError("access denied: missing roles:update permission"), nil
		}
		existing, err := crdstore.Get[v1alpha1.VarroaRole](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("role not found: %v", err)), nil
		}
		if v := strArg(args, "jenkinsRoleRef"); v != "" {
			existing.Spec.JenkinsRoleRef = v
		}
		if v, ok := strSliceArg(args, "jenkinsPermissions"); ok {
			existing.Spec.JenkinsPermissions = v
		}
		if rules, ok := args["apiRules"].([]interface{}); ok {
			existing.Spec.APIRules = nil
			for _, r := range rules {
				if rm, ok := r.(map[string]interface{}); ok {
					rule := v1alpha1.APIRule{
						Resources: strSliceAny(rm["resources"]),
						Verbs:     strSliceAny(rm["verbs"]),
					}
					existing.Spec.APIRules = append(existing.Spec.APIRules, rule)
				}
			}
		}
		if err := crdstore.Apply[v1alpha1.VarroaRole](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update role: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:    "varroarole.updated",
			Message: "VarroaRole " + name + " updated",
		})
		return resultJSON(existing)
	})

	deleteR := mcp.NewTool("delete_varroa_role",
		mcp.WithDescription("Delete a Varroa RBAC role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
	)
	addTool(mcpServer, kindDelete, deleteR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		name := strArg(getArgs(req), "name")
		if !deps.Authorizer.CanDeleteRole(claims) {
			return mcp.NewToolResultError("access denied: missing roles:delete permission"), nil
		}
		if err := crdstore.Delete[v1alpha1.VarroaRole](ctx, deps.Store, name, ""); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete role: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:    "varroarole.deleted",
			Message: "VarroaRole " + name + " deleted",
		})
		return mcp.NewToolResultText("role " + name + " deleted"), nil
	})
}

// varroaRoleSummary is the default projection for list_varroa_roles.
//
// The arrays are the cost: spec.jenkinsPermissions[] (8+ entries per role) and
// spec.apiRules[].resources/verbs. Counts carry the same operational signal —
// "how much API and data-plane access does this role grant" — and the detail is
// one get_varroa_role away. VarroaRole is cluster-scoped, so the summary
// carries no namespace.
type varroaRoleSummary struct {
	Name                   string `json:"name"`
	JenkinsRoleRef         string `json:"jenkinsRoleRef,omitempty"`
	APIRuleCount           int    `json:"apiRuleCount"`
	JenkinsPermissionCount int    `json:"jenkinsPermissionCount"`
}

func summarizeVarroaRole(r *v1alpha1.VarroaRole) varroaRoleSummary {
	if r == nil {
		return varroaRoleSummary{}
	}
	return varroaRoleSummary{
		Name:                   r.Name,
		JenkinsRoleRef:         r.Spec.JenkinsRoleRef,
		APIRuleCount:           len(r.Spec.APIRules),
		JenkinsPermissionCount: len(r.Spec.JenkinsPermissions),
	}
}

var varroaRoleListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "VarroaRole name."},
	{Name: "jenkinsRoleRef", Type: "string", Desc: "Referenced JenkinsRole holding the data-plane permissions."},
	{Name: "apiRuleCount", Type: "integer", Desc: "Number of API authorization rules."},
	{Name: "jenkinsPermissionCount", Type: "integer", Desc: "Number of inline Jenkins permissions."},
})
