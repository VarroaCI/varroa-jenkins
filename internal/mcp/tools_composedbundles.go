//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// =============================================================================
// ComposedBundle tools (7)
// =============================================================================

func registerComposedBundleTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listCB := mcp.NewTool("list_composed_bundles",
		mcp.WithDescription("List composed bundles managed by Varroa. Returns a compact "+
			"summary per bundle (name, namespace, displayName, phase, itemCount, "+
			"inputCount, warningCount, resolvedHash, contentRef, message) \u2014 enough to "+
			"survey a fleet and spot bundles that failed to compose. Use "+
			"get_composed_bundle for the full resource."),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
		mcp.WithBoolean("verbose", mcp.Description("Return full ComposedBundle resources instead "+
			"of summaries. Expensive: full resources carry status.warnings, inputSummary and "+
			"observedRevisions, so a fleet-sized listing can exhaust the context window. Prefer "+
			"get_composed_bundle for detail on specific bundles.")),
		withListOutput(composedBundleListOutputSchema),
	)
	addTool(mcpServer, kindRead, listCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		ns := namespaceOrDefault(args)
		bundles, err := crdstore.List[v1alpha1.ComposedBundle](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError("failed to list composed bundles: " + err.Error()), nil
		}
		if verbose, _ := args["verbose"].(bool); verbose {
			return resultJSON(bundles)
		}
		summaries := make([]composedBundleSummary, 0, len(bundles))
		for _, b := range bundles {
			summaries = append(summaries, summarizeComposedBundle(b))
		}
		return resultJSON(summaries)
	})

	getCB := mcp.NewTool("get_composed_bundle",
		mcp.WithDescription("Get a specific composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Bundle name")),
	)
	addTool(mcpServer, kindRead, getCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ns, _ := getArgs(req)["namespace"].(string)
		name, _ := getArgs(req)["name"].(string)
		bundle, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError("composed bundle not found: " + err.Error()), nil
		}
		return resultJSON(bundle)
	})

	createCB := mcp.NewTool("create_composed_bundle",
		mcp.WithDescription("Create a new composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Bundle name")),
		mcp.WithString("displayName", mcp.Description("Human-readable display name")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithArray("inputs", mcp.Required(), mcp.Description("Bundle inputs (itemRef, gitSource or ociSource)")),
		mcp.WithString("jcascMergeStrategy", mcp.Description("JCasC merge strategy: errorOnConflict (default), override")),
		mcp.WithObject("variables", mcp.Description("Template variables for the bundle")),
	)
	addTool(mcpServer, kindCreate, createCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:      "composedbundle.created",
			Message:   "ComposedBundle " + strArg(args, "name") + " created in " + ns,
			Namespace: ns,
		})
		return resultJSON(bundle)
	})

	updateCB := mcp.NewTool("update_composed_bundle",
		mcp.WithDescription("Update an existing composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Bundle name")),
		mcp.WithString("displayName", mcp.Description("Human-readable display name")),
		mcp.WithString("description", mcp.Description("Human-readable description")),
		mcp.WithString("jcascMergeStrategy", mcp.Description("JCasC merge strategy")),
		mcp.WithObject("variables", mcp.Description("Template variables")),
	)
	addTool(mcpServer, kindUpdate, updateCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		// Key presence, not non-empty value, decides whether a field is touched.
		// Testing the value conflates "leave this alone" with "clear this", so a
		// non-empty field could never be cleared through MCP while REST PUT
		// clears it by submitting a replacement resource. This matches how
		// `variables` already behaves: omitted preserves, {} clears.
		for _, f := range []struct {
			arg   string
			field *string
		}{
			{"description", &existing.Spec.Description},
			{"displayName", &existing.Spec.DisplayName},
			{"jcascMergeStrategy", &existing.Spec.JcascMergeStrategy},
		} {
			if _, ok := args[f.arg]; ok {
				*f.field = strArg(args, f.arg)
			}
		}
		if vars := args["variables"]; vars != nil {
			if err := marshalUnmarshal(vars, &existing.Spec.Variables); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid variables: %v", err)), nil
			}
		}
		if err := crdstore.Apply[v1alpha1.ComposedBundle](ctx, deps.Store, existing); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to update composed bundle: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:      "composedbundle.updated",
			Message:   "ComposedBundle " + name + " updated in " + ns,
			Namespace: ns,
		})
		return resultJSON(existing)
	})

	deleteCB := mcp.NewTool("delete_composed_bundle",
		mcp.WithDescription("Delete a composed bundle"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Bundle name")),
	)
	addTool(mcpServer, kindDelete, deleteCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:      "composedbundle.deleted",
			Message:   "ComposedBundle " + name + " deleted in " + ns,
			Namespace: ns,
		})
		return mcp.NewToolResultText("composed bundle " + ns + "/" + name + " deleted"), nil
	})

	validateCB := mcp.NewTool("validate_composed_bundle",
		mcp.WithDescription("Dry-run validate a composed bundle definition without persisting it: "+
			"composes the given inputs and variables and reports errors, warnings and "+
			"unresolved variables. Nothing is written; use create_composed_bundle to persist."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithArray("inputs", mcp.Required(), mcp.Description("Bundle inputs (itemRef, gitSource or ociSource)")),
		mcp.WithString("jcascMergeStrategy", mcp.Description("JCasC merge strategy: errorOnConflict (default), override")),
		mcp.WithObject("variables", mcp.Description("Template variables for the bundle")),
	)
	addTool(mcpServer, kindRead, validateCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns := namespaceOrDefault(args)
		if ns == "" {
			return mcp.NewToolResultError("namespace is required"), nil
		}
		if !deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "create", ns) {
			return mcp.NewToolResultError("access denied: missing composedbundles:create permission"), nil
		}
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		preview, err := composeBundleFromArgs(ctx, deps, args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		valid := len(preview.Errors) == 0 && len(preview.Missing) == 0
		errs := preview.Errors
		if errs == nil {
			errs = []string{}
		}
		warns := preview.Warnings
		if warns == nil {
			warns = []string{}
		}
		uvars := preview.UnresolvedVariables
		if uvars == nil {
			uvars = []string{}
		}
		return resultJSON(map[string]interface{}{
			"valid":               valid,
			"errors":              errs,
			"warnings":            warns,
			"unresolvedVariables": uvars,
		})
	})

	previewCB := mcp.NewTool("preview_composed_bundle",
		mcp.WithDescription("Preview the resolved output of a composed bundle without persisting it: "+
			"composes the given inputs and variables and returns the rendered YAML plus "+
			"missing, drifted, warnings, unresolved variables and errors. Nothing is "+
			"written; use create_composed_bundle to persist."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Bundle namespace")),
		mcp.WithArray("inputs", mcp.Required(), mcp.Description("Bundle inputs (itemRef, gitSource or ociSource)")),
		mcp.WithString("jcascMergeStrategy", mcp.Description("JCasC merge strategy: errorOnConflict (default), override")),
		mcp.WithObject("variables", mcp.Description("Template variables for the bundle")),
	)
	addTool(mcpServer, kindRead, previewCB, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns := namespaceOrDefault(args)
		if ns == "" {
			return mcp.NewToolResultError("namespace is required"), nil
		}
		if !deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "create", ns) {
			return mcp.NewToolResultError("access denied: missing composedbundles:create permission"), nil
		}
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		preview, err := composeBundleFromArgs(ctx, deps, args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		normalizeComposePreview(preview)
		return resultJSON(preview)
	})
}

