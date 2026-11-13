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
// JenkinsRoleBinding tools (5)
// =============================================================================

func registerJenkinsRoleBindingTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listJRB := mcp.NewTool("list_jenkins_role_bindings",
		mcp.WithDescription("List Jenkins role bindings managed by Varroa. Returns a compact "+
			"summary per binding (name, roleRef, subjectCount, controllerScope, scopeType, scopeFolder) \u2014 enough to "+
			"survey a fleet and see who is bound to which role and how broadly. A row without "+
			"controllerScope binds on every controller. Use "+
			"get_jenkins_role_binding for the full resource. With cluster, the tool lists the bindings of that cluster only."),
		mcp.WithBoolean("verbose", mcp.Description("Return full JenkinsRoleBinding resources instead "+
			"of summaries. Expensive: full resources carry spec.subjects and the controllerScope "+
			"selector map, so a fleet-sized listing can exhaust the context window. Prefer "+
			"get_jenkins_role_binding for detail on specific bindings.")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
		withListOutput(jenkinsRoleBindingListOutputSchema),
	)
	addTool(mcpServer, kindRead, listJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanReadJenkinsRoleBindings(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsrolebindings:read permission"), nil
		}
		args := getArgs(req)
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		bindings, err := listOnCluster[v1alpha1.JenkinsRoleBinding](ctx, deps, cl, "", func(ctx context.Context) ([]json.RawMessage, error) {
			return deps.ConfigBrood.ListJenkinsRoleBindings(ctx, cl.name)
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list Jenkins role bindings%s: %v", cl.onCluster(), err)), nil
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
		mcp.WithDescription("Get a specific Jenkins role binding by name. With cluster, the tool reads the binding on that cluster only."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
	)
	addTool(mcpServer, kindRead, getJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanReadJenkinsRoleBindings(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsrolebindings:read permission"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		binding, err := jenkinsRoleBindingObject(deps, cl, name).get(ctx)
		if err != nil {
			return mcp.NewToolResultError(readErrorText("Jenkins role binding not found", "Jenkins role binding "+name+cl.onCluster(), err)), nil
		}
		return resultJSON(binding)
	})

	createJRB := mcp.NewTool("create_jenkins_role_binding",
		mcp.WithDescription("Create a new Jenkins role binding. With cluster, the tool creates the binding on that cluster only."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name (DNS-1123)")),
		mcp.WithString("roleRef", mcp.Required(), mcp.Description("JenkinsRole name to bind")),
		mcp.WithArray("subjects", mcp.Required(), mcp.Description("Subject references, e.g. [{\"kind\":\"User\",\"name\":\"alice\"}]")),
		mcp.WithObject("controllerScope", mcp.Description("Controller limits: namespaces, controllerSelector, or both. "+
			"controllerSelector is a Kubernetes label selector with matchLabels and matchExpressions, e.g. "+
			"{\"namespaces\":[\"prod-a\"],\"controllerSelector\":{\"matchLabels\":{\"env\":\"prod\"}}}. "+
			"A Jenkins role binding takes no tenants. Omit the argument to bind on every controller.")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
		mcp.WithObject("jenkinsScope", mcp.Description("Jenkins item limits. type is Global, Folder, or Pattern. "+
			"A Folder scope needs folder and takes an optional propagate: None, Children, or Subtree. "+
			"A Pattern scope needs pattern, a regular expression over item full names. "+
			"e.g. {\"type\":\"Folder\",\"folder\":\"payments\",\"propagate\":\"Subtree\"}. Omit the argument for Global scope.")),
	)
	addTool(mcpServer, kindCreateStrict, createJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		controllerScope, err := buildControllerScope(args["controllerScope"])
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		jenkinsScope, err := buildJenkinsScope(args["jenkinsScope"])
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		name := strArg(args, "name")
		binding := &v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				RoleRef:         strArg(args, "roleRef"),
				Subjects:        buildSubjectRefs(args["subjects"]),
				ControllerScope: controllerScope,
				JenkinsScope:    jenkinsScope,
			},
		}
		created, err := createOnCluster(ctx, deps, cl, binding, func(ctx context.Context, body json.RawMessage) (json.RawMessage, error) {
			return deps.ConfigBrood.CreateJenkinsRoleBinding(ctx, cl.name, name, body)
		})
		if err != nil {
			if isAlreadyExistsErr(err) {
				return mcp.NewToolResultError(alreadyExistsText("Jenkins role binding "+name, "update_jenkins_role_binding", cl)), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("failed to create Jenkins role binding%s: %v", cl.onCluster(), err)), nil
		}
		emitConfigActivity(ctx, deps, claims, cl, api.AccessActivityEvent(api.AccessJenkinsRoleBinding, api.AccessCreated, name))
		return resultJSON(created)
	})

	updateJRB := mcp.NewTool("update_jenkins_role_binding",
		mcp.WithDescription("Update an existing Jenkins role binding. When another writer changes the object during the call, the tool reads it again and merges again, up to three times. The tool refuses a federated copy: an object with the label varroa.dev/federated-from. With cluster, the tool reads and writes the binding on that cluster only."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
		mcp.WithString("roleRef", mcp.Description("JenkinsRole name to bind")),
		mcp.WithArray("subjects", mcp.Description("Subject references")),
		mcp.WithObject("controllerScope", mcp.Description("Controller limits: namespaces, controllerSelector, or both. "+
			"This argument replaces the stored controllerScope as a whole. To change one field, send the "+
			"spec.controllerScope of get_jenkins_role_binding on the same cluster with that field changed. Omit the argument to keep the stored controllerScope. "+
			"This tool never removes a stored controllerScope; to bind on every controller, use the Kubernetes API or kubectl.")),
		mcp.WithObject("jenkinsScope", mcp.Description("Jenkins item limits: type Global, Folder, or Pattern, with folder, propagate, or pattern. "+
			"This argument replaces the stored jenkinsScope as a whole. Omit the argument to keep the stored jenkinsScope. "+
			"Send {\"type\":\"Global\"} to remove the item limit.")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
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
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		existing, res := updateObject(ctx, jenkinsRoleBindingObject(deps, cl, name), func(obj *v1alpha1.JenkinsRoleBinding) error {
			if msg := rbac.FederatedCopyMessage("JenkinsRoleBinding", obj.Name, obj.Labels, obj.Annotations); msg != "" {
				return errors.New(msg)
			}
			if v := strArg(args, "roleRef"); v != "" {
				obj.Spec.RoleRef = v
			}
			if subs := buildSubjectRefs(args["subjects"]); subs != nil {
				obj.Spec.Subjects = subs
			}
			controllerScope, err := buildControllerScope(args["controllerScope"])
			if err != nil {
				return err
			}
			if controllerScope != nil {
				obj.Spec.ControllerScope = controllerScope
			}
			jenkinsScope, err := buildJenkinsScope(args["jenkinsScope"])
			if err != nil {
				return err
			}
			if jenkinsScope != nil {
				obj.Spec.JenkinsScope = jenkinsScope
			}
			return nil
		}, updateTexts{
			tool:     "update_jenkins_role_binding",
			object:   "Jenkins role binding " + name + cl.onCluster(),
			notFound: "Jenkins role binding not found",
			failed:   "failed to update Jenkins role binding",
		})
		if res != nil {
			return res, nil
		}
		emitConfigActivity(ctx, deps, claims, cl, api.AccessActivityEvent(api.AccessJenkinsRoleBinding, api.AccessUpdated, name))
		return resultJSON(existing)
	})

	deleteJRB := mcp.NewTool("delete_jenkins_role_binding",
		mcp.WithDescription("Delete a Jenkins role binding. With cluster, the tool deletes the binding on that cluster only."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Binding name")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
	)
	addTool(mcpServer, kindDelete, deleteJRB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		name := strArg(args, "name")
		if !deps.Authorizer.CanDeleteJenkinsRoleBinding(claims) {
			return mcp.NewToolResultError("access denied: missing jenkinsrolebindings:delete permission"), nil
		}
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		if err := deleteOnCluster[v1alpha1.JenkinsRoleBinding](ctx, deps, cl, name, "", func(ctx context.Context) error {
			return deps.ConfigBrood.DeleteJenkinsRoleBinding(ctx, cl.name, name)
		}); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete Jenkins role binding%s: %v", cl.onCluster(), err)), nil
		}
		emitConfigActivity(ctx, deps, claims, cl, api.AccessActivityEvent(api.AccessJenkinsRoleBinding, api.AccessDeleted, name))
		return deletedResult("Jenkins role binding "+name, cl), nil
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
	Name            string                         `json:"name"`
	RoleRef         string                         `json:"roleRef,omitempty"`
	SubjectCount    int                            `json:"subjectCount"`
	ControllerScope *jenkinsControllerScopeSummary `json:"controllerScope,omitempty"`
	ScopeType       string                         `json:"scopeType,omitempty"`
	ScopeFolder     string                         `json:"scopeFolder,omitempty"`
}

// jenkinsControllerScopeSummary names the controllers a binding covers. It
// carries the namespace names and a flag for a label selector. It never carries
// label keys or values.
type jenkinsControllerScopeSummary struct {
	Namespaces []string `json:"namespaces,omitempty"`
	Selector   bool     `json:"selector"`
}

// summarizeControllerScope returns nil when the binding applies to every
// controller: a nil scope, or a scope with no namespaces and no selector terms.
func summarizeControllerScope(scope *v1alpha1.VarroaRoleBindingScope) *jenkinsControllerScopeSummary {
	if scope == nil {
		return nil
	}
	sel := scope.ControllerSelector != nil &&
		(len(scope.ControllerSelector.MatchLabels) > 0 || len(scope.ControllerSelector.MatchExpressions) > 0)
	if len(scope.Namespaces) == 0 && !sel {
		return nil
	}
	return &jenkinsControllerScopeSummary{
		Namespaces: append([]string(nil), scope.Namespaces...),
		Selector:   sel,
	}
}

func summarizeJenkinsRoleBinding(jrb *v1alpha1.JenkinsRoleBinding) jenkinsRoleBindingSummary {
	if jrb == nil {
		return jenkinsRoleBindingSummary{}
	}
	s := jenkinsRoleBindingSummary{
		Name:            jrb.Name,
		RoleRef:         jrb.Spec.RoleRef,
		SubjectCount:    len(jrb.Spec.Subjects),
		ControllerScope: summarizeControllerScope(jrb.Spec.ControllerScope),
	}
	// JenkinsScope is optional: nil means Global scope. Guard before
	// dereference. An unguarded read panics on every Global binding.
	if jrb.Spec.JenkinsScope != nil {
		s.ScopeType = jrb.Spec.JenkinsScope.Type
		s.ScopeFolder = jrb.Spec.JenkinsScope.Folder
	}
	return s
}

// buildControllerScope parses a "controllerScope" argument with the rules of
// buildBindingScope. A Jenkins role binding has no tenant meaning, so the
// parse refuses tenants before a write.
func buildControllerScope(raw interface{}) (*v1alpha1.VarroaRoleBindingScope, error) {
	return parseBindingScope(raw, jenkinsControllerScopeArg)
}

// buildJenkinsScope parses a "jenkinsScope" argument into a JenkinsScope.
// The parse refuses an unknown key, a value that is not a
// string, an unknown type, and a field that the type does not use. An absent
// argument returns nil.
func buildJenkinsScope(raw interface{}) (*v1alpha1.JenkinsScope, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid jenkinsScope: jenkinsScope must be an object with a type of Global, Folder, or Pattern")
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("invalid jenkinsScope: an empty jenkinsScope names no type; name type Global, Folder, or Pattern, or omit jenkinsScope")
	}
	vals := make(map[string]string, len(m))
	for key, v := range m {
		switch key {
		case "type", "folder", "propagate", "pattern":
		default:
			return nil, fmt.Errorf("invalid jenkinsScope: unknown jenkinsScope key %q; allowed keys are type, folder, propagate, pattern", key)
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("invalid jenkinsScope: jenkinsScope.%s must be a string", key)
		}
		vals[key] = s
	}
	_, hasFolder := m["folder"]
	_, hasPropagate := m["propagate"]
	_, hasPattern := m["pattern"]
	js := &v1alpha1.JenkinsScope{Type: vals["type"], Folder: vals["folder"], Propagate: vals["propagate"], Pattern: vals["pattern"]}
	switch js.Type {
	case "":
		return nil, fmt.Errorf("invalid jenkinsScope: jenkinsScope.type is required; name Global, Folder, or Pattern")
	case "Global":
		if hasFolder || hasPropagate || hasPattern {
			return nil, fmt.Errorf("invalid jenkinsScope: a Global jenkinsScope takes no folder, propagate, or pattern")
		}
	case "Folder":
		if hasPattern {
			return nil, fmt.Errorf("invalid jenkinsScope: a Folder jenkinsScope takes no pattern")
		}
		if js.Folder == "" {
			return nil, fmt.Errorf("invalid jenkinsScope: a Folder jenkinsScope needs a non-empty folder")
		}
		if hasPropagate {
			switch js.Propagate {
			case "None", "Children", "Subtree":
			default:
				return nil, fmt.Errorf("invalid jenkinsScope: unknown jenkinsScope.propagate %q; allowed values are None, Children, Subtree", js.Propagate)
			}
		}
	case "Pattern":
		if hasFolder || hasPropagate {
			return nil, fmt.Errorf("invalid jenkinsScope: a Pattern jenkinsScope takes no folder or propagate")
		}
		if js.Pattern == "" {
			return nil, fmt.Errorf("invalid jenkinsScope: a Pattern jenkinsScope needs a non-empty pattern")
		}
	default:
		return nil, fmt.Errorf("invalid jenkinsScope: unknown jenkinsScope.type %q; allowed types are Global, Folder, Pattern", js.Type)
	}
	return js, nil
}

var jenkinsRoleBindingListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "JenkinsRoleBinding name."},
	{Name: "roleRef", Type: "string", Desc: "JenkinsRole name this binding grants."},
	{Name: "subjectCount", Type: "integer", Desc: "Number of bound subjects."},
	{Name: "controllerScope", Type: "object", Desc: "Controller limits: namespaces, and selector (true when a label selector is set). Absent when the binding applies to every controller."},
	{Name: "scopeType", Type: "string", Desc: "In-Jenkins scope type: Global, Folder, or Pattern."},
	{Name: "scopeFolder", Type: "string", Desc: "Folder path, when scopeType is Folder."},
})
