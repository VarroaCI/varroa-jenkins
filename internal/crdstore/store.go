// Package crdstore provides a generic typed store for varroa.dev CRDs,
// replacing per-kind CRUD fanout on ResourceClient.
package crdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Backend is the unstructured-level storage seam. Implemented by
// controller.ClientsetClient (real) and Fake (tests).
type Backend interface {
	// GetObject returns a single object by gvr, namespace, and name.
	// Errors are returned unwrapped so apierrors.IsNotFound works at call sites.
	GetObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)

	// ListObjects lists all objects of gvr. namespace "" means all namespaces
	// (or cluster-scoped kinds). labelSelector "" means no selector.
	ListObjects(ctx context.Context, gvr schema.GroupVersionResource, namespace, labelSelector string) ([]unstructured.Unstructured, error)

	// CreateObject creates an object. The returned object carries the
	// server-assigned resourceVersion and uid.
	CreateObject(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)

	// UpdateObject updates an object. The caller must set resourceVersion on obj.
	UpdateObject(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)

	// DeleteObject deletes an object. A missing object is not an error.
	DeleteObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error

	// PatchObjectStatus merge-patches the /status subresource with
	// {"status": status}. The status value is marshalled to JSON as part of
	// the patch body.
	PatchObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, status any) error

	// PatchObjectMeta merge-patches {"metadata": meta} on the main resource
	// (no subresource). meta is the raw map for the metadata field.
	PatchObjectMeta(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, meta map[string]any) error
}

// ---------------------------------------------------------------------------
// Typed generic helpers
// ---------------------------------------------------------------------------

// Get retrieves a typed CRD by name and namespace. namespace "" for
// cluster-scoped kinds. Returns the backend error unwrapped.
func Get[T any](ctx context.Context, b Backend, name, namespace string) (*T, error) {
	info, err := gvrInfo[T]()
	if err != nil {
		return nil, err
	}
	u, err := b.GetObject(ctx, info.gvr, namespace, name)
	if err != nil {
		return nil, err // unwrapped — callers check apierrors.IsNotFound
	}
	return fromUnstructured[T](u)
}

// List retrieves all typed CRDs, optionally filtered by namespace and
// labelSelector. namespace "" returns objects across all namespaces (or
// cluster-scoped). labelSelector "" means no selector.
func List[T any](ctx context.Context, b Backend, namespace, labelSelector string) ([]*T, error) {
	info, err := gvrInfo[T]()
	if err != nil {
		return nil, err
	}
	items, err := b.ListObjects(ctx, info.gvr, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", info.gvr.Resource, err)
	}
	result := make([]*T, 0, len(items))
	for i := range items {
		t, convErr := fromUnstructured[T](&items[i])
		if convErr != nil {
			return nil, fmt.Errorf("convert %s %s: %w", info.gvr.Resource, items[i].GetName(), convErr)
		}
		result = append(result, t)
	}
	return result, nil
}

// Create persists a typed CRD. The object's resourceVersion is cleared before
// creation (the server assigns one). On success the server-assigned metadata
// is written back into obj via mergeCreateResult.
func Create[T any](ctx context.Context, b Backend, obj *T) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	u, err := toUnstructured(obj, info)
	if err != nil {
		return err
	}
	u.SetResourceVersion("")

	ns := namespaceFromObj(u, info)
	created, err := b.CreateObject(ctx, info.gvr, ns, u)
	if err != nil {
		return fmt.Errorf("create %s: %w", info.gvr.Resource, err)
	}
	// Write back server-assigned metadata so caller sees it.
	mergeCreateResult(obj, created)
	return nil
}

