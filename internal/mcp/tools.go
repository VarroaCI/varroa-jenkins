//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/preflight"
)

// preflightFailuresToolError runs no logic itself; given the checks produced by
// preflight.Run it returns an MCP tool error listing the failing checks, or nil
// when nothing failed. Gives the MCP create/update tools the same fail-blocking
// parity as the HTTP handlers (guard-version-upgrade-path §10).
func preflightFailuresToolError(checks []preflight.Check) *mcp.CallToolResult {
	var failing []preflight.Check
	for _, c := range checks {
		if c.Status == "fail" {
			failing = append(failing, c)
		}
	}
	if len(failing) == 0 {
		return nil
	}
	b, _ := json.Marshal(failing)
	return mcp.NewToolResultError("preflight failed: " + string(b))
}

func registerAllTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	registerControllerTools(mcpServer, deps)
	registerVarroaRoleTools(mcpServer, deps)
	registerVarroaRoleBindingTools(mcpServer, deps)
	registerJenkinsRoleTools(mcpServer, deps)
	registerJenkinsRoleBindingTools(mcpServer, deps)
	registerCatalogSourceTools(mcpServer, deps)
	registerCatalogItemTools(mcpServer, deps)
	registerComposedBundleTools(mcpServer, deps)
	registerProvisioningDefaultsTools(mcpServer, deps)
	registerUserTools(mcpServer, deps)
	registerGroupTools(mcpServer, deps)
	registerActivitySearchMetaTools(mcpServer, deps)
	registerProxyTools(mcpServer, deps)
}

func requireClaims(ctx context.Context) (*auth.Claims, error) {
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil {
		return nil, fmt.Errorf("authentication required")
	}
	return claims, nil
}

// --- Argument extraction helpers ---

func getArgs(req mcp.CallToolRequest) map[string]any {
	if args, ok := req.Params.Arguments.(map[string]any); ok {
		return args
	}
	return map[string]any{}
}

func namespaceOrDefault(args map[string]any) string {
	ns, _ := args["namespace"].(string)
	return ns
}

func strArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func strSliceArg(args map[string]any, key string) ([]string, bool) {
	raw, ok := args[key]
	if !ok {
		return nil, false
	}
	arr, _ := raw.([]interface{})
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

func firstSlice(v []string, _ bool) []string { return v }

func marshalUnmarshal(from, to interface{}) error {
	b, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, to)
}

func objMeta(args map[string]any) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      strArg(args, "name"),
		Namespace: strArg(args, "namespace"),
	}
}

// =============================================================================
// Controller tools (8)
// =============================================================================

func registerControllerTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	clusterOrDefault := func(args map[string]any) string {
		if c := strArg(args, "cluster"); c != "" {
			return c
		}
		if deps.Brood != nil {
			return deps.Brood.LocalCluster()
		}
		return "core"
	}

	listC := mcp.NewTool("list_controllers",
		mcp.WithDescription("List all Jenkins controllers managed by Varroa"),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
		mcp.WithString("cluster", mcp.Description("Optional cluster to filter by (default: local cluster)")),
	)
	mcpServer.AddTool(listC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		ns := strArg(args, "namespace")
		clusterFilter := strArg(args, "cluster")
		if deps.Brood != nil {
			cc, _, err := deps.Brood.ListAll(ctx, ns, clusterFilter)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to list controllers: %v", err)), nil
			}
			controllers := make([]*v1alpha1.Controller, 0, len(cc))
			if deps.Authorizer != nil {
				if claims, _ := requireClaims(ctx); claims != nil {
					for _, c := range cc {
						if deps.Authorizer.CanReadController(claims, c.CR.Namespace, c.CR.Name) {
							controllers = append(controllers, c.CR)
						}
					}
					return mcp.NewToolResultJSON(controllers)
				}
			}
			for _, c := range cc {
				controllers = append(controllers, c.CR)
			}
			return mcp.NewToolResultJSON(controllers)
		}
		// Fallback: direct local list.
		controllers, err := crdstore.List[v1alpha1.Controller](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list controllers: %v", err)), nil
		}
		if deps.Authorizer != nil {
			if claims, _ := requireClaims(ctx); claims != nil {
				filtered := make([]*v1alpha1.Controller, 0)
				for _, c := range controllers {
					if deps.Authorizer.CanReadController(claims, c.Namespace, c.Name) {
						filtered = append(filtered, c)
					}
				}
				controllers = filtered
			}
		}
		return mcp.NewToolResultJSON(controllers)
	})

	getC := mcp.NewTool("get_controller",
		mcp.WithDescription("Get a specific Jenkins controller by namespace and name"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	mcpServer.AddTool(getC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if deps.Authorizer != nil {
			claims, err := requireClaims(ctx)
			if err != nil {
				return mcp.NewToolResultError("authentication required"), nil
			}
			if !deps.Authorizer.CanReadController(claims, ns, name) {
				return mcp.NewToolResultError("access denied: missing controllers:read permission"), nil
			}
		}
		var cr *v1alpha1.Controller
		var err error
		if deps.Brood != nil {
			cr, err = deps.Brood.Get(ctx, cluster, ns, name)
		} else {
			cr, err = crdstore.Get[v1alpha1.Controller](ctx, deps.Store, name, ns)
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("controller not found: %v", err)), nil
		}
		return mcp.NewToolResultJSON(cr)
	})

	createC := mcp.NewTool("create_controller",
		mcp.WithDescription("Create a new Jenkins controller"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("version", mcp.Description("Jenkins version")),
		mcp.WithString("composedBundleRef", mcp.Description("ComposedBundle name to attach")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	mcpServer.AddTool(createC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanCreateController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:create permission"), nil
		}
		cr := &v1alpha1.Controller{
			ObjectMeta: objMeta(args),
			Spec: v1alpha1.ControllerSpec{
				Namespace: ns,
				Version:   strArg(args, "version"),
			},
		}
		if ref := strArg(args, "composedBundleRef"); ref != "" {
			cr.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: ref}
		}
		if deps.Brood != nil {
			crJSON, _ := json.Marshal(cr)
			created, _, err := deps.Brood.Create(ctx, cluster, api.ControllersCreateArgs{Controller: crJSON})
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to create controller: %v", err)), nil
			}
			return mcp.NewToolResultJSON(created)
		}
		// Fallback: local direct.
		checks := preflight.Run(ctx, api.PreflightStore{Store: deps.Store, Client: deps.Client}, cr, nil, preflight.Options{OperatorNamespace: deps.OperatorNamespace, ManagedNamespaces: deps.ManagedNamespaces})
		if errResult := preflightFailuresToolError(checks); errResult != nil {
			return errResult, nil
		}
		if err := crdstore.Apply[v1alpha1.Controller](ctx, deps.Store, cr); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create controller: %v", err)), nil
		}
		return mcp.NewToolResultJSON(cr)
	})

	updateC := mcp.NewTool("update_controller",
		mcp.WithDescription("Update an existing Jenkins controller"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("version", mcp.Description("Jenkins version")),
		mcp.WithString("composedBundleRef", mcp.Description("ComposedBundle name to attach")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	mcpServer.AddTool(updateC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanUpdateController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:update permission"), nil
		}
		if deps.Brood != nil {
			patch, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"version": strArg(args, "version")}})
			updated, _, err := deps.Brood.Update(ctx, cluster, ns, name, patch, "varroa-ui", false)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to update controller: %v", err)), nil
			}
			return mcp.NewToolResultJSON(updated)
		}
		// Fallback: local direct.
		existing, err := crdstore.Get[v1alpha1.Controller](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("controller not found: %v", err)), nil
		}
		priorVersion := existing.Spec.Version
		if v := strArg(args, "version"); v != "" {
			existing.Spec.Version = v
		}
		if ref := strArg(args, "composedBundleRef"); ref != "" {
			existing.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: ref}
		}
		checks := preflight.Run(ctx, api.PreflightStore{Store: deps.Store, Client: deps.Client}, existing, nil, preflight.Options{
			OperatorNamespace: deps.OperatorNamespace,
			ManagedNamespaces: deps.ManagedNamespaces,
			ForUpdate:         true,
			PriorVersion:      priorVersion,
		})
		if errResult := preflightFailuresToolError(checks); errResult != nil {
			return errResult, nil
		}
		if err := crdstore.Apply[v1alpha1.Controller](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update controller: %v", err)), nil
		}
		return mcp.NewToolResultJSON(existing)
	})

	deleteC := mcp.NewTool("delete_controller",
		mcp.WithDescription("Delete a Jenkins controller and all its resources"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	mcpServer.AddTool(deleteC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanDeleteController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:delete permission"), nil
		}
		if deps.Brood != nil {
			if err := deps.Brood.Delete(ctx, cluster, ns, name); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to delete controller: %v", err)), nil
			}
			return mcp.NewToolResultText("controller " + ns + "/" + name + " deleted"), nil
		}
		if err := crdstore.Delete[v1alpha1.Controller](ctx, deps.Store, name, ns); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete controller: %v", err)), nil
		}
		return mcp.NewToolResultText("controller " + ns + "/" + name + " deleted"), nil
	})

	reconcileC := mcp.NewTool("reconcile_controller",
		mcp.WithDescription("Trigger an on-demand reconciliation of a controller"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	mcpServer.AddTool(reconcileC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanApproveRestart(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:approve-restart permission"), nil
		}
		if deps.Reconciler == nil {
			return mcp.NewToolResultError("reconciler not available"), nil
		}
		deps.Reconciler.TriggerReconcile(cluster, name, ns)
		return mcp.NewToolResultText("reconciliation triggered for " + ns + "/" + name), nil
	})

	restartC := mcp.NewTool("restart_controller",
		mcp.WithDescription("Restart a Jenkins controller (safe restart)"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	mcpServer.AddTool(restartC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanManageController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:manage permission"), nil
		}
		if deps.Reconciler == nil {
			return mcp.NewToolResultError("reconciler not available"), nil
		}
		if err := deps.Reconciler.ApproveRestart(ctx, cluster, ns, name, "restart"); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to restart controller: %v", err)), nil
		}
		return mcp.NewToolResultText("restart triggered for " + ns + "/" + name), nil
	})

	logsC := mcp.NewTool("get_controller_logs",
		mcp.WithDescription("Get recent logs from a Jenkins controller pod"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithNumber("tailLines", mcp.Description("Number of log lines (default 100, max 500)")),
	)
	mcpServer.AddTool(logsC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		if deps.Authorizer != nil {
			claims, err := requireClaims(ctx)
			if err != nil {
				return mcp.NewToolResultError("authentication required"), nil
			}
			if !deps.Authorizer.CanReadController(claims, ns, name) {
				return mcp.NewToolResultError("access denied: missing controllers:read permission"), nil
			}
		}
		tailLines := int64(100)
		if n, ok := args["tailLines"].(float64); ok {
			tailLines = int64(n)
			if tailLines < 1 {
				tailLines = 1
			}
			if tailLines > 500 {
				tailLines = 500
			}
		}
		cr, err := crdstore.Get[v1alpha1.Controller](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("controller not found: %v", err)), nil
		}
		uid := string(cr.UID)
		if len(uid) > 8 {
			uid = uid[:8]
		}
		podName := cr.Name + "-" + uid + "-0"
		rc, err := deps.Client.StreamPodLogs(ctx, ns, podName, "jenkins", tailLines, false)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to stream logs: %v", err)), nil
		}
		defer func() { _ = rc.Close() }()
		var buf strings.Builder
		if _, err := io.Copy(&buf, rc); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("error reading logs: %v", err)), nil
		}
		return mcp.NewToolResultText(buf.String()), nil
	})
}

