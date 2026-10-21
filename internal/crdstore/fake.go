package crdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// StatusPatch records a call to PatchObjectStatus.
type StatusPatch struct {
	Namespace string
	Name      string
	Status    any
}

// Fake is an in-memory Backend for unit tests. Safe for concurrent use.
//
// The Fake models optimistic concurrency like the apiserver. Every write that
// changes an object takes the next resourceVersion. A write that carries a
// stale, non-empty resourceVersion answers Conflict.
//
// For a kind whose CRD sets the status subresource, the Fake treats .status
// as the apiserver does. CreateObject drops .status. UpdateObject keeps the
// stored .status. Only UpdateObjectStatus and the status patches change it.
// Seed and SeedObject store .status as given.
//
// The Fake models metadata.generation as the apiserver does. CreateObject sets
// it to 1. UpdateObject keeps the stored value and adds one when anything
// outside metadata changes. The status writes never change it. A status patch
// applies the clearable-key rule of ApplyStatusClears, as ClientsetClient does.
type Fake struct {
	mu sync.Mutex
	// rv is the one resourceVersion counter for the whole Fake. A deleted and
	// re-created object never reuses an earlier value, because the counter
	// never goes back.
	rv            uint64
	objects       map[schema.GroupVersionResource]map[string]*unstructured.Unstructured // gvr → "ns/name" → obj
	statusPatches map[schema.GroupVersionResource][]StatusPatch
	metaPatches   map[schema.GroupVersionResource][]MetaPatch
	failNext      map[failKey]error
	failAlways    map[failKey]error
	afterGet      map[schema.GroupVersionResource]func(namespace, name string)
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
		afterGet:      make(map[schema.GroupVersionResource]func(namespace, name string)),
	}
}

// --- Seed ----------------------------------------------------------------

// Seed converts each typed object via the registry and stores it in the fake.
// An object without a resourceVersion gets the next one, and Seed writes it
// back into obj, so a caller that reads obj later holds the stored value. An
// explicit numeric resourceVersion is kept as given.
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
		if unst.GetResourceVersion() == "" {
			f.bump(unst)
			if m, err := apimeta.Accessor(obj); err == nil {
				m.SetResourceVersion(unst.GetResourceVersion())
			}
		} else if n, err := strconv.ParseUint(unst.GetResourceVersion(), 10, 64); err == nil && n > f.rv {
			f.rv = n
		}
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

// SeedObject stores obj under gvr as given, status included. It is the
// unstructured sibling of Seed, for a test that builds a raw object. An
// object without a resourceVersion gets the next one. An explicit numeric
// resourceVersion is kept as given.
func (f *Fake) SeedObject(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) error {
	if obj.GetName() == "" {
		return fmt.Errorf("crdstore: SeedObject %s: name is empty", gvr.Resource)
	}
	clone := obj.DeepCopy()

	f.mu.Lock()
	defer f.mu.Unlock()
	if clone.GetResourceVersion() == "" {
		f.bump(clone)
	} else if n, err := strconv.ParseUint(clone.GetResourceVersion(), 10, 64); err == nil && n > f.rv {
		f.rv = n
	}
	if f.objects[gvr] == nil {
		f.objects[gvr] = make(map[string]*unstructured.Unstructured)
	}
	f.objects[gvr][nsName(clone.GetNamespace(), clone.GetName())] = clone
	return nil
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
	key := nsName(namespace, name)
	obj, ok := f.objects[gvr][key]
	hook := f.afterGet[gvr]
	// The copy happens under the lock. A patch from another goroutine
	// mutates the stored maps, and a copy outside the lock would read them
	// while that write runs.
	var out *unstructured.Unstructured
	if ok {
		out = obj.DeepCopy()
	}
	f.mu.Unlock()

	if !ok {
		gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
		return nil, apierrors.NewNotFound(gr, name)
	}
	if hook != nil {
		// The hook runs after the read and outside the lock, so it can
		// write through this same Fake. A test uses it to open the window
		// a real apiserver has between a caller's read and that caller's
		// later conditional write.
		hook(namespace, name)
	}
	return out, nil
}