// Update persists a typed CRD. The object must carry a valid resourceVersion.
func Update[T any](ctx context.Context, b Backend, obj *T) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	u, err := toUnstructured(obj, info)
	if err != nil {
		return err
	}
	ns := namespaceFromObj(u, info)
	// Callers (config-authoring forms) often reconstruct objects without
	// server metadata; stamp the live resourceVersion so the update is not
	// rejected, matching the pre-crdstore per-kind Update methods.
	if u.GetResourceVersion() == "" {
		existing, getErr := b.GetObject(ctx, info.gvr, ns, u.GetName())
		if getErr != nil {
			return fmt.Errorf("get existing %s for update: %w", info.gvr.Resource, getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
	}
	created, err := b.UpdateObject(ctx, info.gvr, ns, u)
	if err != nil {
		return fmt.Errorf("update %s: %w", info.gvr.Resource, err)
	}
	mergeCreateResult(obj, created)
	return nil
}

// ErrNotOwned is returned by ApplyOwned when the live object exists and the
// caller's ownership predicate rejects it.
var ErrNotOwned = errors.New("crdstore: object is owned by another writer")

// ApplyOwned creates obj, or updates it only when owned reports true for the
// live object.
//
// It exists because Apply cannot carry this guard. Apply clears
// resourceVersion, calls CreateObject, and on IsAlreadyExists re-Gets purely to
// obtain a resourceVersion before a full-object replace — so a check performed
// before calling Apply is not sufficient: Apply's own internal Get happens
// later and never consults ownership, and an object created in between is still
// clobbered. Apply's contract is unchanged; many call sites depend on it.
//
// Semantics: Get; not found -> Create, looping back to Get on AlreadyExists
// rather than blindly Updating; found and !owned -> ErrNotOwned; found and
// owned -> Update carrying the LIVE resourceVersion, so a concurrent write
// loses with a conflict instead of being silently replaced. One retry from Get
// on conflict.
func ApplyOwned[T any](ctx context.Context, b Backend, obj *T, owned func(live *unstructured.Unstructured) bool) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	u, err := toUnstructured(obj, info)
	if err != nil {
		return err
	}
	ns := namespaceFromObj(u, info)
	name := u.GetName()

	// Two attempts: the second covers a conflict or a create/delete race.
	for attempt := 0; attempt < 2; attempt++ {
		live, getErr := b.GetObject(ctx, info.gvr, ns, name)
		if apierrors.IsNotFound(getErr) {
			u.SetResourceVersion("")
			created, createErr := b.CreateObject(ctx, info.gvr, ns, u)
			if createErr == nil {
				mergeCreateResult(obj, created)
				return nil
			}
			if apierrors.IsAlreadyExists(createErr) {
				// Someone created it between the Get and the Create. Loop back
				// to Get so the ownership check runs against what is really
				// there, rather than replacing it sight unseen.
				continue
			}
			return fmt.Errorf("create %s: %w", info.gvr.Resource, createErr)
		}
		if getErr != nil {
			return fmt.Errorf("get %s: %w", info.gvr.Resource, getErr)
		}
		if owned != nil && !owned(live) {
			return fmt.Errorf("%s %q: %w", info.gvr.Resource, name, ErrNotOwned)
		}
		u.SetResourceVersion(live.GetResourceVersion())
		updated, updErr := b.UpdateObject(ctx, info.gvr, ns, u)
		if updErr == nil {
			mergeCreateResult(obj, updated)
			return nil
		}
		if apierrors.IsConflict(updErr) {
			continue
		}
		return fmt.Errorf("update %s: %w", info.gvr.Resource, updErr)
	}
	return fmt.Errorf("apply %s %q: exhausted retries", info.gvr.Resource, name)
}

// Apply creates or updates a typed CRD. On create, resourceVersion is cleared
// before the call. On IsAlreadyExists, the live object is fetched for its
// resourceVersion, that version is stamped on the object, and Update is
// attempted.
func Apply[T any](ctx context.Context, b Backend, obj *T) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	u, err := toUnstructured(obj, info)
	if err != nil {
		return err
	}
	ns := namespaceFromObj(u, info)

	u.SetResourceVersion("")
	_, err = b.CreateObject(ctx, info.gvr, ns, u)
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := b.GetObject(ctx, info.gvr, ns, u.GetName())
		if getErr != nil {
			return fmt.Errorf("get existing %s for update: %w", info.gvr.Resource, getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		created, updErr := b.UpdateObject(ctx, info.gvr, ns, u)
		if updErr != nil {
			return fmt.Errorf("update %s: %w", info.gvr.Resource, updErr)
		}
		mergeCreateResult(obj, created)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", info.gvr.Resource, err)
	}
	return nil
}

