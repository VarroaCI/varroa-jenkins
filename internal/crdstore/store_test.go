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

func TestRegistry_AllTypesResolve(t *testing.T) {
	tests := []struct {
		name string
		gvr  func() (schema.GroupVersionResource, error)
	}{
		{"Controller", GVRFor[v1alpha1.Controller]},
		{"PodTemplate", GVRFor[v1alpha1.PodTemplate]},
		{"CatalogSource", GVRFor[v1alpha1.CatalogSource]},
		{"CatalogItem", GVRFor[v1alpha1.CatalogItem]},
		{"ComposedBundle", GVRFor[v1alpha1.ComposedBundle]},
		{"BroodOperation", GVRFor[v1alpha1.BroodOperation]},
		{"User", GVRFor[v1alpha1.User]},
		{"VarroaRole", GVRFor[v1alpha1.VarroaRole]},
		{"VarroaRoleBinding", GVRFor[v1alpha1.VarroaRoleBinding]},
		{"JenkinsRole", GVRFor[v1alpha1.JenkinsRole]},
		{"JenkinsRoleBinding", GVRFor[v1alpha1.JenkinsRoleBinding]},
		{"ProvisioningDefaults", GVRFor[v1alpha1.ProvisioningDefaults]},
		{"ControllerClass", GVRFor[v1alpha1.ControllerClass]},
		{"JenkinsVersionProfile", GVRFor[v1alpha1.JenkinsVersionProfile]},
		{"Group", GVRFor[v1alpha1.Group]},
		{"Team", GVRFor[v1alpha1.Team]},
		{"Runner", GVRFor[v1alpha1.Runner]},
		{"UpdateCenter", GVRFor[v1alpha1.UpdateCenter]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gvr, err := tt.gvr()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gvr.Group != "varroa.dev" || gvr.Version != "v1alpha1" || gvr.Resource == "" {
				t.Errorf("unexpected GVR: %+v", gvr)
			}
		})
	}
}

func TestRegistry_UnregisteredTypeErrors(t *testing.T) {
	type notACRD struct{ metav1.TypeMeta }
	_, err := GVRFor[notACRD]()
	if err == nil {
		t.Fatal("expected error for unregistered type")
	}
}

func TestRoundTrip_Namespaced(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	want := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ctrl", Namespace: "test-ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.479.1"},
	}
	MustSeed(f, want)

	got, err := Get[v1alpha1.Controller](ctx, f, "test-ctrl", "test-ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Version != want.Spec.Version {
		t.Errorf("spec version = %q, want %q", got.Spec.Version, want.Spec.Version)
	}
}

func TestRoundTrip_ClusterScoped(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	want := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-team"},
		Spec: v1alpha1.TeamSpec{
			DisplayName: "Platform Team",
			Members:     []string{"alice", "bob"},
		},
	}
	MustSeed(f, want)

	got, err := Get[v1alpha1.Team](ctx, f, "platform-team", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.DisplayName != want.Spec.DisplayName {
		t.Errorf("display name = %q, want %q", got.Spec.DisplayName, want.Spec.DisplayName)
	}
	if len(got.Spec.Members) != 2 {
		t.Errorf("members len = %d, want 2", len(got.Spec.Members))
	}
}

func TestApply_CreatesWhenAbsent(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	obj := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "new-ctrl", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	}
	if err := Apply(ctx, f, obj); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := Get[v1alpha1.Controller](ctx, f, "new-ctrl", "ns")
	if err != nil {
		t.Fatalf("Get after apply: %v", err)
	}
	if got.Spec.Version != "1.0" {
		t.Errorf("version = %q, want %q", got.Spec.Version, "1.0")
	}
}

func TestApply_UpdatesWhenExists(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	existing := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	}
	MustSeed(f, existing)

	obj := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	if err := Apply(ctx, f, obj); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	if err != nil {
		t.Fatalf("Get after apply: %v", err)
	}
	if got.Spec.Version != "2.0" {
		t.Errorf("version = %q, want %q", got.Spec.Version, "2.0")
	}
}

func TestApply_IgnoresPresetResourceVersion(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	// resourceVersion set by caller should be cleared before Create.
	obj := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns", ResourceVersion: "99999"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	}
	if err := Apply(ctx, f, obj); err != nil {
		t.Fatalf("Apply with preset RV: %v", err)
	}
	// Should not error — the preset RV was cleared.
}

