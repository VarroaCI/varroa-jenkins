package controller

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

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
	bindings, unknown := buildDesiredRoleBindings(cr)
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
	bindings, unknown := buildDesiredRoleBindings(cr)
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
	bindings, unknown := buildDesiredRoleBindings(cr)
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
	bindings, unknown := buildDesiredRoleBindings(cr)
	if len(unknown) != 0 {
		t.Errorf("expected 0 unknown, got %v", unknown)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}

	b := bindings[0]
	if b.Name != rbacBindingName("ns", "test", "developer") {
		t.Errorf("expected name %q, got %q", rbacBindingName("ns", "test", "developer"), b.Name)
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
	bindings, unknown := buildDesiredRoleBindings(cr)
	if len(unknown) != 1 || unknown[0] != "superuser" {
		t.Errorf("expected unknown=[superuser], got %v", unknown)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	if bindings[0].Name != rbacBindingName("ns", "test", "admin") {
		t.Errorf("expected name %q, got %q", rbacBindingName("ns", "test", "admin"), bindings[0].Name)
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
	bindings, unknown := buildDesiredRoleBindings(cr)
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

// seedRBACController seeds ns/test with one group per role. The controller
// carries the LabelControllerName label, so the label stamp issues no patch.
func seedRBACController(t *testing.T, store *crdstore.Fake, roles ...string) *v1alpha1.Controller {
	t.Helper()
	return seedRBACControllerAt(t, store, "ns", "test", roles...)
}

// seedRBACControllerAt seeds namespace/name with one group per role.
func seedRBACControllerAt(t *testing.T, store *crdstore.Fake, namespace, name string, roles ...string) *v1alpha1.Controller {
	t.Helper()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{v1alpha1.LabelControllerName: name},
		},
	}
	if len(roles) > 0 {
		cr.Spec.RBACSpec = &v1alpha1.RBACSpec{}
	}
	for _, role := range roles {
		cr.Spec.RBACSpec.Groups = append(cr.Spec.RBACSpec.Groups,
			v1alpha1.RBACGroupBinding{Name: role + "-group", Role: role})
	}
	crdstore.MustSeed(store, cr)
	return cr
}

// labeledBinding returns a JenkinsRoleBinding that carries the three operator
// ownership labels for ns/ctrl.
func labeledBinding(name, ns, ctrl string) *v1alpha1.JenkinsRoleBinding {
	return &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("uid-" + name),
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:           v1alpha1.ManagedByOperator,
				v1alpha1.LabelControllerNamespace: ns,
				v1alpha1.LabelControllerName:      ctrl,
			},
		},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			RoleRef:  "varroa-viewer",
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "hand"}},
		},
	}
}

func bindingExists(t *testing.T, store crdstore.Backend, name string) bool {
	t.Helper()
	_, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), store, name, "")
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get JenkinsRoleBinding %s: %v", name, err)
	}
	return true
}

// rbacDeleteRecorder fails the test on an unfenced delete and records each
// fenced one as "name uid resourceVersion".
type rbacDeleteRecorder struct {
	*crdstore.Fake
	t       *testing.T
	deletes []string
}

func (f *rbacDeleteRecorder) DeleteObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	f.t.Errorf("unfenced delete of %s", name)
	return f.Fake.DeleteObject(ctx, gvr, namespace, name)
}

func (f *rbacDeleteRecorder) DeleteObjectIfUnchanged(ctx context.Context, gvr schema.GroupVersionResource, namespace, name, uid, resourceVersion string) error {
	f.deletes = append(f.deletes, name+" "+uid+" "+resourceVersion)
	return f.Fake.DeleteObjectIfUnchanged(ctx, gvr, namespace, name, uid, resourceVersion)
}

func TestSyncRBACBindings_KeepsLabeledBindingWithOtherName(t *testing.T) {
	store := crdstore.NewFake()
	cr := seedRBACController(t, store, "developer")
	crdstore.MustSeed(store, labeledBinding("payments-admins", "ns", "test"))

	r := &Reconciler{store: store, Logger: slog.Default()}
	if err := r.syncRBACBindings(context.Background(), cr); err != nil {
		t.Fatalf("syncRBACBindings: %v", err)
	}
	if !bindingExists(t, store, "payments-admins") {
		t.Error("labeled binding with a non-derived name was deleted")
	}
	if !bindingExists(t, store, rbacBindingName("ns", "test", "developer")) {
		t.Error("desired developer binding was not written")
	}
}

