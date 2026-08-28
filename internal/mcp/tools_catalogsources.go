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
// CatalogSource tools (6)
// =============================================================================

func registerCatalogSourceTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listCS := mcp.NewTool("list_catalog_sources",
		mcp.WithDescription("List catalog sources managed by Varroa. Returns a compact "+
			"summary per source (name, namespace, repoURL, ociRef, revision, phase, "+
			"itemCount, trusted, lastSyncTime, observedRevision) \u2014 enough to survey a "+
			"fleet and spot sources that failed to sync. Use get_catalog_source for the "+
			"full resource."),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
		mcp.WithBoolean("verbose", mcp.Description("Return full CatalogSource resources instead "+
			"of summaries. Expensive: full resources carry status.conditions and spec "+
			"internals, so a fleet-sized listing can exhaust the context window. Prefer "+
			"get_catalog_source for detail on a specific source.")),
		withListOutput(catalogSourceListOutputSchema),
	)
	addTool(mcpServer, kindRead, listCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		ns := namespaceOrDefault(args)
		sources, err := crdstore.List[v1alpha1.CatalogSource](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list catalog sources: %v", err)), nil
		}
		if verbose, _ := args["verbose"].(bool); verbose {
			return resultJSON(sources)
		}
		summaries := make([]catalogSourceSummary, 0, len(sources))
		for _, s := range sources {
			summaries = append(summaries, summarizeCatalogSource(s))
		}
		return resultJSON(summaries)
	})

	getCS := mcp.NewTool("get_catalog_source",
		mcp.WithDescription("Get a specific catalog source"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Source namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Source name")),
	)
	addTool(mcpServer, kindRead, getCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		source, err := crdstore.Get[v1alpha1.CatalogSource](ctx, deps.Store, strArg(args, "name"), strArg(args, "namespace"))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("catalog source not found: %v", err)), nil
		}
		return resultJSON(source)
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
	addTool(mcpServer, kindCreate, createCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:      "catalogsource.created",
			Message:   "CatalogSource " + strArg(args, "name") + " created in " + ns,
			Namespace: ns,
		})
		return resultJSON(source)
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
	addTool(mcpServer, kindUpdate, updateCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:      "catalogsource.updated",
			Message:   "CatalogSource " + name + " updated in " + ns,
			Namespace: ns,
		})
		return resultJSON(existing)
	})

	deleteCS := mcp.NewTool("delete_catalog_source",
		mcp.WithDescription("Delete a catalog source"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Source namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Source name")),
	)
	addTool(mcpServer, kindDelete, deleteCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		emitActivity(deps, claims, activity.Event{
			Type:      "catalogsource.deleted",
			Message:   "CatalogSource " + name + " deleted in " + ns,
			Namespace: ns,
		})
		return mcp.NewToolResultText("catalog source " + ns + "/" + name + " deleted"), nil
	})

	syncCS := mcp.NewTool("sync_catalog_source",
		mcp.WithDescription("Trigger sync of a catalog source via operator reconciler"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Source namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Source name")),
	)
	addTool(mcpServer, kindAction, syncCS, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		if deps.ConfigBrood == nil {
			return mcp.NewToolResultError("config brood not configured"), nil
		}
		cluster := "core"
		if deps.Brood != nil {
			cluster = deps.Brood.LocalCluster()
		}
		name := strArg(args, "name")
		if err := deps.ConfigBrood.SyncCatalogSource(ctx, cluster, ns, name); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to sync catalog source: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:      "catalogsource.synced",
			Message:   "CatalogSource " + name + " synced in " + ns,
			Namespace: ns,
		})
		return resultJSON(map[string]string{"status": "accepted"})
	})
}

// catalogSourceSummary is the default projection for list_catalog_sources.
//
// A full CatalogSource CR carries status.conditions and spec internals (path,
// secretRef, syncIntervalSeconds) that a fleet survey does not need. The summary
// keeps identity, where the source points, what it found, and when it last
// synced. CatalogSource is namespaced, so the summary carries namespace.
//
// A source is exactly one of git-backed or OCI-backed: the CRD's CEL rule
// requires repoURL or ociRef (the reserved name varroa-update-center is the
// only exception, with neither). Project whichever is set into its own field so
// the field name is never a lie.
type catalogSourceSummary struct {
	Name             string `json:"name"`
	Namespace        string `json:"namespace"`
	RepoURL          string `json:"repoURL,omitempty"`
	OCIRef           string `json:"ociRef,omitempty"`
	Revision         string `json:"revision,omitempty"`
	Phase            string `json:"phase,omitempty"`
	ItemCount        int    `json:"itemCount"`
	Trusted          bool   `json:"trusted"`
	LastSyncTime     string `json:"lastSyncTime,omitempty"`
	ObservedRevision string `json:"observedRevision,omitempty"`
}

func summarizeCatalogSource(cs *v1alpha1.CatalogSource) catalogSourceSummary {
	if cs == nil {
		return catalogSourceSummary{}
	}
	// LastSyncTime is a *metav1.Time and is nil before the first sync; format it
	// as RFC3339 UTC like createdAt in the controller summary.
	lastSync := ""
	if cs.Status.LastSyncTime != nil && !cs.Status.LastSyncTime.IsZero() {
		lastSync = cs.Status.LastSyncTime.UTC().Format("2006-01-02T15:04:05Z")
	}
	return catalogSourceSummary{
		Name:             cs.Name,
		Namespace:        cs.Namespace,
		RepoURL:          cs.Spec.RepoURL,
		OCIRef:           cs.Spec.OCIRef,
		Revision:         cs.Spec.Revision,
		Phase:            string(cs.Status.Phase),
		ItemCount:        cs.Status.ItemCount,
		Trusted:          cs.Spec.Trusted,
		LastSyncTime:     lastSync,
		ObservedRevision: cs.Status.ObservedRevision,
	}
}

var catalogSourceListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "CatalogSource name."},
	{Name: "namespace", Type: "string", Desc: "CatalogSource namespace."},
	{Name: "repoURL", Type: "string", Desc: "Git repository URL, when the source is git-backed."},
	{Name: "ociRef", Type: "string", Desc: "OCI reference, when the source is OCI-backed."},
	{Name: "revision", Type: "string", Desc: "Pinned git revision."},
	{Name: "phase", Type: "string", Desc: "Sync phase: Pending, Syncing, Ready, or Error."},
	{Name: "itemCount", Type: "integer", Desc: "Number of catalog items discovered."},
	{Name: "trusted", Type: "boolean", Desc: "Whether the source is trusted."},
	{Name: "lastSyncTime", Type: "string", Desc: "Last successful sync, RFC3339 UTC."},
	{Name: "observedRevision", Type: "string", Desc: "Revision the last sync actually used."},
})