// Delete deletes a typed CRD by name and namespace. namespace "" for
// cluster-scoped kinds. A missing object is not an error.
func Delete[T any](ctx context.Context, b Backend, name, namespace string) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	if err := b.DeleteObject(ctx, info.gvr, namespace, name); err != nil {
		return fmt.Errorf("delete %s: %w", info.gvr.Resource, err)
	}
	return nil
}

// PatchStatus merge-patches the /status subresource.
func PatchStatus[T any](ctx context.Context, b Backend, name, namespace string, status any) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	if err := b.PatchObjectStatus(ctx, info.gvr, namespace, name, status); err != nil {
		return fmt.Errorf("patch %s status: %w", info.gvr.Resource, err)
	}
	return nil
}

// PatchAnnotations merge-patches metadata.annotations. A nil *string value
// deletes the key (JSON merge-patch null semantics).
func PatchAnnotations[T any](ctx context.Context, b Backend, name, namespace string, ann map[string]*string) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	annMap := make(map[string]any, len(ann))
	for k, v := range ann {
		if v == nil {
			annMap[k] = nil
		} else {
			annMap[k] = *v
		}
	}
	meta := map[string]any{"annotations": annMap}
	if err := b.PatchObjectMeta(ctx, info.gvr, namespace, name, meta); err != nil {
		return fmt.Errorf("patch %s annotations: %w", info.gvr.Resource, err)
	}
	return nil
}

// PatchLabels merge-patches metadata.labels. A nil *string value deletes the
// key (JSON merge-patch null semantics), matching PatchAnnotations.
func PatchLabels[T any](ctx context.Context, b Backend, name, namespace string, labels map[string]*string) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	labelMap := make(map[string]any, len(labels))
	for k, v := range labels {
		if v == nil {
			labelMap[k] = nil
		} else {
			labelMap[k] = *v
		}
	}
	meta := map[string]any{"labels": labelMap}
	if err := b.PatchObjectMeta(ctx, info.gvr, namespace, name, meta); err != nil {
		return fmt.Errorf("patch %s labels: %w", info.gvr.Resource, err)
	}
	return nil
}

// PatchFinalizers patches metadata.finalizers.
func PatchFinalizers[T any](ctx context.Context, b Backend, name, namespace string, finalizers []string) error {
	info, err := gvrInfo[T]()
	if err != nil {
		return err
	}
	meta := map[string]any{"finalizers": finalizers}
	if err := b.PatchObjectMeta(ctx, info.gvr, namespace, name, meta); err != nil {
		return fmt.Errorf("patch %s finalizers: %w", info.gvr.Resource, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// toUnstructured converts a typed CRD to *unstructured.Unstructured.
func toUnstructured[T any](obj *T, info kindInfo) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("convert %s to unstructured: %w", info.gvr.Resource, err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind(info.kind)
	return u, nil
}

// fromUnstructured converts an *unstructured.Unstructured to a typed CRD,
// restoring DeletionTimestamp (which FromUnstructured drops).
func fromUnstructured[T any](u *unstructured.Unstructured) (*T, error) {
	var obj T
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &obj); err != nil {
		return nil, err
	}
	// FromUnstructured does not properly deserialize metav1.Time fields.
	if dt := u.GetDeletionTimestamp(); dt != nil {
		setDeletionTimestamp(&obj, dt)
	}
	return &obj, nil
}

// mergeCreateResult copies server-assigned metadata (uid, resourceVersion,
// generation, creationTimestamp) back into the typed object via JSON
// round-trip, so callers get the persisted version.
func mergeCreateResult(obj any, created *unstructured.Unstructured) {
	if created == nil {
		return
	}
	data, err := json.Marshal(created.Object)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, obj)
}

// namespaceFromObj extracts the namespace from an unstructured object.
// For cluster-scoped kinds (namespaced=false), returns "".
func namespaceFromObj(u *unstructured.Unstructured, info kindInfo) string {
	if !info.namespaced {
		return ""
	}
	return u.GetNamespace()
}
