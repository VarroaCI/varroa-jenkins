//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// =============================================================================
// JenkinsRole tools (5)
// =============================================================================

func registerJenkinsRoleTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listJR := mcp.NewTool("list_jenkins_roles",
		mcp.WithDescription("List Jenkins roles managed by Varroa. Returns a compact "+
			"summary per role (name, roleType, description, permissionCount) \u2014 enough to "+
			"survey the permission sets defined for the fleet. Use get_jenkins_role for "+
			"the full resource. With cluster, the tool lists the roles of that cluster only."),
		mcp.WithBoolean("verbose", mcp.Description("Return full JenkinsRole resources instead "+
			"of summaries. Expensive: full resources carry spec.permissions, so a fleet-sized "+
			"listing can exhaust the context window. Prefer get_jenkins_role for detail on "+
			"specific roles.")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
		withListOutput(jenkinsRoleListOutputSchema),
	)
	addTool(mcpServer, kindRead, listJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanReadJenkinsRoles(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsroles:read permission"), nil
		}
		args := getArgs(req)
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		roles, err := listOnCluster[v1alpha1.JenkinsRole](ctx, deps, cl, "", func(ctx context.Context) ([]json.RawMessage, error) {
			return deps.ConfigBrood.ListJenkinsRoles(ctx, cl.name)
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list Jenkins roles%s: %v", cl.onCluster(), err)), nil
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
		mcp.WithDescription("Get a specific Jenkins role by name. With cluster, the tool reads the role on that cluster only."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
	)
	addTool(mcpServer, kindRead, getJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanReadJenkinsRoles(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsroles:read permission"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		role, err := jenkinsRoleObject(deps, cl, name).get(ctx)
		if err != nil {
			return mcp.NewToolResultError(readErrorText("Jenkins role not found", "Jenkins role "+name+cl.onCluster(), err)), nil
		}
		return resultJSON(role)
	})

	createJR := mcp.NewTool("create_jenkins_role",
		mcp.WithDescription("Create a new Jenkins role. With cluster, the tool creates the role on that cluster only."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name (DNS-1123)")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithString("roleType", mcp.Description("Role type: Global, Item, Agent; default Global")),
		mcp.WithArray("permissions", mcp.Required(), mcp.Description("Permission IDs")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
	)
	addTool(mcpServer, kindCreateStrict, createJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		name := strArg(args, "name")
		role := &v1alpha1.JenkinsRole{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    strArg(args, "roleType"),
				Permissions: firstSlice(strSliceArg(args, "permissions")),
				Description: strArg(args, "description"),
			},
		}
		created, err := createOnCluster(ctx, deps, cl, role, func(ctx context.Context, body json.RawMessage) (json.RawMessage, error) {
			return deps.ConfigBrood.CreateJenkinsRole(ctx, cl.name, name, body)
		})
		if err != nil {
			if isAlreadyExistsErr(err) {
				return mcp.NewToolResultError(alreadyExistsText("Jenkins role "+name, "update_jenkins_role", cl)), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("failed to create Jenkins role%s: %v", cl.onCluster(), err)), nil
		}
		emitConfigActivity(ctx, deps, claims, cl, api.AccessActivityEvent(api.AccessJenkinsRole, api.AccessCreated, name))
		return resultJSON(created)
	})

	updateJR := mcp.NewTool("update_jenkins_role",
		mcp.WithDescription("Update an existing Jenkins role. When another writer changes the object during the call, the tool reads it again and merges again, up to three times. The tool refuses a federated copy: an object with the label varroa.dev/federated-from. With cluster, the tool reads and writes the role on that cluster only."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithString("roleType", mcp.Description("Role type: Global, Item, Agent")),
		mcp.WithArray("permissions", mcp.Description("Permission IDs")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
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
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		existing, res := updateObject(ctx, jenkinsRoleObject(deps, cl, name), func(obj *v1alpha1.JenkinsRole) error {
			if msg := rbac.FederatedCopyMessage("JenkinsRole", obj.Name, obj.Labels, obj.Annotations); msg != "" {
				return errors.New(msg)
			}
			if v := strArg(args, "description"); v != "" {
				obj.Spec.Description = v
			}
			if v := strArg(args, "roleType"); v != "" {
				obj.Spec.RoleType = v
			}
			if v, ok := strSliceArg(args, "permissions"); ok {
				obj.Spec.Permissions = v
			}
			return nil
		}, updateTexts{
			tool:     "update_jenkins_role",
			object:   "Jenkins role " + name + cl.onCluster(),
			notFound: "Jenkins role not found",
			failed:   "failed to update Jenkins role",
		})
		if res != nil {
			return res, nil
		}
		emitConfigActivity(ctx, deps, claims, cl, api.AccessActivityEvent(api.AccessJenkinsRole, api.AccessUpdated, name))
		return resultJSON(existing)
	})

	deleteJR := mcp.NewTool("delete_jenkins_role",
		mcp.WithDescription("Delete a Jenkins role. With cluster, the tool deletes the role on that cluster only."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
	)
	addTool(mcpServer, kindDelete, deleteJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		if !deps.Authorizer.CanDeleteJenkinsRole(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsroles:delete permission"), nil
		}
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		if err := deleteOnCluster[v1alpha1.JenkinsRole](ctx, deps, cl, name, "", func(ctx context.Context) error {
			return deps.ConfigBrood.DeleteJenkinsRole(ctx, cl.name, name)
		}); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete Jenkins role%s: %v", cl.onCluster(), err)), nil
		}
		emitConfigActivity(ctx, deps, claims, cl, api.AccessActivityEvent(api.AccessJenkinsRole, api.AccessDeleted, name))
		return deletedResult("Jenkins role "+name, cl), nil
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