// composeBundleFromArgs builds a ComposedBundleSpec from the create-style
// argument parsing (inputs/variables via marshalUnmarshal) and runs it through
// the composer. Callers guard ConfigBrood before invoking.
func composeBundleFromArgs(ctx context.Context, deps *api.Dependencies, args map[string]any) (*bus.BundleComposePreview, error) {
	var spec v1alpha1.ComposedBundleSpec
	if inputs := args["inputs"]; inputs != nil {
		if err := marshalUnmarshal(inputs, &spec.Inputs); err != nil {
			return nil, fmt.Errorf("invalid inputs: %v", err)
		}
	}
	if vars := args["variables"]; vars != nil {
		if err := marshalUnmarshal(vars, &spec.Variables); err != nil {
			return nil, fmt.Errorf("invalid variables: %v", err)
		}
	}
	spec.JcascMergeStrategy = strArg(args, "jcascMergeStrategy")
	specJSON, _ := json.Marshal(spec)
	preview, err := deps.ConfigBrood.ComposeBundle(ctx, localCluster(deps), namespaceOrDefault(args), specJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to compose bundle: %v", err)
	}
	return preview, nil
}

// localCluster resolves the cluster the brood-local compose tools target: the
// brood's local cluster name, else the core default the package uses.
func localCluster(deps *api.Dependencies) string {
	if deps.Brood != nil && deps.Brood.LocalCluster() != "" {
		return deps.Brood.LocalCluster()
	}
	return "core"
}

