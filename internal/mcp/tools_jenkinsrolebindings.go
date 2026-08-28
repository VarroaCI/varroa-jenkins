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
// JenkinsRoleBinding tools (5)
// =============================================================================

func registerJenkinsRoleBindingTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listJRB := mcp.NewTool("list_jenkins_role_bindings",
		mcp.WithDescription("List Jenkins role bindings managed by Varroa. Returns a compact "+
			"summary per binding (name, roleRef, subjectCount, scopeType, scopeFolder) \u2014 enough to "+
			"survey a fleet and see who is bound to which role and how broadly. Use "+
			"get_jenkins_role_binding for the full resource."),
		mcp.WithBoolean("verbose", mcp.Description("Return full JenkinsRoleBinding resources instead "+
			"of summaries. Expensive: full resources carry spec.subjects and the controllerScope "+
			"selector map, so a fleet-sized listing can exhaust the context window. Prefer "+
			"get_jenkins_role_binding for detail on specific bindings.")),
		withListOutput(jenkinsRoleBindingListOutputSchema),
	)
	addTool(mcpServer, kindRead, listJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		bindings, err := crdstore.List[v1alpha1.JenkinsRoleBinding](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list Jenkins role bindings: %v", err)), nil
		}
		if verbose, _ := args["verbose"].(bool); verbose {
			return resultJSON(bindings)
		}
		summaries := make([]jenkinsRoleBindingSummary, 0, len(bindings))
		for _, b := range bindings {
			summaries = append(summaries, summarizeJenkinsRoleBinding(b))
		}
		return resultJSON(summaries)
	})

	getJRB := mcp.NewTool("get_jenkins_role_binding",
		mcp.WithDescription("Get a specific Jenkins role binding by name"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
	)
	addTool(mcpServer, kindRead, getJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strArg(getArgs(req), "name")
		binding, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Jenkins role binding not found: %v", err)), nil
		}
		return resultJSON(binding)
	})

	createJRB := mcp.NewTool("create_jenkins_role_binding",
		mcp.WithDescription("Create a new Jenkins role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name (DNS-1123)")),
		mcp.WithString("roleRef", mcp.Required(), mcp.Description("JenkinsRole name to bind")),
		mcp.WithArray("subjects", mcp.Required(), mcp.Description("Subject references, e.g. [{\"kind\":\"User\",\"name\":\"alice\"}]")),
	)
	addTool(mcpServer, kindCreate, createJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:    "jenkinsrolebinding.created",
			Message: "JenkinsRoleBinding " + strArg(args, "name") + " created",
		})
		return resultJSON(binding)
	})

	updateJRB := mcp.NewTool("update_jenkins_role_binding",
		mcp.WithDescription("Update an existing Jenkins role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
		mcp.WithString("roleRef", mcp.Description("JenkinsRole name to bind")),
		mcp.WithArray("subjects", mcp.Description("Subject references")),
	)
	addTool(mcpServer, kindUpdate, updateJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:    "jenkinsrolebinding.updated",
			Message: "JenkinsRoleBinding " + name + " updated",
		})
		return resultJSON(existing)
	})

	deleteJRB := mcp.NewTool("delete_jenkins_role_binding",
		mcp.WithDescription("Delete a Jenkins role binding"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
	)
	addTool(mcpServer, kindDelete, deleteJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:    "jenkinsrolebinding.deleted",
			Message: "JenkinsRoleBinding " + name + " deleted",
		})
		return mcp.NewToolResultText("Jenkins role binding " + name + " deleted"), nil
	})
}

// jenkinsRoleBindingSummary is the default projection for
// list_jenkins_role_bindings.
//
// The cost is spec.subjects[] — up to three subject objects per binding — plus
// the nested controllerScope.controllerSelector.matchLabels map. The count
// carries the same operational signal ("who is bound here, how many") and the
// detail is one get_jenkins_role_binding away. JenkinsRoleBinding is
// cluster-scoped (kubebuilder scope=Cluster), so the summary carries no
// namespace.
type jenkinsRoleBindingSummary struct {
	Name         string `json:"name"`
	RoleRef      string `json:"roleRef,omitempty"`
	SubjectCount int    `json:"subjectCount"`
	ScopeType    string `json:"scopeType,omitempty"`
	ScopeFolder  string `json:"scopeFolder,omitempty"`
}

func summarizeJenkinsRoleBinding(jrb *v1alpha1.JenkinsRoleBinding) jenkinsRoleBindingSummary {
	if jrb == nil {
		return jenkinsRoleBindingSummary{}
	}
	s := jenkinsRoleBindingSummary{
		Name:         jrb.Name,
		RoleRef:      jrb.Spec.RoleRef,
		SubjectCount: len(jrb.Spec.Subjects),
	}
	// JenkinsScope is optional: nil means Global scope, and bindings created
	// over MCP never set it. Guard before dereference — an unguarded read
	// panics on every binding the MCP create path produced.
	if jrb.Spec.JenkinsScope != nil {
		s.ScopeType = jrb.Spec.JenkinsScope.Type
		s.ScopeFolder = jrb.Spec.JenkinsScope.Folder
	}
	return s
}

var jenkinsRoleBindingListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "JenkinsRoleBinding name."},
	{Name: "roleRef", Type: "string", Desc: "JenkinsRole name this binding grants."},
	{Name: "subjectCount", Type: "integer", Desc: "Number of bound subjects."},
	{Name: "scopeType", Type: "string", Desc: "In-Jenkins scope type: Global, Folder, or Pattern."},
	{Name: "scopeFolder", Type: "string", Desc: "Folder path, when scopeType is Folder."},
})
