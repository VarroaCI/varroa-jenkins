//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// =============================================================================
// User tools (5) — admin-only
// =============================================================================

// userSummary is the default projection for list_users.
//
// User is namespaced and carries a write-only spec.password plus
// status.credentials.passwordHash. Neither belongs in a listing, and both are
// already stripped by api.SanitizeObject at the resultJSON chokepoint, so the
// summary simply never projects them. LastLogin is the one field with real
// fleet-survey signal beyond identity: a missing lastLogin is how a caller
// spots an account nobody has ever used.
type userSummary struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	LastLogin   string `json:"lastLogin,omitempty"`
}

// summarizeUser projects a User CR down to its summary.
//
// Status.LastLogin is a *metav1.Time and nil for a user who has never logged
// in; the nil guard turns that into an omitted field rather than a JSON null.
// When set it is formatted as RFC3339 UTC, matching summarizeController's
// createdAt.
func summarizeUser(u *v1alpha1.User) userSummary {
	if u == nil {
		return userSummary{}
	}
	lastLogin := ""
	if u.Status.LastLogin != nil {
		lastLogin = u.Status.LastLogin.UTC().Format("2006-01-02T15:04:05Z")
	}
	return userSummary{
		Name:        u.Name,
		Namespace:   u.Namespace,
		DisplayName: u.Spec.DisplayName,
		Email:       u.Spec.Email,
		LastLogin:   lastLogin,
	}
}

var userListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "User name."},
	{Name: "namespace", Type: "string", Desc: "User namespace."},
	{Name: "displayName", Type: "string", Desc: "Human-readable display name."},
	{Name: "email", Type: "string", Desc: "Email address."},
	{Name: "lastLogin", Type: "string", Desc: "Most recent login, RFC3339 UTC; absent if the user never logged in."},
})

func registerUserTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listU := mcp.NewTool("list_users",
		mcp.WithDescription("List all Varroa users (admin only). Returns a compact "+
			"summary per user (name, namespace, displayName, email, lastLogin) \u2014 enough "+
			"to survey the user base and spot accounts that have never logged in (no "+
			"lastLogin). Use get_user for the full resource."),
		mcp.WithString("namespace", mcp.Description("Optional namespace filter")),
		mcp.WithBoolean("verbose", mcp.Description("Return full User resources instead "+
			"of summaries. Expensive: full resources carry status.credentials, "+
			"status.activeControllers and status.observedGroups, so a fleet-sized listing "+
			"can exhaust the context window. Prefer get_user for detail on specific users.")),
		withListOutput(userListOutputSchema),
	)
	addTool(mcpServer, kindRead, listU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		ns := namespaceOrDefault(getArgs(req))
		users, err := crdstore.List[v1alpha1.User](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError("failed to list users: " + err.Error()), nil
		}
		if verbose, _ := getArgs(req)["verbose"].(bool); verbose {
			return resultJSON(users)
		}
		summaries := make([]userSummary, 0, len(users))
		for _, u := range users {
			summaries = append(summaries, summarizeUser(u))
		}
		return resultJSON(summaries)
	})

	getU := mcp.NewTool("get_user",
		mcp.WithDescription("Get a specific Varroa user (admin only)"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("User namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("User name")),
	)
	addTool(mcpServer, kindRead, getU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		ns, _ := getArgs(req)["namespace"].(string)
		name, _ := getArgs(req)["name"].(string)
		user, err := crdstore.Get[v1alpha1.User](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError("user not found: " + err.Error()), nil
		}
		return resultJSON(user)
	})

	createU := mcp.NewTool("create_user",
		mcp.WithDescription("Create a new Varroa user (admin only, local auth only)"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("User namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("User name")),
		mcp.WithString("email", mcp.Description("Email address")),
		mcp.WithString("displayName", mcp.Description("Display name")),
		mcp.WithString("password", mcp.Description("Initial password")),
		mcp.WithArray("groups", mcp.Description("Group memberships")),
	)
	addTool(mcpServer, kindCreate, createU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		user := &v1alpha1.User{
			ObjectMeta: objMeta(args),
			Spec: v1alpha1.UserSpec{
				Email:       strArg(args, "email"),
				DisplayName: strArg(args, "displayName"),
			},
		}
		if err := crdstore.Apply[v1alpha1.User](ctx, deps.Store, user); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create user: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:      "user.created",
			Message:   "User " + strArg(args, "name") + " created in " + strArg(args, "namespace"),
			Namespace: strArg(args, "namespace"),
		})
		return resultJSON(user)
	})

	updateU := mcp.NewTool("update_user",
		mcp.WithDescription("Update an existing Varroa user (admin only)"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("User namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("User name")),
		mcp.WithString("email", mcp.Description("Email address")),
		mcp.WithString("displayName", mcp.Description("Display name")),
		mcp.WithArray("groups", mcp.Description("Group memberships")),
	)
	addTool(mcpServer, kindUpdate, updateU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		existing, err := crdstore.Get[v1alpha1.User](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("user not found: %v", err)), nil
		}
		if v := strArg(args, "email"); v != "" {
			existing.Spec.Email = v
		}
		if v := strArg(args, "displayName"); v != "" {
			existing.Spec.DisplayName = v
		}
		if err := crdstore.Apply[v1alpha1.User](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update user: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:      "user.updated",
			Message:   "User " + name + " updated in " + ns,
			Namespace: ns,
		})
		return resultJSON(existing)
	})

	deleteU := mcp.NewTool("delete_user",
		mcp.WithDescription("Delete a Varroa user (admin only)"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("User namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("User name")),
	)
	addTool(mcpServer, kindDelete, deleteU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		ns, _ := getArgs(req)["namespace"].(string)
		name, _ := getArgs(req)["name"].(string)
		if err := crdstore.Delete[v1alpha1.User](ctx, deps.Store, name, ns); err != nil {
			return mcp.NewToolResultError("failed to delete user: " + err.Error()), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:      "user.deleted",
			Message:   "User " + name + " deleted in " + ns,
			Namespace: ns,
		})
		return mcp.NewToolResultText("user " + ns + "/" + name + " deleted"), nil
	})
}
