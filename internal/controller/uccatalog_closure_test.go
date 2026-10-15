package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// lastItemStatus returns the most recent status patch for one derived item.
func lastItemStatus(t *testing.T, tc *catalogTestClient, name string) *v1alpha1.CatalogItemStatus {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.CatalogItem]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	var out *v1alpha1.CatalogItemStatus
	for _, p := range tc.store.StatusPatches(gvr) {
		if p.Name != name {
			continue
		}
		if s, ok := p.Status.(*v1alpha1.CatalogItemStatus); ok {
			out = s
		}
	}
	if out == nil {
		t.Fatalf("no status patch for %s", name)
	}
	return out
}

func closureCondition(s *v1alpha1.CatalogItemStatus, typ string) *v1alpha1.TemplateCatalogCondition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			return &s.Conditions[i]
		}
	}
	return nil
}

// TestUCSync_UnknownDependencyListInvalidatesTheItem: a closure that holds a
// plugin whose dependency list is unreadable is invalid and stores no content.
// An unrelated plugin still derives normally.
func TestUCSync_UnknownDependencyListInvalidatesTheItem(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		entry("root", "1.0", dep("leaf", "1.0")),
		unknownEntry("leaf"),
		entry("other", "1.0"),
	}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)
	r.Reconcile(context.Background(), src)

	root := lastItemStatus(t, tc, ucItemName("root", "1.0"))
	if root.Valid {
		t.Fatal("root must be invalid while its closure holds an unread dependency list")
	}
	if root.Content != "" || root.ContentHash != "" {
		t.Errorf("an invalid item stores no content: content %q hash %q", root.Content, root.ContentHash)
	}
	if !strings.Contains(root.Message, "leaf@1.0") {
		t.Errorf("message %q does not name leaf@1.0", root.Message)
	}
	if len(root.Closure) == 0 {
		t.Error("an invalid item keeps its closure")
	}
	cc := closureCondition(root, closureCompleteCondition)
	if cc == nil || cc.Status != metav1.ConditionFalse || cc.Reason != reasonUnknownDependencies {
		t.Fatalf("ClosureComplete = %+v, want False/UnknownDependencies", cc)
	}

	other := lastItemStatus(t, tc, ucItemName("other", "1.0"))
	if !other.Valid || other.Content == "" {
		t.Errorf("an unrelated item must stay valid with content: %+v", other)
	}
}

// TestUCSync_ReadableManifestRestoresValidity: once the update center reads the
// dependency list, the next pass makes the item valid again.
func TestUCSync_ReadableManifestRestoresValidity(t *testing.T) {
	tc := newCatalogTestClient()
	seedSingleton(tc)
	uc := newUCTestServer(t)
	uc.entries = []inventoryEntry{
		entry("root", "1.0", dep("leaf", "1.0")),
		unknownEntry("leaf"),
	}
	r := newUCReconciler(t, tc, uc.URL)
	src := reservedSource(testOperatorNS)
	crdstore.MustSeed(tc.store, src)
	r.Reconcile(context.Background(), src)

	if lastItemStatus(t, tc, ucItemName("root", "1.0")).Valid {
		t.Fatal("first pass: root must be invalid")
	}

	uc.entries = []inventoryEntry{
		entry("root", "1.0", dep("leaf", "1.0")),
		entry("leaf", "1.0"),
	}
	elapseSync(src)
	r.Reconcile(context.Background(), src)

	root := lastItemStatus(t, tc, ucItemName("root", "1.0"))
	if !root.Valid {
		t.Fatalf("second pass: root must be valid again: %+v", root)
	}
	if !strings.Contains(root.Content, "leaf") {
		t.Errorf("content %q does not pin leaf", root.Content)
	}
	if root.Message != "" {
		t.Errorf("message must clear: %q", root.Message)
	}
	cc := closureCondition(root, closureCompleteCondition)
	if cc == nil || cc.Status != metav1.ConditionTrue {
		t.Fatalf("ClosureComplete = %+v, want True", cc)
	}
}