// SetAfterGet installs fn, which runs after every GetObject of gvr that
// finds an object. fn runs outside the store lock, so it may write through
// this Fake. A nil fn removes the hook.
func (f *Fake) SetAfterGet(gvr schema.GroupVersionResource, fn func(namespace, name string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fn == nil {
		delete(f.afterGet, gvr)
		return
	}
	f.afterGet[gvr] = fn
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
	if hasStatusSubresource(gvr) {
		// The apiserver drops status on a create when the kind has a status subresource.
		delete(clone.Object, "status")
	}
	clone.SetGeneration(1)
	f.bump(clone)
	f.objects[gvr][key] = clone
	return clone.DeepCopy(), nil
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
	// The Backend contract requires a resourceVersion on an update. The
	// apiserver rejects an unconditional custom resource update the same way.
	if obj.GetResourceVersion() == "" {
		return nil, invalidUpdateVersion(gvr, obj)
	}
	// Optimistic concurrency: reject if resourceVersion doesn't match.
	if obj.GetResourceVersion() != stored.GetResourceVersion() {
		gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
		return nil, apierrors.NewConflict(gr, obj.GetName(), fmt.Errorf("stale resourceVersion"))
	}
	clone := obj.DeepCopy()
	if hasStatusSubresource(gvr) {
		// A main-resource update leaves .status as stored, as the apiserver does.
		if st, ok := stored.Object["status"]; ok {
			clone.Object["status"] = runtime.DeepCopyJSONValue(st)
		} else {
			delete(clone.Object, "status")
		}
	}
	// A client cannot set generation. The apiserver keeps the stored value and
	// adds one when anything outside metadata changes. For a status-subresource
	// kind, .status is already the stored status at this point.
	setGeneration(clone, stored.GetGeneration())
	if !reflect.DeepEqual(withoutMetadata(stored.Object), withoutMetadata(clone.Object)) {
		setGeneration(clone, stored.GetGeneration()+1)
	}
	if reflect.DeepEqual(stored.Object, clone.Object) {
		// An update that changes nothing keeps the value, as the apiserver does.
		return stored.DeepCopy(), nil
	}
	f.bump(clone)
	f.objects[gvr][key] = clone
	return clone.DeepCopy(), nil
}

// invalidUpdateVersion is the Invalid error the apiserver returns for an
// update that names no resourceVersion.
func invalidUpdateVersion(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) error {
	return apierrors.NewInvalid(
		schema.GroupKind{Group: gvr.Group, Kind: obj.GetKind()},
		obj.GetName(),
		field.ErrorList{field.Invalid(
			field.NewPath("metadata", "resourceVersion"),
			obj.GetResourceVersion(),
			"must be specified for an update",
		)},
	)
}

// UpdateObjectStatus replaces only the stored object's "status" field,
// leaving spec/metadata untouched — mirroring the real ClientsetClient's
// /status subresource semantics, where a plain UpdateObject leaves .status
// unchanged. Uses the same optimistic-concurrency check as UpdateObject,
// and advances the stored resourceVersion like PatchObjectStatus.
func (f *Fake) UpdateObjectStatus(_ context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := f.inject("updatestatus", gvr); err != nil {
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
	if obj.GetResourceVersion() == "" {
		return nil, invalidUpdateVersion(gvr, obj)
	}
	if obj.GetResourceVersion() != stored.GetResourceVersion() {
		gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
		return nil, apierrors.NewConflict(gr, obj.GetName(), fmt.Errorf("stale resourceVersion"))
	}
	clone := stored.DeepCopy()
	if st, ok := obj.Object["status"]; ok {
		clone.Object["status"] = st
	} else {
		delete(clone.Object, "status")
	}
	if reflect.DeepEqual(stored.Object, clone.Object) {
		// A status write that changes nothing keeps the value, as the apiserver does.
		return stored.DeepCopy(), nil
	}
	// A status write that changes the object moves the resourceVersion. A Fake
	// that froze it would let a second writer with the same read pass the
	// precondition.
	f.bump(clone)
	f.objects[gvr][key] = clone
	return clone.DeepCopy(), nil
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

// DeleteObjectIfUnchanged removes the object only while it still carries uid
// and resourceVersion, answering Conflict otherwise. The apiserver enforces
// the same precondition, so a test exercising a stale-read race gets the same
// answer here. A missing object is not an error.
func (f *Fake) DeleteObjectIfUnchanged(_ context.Context, gvr schema.GroupVersionResource, namespace, name, uid, resourceVersion string) error {
	if err := f.inject("delete", gvr); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	key := nsName(namespace, name)
	obj, ok := f.objects[gvr][key]
	if !ok {
		return nil // idempotent: match real ClientsetClient behavior
	}
	if uid != "" && string(obj.GetUID()) != uid {
		return apierrors.NewConflict(
			schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, name,
			fmt.Errorf("uid %q does not match %q", uid, obj.GetUID()))
	}
	if resourceVersion != "" && obj.GetResourceVersion() != resourceVersion {
		return apierrors.NewConflict(
			schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, name,
			fmt.Errorf("resourceVersion %q does not match %q", resourceVersion, obj.GetResourceVersion()))
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

	obj := f.objects[gvr][nsName(namespace, name)]
	if obj == nil {
		return nil // best-effort; the real API would also succeed if obj doesn't exist? No, it would fail. But for test ergonomics, be lenient.
	}
	before := obj.DeepCopy()
	if err := applyStatusPatch(gvr, obj, status); err != nil {
		return err
	}
	if !reflect.DeepEqual(before.Object, obj.Object) {
		f.bump(obj)
	}
	return nil
}

// PatchObjectStatusAt applies a status merge patch only while the stored
// object carries resourceVersion, the way the apiserver honors a
// resourceVersion in a patch body. It records the call and shares the
// "patchstatus" error-injection verb with PatchObjectStatus. A missing
// object is not an error, as in PatchObjectStatus, and leaves
// resourceVersion unchanged. It returns the resourceVersion after the patch.
// It is not part of Backend: test clients use it to model a conditional
// status patch.
func (f *Fake) PatchObjectStatusAt(_ context.Context, gvr schema.GroupVersionResource, namespace, name, resourceVersion string, status any) (string, error) {
	if err := f.inject("patchstatus", gvr); err != nil {
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	obj := f.objects[gvr][nsName(namespace, name)]
	if obj == nil {
		f.statusPatches[gvr] = append(f.statusPatches[gvr], StatusPatch{Namespace: namespace, Name: name, Status: status})
		return resourceVersion, nil
	}
	if obj.GetResourceVersion() != resourceVersion {
		gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
		return "", apierrors.NewConflict(gr, name,
			fmt.Errorf("resourceVersion %q does not match %q", resourceVersion, obj.GetResourceVersion()))
	}
	f.statusPatches[gvr] = append(f.statusPatches[gvr], StatusPatch{Namespace: namespace, Name: name, Status: status})
	before := obj.DeepCopy()
	if err := applyStatusPatch(gvr, obj, status); err != nil {
		return "", err
	}
	// A patch that changes the object moves its resourceVersion, and a patch
	// that changes nothing leaves it, as the apiserver does. A Fake that froze
	// it would let a conditional delete pass over a write that really happened.
	if !reflect.DeepEqual(before.Object, obj.Object) {
		f.bump(obj)
	}
	return obj.GetResourceVersion(), nil
}

// applyStatusPatch applies status to obj as a JSON merge patch (RFC 7386),
// matching ClientsetClient's real merge-patch semantics. The body comes from
// StatusPatchMap, so each clearable key that status omits takes its zero value,
// as it does on the apiserver path. The body is then converted to JSON-native
// types, so an int zero does not reach the stored object. The merge recurses
// into nested objects and a null value deletes its key, as RFC 7386 requires;
// a non-object value replaces whatever it lands on, so a list is replaced
// wholesale rather than merged.
func applyStatusPatch(gvr schema.GroupVersionResource, obj *unstructured.Unstructured, status any) error {
	statusMap, err := StatusPatchMap(gvr, status)
	if err != nil {
		return err
	}
	statusMap, _ = jsonNative(statusMap).(map[string]any)
	existing, _ := obj.Object["status"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	mergePatchMap(existing, statusMap)
	obj.Object["status"] = existing
	return nil
}

// bump gives obj the next resourceVersion from the Fake-wide counter. A
// numeric value above the counter raises the counter first, so the next value
// is always greater. The caller holds f.mu.
func (f *Fake) bump(obj *unstructured.Unstructured) {
	if n, err := strconv.ParseUint(obj.GetResourceVersion(), 10, 64); err == nil && n > f.rv {
		f.rv = n
	}
	f.rv++
	obj.SetResourceVersion(strconv.FormatUint(f.rv, 10))
}

// mergePatchMap applies patch onto dst per RFC 7386: a null value deletes its
// key, two objects merge recursively, and any other value replaces what it
// lands on.
func mergePatchMap(dst, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		patchChild, patchIsMap := v.(map[string]any)
		dstChild, dstIsMap := dst[k].(map[string]any)
		if patchIsMap && dstIsMap {
			mergePatchMap(dstChild, patchChild)
			continue
		}
		dst[k] = v
	}
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

	obj := f.objects[gvr][nsName(namespace, name)]
	if obj == nil {
		return nil // best-effort for test ergonomics
	}
	before := obj.DeepCopy()
	applyMetaPatch(obj, meta)
	if !reflect.DeepEqual(before.Object, obj.Object) {
		f.bump(obj)
	}
	return nil
}

// PatchObjectMetaAt applies a metadata patch only while the stored object
// carries resourceVersion, and records the call like PatchObjectMeta. It
// shares the "patchmeta" error-injection verb with PatchObjectMeta. Like the
// apiserver, it answers NotFound for a missing object, and it moves the
// resourceVersion only when the patch changes the object.
func (f *Fake) PatchObjectMetaAt(_ context.Context, gvr schema.GroupVersionResource, namespace, name, resourceVersion string, meta map[string]any) (string, error) {
	if err := f.inject("patchmeta", gvr); err != nil {
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.metaPatches[gvr] = append(f.metaPatches[gvr], MetaPatch{Namespace: namespace, Name: name, Meta: meta})

	gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}
	obj := f.objects[gvr][nsName(namespace, name)]
	if obj == nil {
		return "", apierrors.NewNotFound(gr, name)
	}
	if obj.GetResourceVersion() != resourceVersion {
		return "", apierrors.NewConflict(gr, name,
			fmt.Errorf("resourceVersion %q does not match %q", resourceVersion, obj.GetResourceVersion()))
	}
	before := obj.DeepCopy()
	applyMetaPatch(obj, meta)
	if !reflect.DeepEqual(before.Object, obj.Object) {
		f.bump(obj)
	}
	return obj.GetResourceVersion(), nil
}

// applyMetaPatch shallow-merges each top-level key of meta into the
// metadata of obj.
func applyMetaPatch(obj *unstructured.Unstructured, meta map[string]any) {
	objMeta, ok := obj.Object["metadata"].(map[string]any)
	if !ok {
		objMeta = make(map[string]any)
		obj.Object["metadata"] = objMeta
	}
	for k, v := range meta {
		// The real backend merge-patches over the wire, so the stored object
		// holds JSON-native types. Normalize v the same way: otherwise a raw
		// map[string]string label survives in the fake and panics
		// Unstructured's DeepCopy on a later Get, and mergeAnnotations would
		// miss map[string]string annotation patches entirely.
		v = jsonNative(v)
		switch k {
		case "annotations":
			mergeAnnotations(objMeta, v)
		case "finalizers":
			objMeta["finalizers"] = toAnySlice(v)
		default:
			objMeta[k] = v
		}
	}
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

// jsonNative round-trips a value through JSON so it matches the types the real
// backend stores after a wire merge-patch (map[string]interface{},
// []interface{}, ...). The fake otherwise stores typed Go values directly,
// which breaks Unstructured.DeepCopy on a later Get.
func jsonNative(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return v
	}
	return out
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

// setGeneration sets metadata.generation to gen. A zero gen removes the field,
// so a no-op update does not add generation 0 and move the resourceVersion.
func setGeneration(obj *unstructured.Unstructured, gen int64) {
	if gen == 0 {
		unstructured.RemoveNestedField(obj.Object, "metadata", "generation")
		return
	}
	obj.SetGeneration(gen)
}

// withoutMetadata returns a shallow copy of m that omits the metadata key.
func withoutMetadata(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != "metadata" {
			out[k] = v
		}
	}
	return out
}

func nsName(namespace, name string) string {
	if namespace == "" {
		return "/" + name
	}
	return namespace + "/" + name
}
