package crdstore

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func ownedByLabel(want string) func(*unstructured.Unstructured) bool {
	return func(live *unstructured.Unstructured) bool {
		return live.GetLabels()["owner"] == want
	}
}

func item(name, owner, content string) *v1alpha1.CatalogItem { //nolint:unparam // name is a parameter so a test can use two distinct items when it needs to
	return &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{"owner": owner}},
		Spec:       v1alpha1.CatalogItemSpec{SourceRef: owner, Path: content},
	}
}

func TestApplyOwned_CreatesWhenAbsent(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	if err := ApplyOwned(ctx, f, item("i", "alpha", "v1"), ownedByLabel("alpha")); err != nil {
		t.Fatalf("ApplyOwned: %v", err)
	}
	got, err := Get[v1alpha1.CatalogItem](ctx, f, "i", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Path != "v1" {
		t.Errorf("path = %q", got.Spec.Path)
	}
}

func TestApplyOwned_UpdatesWhenOwned(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, item("i", "alpha", "v1"))
	if err := ApplyOwned(ctx, f, item("i", "alpha", "v2"), ownedByLabel("alpha")); err != nil {
		t.Fatalf("ApplyOwned: %v", err)
	}
	got, _ := Get[v1alpha1.CatalogItem](ctx, f, "i", "ns")
	if got.Spec.Path != "v2" {
		t.Errorf("path = %q, want v2", got.Spec.Path)
	}
}

// TestApplyOwned_RefusesForeignObject is the guard Apply cannot carry: Apply's
// own internal Get happens after its Create fails, and never consults ownership.
func TestApplyOwned_RefusesForeignObject(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, item("i", "alpha", "owned-by-alpha"))

	err := ApplyOwned(ctx, f, item("i", "beta", "clobber"), ownedByLabel("beta"))
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned", err)
	}
	got, _ := Get[v1alpha1.CatalogItem](ctx, f, "i", "ns")
	if got.Spec.Path != "owned-by-alpha" {
		t.Errorf("the foreign object was modified: %q", got.Spec.Path)
	}
}

// conflictOnceBackend fails the first UpdateObject with a conflict, mirroring a
// concurrent writer winning the race.
type conflictOnceBackend struct {
	Backend
	fired bool
}

func (c *conflictOnceBackend) UpdateObject(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if !c.fired {
		c.fired = true
		return nil, apierrors.NewConflict(gvr.GroupResource(), obj.GetName(), errors.New("modified"))
	}
	return c.Backend.UpdateObject(ctx, gvr, ns, obj)
}

func TestApplyOwned_RetriesOnceOnConflict(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, item("i", "alpha", "v1"))
	b := &conflictOnceBackend{Backend: f}

	if err := ApplyOwned(ctx, b, item("i", "alpha", "v2"), ownedByLabel("alpha")); err != nil {
		t.Fatalf("a single conflict should be retried, got %v", err)
	}
	if !b.fired {
		t.Error("the conflict path was not exercised")
	}
	got, _ := Get[v1alpha1.CatalogItem](ctx, f, "i", "ns")
	if got.Spec.Path != "v2" {
		t.Errorf("path = %q", got.Spec.Path)
	}
}

// alwaysConflictBackend never lets an update through: the caller must surface
// the conflict rather than looping or silently succeeding.
type alwaysConflictBackend struct{ Backend }

func (c *alwaysConflictBackend) UpdateObject(_ context.Context, gvr schema.GroupVersionResource, _ string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return nil, apierrors.NewConflict(gvr.GroupResource(), obj.GetName(), errors.New("modified"))
}

func TestApplyOwned_ConflictDoesNotClobber(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, item("i", "alpha", "concurrent"))

	err := ApplyOwned(ctx, &alwaysConflictBackend{Backend: f}, item("i", "alpha", "mine"), ownedByLabel("alpha"))
	if err == nil {
		t.Fatal("a persistent conflict must be reported, not swallowed")
	}
	got, _ := Get[v1alpha1.CatalogItem](ctx, f, "i", "ns")
	if got.Spec.Path != "concurrent" {
		t.Errorf("the concurrent modification was silently replaced: %q", got.Spec.Path)
	}
}

// createRaceBackend reports NotFound on the first Get and AlreadyExists on the
// resulting Create, which is the create/create race. ApplyOwned must loop back
// to Get so the ownership check runs against what is really there, rather than
// blind-Updating.
type createRaceBackend struct {
	Backend
	gets int
}

func (c *createRaceBackend) GetObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	c.gets++
	if c.gets == 1 {
		return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
	}
	return c.Backend.GetObject(ctx, gvr, ns, name)
}

func (c *createRaceBackend) CreateObject(_ context.Context, gvr schema.GroupVersionResource, _ string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return nil, apierrors.NewAlreadyExists(gvr.GroupResource(), obj.GetName())
}

func TestApplyOwned_CreateRaceRechecksOwnership(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, item("i", "alpha", "won-the-race"))
	b := &createRaceBackend{Backend: f}

	err := ApplyOwned(ctx, b, item("i", "beta", "clobber"), ownedByLabel("beta"))
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned after looping back to Get", err)
	}
	if b.gets < 2 {
		t.Errorf("expected a second Get after AlreadyExists, got %d", b.gets)
	}
	got, _ := Get[v1alpha1.CatalogItem](ctx, f, "i", "ns")
	if got.Spec.Path != "won-the-race" {
		t.Errorf("the racing writer's object was replaced: %q", got.Spec.Path)
	}
}

func TestApplyOwned_NilPredicateAllowsUpdate(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, item("i", "alpha", "v1"))
	if err := ApplyOwned(ctx, f, item("i", "alpha", "v2"), nil); err != nil {
		t.Fatalf("ApplyOwned: %v", err)
	}
}
