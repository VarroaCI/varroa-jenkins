package crdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// StatusPatch records a call to PatchObjectStatus.
type StatusPatch struct {
	Namespace string
	Name      string
	Status    any
}

// Fake is an in-memory Backend for unit tests. Safe for concurrent use.
type Fake struct {
	mu            sync.Mutex
	objects       map[schema.GroupVersionResource]map[string]*unstructured.Unstructured // gvr → "ns/name" → obj
	statusPatches map[schema.GroupVersionResource][]StatusPatch
	metaPatches   map[schema.GroupVersionResource][]MetaPatch
	failNext      map[failKey]error
	failAlways    map[failKey]error
}

type failKey struct {
	verb string
	gvr  schema.GroupVersionResource
}

// NewFake returns an empty Fake store.
func NewFake() *Fake {
	return &Fake{
		objects:       make(map[schema.GroupVersionResource]map[string]*unstructured.Unstructured),
		statusPatches: make(map[schema.GroupVersionResource][]StatusPatch),
		metaPatches:   make(map[schema.GroupVersionResource][]MetaPatch),
		failNext:      make(map[failKey]error),
		failAlways:    make(map[failKey]error),
	}
}

// --- Seed ----------------------------------------------------------------

// Seed converts each typed object via the registry and stores it in the fake.
// Returns an error when a type is not registered or conversion fails.
func (f *Fake) Seed(objs ...any) error {
	for _, obj := range objs {
		t := reflect.TypeOf(obj).Elem()
		info, ok := registry[t]
		if !ok {
			return fmt.Errorf("crdstore: type %T not registered", obj)
		}
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return fmt.Errorf("crdstore: convert %T to unstructured: %w", obj, err)
		}
		unst := &unstructured.Unstructured{Object: u}
		unst.SetAPIVersion("varroa.dev/v1alpha1")
		unst.SetKind(info.kind)

		f.mu.Lock()
		if f.objects[info.gvr] == nil {
			f.objects[info.gvr] = make(map[string]*unstructured.Unstructured)
		}
		f.objects[info.gvr][nsName(unst.GetNamespace(), unst.GetName())] = unst
		f.mu.Unlock()
	}
	return nil
}

// MustSeed calls Seed and panics on error — test ergonomics.
func MustSeed(f *Fake, objs ...any) {
	if err := f.Seed(objs...); err != nil {
		panic(err)
	}
}

// --- Error injection ------------------------------------------------------

// FailNext makes the next call matching verb and gvr return err. One-shot.
func (f *Fake) FailNext(verb string, gvr schema.GroupVersionResource, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[failKey{verb, gvr}] = err
}

// FailAlways makes every subsequent call matching verb and gvr return err.
func (f *Fake) FailAlways(verb string, gvr schema.GroupVersionResource, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAlways[failKey{verb, gvr}] = err
}

func (f *Fake) inject(verb string, gvr schema.GroupVersionResource) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := failKey{verb, gvr}
	if err, ok := f.failNext[key]; ok {
		delete(f.failNext, key)
		return err
	}
	return f.failAlways[key]
}

// --- Backend implementation -----------------------------------------------

// GetObject returns the stored object or a NotFound error.
func (f *Fake) GetObject(_ context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	if err := f.inject("get", gvr); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	key := nsName(namespace, name)
	obj, ok := f.objects[gvr][key]
	if !ok {
		gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
		return nil, apierrors.NewNotFound(gr, name)
	}
	return obj.DeepCopy(), nil
}