// normalizeComposePreview fills nil slice fields so the preview serializes with
// [] rather than null, matching the REST preview envelope.
func normalizeComposePreview(p *bus.BundleComposePreview) {
	if p == nil {
		return
	}
	if p.Missing == nil {
		p.Missing = []string{}
	}
	if p.Drifted == nil {
		p.Drifted = []string{}
	}
	if p.Warnings == nil {
		p.Warnings = []string{}
	}
	if p.UnresolvedVariables == nil {
		p.UnresolvedVariables = []string{}
	}
	if p.Errors == nil {
		p.Errors = []string{}
	}
}

// messagePreviewLimit bounds the message a composedBundleSummary carries. The
// message mirrors the status.warnings text (the very content this projection
// drops), so warningCount is the real signal and message is only a preview —
// the full text is one get_composed_bundle away.
const messagePreviewLimit = 160

// composedBundleSummary is the default projection for list_composed_bundles.
//
// The arrays are the cost: status.warnings, status.inputSummary and
// status.observedRevisions dominate a serialized ComposedBundle and are 2.3 kB
// per object at fleet scale. Counts carry the same operational signal — "is
// this bundle healthy, how big is it" — and the detail is one
// get_composed_bundle away. ComposedBundle is namespaced, so the summary
// carries namespace.
type composedBundleSummary struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	DisplayName  string `json:"displayName,omitempty"`
	Phase        string `json:"phase,omitempty"`
	ItemCount    int    `json:"itemCount"`
	InputCount   int    `json:"inputCount"`
	WarningCount int    `json:"warningCount"`
	ResolvedHash string `json:"resolvedHash,omitempty"`
	ContentRef   string `json:"contentRef,omitempty"`
	Message      string `json:"message,omitempty"`
}

func summarizeComposedBundle(cb *v1alpha1.ComposedBundle) composedBundleSummary {
	if cb == nil {
		return composedBundleSummary{}
	}
	msg := cb.Status.Message
	if runes := []rune(msg); len(runes) > messagePreviewLimit {
		msg = string(runes[:messagePreviewLimit]) + "…"
	}
	return composedBundleSummary{
		Name:         cb.Name,
		Namespace:    cb.Namespace,
		DisplayName:  cb.Spec.DisplayName,
		Phase:        string(cb.Status.Phase),
		ItemCount:    cb.Status.ItemCount,
		InputCount:   len(cb.Status.InputSummary),
		WarningCount: len(cb.Status.Warnings),
		ResolvedHash: cb.Status.ResolvedHash,
		ContentRef:   cb.Status.ContentRef,
		Message:      msg,
	}
}

var composedBundleListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "ComposedBundle name."},
	{Name: "namespace", Type: "string", Desc: "ComposedBundle namespace."},
	{Name: "displayName", Type: "string", Desc: "Human-readable name."},
	{Name: "phase", Type: "string", Desc: "Composition phase."},
	{Name: "itemCount", Type: "integer", Desc: "Number of composed items."},
	{Name: "inputCount", Type: "integer", Desc: "Number of declared inputs."},
	{Name: "warningCount", Type: "integer", Desc: "Number of composition warnings; get_composed_bundle for the text."},
	{Name: "resolvedHash", Type: "string", Desc: "Hash of the resolved content."},
	{Name: "contentRef", Type: "string", Desc: "ConfigMap holding the composed content."},
	{Name: "message", Type: "string", Desc: "Preview of the most recent status message, truncated to 160 runes; full text via get_composed_bundle."},
})
