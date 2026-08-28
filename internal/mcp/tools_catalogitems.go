//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
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
			"sourceRef, contentHash, valid) rather than raw CRs."),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
		mcp.WithString("source", mcp.Description("Optional catalog source name filter")),
		withListOutput(catalogItemListOutputSchema),
	)
	addTool(mcpServer, kindRead, listCI, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ns := namespaceOrDefault(getArgs(req))
		source, _ := getArgs(req)["source"].(string)
		items, err := crdstore.List[v1alpha1.CatalogItem](ctx, deps.Store, ns, "")
		if err != nil {
			return mcp.NewToolResultError("failed to list catalog items: " + err.Error()), nil
		}
		summaries := make([]any, 0, len(items))
		for _, item := range items {
			if source != "" && item.Spec.SourceRef != source {
				continue
			}
			summaries = append(summaries, controller.ProjectCatalogItemSummary(item))
		}
		return resultJSON(summaries)
	})

	getCI := mcp.NewTool("get_catalog_item",
		mcp.WithDescription("Get a specific catalog item"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Item namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Item name")),
	)
	addTool(mcpServer, kindRead, getCI, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ns, _ := getArgs(req)["namespace"].(string)
		name, _ := getArgs(req)["name"].(string)
		item, err := crdstore.Get[v1alpha1.CatalogItem](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError("catalog item not found: " + err.Error()), nil
		}
		return resultJSON(item)
	})
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
	{Name: "type", Type: "string", Desc: "Item type."},
	{Name: "version", Type: "string", Desc: "Item version."},
	{Name: "sourceRef", Type: "string", Desc: "Owning CatalogSource."},
	{Name: "contentHash", Type: "string", Desc: "Hash of the item content."},
	{Name: "valid", Type: "boolean", Desc: "Whether the item parsed and validated."},
})