func TestList_LabelSelector(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	a := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", Labels: map[string]string{"env": "prod"}},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	}
	b := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns", Labels: map[string]string{"env": "dev"}},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	MustSeed(f, a, b)

	got, err := List[v1alpha1.Controller](ctx, f, "ns", "env=prod")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].Name != "a" {
		t.Errorf("expected 'a', got %q", got[0].Name)
	}
}

func TestList_CrossNamespace(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	a := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns1"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	}
	b := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns2"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.0"},
	}
	MustSeed(f, a, b)

	got, err := List[v1alpha1.Controller](ctx, f, "", "")
	if err != nil {
		t.Fatalf("List cross-ns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
}

func TestPatchStatus_RecordedAndVisible(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	obj := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "1.0"},
	}
	MustSeed(f, obj)

	status := &v1alpha1.ControllerStatus{Phase: "Connected"}
	if err := PatchStatus[v1alpha1.Controller](ctx, f, "ctrl", "ns", status); err != nil {
		t.Fatalf("PatchStatus: %v", err)
	}

	// Check recorded.
	gvr, _ := GVRFor[v1alpha1.Controller]()
	patches := f.StatusPatches(gvr)
	if len(patches) != 1 {
		t.Fatalf("expected 1 status patch, got %d", len(patches))
	}
	if patches[0].Name != "ctrl" {
		t.Errorf("patch name = %q, want %q", patches[0].Name, "ctrl")
	}

	// Check visible via Get.
	got, err := Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	if err != nil {
		t.Fatalf("Get after patch: %v", err)
	}
	if got.Status.Phase != "Connected" {
		t.Errorf("status phase = %q, want %q", got.Status.Phase, "Connected")
	}
}

func TestPatchAnnotations_NilDeletes(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	obj := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "ctrl",
			Namespace:   "ns",
			Annotations: map[string]string{"keep": "val", "remove": "old"},
		},
		Spec: v1alpha1.ControllerSpec{Version: "1.0"},
	}
	MustSeed(f, obj)

	ann := map[string]*string{
		"keep":   strPtr("new-val"),
		"remove": nil, // delete
		"add":    strPtr("added"),
	}
	if err := PatchAnnotations[v1alpha1.Controller](ctx, f, "ctrl", "ns", ann); err != nil {
		t.Fatalf("PatchAnnotations: %v", err)
	}

	got, err := Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Annotations["keep"] != "new-val" {
		t.Errorf("keep = %q, want %q", got.Annotations["keep"], "new-val")
	}
	if _, ok := got.Annotations["remove"]; ok {
		t.Error("remove key should have been deleted")
	}
	if got.Annotations["add"] != "added" {
		t.Errorf("add = %q, want %q", got.Annotations["add"], "added")
	}
}

func TestDeletionTimestamp_Restored(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	now := metav1.Now()
	// Directly seed an unstructured with deletionTimestamp set so we test
	// the FromUnstructured restore path.
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata": map[string]any{
			"name":              "dying",
			"namespace":         "ns",
			"deletionTimestamp": now.UTC().Format("2006-01-02T15:04:05Z"),
		},
		"spec": map[string]any{},
	}}

	f.mu.Lock()
	gvr, _ := GVRFor[v1alpha1.Controller]()
	if f.objects[gvr] == nil {
		f.objects[gvr] = make(map[string]*unstructured.Unstructured)
	}
	f.objects[gvr]["ns/dying"] = u
	f.mu.Unlock()

	got, err := Get[v1alpha1.Controller](ctx, f, "dying", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DeletionTimestamp == nil {
		t.Fatal("DeletionTimestamp should be non-nil")
	}
}

func TestFailNext_SurfacesError(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	gvr, _ := GVRFor[v1alpha1.Controller]()
	injected := errors.New("injected failure")
	f.FailNext("get", gvr, injected)

	_, err := Get[v1alpha1.Controller](ctx, f, "any", "ns")
	if err == nil {
		t.Fatal("expected injected error")
	}
	if !errors.Is(err, injected) {
		t.Errorf("error = %v, want %v", err, injected)
	}

	// Second call should succeed (one-shot).
	MustSeed(f, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "any", Namespace: "ns"},
	})
	got, err := Get[v1alpha1.Controller](ctx, f, "any", "ns")
	if err != nil {
		t.Fatalf("second Get after FailNext: %v", err)
	}
	if got.Name != "any" {
		t.Errorf("name = %q, want %q", got.Name, "any")
	}
}

