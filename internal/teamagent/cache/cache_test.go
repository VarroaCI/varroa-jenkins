package cache

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func teamAgentObj(ns, name, uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "TeamAgent",
		"metadata":   map[string]any{"name": name, "namespace": ns, "uid": uid},
		"spec":       map[string]any{"teamRef": map[string]any{"name": "alpha"}},
	}}
}

func teamObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Team",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{},
	}}
}

func newFakeDynamic(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	kinds := map[schema.GroupVersionResource]string{
		gvrTeamAgents:     "TeamAgentList",
		gvrTeams:          "TeamList",
		gvrCatalogSources: "CatalogSourceList",
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objs...)
}

// startCache runs the cache until the test ends and waits for the first sync.
func startCache(t *testing.T, c *Cache) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for !c.Synced() {
		if time.Now().After(deadline) {
			t.Fatal("cache did not sync")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCache_ListByUIDAndAcceptInput(t *testing.T) {
	dyn := newFakeDynamic(
		teamAgentObj("tenant-a", "alpha", "uid-1"),
		teamObj("alpha"),
	)
	c := New(dyn, "varroa-system", nil)
	startCache(t, c)

	if got := len(c.List()); got != 1 {
		t.Fatalf("List = %d objects, want 1", got)
	}
	ta, ok := c.ByUID("uid-1")
	if !ok || ta.Name != "alpha" || ta.Namespace != "tenant-a" {
		t.Fatalf("ByUID(uid-1) = %+v, %v", ta, ok)
	}
	if _, ok := c.ByUID("uid-missing"); ok {
		t.Fatal("ByUID found an unknown uid")
	}
	in := c.AcceptInput()
	if in.ReleaseNamespace != "varroa-system" {
		t.Errorf("ReleaseNamespace = %q", in.ReleaseNamespace)
	}
	if _, ok := in.Teams["alpha"]; !ok {
		t.Errorf("AcceptInput.Teams lacks alpha: %v", in.Teams)
	}
	if _, ok := c.CatalogSource("tenant-a", "missing"); ok {
		t.Error("CatalogSource found a missing object")
	}
}

func TestCache_OnChangeFiresOnWrite(t *testing.T) {
	dyn := newFakeDynamic(teamAgentObj("tenant-a", "alpha", "uid-1"))
	c := New(dyn, "varroa-system", nil)
	var fired atomic.Int32
	c.OnChange(func() { fired.Add(1) })
	startCache(t, c)

	_, err := dyn.Resource(gvrTeamAgents).Namespace("tenant-b").Create(
		context.Background(), teamAgentObj("tenant-b", "beta", "uid-2"), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(c.List()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("cache never saw the new object; List = %d", len(c.List()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("OnChange did not fire on a write")
	}
}