// =============================================================================
// VarroaRole tools (5)
// =============================================================================

func registerVarroaRoleTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listR := mcp.NewTool("list_varroa_roles",
		mcp.WithDescription("List all Varroa RBAC roles"),
	)
	mcpServer.AddTool(listR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roles, err := crdstore.List[v1alpha1.VarroaRole](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list roles: %v", err)), nil
		}
		return mcp.NewToolResultJSON(roles)
	})

	getR := mcp.NewTool("get_varroa_role",
		mcp.WithDescription("Get a specific Varroa RBAC role by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
	)
	mcpServer.AddTool(getR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strArg(getArgs(req), "name")
		role, err := crdstore.Get[v1alpha1.VarroaRole](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("role not found: %v", err)), nil
		}
		return mcp.NewToolResultJSON(role)
	})

	createR := mcp.NewTool("create_varroa_role",
		mcp.WithDescription("Create a new Varroa RBAC role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name (DNS-1123)")),
		mcp.WithString("jenkinsRoleRef", mcp.Description("Reference to a JenkinsRole name")),
		mcp.WithArray("apiRules", mcp.Description("API permission rules")),
		mcp.WithArray("jenkinsPermissions", mcp.Description("Jenkins permissions (legacy inline)")),
	)
	mcpServer.AddTool(createR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(role)
	})

	updateR := mcp.NewTool("update_varroa_role",
		mcp.WithDescription("Update an existing Varroa RBAC role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
		mcp.WithString("jenkinsRoleRef", mcp.Description("Reference to a JenkinsRole name")),
		mcp.WithArray("apiRules", mcp.Description("API permission rules")),
		mcp.WithArray("jenkinsPermissions", mcp.Description("Jenkins permissions (legacy inline)")),
	)
	mcpServer.AddTool(updateR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(existing)
	})

	deleteR := mcp.NewTool("delete_varroa_role",
		mcp.WithDescription("Delete a Varroa RBAC role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
	)
	mcpServer.AddTool(deleteR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultText("role " + name + " deleted"), nil
	})
}

func strSliceAny(v interface{}) []string {
	arr, _ := v.([]interface{})
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// =============================================================================
// VarroaRoleBinding tools (5)
// =============================================================================

func registerVarroaRoleBindingTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listRB := mcp.NewTool("list_varroa_role_bindings",
		mcp.WithDescription("List all Varroa RBAC role bindings"),
	)
	mcpServer.AddTool(listRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		bindings, err := crdstore.List[v1alpha1.VarroaRoleBinding](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list role bindings: %v", err)), nil
		}
		return mcp.NewToolResultJSON(bindings)
	})

	getRB := mcp.NewTool("get_varroa_role_binding",
		mcp.WithDescription("Get a specific Varroa RBAC role binding by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
	)
	mcpServer.AddTool(getRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strArg(getArgs(req), "name")
		binding, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("role binding not found: %v", err)), nil
		}
		return mcp.NewToolResultJSON(binding)
	})

	createRB := mcp.NewTool("create_varroa_role_binding",
		mcp.WithDescription("Create a new Varroa RBAC role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name (DNS-1123)")),
		mcp.WithString("roleRef", mcp.Required(), mcp.Description("VarroaRole name to bind")),
		mcp.WithArray("subjects", mcp.Required(), mcp.Description("Subject references, e.g. [{\"kind\":\"User\",\"name\":\"alice\"}]")),
		mcp.WithObject("scope", mcp.Description("Scope restrictions (namespaces, controllerSelector)")),
	)
	mcpServer.AddTool(createRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanCreateRoleBinding(claims) {
			return mcp.NewToolResultError("access denied: missing rolebindings:create permission"), nil
		}
		args := getArgs(req)
		binding := &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: strArg(args, "name")},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				RoleRef:  strArg(args, "roleRef"),
				Subjects: buildSubjectRefs(args["subjects"]),
				Scope:    buildBindingScope(args["scope"]),
			},
		}
		if err := crdstore.Apply[v1alpha1.VarroaRoleBinding](ctx, deps.Store, binding); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create role binding: %v", err)), nil
		}
		return mcp.NewToolResultJSON(binding)
	})

	updateRB := mcp.NewTool("update_varroa_role_binding",
		mcp.WithDescription("Update an existing Varroa RBAC role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
		mcp.WithString("roleRef", mcp.Description("VarroaRole name to bind")),
		mcp.WithArray("subjects", mcp.Description("Subject references")),
		mcp.WithObject("scope", mcp.Description("Scope restrictions")),
	)
	mcpServer.AddTool(updateRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		if !deps.Authorizer.CanUpdateRoleBinding(claims) {
			return mcp.NewToolResultError("access denied: missing rolebindings:update permission"), nil
		}
		existing, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("role binding not found: %v", err)), nil
		}
		if v := strArg(args, "roleRef"); v != "" {
			existing.Spec.RoleRef = v
		}
		if subs := buildSubjectRefs(args["subjects"]); subs != nil {
			existing.Spec.Subjects = subs
		}
		if scope := buildBindingScope(args["scope"]); scope != nil {
			existing.Spec.Scope = scope
		}
		if err := crdstore.Apply[v1alpha1.VarroaRoleBinding](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update role binding: %v", err)), nil
		}
		return mcp.NewToolResultJSON(existing)
	})

	deleteRB := mcp.NewTool("delete_varroa_role_binding",
		mcp.WithDescription("Delete a Varroa RBAC role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
	)
	mcpServer.AddTool(deleteRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		name := strArg(getArgs(req), "name")
		if !deps.Authorizer.CanDeleteRoleBinding(claims) {
			return mcp.NewToolResultError("access denied: missing rolebindings:delete permission"), nil
		}
		if err := crdstore.Delete[v1alpha1.VarroaRoleBinding](ctx, deps.Store, name, ""); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete role binding: %v", err)), nil
		}
		return mcp.NewToolResultText("role binding " + name + " deleted"), nil
	})
}

