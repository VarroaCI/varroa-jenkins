package mcp

import (
	"encoding/json"
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
	return &api.Dependencies{Client: &stubClient{}, Store: store}
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
