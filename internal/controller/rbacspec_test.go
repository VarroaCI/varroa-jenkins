package controller

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func TestBuildDesiredRoleBindings_EmptyRBACSpec(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "ns",
		},
	}
	builtins := []*v1alpha1.VarroaRole{}
	bindings, unknown := buildDesiredRoleBindings(cr, builtins)
	if bindings != nil {
		t.Errorf("expected nil bindings, got %v", bindings)
	}
	if unknown != nil {
		t.Errorf("expected nil unknown, got %v", unknown)
	}
}

func TestBuildDesiredRoleBindings_NilRBACSpec(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "ns",
		},
		Spec: v1alpha1.ControllerSpec{
			RBACSpec: nil,
		},
	}
	builtins := []*v1alpha1.VarroaRole{}
	bindings, unknown := buildDesiredRoleBindings(cr, builtins)
	if bindings != nil {
		t.Errorf("expected nil bindings, got %v", bindings)
	}
	if unknown != nil {
		t.Errorf("expected nil unknown, got %v", unknown)
	}
}

func TestBuildDesiredRoleBindings_EmptyGroups(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "ns",
		},
		Spec: v1alpha1.ControllerSpec{
			RBACSpec: &v1alpha1.RBACSpec{
				Groups: []v1alpha1.RBACGroupBinding{},
			},
		},
	}
	builtins := []*v1alpha1.VarroaRole{}
	bindings, unknown := buildDesiredRoleBindings(cr, builtins)
	if bindings != nil {
		t.Errorf("expected nil bindings, got %v", bindings)
	}
	if unknown != nil {
		t.Errorf("expected nil unknown, got %v", unknown)
	}
}

func TestBuildDesiredRoleBindings_TwoGroupsOneRole(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "ns",
		},
		Spec: v1alpha1.ControllerSpec{
			RBACSpec: &v1alpha1.RBACSpec{
				Groups: []v1alpha1.RBACGroupBinding{
					{Name: "devs", Role: "developer"},
					{Name: "sres", Role: "developer"},
				},
			},
		},
	}
	builtins := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "developer",
				Labels: map[string]string{
					v1alpha1.LabelBuiltin: "true",
				},
			},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsRoleRef: "varroa-developer",
			},
		},
	}
	bindings, unknown := buildDesiredRoleBindings(cr, builtins)
	if len(unknown) != 0 {
		t.Errorf("expected 0 unknown, got %v", unknown)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}

	b := bindings[0]
	if b.Name != "ctrl-ns-test-developer" {
		t.Errorf("expected name 'ctrl-ns-test-developer', got %q", b.Name)
	}
	if b.Spec.RoleRef != "varroa-developer" {
		t.Errorf("expected RoleRef 'varroa-developer', got %q", b.Spec.RoleRef)
	}
	if len(b.Spec.Subjects) != 2 {
		t.Errorf("expected 2 subjects, got %d", len(b.Spec.Subjects))
	}
	if b.Spec.Subjects[0].Kind != "Group" || b.Spec.Subjects[0].Name != "devs" {
		t.Errorf("unexpected subject[0]: %+v", b.Spec.Subjects[0])
	}
	if b.Spec.Subjects[1].Kind != "Group" || b.Spec.Subjects[1].Name != "sres" {
		t.Errorf("unexpected subject[1]: %+v", b.Spec.Subjects[1])
	}

	// Labels
	if b.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByOperator {
		t.Errorf("expected managed-by label, got %v", b.Labels)
	}
	if b.Labels[v1alpha1.LabelControllerNamespace] != "ns" {
		t.Errorf("expected controller-namespace label 'ns', got %q", b.Labels[v1alpha1.LabelControllerNamespace])
	}
	if b.Labels[v1alpha1.LabelControllerName] != "test" {
		t.Errorf("expected controller-name label 'test', got %q", b.Labels[v1alpha1.LabelControllerName])
	}

	// ControllerScope
	if b.Spec.ControllerScope == nil {
		t.Fatal("expected ControllerScope to be set")
	}
	if !reflect.DeepEqual(b.Spec.ControllerScope.Namespaces, []string{"ns"}) {
		t.Errorf("expected Namespaces [ns], got %v", b.Spec.ControllerScope.Namespaces)
	}
	if b.Spec.ControllerScope.ControllerSelector == nil {
		t.Fatal("expected ControllerSelector to be set")
	}
	expectedMatchLabels := map[string]string{v1alpha1.LabelControllerName: "test"}
	if !reflect.DeepEqual(b.Spec.ControllerScope.ControllerSelector.MatchLabels, expectedMatchLabels) {
		t.Errorf("expected MatchLabels %v, got %v", expectedMatchLabels, b.Spec.ControllerScope.ControllerSelector.MatchLabels)
	}

	// JenkinsScope should be nil (Global)
	if b.Spec.JenkinsScope != nil {
		t.Errorf("expected nil JenkinsScope, got %v", b.Spec.JenkinsScope)
	}
}