func buildSubjectRefs(raw interface{}) []v1alpha1.SubjectRef {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	subjects := make([]v1alpha1.SubjectRef, 0, len(arr))
	for _, s := range arr {
		m, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		subjects = append(subjects, v1alpha1.SubjectRef{
			Kind: strVal(m, "kind"),
			Name: strVal(m, "name"),
		})
	}
	return subjects
}

func strVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func buildBindingScope(raw interface{}) *v1alpha1.VarroaRoleBindingScope {
	if raw == nil {
		return nil
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	scope := &v1alpha1.VarroaRoleBindingScope{
		Namespaces: strSliceAny(m["namespaces"]),
	}
	if cs, ok := m["controllerSelector"].(map[string]interface{}); ok {
		scope.ControllerSelector = &metav1.LabelSelector{}
		if ml, ok := cs["matchLabels"].(map[string]interface{}); ok {
			scope.ControllerSelector.MatchLabels = make(map[string]string, len(ml))
			for k, v := range ml {
				if s, ok := v.(string); ok {
					scope.ControllerSelector.MatchLabels[k] = s
				}
			}
		}
	}
	return scope
}

// =============================================================================
// JenkinsRole tools (5)
// =============================================================================

func registerJenkinsRoleTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listJR := mcp.NewTool("list_jenkins_roles",
		mcp.WithDescription("List all Jenkins roles"),
	)
	mcpServer.AddTool(listJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roles, err := crdstore.List[v1alpha1.JenkinsRole](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list Jenkins roles: %v", err)), nil
		}
		return mcp.NewToolResultJSON(roles)
	})

	getJR := mcp.NewTool("get_jenkins_role",
		mcp.WithDescription("Get a specific Jenkins role by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
	)
	mcpServer.AddTool(getJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strArg(getArgs(req), "name")
		role, err := crdstore.Get[v1alpha1.JenkinsRole](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Jenkins role not found: %v", err)), nil
		}
		return mcp.NewToolResultJSON(role)
	})

	createJR := mcp.NewTool("create_jenkins_role",
		mcp.WithDescription("Create a new Jenkins role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name (DNS-1123)")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithString("roleType", mcp.Description("Role type: Global, Item, Agent; default Global")),
		mcp.WithArray("permissions", mcp.Required(), mcp.Description("Permission IDs")),
	)
	mcpServer.AddTool(createJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(role)
	})

	updateJR := mcp.NewTool("update_jenkins_role",
		mcp.WithDescription("Update an existing Jenkins role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithString("roleType", mcp.Description("Role type: Global, Item, Agent")),
		mcp.WithArray("permissions", mcp.Description("Permission IDs")),
	)
	mcpServer.AddTool(updateJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(existing)
	})

	deleteJR := mcp.NewTool("delete_jenkins_role",
		mcp.WithDescription("Delete a Jenkins role"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Role name")),
	)
	mcpServer.AddTool(deleteJR, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultText("Jenkins role " + name + " deleted"), nil
	})
}

// =============================================================================
// JenkinsRoleBinding tools (5)
// =============================================================================

func registerJenkinsRoleBindingTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listJRB := mcp.NewTool("list_jenkins_role_bindings",
		mcp.WithDescription("List all Jenkins role bindings"),
	)
	mcpServer.AddTool(listJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		bindings, err := crdstore.List[v1alpha1.JenkinsRoleBinding](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list Jenkins role bindings: %v", err)), nil
		}
		return mcp.NewToolResultJSON(bindings)
	})

	getJRB := mcp.NewTool("get_jenkins_role_binding",
		mcp.WithDescription("Get a specific Jenkins role binding by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
	)
	mcpServer.AddTool(getJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strArg(getArgs(req), "name")
		binding, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Jenkins role binding not found: %v", err)), nil
		}
		return mcp.NewToolResultJSON(binding)
	})

	createJRB := mcp.NewTool("create_jenkins_role_binding",
		mcp.WithDescription("Create a new Jenkins role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name (DNS-1123)")),
		mcp.WithString("roleRef", mcp.Required(), mcp.Description("JenkinsRole name to bind")),
		mcp.WithArray("subjects", mcp.Required(), mcp.Description("Subject references, e.g. [{\"kind\":\"User\",\"name\":\"alice\"}]")),
	)
	mcpServer.AddTool(createJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		if !deps.Authorizer.CanCreateJenkinsRoleBinding(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsrolebindings:create permission"), nil
		}
		binding := &v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: strArg(args, "name")},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				RoleRef:  strArg(args, "roleRef"),
				Subjects: buildSubjectRefs(args["subjects"]),
			},
		}
		if err := crdstore.Apply[v1alpha1.JenkinsRoleBinding](ctx, deps.Store, binding); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create Jenkins role binding: %v", err)), nil
		}
		return mcp.NewToolResultJSON(binding)
	})

	updateJRB := mcp.NewTool("update_jenkins_role_binding",
		mcp.WithDescription("Update an existing Jenkins role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
		mcp.WithString("roleRef", mcp.Description("JenkinsRole name to bind")),
		mcp.WithArray("subjects", mcp.Description("Subject references")),
	)
	mcpServer.AddTool(updateJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		if !deps.Authorizer.CanUpdateJenkinsRoleBinding(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsrolebindings:update permission"), nil
		}
		existing, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Jenkins role binding not found: %v", err)), nil
		}
		if v := strArg(args, "roleRef"); v != "" {
			existing.Spec.RoleRef = v
		}
		if subs := buildSubjectRefs(args["subjects"]); subs != nil {
			existing.Spec.Subjects = subs
		}
		if err := crdstore.Apply[v1alpha1.JenkinsRoleBinding](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update Jenkins role binding: %v", err)), nil
		}
		return mcp.NewToolResultJSON(existing)
	})

	deleteJRB := mcp.NewTool("delete_jenkins_role_binding",
		mcp.WithDescription("Delete a Jenkins role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
	)
	mcpServer.AddTool(deleteJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		name := strArg(getArgs(req), "name")
		if !deps.Authorizer.CanDeleteJenkinsRoleBinding(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsrolebindings:delete permission"), nil
		}
		if err := crdstore.Delete[v1alpha1.JenkinsRoleBinding](ctx, deps.Store, name, ""); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete Jenkins role binding: %v", err)), nil
		}
		return mcp.NewToolResultText("Jenkins role binding " + name + " deleted"), nil
	})
}

