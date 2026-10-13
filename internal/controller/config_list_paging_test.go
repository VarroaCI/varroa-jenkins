package controller

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestHandleItemsListPagesAboveBudget walks every page of a catalog item list
// whose summaries together exceed bus.ListPageBudget.
func TestHandleItemsListPagesAboveBudget(t *testing.T) {
	fc := newFakeClient()
	bigDesc := strings.Repeat("d", 100*1024)
	const n = 12
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("item-%02d", i)
		fc.items[key("ns-a", name)] = &v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns-a"},
			Spec: v1alpha1.CatalogItemSpec{
				SourceRef: "source-a",
				Type:      v1alpha1.CatalogItemItem,
				Variables: []v1alpha1.CatalogVariable{{Name: "v", Description: bigDesc}},
			},
			Status: v1alpha1.CatalogItemStatus{Valid: true},
		}
	}
	cr, _ := newTestConfigCRUD(fc, "op-ns", nil)

	var names []string
	after := ""
	pages := 0
	for {
		var resp bus.ConfigListResponse
		jsonUnmarshal(t, cr.HandleItemsList(jsonMarshal(t, bus.ConfigListRequest{Namespace: "ns-a", After: after})), &resp)
		if resp.Code != "" {
			t.Fatalf("page %d failed: code=%q error=%q", pages, resp.Code, resp.Error)
		}
		for _, raw := range resp.Items {
			var summary bus.CatalogItemSummary
			jsonUnmarshal(t, raw, &summary)
			names = append(names, summary.Name)
		}
		pages++
		if resp.Next == "" {
			break
		}
		after = resp.Next
		if pages > n {
			t.Fatal("paging did not end")
		}
	}
	if pages < 2 {
		t.Fatalf("expected more than one page, got %d", pages)
	}
	if len(names) != n {
		t.Fatalf("got %d items across pages, want %d", len(names), n)
	}
	for i, name := range names {
		if want := fmt.Sprintf("item-%02d", i); name != want {
			t.Fatalf("item %d = %q, want %q", i, name, want)
		}
	}
}

// TestHandleVersionProfilesViewPages walks every page of a version profile
// view above the budget and checks that resolved plugins are still filled.
func TestHandleVersionProfilesViewPages(t *testing.T) {
	fc := newFakeClient()
	bigCasc := strings.Repeat("c", 100*1024)
	const n = 10
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("profile-%02d", i)
		ref := "pluginset-" + name
		fc.profiles[name] = &v1alpha1.JenkinsVersionProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.JenkinsVersionProfileSpec{
				Version: "2.479.3",
				JCasC:   &v1alpha1.VersionJCasC{Content: bigCasc},
			},
			Status: v1alpha1.JenkinsVersionProfileStatus{ContentRef: ref},
		}
		fc.configMaps[key("op-ns", ref)] = map[string]string{
			"plugins.yaml": "plugins:\n- artifactId: git\n  version: \"5.2\"\n",
		}
	}
	cr, _ := newTestConfigCRUD(fc, "op-ns", nil)

	var names []string
	after := ""
	pages := 0
	for {
		var resp bus.VersionProfileViewResponse
		jsonUnmarshal(t, cr.HandleVersionProfilesView(jsonMarshal(t, bus.ConfigListRequest{After: after})), &resp)
		if resp.Code != "" {
			t.Fatalf("page %d failed: code=%q error=%q", pages, resp.Code, resp.Error)
		}
		for _, raw := range resp.Profiles {
			var view bus.VersionProfileView
			jsonUnmarshal(t, raw, &view)
			var item v1alpha1.JenkinsVersionProfile
			jsonUnmarshal(t, view.Item, &item)
			names = append(names, item.Name)
			if len(view.ResolvedPlugins) != 1 || view.ResolvedPlugins[0] != "git@5.2" {
				t.Fatalf("profile %s resolved plugins = %v, want [git@5.2]", item.Name, view.ResolvedPlugins)
			}
		}
		pages++
		if resp.Next == "" {
			break
		}
		after = resp.Next
		if pages > n {
			t.Fatal("paging did not end")
		}
	}
	if pages < 2 {
		t.Fatalf("expected more than one page, got %d", pages)
	}
	if len(names) != n {
		t.Fatalf("got %d profiles across pages, want %d", len(names), n)
	}
}

// TestHandleCellPoolsListAfter checks that cellpools.list honours After.
func TestHandleCellPoolsListAfter(t *testing.T) {
	c, _ := cellPoolCRUD(t)
	var all bus.ConfigListResponse
	jsonUnmarshal(t, c.HandleCellPoolsList([]byte(`{}`)), &all)
	if len(all.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(all.Items))
	}
	var first v1alpha1.CellPool
	jsonUnmarshal(t, all.Items[0], &first)

	var rest bus.ConfigListResponse
	jsonUnmarshal(t, c.HandleCellPoolsList(jsonMarshal(t, bus.ConfigListRequest{After: "/" + first.Name})), &rest)
	if rest.Code != "" || len(rest.Items) != 1 {
		t.Fatalf("after %q: code=%q items=%d, want one item", first.Name, rest.Code, len(rest.Items))
	}
	var second v1alpha1.CellPool
	jsonUnmarshal(t, rest.Items[0], &second)
	if second.Name == first.Name {
		t.Fatalf("after returned the same pool %q", first.Name)
	}
}