func TestFailAlways_PersistentError(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	gvr, _ := GVRFor[v1alpha1.Controller]()
	injected := errors.New("persistent failure")
	f.FailAlways("list", gvr, injected)

	_, err := List[v1alpha1.Controller](ctx, f, "", "")
	if err == nil {
		t.Fatal("expected injected error on first call")
	}

	_, err = List[v1alpha1.Controller](ctx, f, "", "")
	if err == nil {
		t.Fatal("expected injected error on second call")
	}
}

// --- helpers ---

func strPtr(s string) *string { return &s }

func seedCASController(t *testing.T, f *Fake) {
	t.Helper()
	MustSeed(f, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns", ResourceVersion: "1"},
		Status:     v1alpha1.ControllerStatus{Phase: "Connected", ConfigHash: "old"},
	})
}

// TestUpdate_StaleResourceVersionConflictsAndKeepsTheConcurrentWrite pins that
// Update sends the read resourceVersion, so a write since the read is not
// overwritten.
func TestUpdate_StaleResourceVersionConflictsAndKeepsTheConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "ns", ResourceVersion: "1"},
	})
	gvr, _ := GVRFor[v1alpha1.User]()

	stale, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := f.PatchObjectMetaAt(ctx, gvr, "ns", "alice", "1", map[string]any{
		"annotations": map[string]any{"x": "concurrent"},
	}); err != nil {
		t.Fatalf("concurrent patch: %v", err)
	}

	stale.Spec.Email = "stale@example.com"
	err = Update(ctx, f, stale)
	if !apierrors.IsConflict(err) {
		t.Fatalf("Update with a stale resourceVersion = %v, want Conflict", err)
	}
	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get after conflict: %v", err)
	}
	if got.Annotations["x"] != "concurrent" {
		t.Errorf("concurrent annotation = %q, want %q", got.Annotations["x"], "concurrent")
	}
	if got.Spec.Email == "stale@example.com" {
		t.Errorf("stale write reached the store")
	}
}

// TestMutateStatus_RetriesOnConflictAndKeepsAConcurrentWrite pins that a
// write between the read and the status update makes MutateStatus read
// again, and that the final status holds both writes.
func TestMutateStatus_RetriesOnConflictAndKeepsAConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	seedCASController(t, f)
	gvr, _ := GVRFor[v1alpha1.Controller]()
	fired := false
	f.SetAfterGet(gvr, func(ns, name string) {
		if fired {
			return
		}
		fired = true
		if err := f.PatchObjectStatus(ctx, gvr, ns, name, map[string]any{"configHash": "owner"}); err != nil {
			t.Errorf("concurrent patch: %v", err)
		}
	})

	calls := 0
	changed, err := MutateStatus(ctx, f, "ctrl", "ns", func(c *v1alpha1.Controller) (bool, error) {
		calls++
		c.Status.RBACHash = "approve"
		return true, nil
	})
	if err != nil {
		t.Fatalf("MutateStatus: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if calls != 2 {
		t.Fatalf("mutate calls = %d, want 2", calls)
	}
	got, err := Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.ConfigHash != "owner" || got.Status.RBACHash != "approve" {
		t.Fatalf("status = configHash %q rbacHash %q, want owner and approve", got.Status.ConfigHash, got.Status.RBACHash)
	}
	if got.Status.Phase != "Connected" {
		t.Fatalf("phase = %q, want Connected", got.Status.Phase)
	}
}

// TestMutateStatus_NoChangeWritesNothing pins that a mutate that reports no
// change leaves the stored object and its resourceVersion as they are.
func TestMutateStatus_NoChangeWritesNothing(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	seedCASController(t, f)

	changed, err := MutateStatus(ctx, f, "ctrl", "ns", func(*v1alpha1.Controller) (bool, error) {
		return false, nil
	})
	if err != nil || changed {
		t.Fatalf("MutateStatus = %v, %v; want false, nil", changed, err)
	}
	got, _ := Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	if got.ResourceVersion != "1" {
		t.Fatalf("resourceVersion = %q, want 1", got.ResourceVersion)
	}
}

