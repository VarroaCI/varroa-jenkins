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
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// =============================================================================
// CatalogItem tools (2)
// =============================================================================

func registerCatalogItemTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listCI := mcp.NewTool("list_catalog_items",
		mcp.WithDescription("List all catalog items. Returns a flat projection per "+
			"item (name, namespace, displayName, description, type, version, "+
			"sourceRef, contentHash, valid) rather than raw CRs. For an item whose "+
			"type is pipeline-template, the content this tool does not return is "+
			"one PipelineTemplate document, not items YAML; this tool does not "+
			"parse it. With cluster, the tool lists the items of that cluster only."),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
		mcp.WithString("source", mcp.Description("Optional catalog source name filter")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
		withListOutput(catalogItemListOutputSchema),
	)
	addTool(mcpServer, kindRead, listCI, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanReadCatalogItems(claims) {
			return mcp.NewToolResultError("access denied: missing catalogitems:read permission"), nil
		}
		args := getArgs(req)
		ns := namespaceOrDefault(args)
		source, _ := args["source"].(string)
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		rows, err := listCatalogItemRows(ctx, deps, cl, ns)
		if err != nil {
			return mcp.NewToolResultError("failed to list catalog items" + cl.onCluster() + ": " + err.Error()), nil
		}
		keep := deps.Authorizer.ConfigReadFilter(claims, "catalogitems", cl.operatorNamespace(ctx, deps))
		summaries := make([]bus.CatalogItemSummary, 0, len(rows))
		for _, s := range rows {
			if !keep(s.Namespace) {
				continue
			}
			if source != "" && s.SourceRef != source {
				continue
			}
			summaries = append(summaries, s)
		}
		return resultJSON(summaries)
	})

	getCI := mcp.NewTool("get_catalog_item",
		mcp.WithDescription("Get a specific catalog item, as its raw CR. When "+
			"spec.type is pipeline-template, status.content is one PipelineTemplate "+
			"document, not items YAML; this tool does not parse it. With cluster, the "+
			"tool reads the item on that cluster only."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Item namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Item name")),
		mcp.WithString("cluster", mcp.Description(clusterArgDescription)),
	)
	addTool(mcpServer, kindRead, getCI, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cl, res := resolveConfigCluster(ctx, deps, args)
		if res != nil {
			return res, nil
		}
		if !deps.Authorizer.CanReadConfigInNamespace(claims, "catalogitems", ns, cl.operatorNamespace(ctx, deps)) {
			return mcp.NewToolResultError("access denied: missing catalogitems:read permission in namespace " + ns), nil
		}
		item, err := catalogItemObject(deps, cl, ns, name).get(ctx)
		if err != nil {
			return mcp.NewToolResultError(readErrorText("catalog item not found", "catalog item "+ns+"/"+name+cl.onCluster(), err)), nil
		}
		return resultJSON(item)
	})
}

// listCatalogItemRows returns the catalog item summaries of the cluster c. The
// local cluster projects deps.Store. A member answers its own summaries.
func listCatalogItemRows(ctx context.Context, deps *api.Dependencies, c configCluster, ns string) ([]bus.CatalogItemSummary, error) {
	if c.local {
		items, err := crdstore.List[v1alpha1.CatalogItem](ctx, deps.Store, ns, "")
		if err != nil {
			return nil, err
		}
		out := make([]bus.CatalogItemSummary, 0, len(items))
		for _, item := range items {
			out = append(out, controller.ProjectCatalogItemSummary(item))
		}
		return out, nil
	}
	raws, err := deps.ConfigBrood.ListCatalogItems(ctx, c.name, ns, api.CatalogItemFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]bus.CatalogItemSummary, 0, len(raws))
	for i, raw := range raws {
		var s bus.CatalogItemSummary
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("decode item %d: %w", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// catalogItemListOutputSchema describes the result of list_catalog_items.
//
// The tool already returns a flat projected shape (bus.CatalogItemSummary),
// not raw CRs, so there is no summarizer here — the schema is declared over
// the shape it already emits. It declares the nine core scalar fields; the
// remaining optional fields (tags, variables, message, pluginName, compat)
// ride through the anyOf open-object branch.
var catalogItemListOutputSchema = listOutputSchema("Catalog item projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "Item name."},
	{Name: "namespace", Type: "string", Desc: "Item namespace."},
	{Name: "displayName", Type: "string", Desc: "Human-readable name."},
	{Name: "description", Type: "string", Desc: "Item description."},
	{Name: "type", Type: "string", Desc: "Item type. For \"pipeline-template\", the item's content is one PipelineTemplate document, not items YAML."},
	{Name: "version", Type: "string", Desc: "Item version."},
	{Name: "sourceRef", Type: "string", Desc: "Owning CatalogSource."},
	{Name: "contentHash", Type: "string", Desc: "Hash of the item content."},
	{Name: "valid", Type: "boolean", Desc: "Whether the item parsed and validated."},
})
