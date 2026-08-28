//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// =============================================================================
// VarroaRoleBinding tools (5)
// =============================================================================

func registerVarroaRoleBindingTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listRB := mcp.NewTool("list_varroa_role_bindings",
		mcp.WithDescription("List Varroa RBAC role bindings managed by Varroa. Returns a compact "+
			"summary per binding (name, roleRef, subjectCount, scopeNamespaces) \u2014 enough to "+
			"survey a fleet and see who is bound to which role and how broadly. Use "+
			"get_varroa_role_binding for the full resource."),
		mcp.WithBoolean("verbose", mcp.Description("Return full VarroaRoleBinding resources instead "+
			"of summaries. Expensive: full resources carry spec.subjects and the scope "+
			"namespaces list, so a fleet-sized listing can exhaust the context window. Prefer "+
			"get_varroa_role_binding for detail on specific bindings.")),
		withListOutput(varroaRoleBindingListOutputSchema),
	)
	addTool(mcpServer, kindRead, listRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		bindings, err := crdstore.List[v1alpha1.VarroaRoleBinding](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list role bindings: %v", err)), nil
		}
		if verbose, _ := args["verbose"].(bool); verbose {
			return resultJSON(bindings)
		}
		summaries := make([]varroaRoleBindingSummary, 0, len(bindings))
		for _, b := range bindings {
			summaries = append(summaries, summarizeVarroaRoleBinding(b))
		}
		return resultJSON(summaries)
	})

	getRB := mcp.NewTool("get_varroa_role_binding",
		mcp.WithDescription("Get a specific Varroa RBAC role binding by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
	)
	addTool(mcpServer, kindRead, getRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strArg(getArgs(req), "name")
		binding, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("role binding not found: %v", err)), nil
		}
		return resultJSON(binding)
	})

	createRB := mcp.NewTool("create_varroa_role_binding",
		mcp.WithDescription("Create a new Varroa RBAC role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name (DNS-1123)")),
		mcp.WithString("roleRef", mcp.Required(), mcp.Description("VarroaRole name to bind")),
		mcp.WithArray("subjects", mcp.Required(), mcp.Description("Subject references, e.g. [{\"kind\":\"User\",\"name\":\"alice\"}]")),
		mcp.WithObject("scope", mcp.Description("Scope restrictions (namespaces, controllerSelector)")),
	)
	addTool(mcpServer, kindCreate, createRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:    "varroarolebinding.created",
			Message: "VarroaRoleBinding " + strArg(args, "name") + " created",
		})
		return resultJSON(binding)
	})

	updateRB := mcp.NewTool("update_varroa_role_binding",
		mcp.WithDescription("Update an existing Varroa RBAC role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
		mcp.WithString("roleRef", mcp.Description("VarroaRole name to bind")),
		mcp.WithArray("subjects", mcp.Description("Subject references")),
		mcp.WithObject("scope", mcp.Description("Scope restrictions")),
	)
	addTool(mcpServer, kindUpdate, updateRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:    "varroarolebinding.updated",
			Message: "VarroaRoleBinding " + name + " updated",
		})
		return resultJSON(existing)
	})

	deleteRB := mcp.NewTool("delete_varroa_role_binding",
		mcp.WithDescription("Delete a Varroa RBAC role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
	)
	addTool(mcpServer, kindDelete, deleteRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:    "varroarolebinding.deleted",
			Message: "VarroaRoleBinding " + name + " deleted",
		})
		return mcp.NewToolResultText("role binding " + name + " deleted"), nil
	})
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

// varroaRoleBindingSummary is the default projection for
// list_varroa_role_bindings.
//
// The cost is spec.subjects[] — one live binding carries five subjects — plus
// the scope namespaces list. The counts carry the same operational signal
// ("who is bound here, how many, how broadly") and the detail is one
// get_varroa_role_binding away. VarroaRoleBinding is cluster-scoped
// (kubebuilder scope=Cluster), so the summary carries no namespace.
type varroaRoleBindingSummary struct {
	Name            string `json:"name"`
	RoleRef         string `json:"roleRef,omitempty"`
	SubjectCount    int    `json:"subjectCount"`
	ScopeNamespaces int    `json:"scopeNamespaces"`
}

func summarizeVarroaRoleBinding(vrb *v1alpha1.VarroaRoleBinding) varroaRoleBindingSummary {
	if vrb == nil {
		return varroaRoleBindingSummary{}
	}
	s := varroaRoleBindingSummary{
		Name:         vrb.Name,
		RoleRef:      vrb.Spec.RoleRef,
		SubjectCount: len(vrb.Spec.Subjects),
	}
	// Scope is optional: nil means a cluster-wide binding. Guard before
	// dereference — an unguarded len(r.Spec.Scope.Namespaces) panics on
	// every cluster-wide binding, taking the whole listing down with it.
	if vrb.Spec.Scope != nil {
		s.ScopeNamespaces = len(vrb.Spec.Scope.Namespaces)
	}
	return s
}

var varroaRoleBindingListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "VarroaRoleBinding name."},
	{Name: "roleRef", Type: "string", Desc: "VarroaRole name this binding grants."},
	{Name: "subjectCount", Type: "integer", Desc: "Number of bound subjects."},
	{Name: "scopeNamespaces", Type: "integer", Desc: "Number of namespaces the binding is scoped to; 0 means cluster-wide."},
})
