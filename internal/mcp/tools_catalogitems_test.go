package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// catalogItemDeps seeds three catalog items in one namespace: two owned by
// "platform-catalog", one by "other-source".
func catalogItemDeps() *api.Dependencies {
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		&v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{Name: "item-1", Namespace: "ns"},
			Spec: v1alpha1.CatalogItemSpec{
				SourceRef: "platform-catalog",
				Type:      v1alpha1.CatalogItemPlugin,
				Path:      "plugins/a.yaml",
			},
		},
		&v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{Name: "item-2", Namespace: "ns"},
			Spec: v1alpha1.CatalogItemSpec{
				SourceRef: "platform-catalog",
				Type:      v1alpha1.CatalogItemPlugin,
				Path:      "plugins/b.yaml",
			},
		},
		&v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{Name: "item-3", Namespace: "ns"},
			Spec: v1alpha1.CatalogItemSpec{
				SourceRef: "other-source",
				Type:      v1alpha1.CatalogItemItem,
				Path:      "items/c.yaml",
			},
		},
	)
	return &api.Dependencies{
		Authorizer: adminTestAuthorizer(), Client: &stubClient{}, Store: store}
}

// listCatalogItemsResult calls list_catalog_items and returns the decoded
// structuredContent.
func listCatalogItemsResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(catalogItemDeps())
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_catalog_items",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_catalog_items returned error: %v", tr.Content)
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var payload struct {
		StructuredContent map[string]any `json:"structuredContent"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if payload.StructuredContent == nil {
		t.Fatalf("no structuredContent in result: %s", b)
	}
	return payload.StructuredContent
}

// Without a source filter every item in the namespace is returned.
func TestListCatalogItems_ReturnsAllWithoutSourceFilter(t *testing.T) {
	sc := listCatalogItemsResult(t, map[string]interface{}{"namespace": "ns"})
	items, ok := sc["items"].([]any)
	if !ok {
		t.Fatalf("expected items array, got %T", sc["items"])
	}
	if len(items) != 3 {
		t.Errorf("got %d items, want 3: %v", len(items), items)
	}
}

// A source filter must drop items owned by other sources.
func TestListCatalogItems_FiltersBySource(t *testing.T) {
	sc := listCatalogItemsResult(t, map[string]interface{}{
		"namespace": "ns",
		"source":    "platform-catalog",
	})
	items, ok := sc["items"].([]any)
	if !ok {
		t.Fatalf("expected items array, got %T", sc["items"])
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %v", len(items), items)
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("expected object item, got %T", raw)
		}
		if item["sourceRef"] != "platform-catalog" {
			t.Errorf("item %v sourceRef = %v, want platform-catalog", item["name"], item["sourceRef"])
		}
	}
}

// --- Task 7.5a: pipeline-template wording pinned on the tool descriptions ---

// catalogItemTool is the subset of a tools/list entry this test cares about.
type catalogItemTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func catalogItemToolDescriptions(t *testing.T) map[string]string {
	t.Helper()
	handler := NewHandler(catalogItemDeps())
	resp := mcpRequest(t, handler, "tools/list", map[string]interface{}{}, nil)
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal tools/list: %v", err)
	}
	var payload struct {
		Tools []catalogItemTool `json:"tools"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	out := map[string]string{}
	for _, tool := range payload.Tools {
		out[tool.Name] = tool.Description
	}
	return out
}

// TestListCatalogItems_DescriptionNamesPipelineTemplateShape pins the
// list_catalog_items description's statement that a pipeline-template
// item's content is a PipelineTemplate document, not items YAML, and that
// the tool does not parse it — so the wording cannot silently drift back
// to describing pipeline-template content as items YAML.
func TestListCatalogItems_DescriptionNamesPipelineTemplateShape(t *testing.T) {
	desc, ok := catalogItemToolDescriptions(t)["list_catalog_items"]
	if !ok {
		t.Fatal("list_catalog_items is not registered")
	}
	if !strings.Contains(desc, "PipelineTemplate document") {
		t.Errorf("description = %q, want it to name the PipelineTemplate document shape", desc)
	}
	if !strings.Contains(desc, "not items YAML") {
		t.Errorf("description = %q, want it to say pipeline-template content is not items YAML", desc)
	}
	if !strings.Contains(desc, "does not parse it") {
		t.Errorf("description = %q, want it to say this tool does not parse the content", desc)
	}
}

// TestGetCatalogItem_DescriptionNamesPipelineTemplateShape is the same
// pin for get_catalog_item.
func TestGetCatalogItem_DescriptionNamesPipelineTemplateShape(t *testing.T) {
	desc, ok := catalogItemToolDescriptions(t)["get_catalog_item"]
	if !ok {
		t.Fatal("get_catalog_item is not registered")
	}
	if !strings.Contains(desc, "PipelineTemplate document") {
		t.Errorf("description = %q, want it to name the PipelineTemplate document shape", desc)
	}
	if !strings.Contains(desc, "not items YAML") {
		t.Errorf("description = %q, want it to say pipeline-template content is not items YAML", desc)
	}
	if !strings.Contains(desc, "does not parse it") {
		t.Errorf("description = %q, want it to say this tool does not parse the content", desc)
	}
}

func TestMCPListCatalogItems_ScopedDeveloperFiltered(t *testing.T) {
	deps := scopedConfigDeps([]v1alpha1.APIRule{{Resources: []string{"catalogitems"}, Verbs: []string{"read"}}})
	seedConfig(t, deps,
		&v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "team-a"}, Spec: v1alpha1.CatalogItemSpec{SourceRef: "s", Type: v1alpha1.CatalogItemItem, Path: "a.yaml"}},
		&v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "team-b"}, Spec: v1alpha1.CatalogItemSpec{SourceRef: "s", Type: v1alpha1.CatalogItemItem, Path: "b.yaml"}},
		&v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "varroa-system"}, Spec: v1alpha1.CatalogItemSpec{SourceRef: "s", Type: v1alpha1.CatalogItemItem, Path: "p.yaml"}},
	)
	tr := callToolAs(t, deps, mcpScopedDevClaims, "list_catalog_items", map[string]interface{}{})
	requireNamespaces(t, listedNamespaces(t, tr), "team-a", "varroa-system")
}

func TestMCPGetCatalogItem_ScopedDeveloperOtherNamespaceDenied(t *testing.T) {
	deps := scopedConfigDeps([]v1alpha1.APIRule{{Resources: []string{"catalogitems"}, Verbs: []string{"read"}}})
	seedConfig(t, deps, &v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: "item", Namespace: "team-b"}, Spec: v1alpha1.CatalogItemSpec{SourceRef: "s", Type: v1alpha1.CatalogItemItem, Path: "b.yaml"}})
	tr := callToolAs(t, deps, mcpScopedDevClaims, "get_catalog_item", map[string]interface{}{"namespace": "team-b", "name": "item"})
	requireAccessDenied(t, tr)
}