func TestSyncRBACBindings_DeletesOwnedBindingOfRemovedRole(t *testing.T) {
	store := crdstore.NewFake()
	cr := seedRBACController(t, store, "developer")
	crdstore.MustSeed(store, labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "test"))

	r := &Reconciler{store: store, Logger: slog.Default()}
	if err := r.syncRBACBindings(context.Background(), cr); err != nil {
		t.Fatalf("syncRBACBindings: %v", err)
	}
	if bindingExists(t, store, rbacBindingName("ns", "test", "admin")) {
		t.Error("owned binding of a removed role survived")
	}
	if !bindingExists(t, store, rbacBindingName("ns", "test", "developer")) {
		t.Error("desired developer binding missing")
	}
}

func TestSyncRBACBindings_KeepsUnlabeledBindingAtDerivedName(t *testing.T) {
	store := crdstore.NewFake()
	cr := seedRBACController(t, store, "developer")
	unlabeled := labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "test")
	unlabeled.Labels = nil
	crdstore.MustSeed(store, unlabeled)

	r := &Reconciler{store: store, Logger: slog.Default()}
	if err := r.syncRBACBindings(context.Background(), cr); err != nil {
		t.Fatalf("syncRBACBindings: %v", err)
	}
	if !bindingExists(t, store, rbacBindingName("ns", "test", "admin")) {
		t.Error("unlabeled object at a derived name was deleted")
	}
}

func TestSyncRBACBindings_DeleteCarriesListedVersion(t *testing.T) {
	store := crdstore.NewFake()
	cr := seedRBACController(t, store, "developer")
	stale := labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "test")
	crdstore.MustSeed(store, stale)

	rec := &rbacDeleteRecorder{Fake: store, t: t}
	r := &Reconciler{store: rec, Logger: slog.Default()}
	if err := r.syncRBACBindings(context.Background(), cr); err != nil {
		t.Fatalf("syncRBACBindings: %v", err)
	}
	name := rbacBindingName("ns", "test", "admin")
	want := []string{name + " uid-" + name + " " + stale.ResourceVersion}
	if !reflect.DeepEqual(rec.deletes, want) {
		t.Errorf("deletes = %q, want %q", rec.deletes, want)
	}
}

func TestSyncRBACBindings_DeleteConflictKeepsBinding(t *testing.T) {
	store := crdstore.NewFake()
	cr := seedRBACController(t, store, "developer")
	crdstore.MustSeed(store, labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "test"))
	jrbGVR, err := crdstore.GVRFor[v1alpha1.JenkinsRoleBinding]()
	if err != nil {
		t.Fatal(err)
	}
	store.FailNext("delete", jrbGVR, apierrors.NewConflict(jrbGVR.GroupResource(), rbacBindingName("ns", "test", "admin"), nil))

	r := &Reconciler{store: store, Logger: slog.Default()}
	if err := r.syncRBACBindings(context.Background(), cr); err != nil {
		t.Fatalf("syncRBACBindings: %v", err)
	}
	if !bindingExists(t, store, rbacBindingName("ns", "test", "admin")) {
		t.Error("binding was deleted despite a conflict")
	}
}