func TestBuildDesiredRoleBindings_UnknownRoleSkipped(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "ns",
		},
		Spec: v1alpha1.ControllerSpec{
			RBACSpec: &v1alpha1.RBACSpec{
				Groups: []v1alpha1.RBACGroupBinding{
					{Name: "admins", Role: "admin"},
					{Name: "superusers", Role: "superuser"},
				},
			},
		},
	}
	builtins := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "admin",
				Labels: map[string]string{
					v1alpha1.LabelBuiltin: "true",
				},
			},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsRoleRef: "varroa-admin",
			},
		},
	}
	bindings, unknown := buildDesiredRoleBindings(cr, builtins)
	if len(unknown) != 1 || unknown[0] != "superuser" {
		t.Errorf("expected unknown=[superuser], got %v", unknown)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	if bindings[0].Name != "ctrl-ns-test-admin" {
		t.Errorf("expected name 'ctrl-ns-test-admin', got %q", bindings[0].Name)
	}
	if bindings[0].Spec.RoleRef != "varroa-admin" {
		t.Errorf("expected RoleRef 'varroa-admin', got %q", bindings[0].Spec.RoleRef)
	}
}

func TestBuildDesiredRoleBindings_MultipleRoles(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "ns",
		},
		Spec: v1alpha1.ControllerSpec{
			RBACSpec: &v1alpha1.RBACSpec{
				Groups: []v1alpha1.RBACGroupBinding{
					{Name: "admins", Role: "admin"},
					{Name: "devs", Role: "developer"},
					{Name: "ops", Role: "operator"},
				},
			},
		},
	}
	builtins := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "admin",
				Labels: map[string]string{
					v1alpha1.LabelBuiltin: "true",
				},
			},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsRoleRef: "varroa-admin",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "developer",
				Labels: map[string]string{
					v1alpha1.LabelBuiltin: "true",
				},
			},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsRoleRef: "varroa-developer",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "operator",
				Labels: map[string]string{
					v1alpha1.LabelBuiltin: "true",
				},
			},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsRoleRef: "varroa-operator",
			},
		},
	}
	bindings, unknown := buildDesiredRoleBindings(cr, builtins)
	if len(unknown) != 0 {
		t.Errorf("expected 0 unknown, got %v", unknown)
	}
	if len(bindings) != 3 {
		t.Fatalf("expected 3 bindings, got %d", len(bindings))
	}

	byRole := make(map[string]*v1alpha1.JenkinsRoleBinding)
	for _, b := range bindings {
		byRole[b.Spec.RoleRef] = b
	}

	adminBinding := byRole["varroa-admin"]
	if adminBinding == nil {
		t.Fatal("expected varroa-admin binding")
	}
	if len(adminBinding.Spec.Subjects) != 1 || adminBinding.Spec.Subjects[0].Name != "admins" {
		t.Errorf("unexpected admin subjects: %+v", adminBinding.Spec.Subjects)
	}

	devBinding := byRole["varroa-developer"]
	if devBinding == nil {
		t.Fatal("expected varroa-developer binding")
	}
	if len(devBinding.Spec.Subjects) != 1 || devBinding.Spec.Subjects[0].Name != "devs" {
		t.Errorf("unexpected dev subjects: %+v", devBinding.Spec.Subjects)
	}

	opBinding := byRole["varroa-operator"]
	if opBinding == nil {
		t.Fatal("expected varroa-operator binding")
	}
	if len(opBinding.Spec.Subjects) != 1 || opBinding.Spec.Subjects[0].Name != "ops" {
		t.Errorf("unexpected operator subjects: %+v", opBinding.Spec.Subjects)
	}
}

// TestSyncRBACBindings_LabelStampClaimsNoSpec (5.9): the RBAC label stamp is a
// labels-only metadata merge patch. It must not write the whole Controller — a
// full-object Apply would transfer ownership of every spec field present — so
// the stored spec stays byte-identical and the only write is
// {"metadata":{"labels":{...}}}.
func TestSyncRBACBindings_LabelStampClaimsNoSpec(t *testing.T) {
	store := crdstore.NewFake()
	ctrlGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatal(err)
	}
	wantSpec := v1alpha1.ControllerSpec{Version: "2.479", ClassName: "standard"}
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       wantSpec,
	})

	r := &Reconciler{store: store, Logger: slog.Default()}
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       wantSpec,
	}
	if err := r.syncRBACBindings(context.Background(), cr); err != nil {
		t.Fatalf("syncRBACBindings: %v", err)
	}

	stored, err := crdstore.Get[v1alpha1.Controller](context.Background(), store, "test", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	if !reflect.DeepEqual(stored.Spec, wantSpec) {
		t.Fatalf("label stamp changed spec: %+v", stored.Spec)
	}

	patches := store.MetaPatches(ctrlGVR)
	if len(patches) != 1 {
		t.Fatalf("metadata patches = %d, want exactly 1 (labels-only)", len(patches))
	}
	p := patches[0]
	labels, ok := p.Meta["labels"].(map[string]any)
	if !ok || labels[v1alpha1.LabelControllerName] != "test" {
		t.Fatalf("metadata patch labels = %#v, want %q: %q", p.Meta["labels"], v1alpha1.LabelControllerName, "test")
	}
	if _, hasSpec := p.Meta["spec"]; hasSpec {
		t.Fatalf("label stamp carried a spec key: %#v", p.Meta)
	}
}