// TestMutateStatus_ReturnsTheMutateError pins that a mutate error stops the
// write and reaches the caller.
func TestMutateStatus_ReturnsTheMutateError(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	seedCASController(t, f)
	refused := errors.New("refused")

	changed, err := MutateStatus(ctx, f, "ctrl", "ns", func(*v1alpha1.Controller) (bool, error) {
		return true, refused
	})
	if !errors.Is(err, refused) || changed {
		t.Fatalf("MutateStatus = %v, %v; want false, refused", changed, err)
	}
}

// TestMutateStatus_ReturnsTheConflictAfterTheRetryBudget pins that a status
// write that always conflicts ends with the conflict, not a silent success.
func TestMutateStatus_ReturnsTheConflictAfterTheRetryBudget(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	seedCASController(t, f)
	gvr, _ := GVRFor[v1alpha1.Controller]()
	f.FailAlways("updatestatus", gvr, apierrors.NewConflict(
		schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, "ctrl", errors.New("stale")))

	changed, err := MutateStatus(ctx, f, "ctrl", "ns", func(c *v1alpha1.Controller) (bool, error) {
		c.Status.RBACHash = "approve"
		return true, nil
	})
	if !apierrors.IsConflict(err) || changed {
		t.Fatalf("MutateStatus = %v, %v; want false and a conflict", changed, err)
	}
}

// TestFakeUpdateObjectStatus_BumpsResourceVersion pins that a second writer
// that holds the same read loses the precondition.
func TestFakeUpdateObjectStatus_BumpsResourceVersion(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	seedCASController(t, f)

	first, _ := Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	second, _ := Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	first.Status.ConfigHash = "first"
	if err := UpdateStatus(ctx, f, first); err != nil {
		t.Fatalf("first UpdateStatus: %v", err)
	}
	if first.ResourceVersion != "2" {
		t.Fatalf("resourceVersion after write = %q, want 2", first.ResourceVersion)
	}
	second.Status.ConfigHash = "second"
	if err := UpdateStatus(ctx, f, second); !apierrors.IsConflict(err) {
		t.Fatalf("second UpdateStatus = %v, want a conflict", err)
	}
}

// TestPatchMetaAt_ConflictsOnStaleVersion pins that a metadata patch from a
// stale resourceVersion answers a conflict and changes nothing, and that a
// patch from the current version lands and returns the next version.
func TestPatchMetaAt_ConflictsOnStaleVersion(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	seedCASController(t, f)
	gvr, _ := GVRFor[v1alpha1.Controller]()
	if err := f.PatchObjectStatus(ctx, gvr, "ns", "ctrl", map[string]any{"configHash": "other"}); err != nil {
		t.Fatalf("concurrent patch: %v", err)
	}

	meta := map[string]any{"labels": map[string]any{"k": "v"}}
	if _, err := PatchMetaAt[v1alpha1.Controller](ctx, f, "ctrl", "ns", "1", meta); !apierrors.IsConflict(err) {
		t.Fatalf("stale PatchMetaAt error = %v, want a conflict", err)
	}
	got, err := Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Labels["k"] != "" {
		t.Fatalf("label after stale patch = %q, want none", got.Labels["k"])
	}

	rv, err := PatchMetaAt[v1alpha1.Controller](ctx, f, "ctrl", "ns", got.ResourceVersion, meta)
	if err != nil {
		t.Fatalf("current PatchMetaAt: %v", err)
	}
	got, err = Get[v1alpha1.Controller](ctx, f, "ctrl", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Labels["k"] != "v" {
		t.Fatalf("label = %q, want v", got.Labels["k"])
	}
	if rv != "3" || got.ResourceVersion != "3" {
		t.Fatalf("returned resourceVersion = %q, stored %q, want 3", rv, got.ResourceVersion)
	}
}

// TestPatchMetaAt_NoChangeKeepsVersion pins that a patch that changes
// nothing keeps the resourceVersion, as the apiserver does, so a caller that
// chains its writes keeps a valid version.
func TestPatchMetaAt_NoChangeKeepsVersion(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns", ResourceVersion: "4", Finalizers: []string{"varroa.dev/finalizer"}},
	})
	rv, err := PatchMetaAt[v1alpha1.Controller](ctx, f, "ctrl", "ns", "4",
		map[string]any{"finalizers": []string{"varroa.dev/finalizer"}})
	if err != nil {
		t.Fatalf("PatchMetaAt: %v", err)
	}
	if rv != "4" {
		t.Fatalf("resourceVersion = %q, want 4", rv)
	}
}
