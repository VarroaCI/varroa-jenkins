package crdstore

import (
	"context"
	"errors"
	"testing"

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
