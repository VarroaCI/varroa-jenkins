package api

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func newTestControllerIndexer(t testing.TB, crs ...*v1alpha1.Controller) cache.Indexer {
	t.Helper()
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, cr := range crs {
		if err := idx.Add(cr); err != nil {
			t.Fatalf("add %s/%s: %v", cr.Namespace, cr.Name, err)
		}
	}
	return idx
}

func testController(ns, name string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
}

func TestIndexerControllerListerFiltersNamespaceAndSorts(t *testing.T) {
	idx := newTestControllerIndexer(t,
		testController("team-b", "zeta"),
		testController("team-a", "beta"),
		testController("team-b", "alpha"),
		testController("team-a", "alpha"),
	)
	lister := NewIndexerControllerLister(idx)

	all, err := lister.ListControllers("")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	want := []string{"team-a/alpha", "team-a/beta", "team-b/alpha", "team-b/zeta"}
	if len(all) != len(want) {
		t.Fatalf("list all: got %d rows, want %d", len(all), len(want))
	}
	for i, cr := range all {
		if got := cr.Namespace + "/" + cr.Name; got != want[i] {
			t.Errorf("row %d: got %s, want %s", i, got, want[i])
		}
	}

	team, err := lister.ListControllers("team-a")
	if err != nil {
		t.Fatalf("list team-a: %v", err)
	}
	if len(team) != 2 || team[0].Name != "alpha" || team[1].Name != "beta" {
		t.Fatalf("list team-a: got %d rows, want alpha and beta", len(team))
	}
	for _, cr := range team {
		if cr.Namespace != "team-a" {
			t.Errorf("list team-a returned %s/%s", cr.Namespace, cr.Name)
		}
	}
}

func TestIndexerControllerListerReturnsCopies(t *testing.T) {
	idx := newTestControllerIndexer(t, testController("team-a", "alpha"))
	lister := NewIndexerControllerLister(idx)

	rows, err := lister.ListControllers("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	rows[0].Spec.Version = "mutated"

	again, err := lister.ListControllers("")
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	if again[0].Spec.Version != "2.0" {
		t.Fatalf("cache changed after caller mutation: version %q", again[0].Spec.Version)
	}
}

func TestControllerCacheTransformStripsManagedFields(t *testing.T) {
	cr := testController("team-a", "alpha")
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		t.Fatalf("to unstructured: %v", err)
	}
	u := &unstructured.Unstructured{Object: raw}
	u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}})
	ts := metav1.Now()
	u.SetDeletionTimestamp(&ts)

	obj, err := ControllerCacheTransform(u)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	got, ok := obj.(*v1alpha1.Controller)
	if !ok {
		t.Fatalf("transform returned %T, want *v1alpha1.Controller", obj)
	}
	if got.ManagedFields != nil {
		t.Errorf("managedFields not stripped: %v", got.ManagedFields)
	}
	if got.DeletionTimestamp == nil {
		t.Error("deletionTimestamp dropped by transform")
	}
	if got.Name != "alpha" || got.Spec.Version != "2.0" {
		t.Errorf("transform lost fields: %s/%s", got.Name, got.Spec.Version)
	}
}

func TestDependenciesLocalControllersFallsBackToStore(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, testController("team-a", "from-store"))

	deps := &Dependencies{Store: store}
	rows, err := deps.LocalControllers(context.Background(), "")
	if err != nil {
		t.Fatalf("list via store: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "from-store" {
		t.Fatalf("store fallback: got %d rows", len(rows))
	}

	deps.Controllers = NewIndexerControllerLister(newTestControllerIndexer(t, testController("team-a", "from-cache")))
	rows, err = deps.LocalControllers(context.Background(), "")
	if err != nil {
		t.Fatalf("list via lister: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "from-cache" {
		t.Fatalf("lister path: got %d rows, want only from-cache", len(rows))
	}
}

func TestIndexerControllerListerRejectsForeignType(t *testing.T) {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if err := idx.Add(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "raw", "namespace": "team-a"}}}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := NewIndexerControllerLister(idx).ListControllers(""); err == nil {
		t.Fatal("ListControllers accepted a non-Controller cache entry; want an error")
	}
}