// ListObjects returns stored objects, optionally filtered by namespace and
// labelSelector. namespace "" returns all.
func (f *Fake) ListObjects(_ context.Context, gvr schema.GroupVersionResource, namespace, labelSelector string) ([]unstructured.Unstructured, error) {
	if err := f.inject("list", gvr); err != nil {
		return nil, err
	}

	var sel labels.Selector
	if labelSelector != "" {
		var parseErr error
		sel, parseErr = labels.Parse(labelSelector)
		if parseErr != nil {
			return nil, fmt.Errorf("parse label selector: %w", parseErr)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	gvrMap := f.objects[gvr]
	result := make([]unstructured.Unstructured, 0, len(gvrMap))
	for _, obj := range gvrMap {
		if namespace != "" && obj.GetNamespace() != namespace {
			continue
		}
		if sel != nil && !sel.Matches(labels.Set(obj.GetLabels())) {
			continue
		}
		result = append(result, *obj.DeepCopy())
	}
	return result, nil
}

// CreateObject stores the object. Returns AlreadyExists when present.
func (f *Fake) CreateObject(_ context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := f.inject("create", gvr); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	key := nsName(namespace, obj.GetName())
	if f.objects[gvr] == nil {
		f.objects[gvr] = make(map[string]*unstructured.Unstructured)
	}
	if _, ok := f.objects[gvr][key]; ok {
		gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
		return nil, apierrors.NewAlreadyExists(gr, obj.GetName())
	}
	clone := obj.DeepCopy()
	f.objects[gvr][key] = clone
	return clone, nil
}

// UpdateObject updates the stored object. Returns NotFound when absent.
func (f *Fake) UpdateObject(_ context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := f.inject("update", gvr); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	key := nsName(namespace, obj.GetName())
	stored, ok := f.objects[gvr][key]
	if !ok {
		gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
		return nil, apierrors.NewNotFound(gr, obj.GetName())
	}
	// Optimistic concurrency: reject if resourceVersion doesn't match.
	if obj.GetResourceVersion() != "" && obj.GetResourceVersion() != stored.GetResourceVersion() {
		gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
		return nil, apierrors.NewConflict(gr, obj.GetName(), fmt.Errorf("stale resourceVersion"))
	}
	clone := obj.DeepCopy()
	f.objects[gvr][key] = clone
	return clone, nil
}

// DeleteObject removes the object. A missing object is not an error.
func (f *Fake) DeleteObject(_ context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	if err := f.inject("delete", gvr); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	key := nsName(namespace, name)
	if _, ok := f.objects[gvr][key]; !ok {
		return nil // idempotent: match real ClientsetClient behavior
	}
	delete(f.objects[gvr], key)
	return nil
}

// PatchObjectStatus records the patch and applies it to the stored object.
func (f *Fake) PatchObjectStatus(_ context.Context, gvr schema.GroupVersionResource, namespace, name string, status any) error {
	if err := f.inject("patchstatus", gvr); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Record the patch.
	sp := StatusPatch{Namespace: namespace, Name: name, Status: status}
	f.statusPatches[gvr] = append(f.statusPatches[gvr], sp)

	// Apply: marshal the status value, unmarshal to map[string]any, set on obj.
	key := nsName(namespace, name)
	obj := f.objects[gvr][key]
	if obj == nil {
		return nil // best-effort; the real API would also succeed if obj doesn't exist? No, it would fail. But for test ergonomics, be lenient.
	}
	statusJSON, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	var statusMap map[string]any
	if err := json.Unmarshal(statusJSON, &statusMap); err != nil {
		return fmt.Errorf("unmarshal status: %w", err)
	}
	obj.Object["status"] = statusMap
	return nil
}

// MetaPatch records one PatchObjectMeta call.
type MetaPatch struct {
	Namespace, Name string
	Meta            map[string]any
}

// MetaPatches returns every recorded PatchObjectMeta call for gvr, in order,
// including calls against objects that did not exist.
func (f *Fake) MetaPatches(gvr schema.GroupVersionResource) []MetaPatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]MetaPatch, len(f.metaPatches[gvr]))
	copy(out, f.metaPatches[gvr])
	return out
}

// PatchObjectMeta applies a shallow merge into metadata and records the call.
func (f *Fake) PatchObjectMeta(_ context.Context, gvr schema.GroupVersionResource, namespace, name string, meta map[string]any) error {
	if err := f.inject("patchmeta", gvr); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.metaPatches[gvr] = append(f.metaPatches[gvr], MetaPatch{Namespace: namespace, Name: name, Meta: meta})

	key := nsName(namespace, name)
	obj := f.objects[gvr][key]
	if obj == nil {
		return nil // best-effort for test ergonomics
	}

	objMeta, ok := obj.Object["metadata"].(map[string]any)
	if !ok {
		objMeta = make(map[string]any)
		obj.Object["metadata"] = objMeta
	}

	// Shallow merge each top-level key from meta into objMeta.
	for k, v := range meta {
		switch k {
		case "annotations":
			mergeAnnotations(objMeta, v)
		case "finalizers":
			objMeta["finalizers"] = toAnySlice(v)
		default:
			objMeta[k] = v
		}
	}
	return nil
}

// mergeAnnotations merges annotation patches with nil-means-delete semantics.
func mergeAnnotations(objMeta map[string]any, patchVal any) {
	patchMap, ok := patchVal.(map[string]any)
	if !ok {
		return
	}
	existing, _ := objMeta["annotations"].(map[string]any)
	if existing == nil {
		existing = make(map[string]any)
		objMeta["annotations"] = existing
	}
	for k, v := range patchMap {
		if v == nil {
			delete(existing, k)
		} else {
			existing[k] = v
		}
	}
}

// toAnySlice converts a []string value to []any so that runtime.DeepCopyJSON
// can handle it (it only knows how to deep-copy []any, not []string).
func toAnySlice(v any) any {
	ss, ok := v.([]string)
	if !ok {
		return v
	}
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// --- Accessors ------------------------------------------------------------

// StatusPatches returns all recorded status patches for the given gvr.
func (f *Fake) StatusPatches(gvr schema.GroupVersionResource) []StatusPatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StatusPatch, len(f.statusPatches[gvr]))
	copy(out, f.statusPatches[gvr])
	return out
}

// --- Helpers --------------------------------------------------------------

func nsName(namespace, name string) string {
	if namespace == "" {
		return "/" + name
	}
	return namespace + "/" + name
}