func TestDeleteControllerBindings_DeletesOnlyOwned(t *testing.T) {
	store := crdstore.NewFake()
	cr := seedRBACController(t, store)
	crdstore.MustSeed(store,
		labeledBinding(rbacBindingName("ns", "test", "viewer"), "ns", "test"),
		labeledBinding("copy-of-viewer", "ns", "test"),
		labeledBinding(rbacBindingName("ns", "other", "viewer"), "ns", "other"),
	)
	unlabeled := labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "test")
	unlabeled.Labels = nil
	crdstore.MustSeed(store, unlabeled)

	r := &Reconciler{store: store, Logger: slog.Default()}
	if err := r.deleteControllerBindings(context.Background(), cr); err != nil {
		t.Fatalf("deleteControllerBindings: %v", err)
	}
	if bindingExists(t, store, rbacBindingName("ns", "test", "viewer")) {
		t.Error("owned binding survived the finalizer")
	}
	for _, kept := range []string{"copy-of-viewer", rbacBindingName("ns", "other", "viewer"), rbacBindingName("ns", "test", "admin")} {
		if !bindingExists(t, store, kept) {
			t.Errorf("%s was deleted but is not owned", kept)
		}
	}
}

func TestOwnsRBACBinding(t *testing.T) {
	cases := []struct {
		name    string
		binding *v1alpha1.JenkinsRoleBinding
		ns, ctl string
		want    bool
	}{
		{"owned", labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "test"), "ns", "test", true},
		{"labels with another name", labeledBinding("payments-admins", "ns", "test"), "ns", "test", false},
		{"derived name without labels", func() *v1alpha1.JenkinsRoleBinding {
			b := labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "test")
			b.Labels = nil
			return b
		}(), "ns", "test", false},
		{"suffix not an rbacSpec role", labeledBinding(rbacBindingName("ns", "test", "superuser"), "ns", "test"), "ns", "test", false},
		{"managed-by not the operator", func() *v1alpha1.JenkinsRoleBinding {
			b := labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "test")
			b.Labels[v1alpha1.LabelManagedBy] = "local"
			return b
		}(), "ns", "test", false},
		{"labels of another controller", labeledBinding(rbacBindingName("ns", "test", "admin"), "ns", "other"), "ns", "test", false},
		{"hash name of another controller with labels of this one", labeledBinding(rbacBindingName("a-b", "c", "admin"), "a", "b-c"), "a", "b-c", false},
		{"dash-joined name with labels of this controller", labeledBinding("ctrl-ns-test-admin", "ns", "test"), "ns", "test", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownsRBACBinding(tc.binding, tc.ns, tc.ctl); got != tc.want {
				t.Errorf("ownsRBACBinding = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRBACSpecRoleRefs(t *testing.T) {
	want := map[string]string{
		"admin":     "varroa-admin",
		"operator":  "varroa-operator",
		"developer": "varroa-developer",
		"viewer":    "varroa-viewer",
	}
	// The CRD enum on RBACGroupBinding.Role lists these same four roles.
	if got := rbacSpecRoleRefs(); !reflect.DeepEqual(got, want) {
		t.Errorf("rbacSpecRoleRefs = %v, want %v", got, want)
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
	// Seed writes the stored resourceVersion back into cr.
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       wantSpec,
	}
	crdstore.MustSeed(store, cr)

	r := &Reconciler{store: store, Logger: slog.Default()}
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

func TestRBACBindingName_Golden(t *testing.T) {
	cases := []struct{ ns, name, role, want string }{
		{"ns", "test", "developer", "ctrl-cf4a83dbe7472025-developer"},
		{"a-b", "c", "admin", "ctrl-4e84717d75bd540d-admin"},
		{"a", "b-c", "admin", "ctrl-b88f83c840abe581-admin"},
		{"prod-a", "jenkins-1", "admin", "ctrl-fbff6b43b023e716-admin"},
	}
	for _, tc := range cases {
		if got := rbacBindingName(tc.ns, tc.name, tc.role); got != tc.want {
			t.Errorf("rbacBindingName(%q, %q, %q) = %q, want %q", tc.ns, tc.name, tc.role, got, tc.want)
		}
	}
}

func TestRBACBindingName_ValidForLongNames(t *testing.T) {
	ns := strings.Repeat("a", 63)
	name := strings.Repeat("b", 253)
	for role := range rbacSpecRoleRefs() {
		n := rbacBindingName(ns, name, role)
		if errs := validation.IsDNS1123Subdomain(n); len(errs) != 0 {
			t.Errorf("rbacBindingName for role %q is not a valid name: %v", role, errs)
		}
		if len(n) > 31 {
			t.Errorf("rbacBindingName for role %q has %d characters, want at most 31", role, len(n))
		}
	}
}

func TestSyncRBACBindings_DashSplitControllersKeepOwnBindings(t *testing.T) {
	store := crdstore.NewFake()
	a := seedRBACControllerAt(t, store, "a-b", "c", "admin")
	b := seedRBACControllerAt(t, store, "a", "b-c", "admin")
	ctx := context.Background()
	r := &Reconciler{store: store, Logger: slog.Default()}

	syncAll := func(order ...*v1alpha1.Controller) {
		t.Helper()
		for _, cr := range order {
			if err := r.syncRBACBindings(ctx, cr); err != nil {
				t.Fatalf("syncRBACBindings %s/%s: %v", cr.Namespace, cr.Name, err)
			}
		}
	}
	versions := func() map[string]string {
		t.Helper()
		out := make(map[string]string)
		for _, n := range []string{rbacBindingName("a-b", "c", "admin"), rbacBindingName("a", "b-c", "admin")} {
			obj, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, store, n, "")
			if err != nil {
				t.Fatalf("get %s: %v", n, err)
			}
			out[n] = obj.ResourceVersion
		}
		return out
	}

	syncAll(a, b)
	firstRound := versions()
	syncAll(b, a)
	if got := versions(); !reflect.DeepEqual(got, firstRound) {
		t.Errorf("bindings rewritten on a later sync: before %v, after %v", firstRound, got)
	}

	for _, cr := range []*v1alpha1.Controller{a, b} {
		obj, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, store, rbacBindingName(cr.Namespace, cr.Name, "admin"), "")
		if err != nil {
			t.Fatalf("get binding of %s/%s: %v", cr.Namespace, cr.Name, err)
		}
		if obj.Labels[v1alpha1.LabelControllerNamespace] != cr.Namespace || obj.Labels[v1alpha1.LabelControllerName] != cr.Name {
			t.Errorf("binding of %s/%s carries labels of another controller: %v", cr.Namespace, cr.Name, obj.Labels)
		}
		if got := obj.Spec.ControllerScope.Namespaces; !reflect.DeepEqual(got, []string{cr.Namespace}) {
			t.Errorf("binding of %s/%s scope namespaces = %v", cr.Namespace, cr.Name, got)
		}
		if got := obj.Spec.ControllerScope.ControllerSelector.MatchLabels[v1alpha1.LabelControllerName]; got != cr.Name {
			t.Errorf("binding of %s/%s scope selector = %q", cr.Namespace, cr.Name, got)
		}
	}
}

func TestRBACSpecBindingOwner(t *testing.T) {
	owned := labeledBinding(rbacBindingName("prod-a", "jenkins-1", "admin"), "prod-a", "jenkins-1")
	renamed := labeledBinding("payments-admins", "prod-a", "jenkins-1")
	crossed := labeledBinding(rbacBindingName("a-b", "c", "admin"), "a", "b-c")
	local := labeledBinding(rbacBindingName("prod-a", "jenkins-1", "admin"), "prod-a", "jenkins-1")
	local.Labels[v1alpha1.LabelManagedBy] = "local"
	noNS := labeledBinding(rbacBindingName("prod-a", "jenkins-1", "admin"), "", "jenkins-1")
	cases := []struct {
		name   string
		b      *v1alpha1.JenkinsRoleBinding
		ns, n  string
		wantOK bool
	}{
		{"owned", owned, "prod-a", "jenkins-1", true},
		{"renamed copy keeps labels", renamed, "", "", false},
		{"derived name of another controller", crossed, "", "", false},
		{"managed-by is not the operator", local, "", "", false},
		{"empty namespace label", noNS, "", "", false},
		{"nil binding", nil, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, n, ok := RBACSpecBindingOwner(tc.b)
			if ok != tc.wantOK || ns != tc.ns || n != tc.n {
				t.Errorf("RBACSpecBindingOwner() = (%q, %q, %v), want (%q, %q, %v)", ns, n, ok, tc.ns, tc.n, tc.wantOK)
			}
		})
	}
}
