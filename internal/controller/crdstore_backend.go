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

// UpdateObjectStatus replaces the /status subresource. Like UpdateObject, obj
// must carry a valid resourceVersion — a concurrent write since that
// resourceVersion was read surfaces as apierrors.IsConflict. Required for any
// kind registered with `subresources: {status: {}}` (e.g. ProfileCandidate),
// whose .status a plain UpdateObject silently leaves unchanged.
func (c *ClientsetClient) UpdateObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if namespace == "" {
		return c.dynamic.Resource(gvr).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
	}
	return c.dynamic.Resource(gvr).Namespace(namespace).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
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

// DeleteObjectIfUnchanged deletes an object only while it still carries uid
// and resourceVersion. The apiserver answers Conflict when either has moved,
// so a caller that decided from an earlier read never acts on a stale view.
// A missing object is not an error.
func (c *ClientsetClient) DeleteObjectIfUnchanged(ctx context.Context, gvr schema.GroupVersionResource, namespace, name, uid, resourceVersion string) error {
	pre := &metav1.Preconditions{}
	if uid != "" {
		v := types.UID(uid)
		pre.UID = &v
	}
	if resourceVersion != "" {
		v := resourceVersion
		pre.ResourceVersion = &v
	}
	opts := metav1.DeleteOptions{}
	if pre.UID != nil || pre.ResourceVersion != nil {
		opts.Preconditions = pre
	}

	var err error
	if namespace == "" {
		err = c.dynamic.Resource(gvr).Delete(ctx, name, opts)
	} else {
		err = c.dynamic.Resource(gvr).Namespace(namespace).Delete(ctx, name, opts)
	}
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// PatchObjectStatus merge-patches the /status subresource. The body comes
// from crdstore.StatusPatchMap, which sets each clearable key that the typed
// status omits to its zero value, so a stale server-side value clears.
func (c *ClientsetClient) PatchObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, status any) error {
	statusMap, err := crdstore.StatusPatchMap(gvr, status)
	if err != nil {
		return fmt.Errorf("build status patch: %w", err)
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

// PatchObjectMetaAt merge-patches {"metadata": meta} with resourceVersion in
// the body. The apiserver treats a resourceVersion in a patch as a
// precondition and answers Conflict when the object moved since.
func (c *ClientsetClient) PatchObjectMetaAt(ctx context.Context, gvr schema.GroupVersionResource, namespace, name, resourceVersion string, meta map[string]any) (string, error) {
	// An empty resourceVersion in a patch is no precondition at all.
	if resourceVersion == "" {
		return "", fmt.Errorf("patch %s %q metadata: resourceVersion is required", gvr.Resource, name)
	}
	body := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		body[k] = v
	}
	body["resourceVersion"] = resourceVersion
	data, err := json.Marshal(map[string]any{"metadata": body})
	if err != nil {
		return "", fmt.Errorf("marshal metadata patch: %w", err)
	}
	var out *unstructured.Unstructured
	if namespace == "" {
		out, err = c.dynamic.Resource(gvr).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	} else {
		out, err = c.dynamic.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	}
	if err != nil {
		return "", err
	}
	return out.GetResourceVersion(), nil
}