// =============================================================================
// CatalogSource tools (6)
// =============================================================================

func registerCatalogSourceTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listCS := mcp.NewTool("list_catalog_sources",
		mcp.WithDescription("List all catalog sources"),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
	)
	mcpServer.AddTool(listCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ns := namespaceOrDefault(getArgs(req))
		sources, err := crdstore.List[v1alpha1.CatalogSource](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list catalog sources: %v", err)), nil
		}
		return mcp.NewToolResultJSON(sources)
	})

	getCS := mcp.NewTool("get_catalog_source",
		mcp.WithDescription("Get a specific catalog source"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Source namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Source name")),
	)
	mcpServer.AddTool(getCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		source, err := crdstore.Get[v1alpha1.CatalogSource](ctx, deps.Store, strArg(args, "name"), strArg(args, "namespace"))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("catalog source not found: %v", err)), nil
		}
		return mcp.NewToolResultJSON(source)
	})

	createCS := mcp.NewTool("create_catalog_source",
		mcp.WithDescription("Create a new catalog source"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Source namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Source name")),
		mcp.WithString("repoURL", mcp.Required(), mcp.Description("Git repository URL")),
		mcp.WithString("revision", mcp.Description("Git revision")),
		mcp.WithString("path", mcp.Description("Path within repo")),
		mcp.WithString("secretRef", mcp.Description("Git auth secret name")),
	)
	mcpServer.AddTool(createCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns := strArg(args, "namespace")
		if !deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "create", ns) {
			return mcp.NewToolResultError("access denied: missing catalogsources:create permission"), nil
		}
		source := &v1alpha1.CatalogSource{
			ObjectMeta: objMeta(args),
			Spec: v1alpha1.CatalogSourceSpec{
				RepoURL:   strArg(args, "repoURL"),
				Revision:  strArg(args, "revision"),
				Path:      strArg(args, "path"),
				SecretRef: strArg(args, "secretRef"),
			},
		}
		source.Namespace = ns
		if err := crdstore.Apply[v1alpha1.CatalogSource](ctx, deps.Store, source); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create catalog source: %v", err)), nil
		}
		return mcp.NewToolResultJSON(source)
	})

	updateCS := mcp.NewTool("update_catalog_source",
		mcp.WithDescription("Update an existing catalog source"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Source namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Source name")),
		mcp.WithString("repoURL", mcp.Description("Git repository URL")),
		mcp.WithString("revision", mcp.Description("Git revision")),
		mcp.WithString("path", mcp.Description("Path within repo")),
		mcp.WithString("secretRef", mcp.Description("Git auth secret name")),
	)
	mcpServer.AddTool(updateCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		if !deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "update", ns) {
			return mcp.NewToolResultError("access denied: missing catalogsources:update permission"), nil
		}
		existing, err := crdstore.Get[v1alpha1.CatalogSource](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("catalog source not found: %v", err)), nil
		}
		if v := strArg(args, "repoURL"); v != "" {
			existing.Spec.RepoURL = v
		}
		if v := strArg(args, "revision"); v != "" {
			existing.Spec.Revision = v
		}
		if v := strArg(args, "path"); v != "" {
			existing.Spec.Path = v
		}
		if v := strArg(args, "secretRef"); v != "" {
			existing.Spec.SecretRef = v
		}
		if err := crdstore.Apply[v1alpha1.CatalogSource](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update catalog source: %v", err)), nil
		}
		return mcp.NewToolResultJSON(existing)
	})

	deleteCS := mcp.NewTool("delete_catalog_source",
		mcp.WithDescription("Delete a catalog source"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Source namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Source name")),
	)
	mcpServer.AddTool(deleteCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		if !deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "delete", ns) {
			return mcp.NewToolResultError("access denied: missing catalogsources:delete permission"), nil
		}
		if err := crdstore.Delete[v1alpha1.CatalogSource](ctx, deps.Store, name, ns); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete catalog source: %v", err)), nil
		}
		return mcp.NewToolResultText("catalog source " + ns + "/" + name + " deleted"), nil
	})

	syncCS := mcp.NewTool("sync_catalog_source",
		mcp.WithDescription("Trigger sync of a catalog source via operator reconciler"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Source namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Source name")),
	)
	mcpServer.AddTool(syncCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns := strArg(args, "namespace")
		if !deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "update", ns) {
			return mcp.NewToolResultError("access denied: missing catalogsources:update permission"), nil
		}
		return mcp.NewToolResultText("catalog source sync triggered for " + ns + "/" + strArg(args, "name")), nil
	})
}

// =============================================================================
// CatalogItem tools (2)
// =============================================================================

func registerCatalogItemTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listCI := mcp.NewTool("list_catalog_items",
		mcp.WithDescription("List all catalog items"),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
		mcp.WithString("source", mcp.Description("Optional catalog source name filter")),
	)
	mcpServer.AddTool(listCI, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ns := namespaceOrDefault(getArgs(req))
		items, err := crdstore.List[v1alpha1.CatalogItem](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError("failed to list catalog items: " + err.Error()), nil
		}
		summaries := make([]any, 0, len(items))
		for _, item := range items {
			summaries = append(summaries, controller.ProjectCatalogItemSummary(item))
		}
		return mcp.NewToolResultJSON(summaries)
	})

	getCI := mcp.NewTool("get_catalog_item",
		mcp.WithDescription("Get a specific catalog item"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Item namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Item name")),
	)
	mcpServer.AddTool(getCI, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ns, _ := getArgs(req)["namespace"].(string)
		name, _ := getArgs(req)["name"].(string)
		item, err := crdstore.Get[v1alpha1.CatalogItem](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError("catalog item not found: " + err.Error()), nil
		}
		return mcp.NewToolResultJSON(item)
	})
}

// =============================================================================
// ComposedBundle tools (7)
// =============================================================================

func registerComposedBundleTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listCB := mcp.NewTool("list_composed_bundles",
		mcp.WithDescription("List all composed bundles"),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
	)
	mcpServer.AddTool(listCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ns := namespaceOrDefault(getArgs(req))
		bundles, err := crdstore.List[v1alpha1.ComposedBundle](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError("failed to list composed bundles: " + err.Error()), nil
		}
		return mcp.NewToolResultJSON(bundles)
	})

	getCB := mcp.NewTool("get_composed_bundle",
		mcp.WithDescription("Get a specific composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Bundle name")),
	)
	mcpServer.AddTool(getCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ns, _ := getArgs(req)["namespace"].(string)
		name, _ := getArgs(req)["name"].(string)
		bundle, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError("composed bundle not found: " + err.Error()), nil
		}
		return mcp.NewToolResultJSON(bundle)
	})

	createCB := mcp.NewTool("create_composed_bundle",
		mcp.WithDescription("Create a new composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Bundle name")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithArray("inputs", mcp.Required(), mcp.Description("Bundle inputs (itemRef or gitSource)")),
		mcp.WithString("jcascMergeStrategy", mcp.Description("JCasC merge strategy: errorOnConflict (default), override")),
		mcp.WithObject("variables", mcp.Description("Template variables for the bundle")),
	)
	mcpServer.AddTool(createCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns := strArg(args, "namespace")
		if !deps.Authorizer.CanWriteComposedBundles(claims, "create") {
			return mcp.NewToolResultError("access denied: missing composedbundles:create permission"), nil
		}
		bundle := &v1alpha1.ComposedBundle{
			ObjectMeta: objMeta(args),
			Spec: v1alpha1.ComposedBundleSpec{
				DisplayName:        strArg(args, "displayName"),
				Description:        strArg(args, "description"),
				JcascMergeStrategy: strArg(args, "jcascMergeStrategy"),
			},
		}
		bundle.Namespace = ns
		if inputs := args["inputs"]; inputs != nil {
			if err := marshalUnmarshal(inputs, &bundle.Spec.Inputs); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid inputs: %v", err)), nil
			}
		}
		if vars := args["variables"]; vars != nil {
			if err := marshalUnmarshal(vars, &bundle.Spec.Variables); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid variables: %v", err)), nil
			}
		}
		if err := crdstore.Apply[v1alpha1.ComposedBundle](ctx, deps.Store, bundle); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create composed bundle: %v", err)), nil
		}
		return mcp.NewToolResultJSON(bundle)
	})

	updateCB := mcp.NewTool("update_composed_bundle",
		mcp.WithDescription("Update an existing composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Bundle name")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithString("jcascMergeStrategy", mcp.Description("JCasC merge strategy")),
		mcp.WithObject("variables", mcp.Description("Template variables")),
	)
	mcpServer.AddTool(updateCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		if !deps.Authorizer.CanWriteComposedBundles(claims, "update") {
			return mcp.NewToolResultError("access denied: missing composedbundles:update permission"), nil
		}
		existing, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("composed bundle not found: %v", err)), nil
		}
		if v := strArg(args, "description"); v != "" {
			existing.Spec.Description = v
		}
		if v := strArg(args, "jcascMergeStrategy"); v != "" {
			existing.Spec.JcascMergeStrategy = v
		}
		if err := crdstore.Apply[v1alpha1.ComposedBundle](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update composed bundle: %v", err)), nil
		}
		return mcp.NewToolResultJSON(existing)
	})

	deleteCB := mcp.NewTool("delete_composed_bundle",
		mcp.WithDescription("Delete a composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Bundle name")),
	)
	mcpServer.AddTool(deleteCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		if !deps.Authorizer.CanWriteComposedBundles(claims, "delete") {
			return mcp.NewToolResultError("access denied: missing composedbundles:delete permission"), nil
		}
		if err := crdstore.Delete[v1alpha1.ComposedBundle](ctx, deps.Store, name, ns); err != nil {
			return mcp.NewToolResultError("failed to delete composed bundle: " + err.Error()), nil
		}
		return mcp.NewToolResultText("composed bundle " + ns + "/" + name + " deleted"), nil
	})

	validateCB := mcp.NewTool("validate_composed_bundle",
		mcp.WithDescription("Dry-run validate a composed bundle definition"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithArray("inputs", mcp.Required(), mcp.Description("Bundle inputs")),
	)
	mcpServer.AddTool(validateCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("validation not available through MCP; use the REST API at POST /api/v1/composedbundles/validate"), nil
	})

	previewCB := mcp.NewTool("preview_composed_bundle",
		mcp.WithDescription("Preview the resolved output of a composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithArray("inputs", mcp.Required(), mcp.Description("Bundle inputs")),
	)
	mcpServer.AddTool(previewCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("preview not available through MCP; use the REST API at POST /api/v1/composedbundles/{ns}/preview"), nil
	})
}

// =============================================================================
// ProvisioningDefaults tools (2)
// =============================================================================

func registerProvisioningDefaultsTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	clusterOrDefault := func(args map[string]any) string {
		if c := strArg(args, "cluster"); c != "" {
			return c
		}
		if deps.Brood != nil && deps.Brood.LocalCluster() != "" {
			return deps.Brood.LocalCluster()
		}
		return "core"
	}

	getPD := mcp.NewTool("get_provisioning_defaults",
		mcp.WithDescription("Get a provisioning defaults configuration by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Provisioning defaults name (e.g. 'varroa-defaults')")),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
	)
	mcpServer.AddTool(getPD, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		name, cluster := strArg(args, "name"), clusterOrDefault(args)
		var raw json.RawMessage
		var err error
		if deps.ConfigBrood != nil {
			raw, err = deps.ConfigBrood.GetProvisioningDefaults(ctx, cluster, name)
		} else {
			pd, getErr := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, deps.Store, name, "")
			err = getErr
			raw, _ = json.Marshal(pd)
		}
		if err != nil {
			return mcp.NewToolResultError("provisioning defaults not found: " + err.Error()), nil
		}
		return mcp.NewToolResultText(string(raw)), nil
	})

	updatePD := mcp.NewTool("update_provisioning_defaults",
		mcp.WithDescription("Update provisioning defaults configuration"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Provisioning defaults name")),
		mcp.WithString("rootDomain", mcp.Description("Root domain for auto-derived ingress hosts")),
		mcp.WithString("storageClass", mcp.Description("Default storage class")),
		mcp.WithString("defaultStorage", mcp.Description("Default PVC size (e.g. 20Gi)")),
		mcp.WithString("defaultVersion", mcp.Description("Default Jenkins version")),
		mcp.WithString("defaultCPU", mcp.Description("Default CPU request")),
		mcp.WithString("defaultMemory", mcp.Description("Default memory request")),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
	)
	mcpServer.AddTool(updatePD, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanUpdateProvisioningDefaults(claims) {
			return mcp.NewToolResultError("access denied: missing provisioningdefaults:update permission"), nil
		}
		args := getArgs(req)
		name, cluster := strArg(args, "name"), clusterOrDefault(args)
		existing, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, deps.Store, name, "")
		if err != nil {
			existing = &v1alpha1.ProvisioningDefaults{ObjectMeta: metav1.ObjectMeta{Name: name}}
		}
		if v := strArg(args, "rootDomain"); v != "" {
			existing.Spec.RootDomain = v
		}
		if v := strArg(args, "storageClass"); v != "" {
			existing.Spec.StorageClass = v
		}
		if v := strArg(args, "defaultStorage"); v != "" {
			existing.Spec.DefaultStorage = v
		}
		if v := strArg(args, "defaultVersion"); v != "" {
			existing.Spec.DefaultVersion = v
		}
		if v := strArg(args, "defaultCPU"); v != "" {
			existing.Spec.DefaultCPU = v
		}
		if v := strArg(args, "defaultMemory"); v != "" {
			existing.Spec.DefaultMemory = v
		}
		raw, _ := json.Marshal(existing)
		if deps.ConfigBrood != nil {
			raw, err = deps.ConfigBrood.UpdateProvisioningDefaults(ctx, cluster, name, raw)
		} else {
			err = crdstore.Apply[v1alpha1.ProvisioningDefaults](ctx, deps.Store, existing)
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update provisioning defaults: %v", err)), nil
		}
		return mcp.NewToolResultText(string(raw)), nil
	})

	listJVP := mcp.NewTool("list_jenkins_version_profile",
		mcp.WithDescription("List JenkinsVersionProfile CRDs on a cluster"),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
	)
	mcpServer.AddTool(listJVP, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cluster := clusterOrDefault(getArgs(req))
		items, err := deps.ConfigBrood.ListVersionProfiles(ctx, cluster)
		if err != nil {
			return mcp.NewToolResultError("failed to list version profiles: " + err.Error()), nil
		}
		return mcp.NewToolResultJSON(items)
	})

	getJVP := mcp.NewTool("get_jenkins_version_profile",
		mcp.WithDescription("Get a JenkinsVersionProfile CRD"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Profile name")),
		mcp.WithString("cluster", mcp.Description("Target cluster (default: local cluster)")),
	)
	mcpServer.AddTool(getJVP, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		raw, err := deps.ConfigBrood.GetVersionProfile(ctx, clusterOrDefault(args), strArg(args, "name"))
		if err != nil {
			return mcp.NewToolResultError("version profile not found: " + err.Error()), nil
		}
		return mcp.NewToolResultText(string(raw)), nil
	})

	writeProfile := func(verb string, authz func(*auth.Claims) bool, call func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			claims, err := requireClaims(ctx)
			if err != nil {
				return mcp.NewToolResultError("authentication required"), nil
			}
			if deps.Authorizer == nil || !authz(claims) {
				return mcp.NewToolResultError("access denied: missing version-profiles:" + verb + " permission"), nil
			}
			args := getArgs(req)
			name := strArg(args, "name")
			var obj map[string]any
			if raw := strArg(args, "object"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &obj); err != nil {
					return mcp.NewToolResultError("invalid object JSON: " + err.Error()), nil
				}
			} else {
				obj = map[string]any{"apiVersion": "varroa.dev/v1alpha1", "kind": "JenkinsVersionProfile", "metadata": map[string]any{"name": name}, "spec": map[string]any{}}
			}
			raw, _ := json.Marshal(obj)
			out, err := call(ctx, clusterOrDefault(args), name, raw)
			if err != nil {
				return mcp.NewToolResultError("failed to " + verb + " version profile: " + err.Error()), nil
			}
			return mcp.NewToolResultText(string(out)), nil
		}
	}

	for _, spec := range []struct {
		name string
		desc string
		auth func(*auth.Claims) bool
		call func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	}{
		{
			"create_jenkins_version_profile",
			"Create a JenkinsVersionProfile CRD",
			func(claims *auth.Claims) bool {
				return deps.Authorizer != nil && deps.Authorizer.CanCreateVersionProfile(claims)
			},
			func(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
				if deps.ConfigBrood == nil {
					return nil, fmt.Errorf("config brood not configured")
				}
				return deps.ConfigBrood.CreateVersionProfile(ctx, cluster, name, obj)
			},
		},
		{
			"update_jenkins_version_profile",
			"Update a JenkinsVersionProfile CRD",
			func(claims *auth.Claims) bool {
				return deps.Authorizer != nil && deps.Authorizer.CanUpdateVersionProfile(claims)
			},
			func(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
				if deps.ConfigBrood == nil {
					return nil, fmt.Errorf("config brood not configured")
				}
				return deps.ConfigBrood.UpdateVersionProfile(ctx, cluster, name, obj)
			},
		},
	} {
		tool := mcp.NewTool(spec.name, mcp.WithDescription(spec.desc), mcp.WithString("name", mcp.Required()), mcp.WithString("object"), mcp.WithString("cluster"))
		mcpServer.AddTool(tool, writeProfile(strings.TrimPrefix(spec.name, "_jenkins_version_profile"), spec.auth, spec.call))
	}

	deleteJVP := mcp.NewTool("delete_jenkins_version_profile", mcp.WithDescription("Delete a JenkinsVersionProfile CRD"), mcp.WithString("name", mcp.Required()), mcp.WithString("cluster"))
	mcpServer.AddTool(deleteJVP, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer == nil || !deps.Authorizer.CanDeleteVersionProfile(claims) {
			return mcp.NewToolResultError("access denied: missing version-profiles:delete permission"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		if err := deps.ConfigBrood.DeleteVersionProfile(ctx, clusterOrDefault(args), name); err != nil {
			return mcp.NewToolResultError("failed to delete version profile: " + err.Error()), nil
		}
		return mcp.NewToolResultText("version profile " + name + " deleted"), nil
	})
}

