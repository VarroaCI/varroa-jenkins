//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
		mcp.WithString("repoURL", mcp.Description("Git repository URL (exactly one of repoURL or ociRef)")),
		mcp.WithString("ociRef", mcp.Description("OCI artifact reference (exactly one of repoURL or ociRef)")),
		mcp.WithString("revision", mcp.Description("Git revision")),
		mcp.WithString("path", mcp.Description("Path within repo")),
		mcp.WithString("secretRef", mcp.Description("Auth secret name (git credentials or registry pull secret)")),
		mcp.WithBoolean("trusted", mcp.Description("Mark the source trusted")),
		mcp.WithNumber("syncIntervalSeconds", mcp.Description("Sync interval in seconds: 0 (use the default, 300) or 30 to 31536000")),
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
		spec := v1alpha1.CatalogSourceSpec{
			RepoURL:   strArg(args, "repoURL"),
			OCIRef:    strArg(args, "ociRef"),
			Revision:  strArg(args, "revision"),
			Path:      strArg(args, "path"),
			SecretRef: strArg(args, "secretRef"),
		}
		if spec.OCIRef != "" && spec.RepoURL == "" {
			// An OCI source has no git revision; drop one passed alongside
			// so create and update store the same shape.
			spec.Revision = ""
		}
		if v, ok := boolArg(args, "trusted"); ok {
			spec.Trusted = v
		}
		if v, ok, err := intArg(args, "syncIntervalSeconds"); err != nil {
			return mcp.NewToolResultError("invalid catalog source: " + err.Error()), nil
		} else if ok {
			if err := checkSyncInterval(v); err != nil {
				return mcp.NewToolResultError("invalid catalog source: " + err.Error()), nil
			}
			spec.SyncIntervalSeconds = v
		}
		if strArg(args, "name") == v1alpha1.UpdateCenterCatalogSourceName {
			// The reserved source is operator-created and namespace-bound;
			// a create through the tool would persist an object the
			// operator marks Error, and the operator re-asserts its spec, so
			// neither create nor update goes through the tool.
			return mcp.NewToolResultError("invalid catalog source: the reserved update-center source is created and reconciled by the operator"), nil
		}
		if err := validateCatalogSourceSpec(strArg(args, "name"), &spec); err != nil {
			return mcp.NewToolResultError("invalid catalog source: " + err.Error()), nil
		}
		source := &v1alpha1.CatalogSource{
			ObjectMeta: objMeta(args),
			Spec:       spec,
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
		mcp.WithString("repoURL", mcp.Description("Git repository URL (exactly one of repoURL or ociRef)")),
		mcp.WithString("ociRef", mcp.Description("OCI artifact reference (exactly one of repoURL or ociRef)")),
		mcp.WithString("revision", mcp.Description("Git revision")),
		mcp.WithString("path", mcp.Description("Path within repo")),
		mcp.WithString("secretRef", mcp.Description("Auth secret name (git credentials or registry pull secret)")),
		mcp.WithBoolean("trusted", mcp.Description("Mark the source trusted")),
		mcp.WithNumber("syncIntervalSeconds", mcp.Description("Sync interval in seconds: 0 (use the default, 300) or 30 to 31536000")),
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
		if name == v1alpha1.UpdateCenterCatalogSourceName {
			// The operator re-asserts this source's spec every tick, so an
			// edit through the tool would only appear to take effect.
			return mcp.NewToolResultError("invalid catalog source: the reserved update-center source is reconciled by the operator and cannot be edited"), nil
		}
		existing, err := crdstore.Get[v1alpha1.CatalogSource](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("catalog source not found: %v", err)), nil
		}
		// Key presence decides whether a field is touched (omit = preserve, "" =
		// clear). Choosing a source kind clears the other kind's fields so the
		// CRD's at-most-one rule holds; asking for both in one call is rejected
		// before anything is mutated, like create.
		if strArg(args, "repoURL") != "" && strArg(args, "ociRef") != "" {
			return mcp.NewToolResultError("invalid catalog source: only one of repoURL or ociRef may be set"), nil
		}
		for _, f := range []struct {
			arg   string
			field *string
		}{
			{"revision", &existing.Spec.Revision},
			{"path", &existing.Spec.Path},
			{"secretRef", &existing.Spec.SecretRef},
		} {
			if _, ok := args[f.arg]; ok {
				*f.field = strArg(args, f.arg)
			}
		}
		if v, ok := boolArg(args, "trusted"); ok {
			existing.Spec.Trusted = v
		}
		// Source kind is settled after the plain string fields so a git field
		// passed alongside ociRef cannot survive the clear below.
		if _, ok := args["ociRef"]; ok {
			existing.Spec.OCIRef = strArg(args, "ociRef")
			if existing.Spec.OCIRef != "" {
				existing.Spec.RepoURL, existing.Spec.Revision = "", ""
			}
		}
		if _, ok := args["repoURL"]; ok {
			existing.Spec.RepoURL = strArg(args, "repoURL")
			if existing.Spec.RepoURL != "" {
				existing.Spec.OCIRef = ""
			}
		}
		if v, ok, err := intArg(args, "syncIntervalSeconds"); err != nil {
			return mcp.NewToolResultError("invalid catalog source: " + err.Error()), nil
		} else if ok {
			if err := checkSyncInterval(v); err != nil {
				return mcp.NewToolResultError("invalid catalog source: " + err.Error()), nil
			}
			existing.Spec.SyncIntervalSeconds = v
		}
		if err := validateCatalogSourceSpec(name, &existing.Spec); err != nil {
			return mcp.NewToolResultError("invalid catalog source: " + err.Error()), nil
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

// validateCatalogSourceSpec mirrors the CRD's CEL rule so an agent gets a
// readable error instead of an apply rejection: at most one of repoURL or
// ociRef, and at least one unless the source is the reserved update-center
// entry, which is the only source allowed with neither.
// minSyncIntervalSeconds matches the floor the catalog controller clamps to.
// Rejecting a lower value here keeps the stored spec honest about the cadence
// the controller will actually run; zero means "use the default".
const (
	minSyncIntervalSeconds = 30
	maxSyncIntervalSeconds = 31536000
)

func checkSyncInterval(v int) error {
	if v != 0 && (v < minSyncIntervalSeconds || v > maxSyncIntervalSeconds) {
		return fmt.Errorf("syncIntervalSeconds must be 0 (default) or between %d and %d, got %d", minSyncIntervalSeconds, maxSyncIntervalSeconds, v)
	}
	return nil
}

func validateCatalogSourceSpec(name string, spec *v1alpha1.CatalogSourceSpec) error {
	switch {
	case spec.RepoURL != "" && spec.OCIRef != "":
		return errors.New("only one of repoURL or ociRef may be set")
	case name == v1alpha1.UpdateCenterCatalogSourceName:
		if spec.RepoURL != "" || spec.OCIRef != "" {
			return errors.New("the reserved update-center source carries neither repoURL nor ociRef")
		}
	case spec.RepoURL == "" && spec.OCIRef == "":
		return errors.New("exactly one of repoURL or ociRef is required")
	case spec.RepoURL != "" && !strings.HasPrefix(spec.RepoURL, "https://") &&
		!strings.HasPrefix(spec.RepoURL, "ssh://") && !strings.HasPrefix(spec.RepoURL, "git@"):
		return errors.New("repoURL must start with https://, ssh://, or git@")
	}
	return nil
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
