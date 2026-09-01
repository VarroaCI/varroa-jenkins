package api

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// PreflightStore adapts crdstore.Backend + ResourceClient into preflight.Deps.
type PreflightStore struct {
	Store  crdstore.Backend
	Client controller.ResourceClient
}

// crdstore.Backend methods

// GetObject implements crdstore.Backend.
func (a PreflightStore) GetObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	return a.Store.GetObject(ctx, gvr, ns, name)
}

// ListObjects implements crdstore.Backend.
func (a PreflightStore) ListObjects(ctx context.Context, gvr schema.GroupVersionResource, ns, sel string) ([]unstructured.Unstructured, error) {
	return a.Store.ListObjects(ctx, gvr, ns, sel)
}

// CreateObject implements crdstore.Backend.
func (a PreflightStore) CreateObject(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return a.Store.CreateObject(ctx, gvr, ns, obj)
}

// UpdateObject implements crdstore.Backend.
func (a PreflightStore) UpdateObject(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return a.Store.UpdateObject(ctx, gvr, ns, obj)
}

// UpdateObjectStatus implements crdstore.Backend.
func (a PreflightStore) UpdateObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return a.Store.UpdateObjectStatus(ctx, gvr, ns, obj)
}

// DeleteObject implements crdstore.Backend.
func (a PreflightStore) DeleteObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) error {
	return a.Store.DeleteObject(ctx, gvr, ns, name)
}

// PatchObjectStatus implements crdstore.Backend.
func (a PreflightStore) PatchObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, status any) error {
	return a.Store.PatchObjectStatus(ctx, gvr, ns, name, status)
}

// PatchObjectMeta implements crdstore.Backend.
func (a PreflightStore) PatchObjectMeta(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, meta map[string]any) error {
	return a.Store.PatchObjectMeta(ctx, gvr, ns, name, meta)
}

// ResourceClient methods needed by preflight

// ListResourceQuotas delegates to Client.
func (a PreflightStore) ListResourceQuotas(ctx context.Context, namespace string) ([]corev1.ResourceQuota, error) {
	return a.Client.ListResourceQuotas(ctx, namespace)
}

// ListIngressHosts delegates to Client.
func (a PreflightStore) ListIngressHosts(ctx context.Context) (map[string][]string, error) {
	return a.Client.ListIngressHosts(ctx)
}

// GetNamespace delegates to Client.
func (a PreflightStore) GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	return a.Client.GetNamespace(ctx, name)
}