// =============================================================================
// User tools (5) — admin-only
// =============================================================================

func registerUserTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listU := mcp.NewTool("list_users",
		mcp.WithDescription("List all Varroa users (admin only)"),
		mcp.WithString("namespace", mcp.Description("Optional namespace filter")),
	)
	mcpServer.AddTool(listU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(users)
	})

	getU := mcp.NewTool("get_user",
		mcp.WithDescription("Get a specific Varroa user (admin only)"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("User namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("User name")),
	)
	mcpServer.AddTool(getU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(user)
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
	mcpServer.AddTool(createU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(user)
	})

	updateU := mcp.NewTool("update_user",
		mcp.WithDescription("Update an existing Varroa user (admin only)"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("User namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("User name")),
		mcp.WithString("email", mcp.Description("Email address")),
		mcp.WithString("displayName", mcp.Description("Display name")),
		mcp.WithArray("groups", mcp.Description("Group memberships")),
	)
	mcpServer.AddTool(updateU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(existing)
	})

	deleteU := mcp.NewTool("delete_user",
		mcp.WithDescription("Delete a Varroa user (admin only)"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("User namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("User name")),
	)
	mcpServer.AddTool(deleteU, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultText("user " + ns + "/" + name + " deleted"), nil
	})
}

// =============================================================================
// Group tools (3) — admin-only
// =============================================================================

func registerGroupTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listG := mcp.NewTool("list_groups",
		mcp.WithDescription("List all Varroa groups (admin only)"),
	)
	mcpServer.AddTool(listG, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(groups)
	})

	createG := mcp.NewTool("create_group",
		mcp.WithDescription("Create a new Varroa group (admin only, local auth only)"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Group name (DNS-1123)")),
		mcp.WithArray("members", mcp.Description("Group members")),
	)
	mcpServer.AddTool(createG, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(group)
	})

	deleteG := mcp.NewTool("delete_group",
		mcp.WithDescription("Delete a Varroa group (admin only)"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Group name")),
	)
	mcpServer.AddTool(deleteG, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultText("group " + name + " deleted"), nil
	})
}

// =============================================================================
// Activity, Search, Me/Meta tools (4)
// =============================================================================

func registerActivitySearchMetaTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	activityTool := mcp.NewTool("list_activity",
		mcp.WithDescription("List recent Varroa activity events"),
		mcp.WithString("controller", mcp.Description("Optional controller name filter")),
	)
	mcpServer.AddTool(activityTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		controller, _ := getArgs(req)["controller"].(string)
		if deps.ActivityStore != nil {
			events := deps.ActivityStore.List(controller)
			return mcp.NewToolResultJSON(events)
		}
		return mcp.NewToolResultJSON([]interface{}{})
	})

	searchTool := mcp.NewTool("search",
		mcp.WithDescription("Search across controllers and other Varroa resources"),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query string")),
		mcp.WithString("namespace", mcp.Description("Optional namespace filter")),
	)
	mcpServer.AddTool(searchTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := getArgs(req)["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("query parameter is required"), nil
		}
		ns := namespaceOrDefault(getArgs(req))
		controllers, err := crdstore.List[v1alpha1.Controller](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError("search failed: " + err.Error()), nil
		}
		_ = query
		_ = controllers
		return mcp.NewToolResultJSON([]interface{}{})
	})

	meTool := mcp.NewTool("get_me",
		mcp.WithDescription("Get the current authenticated user's profile"),
	)
	mcpServer.AddTool(meTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		return mcp.NewToolResultJSON(map[string]interface{}{
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
	mcpServer.AddTool(permsTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer != nil {
			caps := deps.Authorizer.EffectivePermissions(claims)
			return mcp.NewToolResultJSON(caps)
		}
		return mcp.NewToolResultJSON(map[string]interface{}{})
	})
}
