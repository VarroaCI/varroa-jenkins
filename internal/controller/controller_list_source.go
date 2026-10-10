package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ListSource lists Controllers for the controllers.list handler.
// An empty namespace means all namespaces. Callers may mutate the results.
type ListSource interface {
	ListControllers(ctx context.Context, namespace string) ([]*v1alpha1.Controller, error)
}

// cacheControllerListSource reads Controllers from a controller-runtime reader.
type cacheControllerListSource struct {
	reader client.Reader
}

// NewCacheControllerListSource reads the manager's informer cache. A page
// sends no list request to the Kubernetes API server.
func NewCacheControllerListSource(r client.Reader) ListSource {
	return &cacheControllerListSource{reader: r}
}

// ListControllers copies each cached item, so a caller may mutate the result.
func (s *cacheControllerListSource) ListControllers(ctx context.Context, namespace string) ([]*v1alpha1.Controller, error) {
	var list v1alpha1.ControllerList
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := s.reader.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	out := make([]*v1alpha1.Controller, len(list.Items))
	for i := range list.Items {
		out[i] = &list.Items[i]
		out[i].APIVersion = v1alpha1.SchemeGroupVersion.String()
		out[i].Kind = "Controller"
	}
	return out, nil
}

// storeControllerListSource reads Controllers through a crdstore.Backend.
type storeControllerListSource struct {
	store crdstore.Backend
}

// NewStoreControllerListSource reads through a crdstore.Backend. Tests and
// tools use it.
func NewStoreControllerListSource(s crdstore.Backend) ListSource {
	return &storeControllerListSource{store: s}
}

// ListControllers lists through the store.
func (s *storeControllerListSource) ListControllers(ctx context.Context, namespace string) ([]*v1alpha1.Controller, error) {
	return crdstore.List[v1alpha1.Controller](ctx, s.store, namespace, "")
}
