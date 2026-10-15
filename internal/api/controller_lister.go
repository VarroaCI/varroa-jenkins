package api

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ControllerLister lists the local cluster's Controllers from a synced cache.
// An empty namespace means all namespaces. Callers may mutate the returned
// objects.
type ControllerLister interface {
	ListControllers(namespace string) ([]*v1alpha1.Controller, error)
}

type indexerControllerLister struct {
	idx cache.Indexer
}

// NewIndexerControllerLister returns a ControllerLister backed by idx. The
// indexer must hold *v1alpha1.Controller values and be indexed by
// cache.NamespaceIndex.
func NewIndexerControllerLister(idx cache.Indexer) ControllerLister {
	return &indexerControllerLister{idx: idx}
}

// ListControllers returns deep copies sorted by namespace, then name.
func (l *indexerControllerLister) ListControllers(namespace string) ([]*v1alpha1.Controller, error) {
	var items []any
	if namespace == "" {
		items = l.idx.List()
	} else {
		var err error
		items, err = l.idx.ByIndex(cache.NamespaceIndex, namespace)
		if err != nil {
			return nil, fmt.Errorf("list cached controllers in namespace %q: %w", namespace, err)
		}
	}
	out := make([]*v1alpha1.Controller, 0, len(items))
	for _, item := range items {
		cr, ok := item.(*v1alpha1.Controller)
		if !ok {
			return nil, fmt.Errorf("cached controller index holds %T, want *v1alpha1.Controller", item)
		}
		out = append(out, cr.DeepCopy())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ControllerCacheTransform converts an unstructured Controller from a list or
// watch into a *v1alpha1.Controller for the informer cache. It strips
// managedFields, which the cache never reads. Callers must not depend on
// managedFields from a cached list. Other object types pass through unchanged.
func ControllerCacheTransform(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	u.SetManagedFields(nil)
	ctrl := &v1alpha1.Controller{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ctrl); err != nil {
		return nil, err
	}
	// FromUnstructured drops the deletion timestamp on this path, so set it back.
	if dt := u.GetDeletionTimestamp(); dt != nil {
		ctrl.DeletionTimestamp = dt
	}
	return ctrl, nil
}

// localControllers lists local Controllers from l when it is set. Otherwise it
// reads the apiserver through store.
func localControllers(ctx context.Context, l ControllerLister, store crdstore.Backend, namespace string) ([]*v1alpha1.Controller, error) {
	if l != nil {
		return l.ListControllers(namespace)
	}
	return crdstore.List[v1alpha1.Controller](ctx, store, namespace, "")
}

// LocalControllers lists the local cluster's Controllers. Use it for every
// local Controller list. In the BFF it reads the informer cache; without one it
// reads through Store.
func (d *Dependencies) LocalControllers(ctx context.Context, namespace string) ([]*v1alpha1.Controller, error) {
	return localControllers(ctx, d.Controllers, d.Store, namespace)
}
