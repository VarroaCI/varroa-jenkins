package controller

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// Compile-time assertion that ClientsetClient satisfies crdstore.Backend.
var _ crdstore.Backend = (*ClientsetClient)(nil)

// GetObject returns a single object by gvr, namespace, and name.
// Errors are returned unwrapped so apierrors.IsNotFound works at call sites.
func (c *ClientsetClient) GetObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	if namespace == "" {
		return c.dynamic.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	}
	return c.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// ListObjects lists all objects of gvr. namespace "" means all namespaces
// (or cluster-scoped kinds). labelSelector "" means no selector.
func (c *ClientsetClient) ListObjects(ctx context.Context, gvr schema.GroupVersionResource, namespace, labelSelector string) ([]unstructured.Unstructured, error) {
	opts := metav1.ListOptions{}
	if labelSelector != "" {
		opts.LabelSelector = labelSelector
	}
	var list *unstructured.UnstructuredList
	var err error
	if namespace != "" {
		list, err = c.dynamic.Resource(gvr).Namespace(namespace).List(ctx, opts)
	} else {
		list, err = c.dynamic.Resource(gvr).List(ctx, opts)
	}
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// CreateObject creates an object via the dynamic client.
func (c *ClientsetClient) CreateObject(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if namespace == "" {
		return c.dynamic.Resource(gvr).Create(ctx, obj, metav1.CreateOptions{})
	}
	return c.dynamic.Resource(gvr).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
}

// UpdateObject updates an object via the dynamic client.
func (c *ClientsetClient) UpdateObject(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if namespace == "" {
		return c.dynamic.Resource(gvr).Update(ctx, obj, metav1.UpdateOptions{})
	}
	return c.dynamic.Resource(gvr).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{})
}

// DeleteObject deletes an object by name. A missing object is not an error.
func (c *ClientsetClient) DeleteObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	var err error
	if namespace == "" {
		err = c.dynamic.Resource(gvr).Delete(ctx, name, metav1.DeleteOptions{})
	} else {
		err = c.dynamic.Resource(gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// PatchObjectStatus merge-patches the /status subresource.
// Fields tagged with `json:"...,omitempty"` that are zero-valued in the Go
// struct are dropped during json.Marshal, which means a JSON merge patch
// would leave a stale prior value in etcd. To avoid that, this method
// round-trips through a map and explicitly sets every omitted field to its
// Go zero value (nil for pointers/slices/maps, "" for strings, 0 for
// numbers, false for bools), so the merge patch clears them.
func (c *ClientsetClient) PatchObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, status any) error {
	// Marshal the typed status (honoring omitempty — partial patch), then
	// force-null the per-kind clearable keys so stale server-side values
	// clear (RFC 7386: null deletes the key).
	statusJSON, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	var statusMap map[string]any
	if err := json.Unmarshal(statusJSON, &statusMap); err != nil {
		return fmt.Errorf("unmarshal status: %w", err)
	}
	for key, zero := range crdstore.ClearStatusFields(gvr) {
		if _, ok := statusMap[key]; !ok {
			statusMap[key] = zero
		}
	}
	data, err := json.Marshal(map[string]any{"status": statusMap})
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}
	if namespace == "" {
		_, err = c.dynamic.Resource(gvr).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{}, "status")
	} else {
		_, err = c.dynamic.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{}, "status")
	}
	return err
}

// PatchObjectMeta merge-patches {"metadata": meta} on the main resource.
func (c *ClientsetClient) PatchObjectMeta(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, meta map[string]any) error {
	patch := map[string]any{"metadata": meta}
	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal metadata patch: %w", err)
	}
	if namespace == "" {
		_, err = c.dynamic.Resource(gvr).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	} else {
		_, err = c.dynamic.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	}
	return err
}
