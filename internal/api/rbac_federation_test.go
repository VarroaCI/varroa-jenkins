package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

type fakeFederationStore struct{ items map[string]interface{} }

func (s *fakeFederationStore) List() []interface{} {
	out := make([]interface{}, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out
}

func (s *fakeFederationStore) GetByKey(key string) (interface{}, bool, error) {
	item, ok := s.items[key]
	return item, ok, nil
}

type fakeConfigBrood struct {
	roles       map[string]map[string]*v1alpha1.JenkinsRole
	bindings    map[string]map[string]*v1alpha1.JenkinsRoleBinding
	unreachable map[string]bool
	// hiddenFromList omits a role from ListJenkinsRoles while keeping it
	// visible to Get/Create, simulating a role created between the
	// reconciler's list and its create (the isConvergentCreate race).
	hiddenFromList map[string]bool
	ops            []string
}

func newFakeConfigBrood() *fakeConfigBrood {
	return &fakeConfigBrood{roles: map[string]map[string]*v1alpha1.JenkinsRole{}, bindings: map[string]map[string]*v1alpha1.JenkinsRoleBinding{}, unreachable: map[string]bool{}}
}

func (f *fakeConfigBrood) ensure(cluster string) {
	if f.roles[cluster] == nil {
		f.roles[cluster] = map[string]*v1alpha1.JenkinsRole{}
	}
	if f.bindings[cluster] == nil {
		f.bindings[cluster] = map[string]*v1alpha1.JenkinsRoleBinding{}
	}
}

func (f *fakeConfigBrood) ListJenkinsRoles(_ context.Context, cluster string) ([]json.RawMessage, error) {
	if f.unreachable[cluster] {
		return nil, errors.New("offline")
	}
	f.ensure(cluster)
	if len(f.hiddenFromList) > 0 {
		visible := map[string]*v1alpha1.JenkinsRole{}
		for name, role := range f.roles[cluster] {
			if !f.hiddenFromList[name] {
				visible[name] = role
			}
		}
		return marshalValues(visible)
	}
	return marshalValues(f.roles[cluster])
}

func (f *fakeConfigBrood) GetJenkinsRole(_ context.Context, cluster, name string) (json.RawMessage, error) {
	f.ensure(cluster)
	role := f.roles[cluster][name]
	if role == nil {
		return nil, &BroodError{Code: bus.CodeNotFound}
	}
	return json.Marshal(role)
}

func (f *fakeConfigBrood) CreateJenkinsRole(_ context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	f.ensure(cluster)
	if f.roles[cluster][name] != nil {
		return nil, &BroodError{Code: bus.CodeConflict}
	}
	var role v1alpha1.JenkinsRole
	_ = json.Unmarshal(obj, &role)
	f.roles[cluster][name] = &role
	f.ops = append(f.ops, "create-role:"+cluster+":"+name)
	return obj, nil
}

func (f *fakeConfigBrood) UpdateJenkinsRole(_ context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	f.ensure(cluster)
	if f.roles[cluster][name] == nil {
		return nil, &BroodError{Code: bus.CodeNotFound}
	}
	var role v1alpha1.JenkinsRole
	_ = json.Unmarshal(obj, &role)
	f.roles[cluster][name] = &role
	f.ops = append(f.ops, "update-role:"+cluster+":"+name)
	return obj, nil
}

func (f *fakeConfigBrood) DeleteJenkinsRole(_ context.Context, cluster, name string) error {
	f.ensure(cluster)
	delete(f.roles[cluster], name)
	f.ops = append(f.ops, "delete-role:"+cluster+":"+name)
	return nil
}

func (f *fakeConfigBrood) ListJenkinsRoleBindings(_ context.Context, cluster string) ([]json.RawMessage, error) {
	if f.unreachable[cluster] {
		return nil, errors.New("offline")
	}
	f.ensure(cluster)
	return marshalValues(f.bindings[cluster])
}

func (f *fakeConfigBrood) GetJenkinsRoleBinding(_ context.Context, cluster, name string) (json.RawMessage, error) {
	f.ensure(cluster)
	binding := f.bindings[cluster][name]
	if binding == nil {
		return nil, &BroodError{Code: bus.CodeNotFound}
	}
	return json.Marshal(binding)
}

func (f *fakeConfigBrood) CreateJenkinsRoleBinding(_ context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	f.ensure(cluster)
	if f.bindings[cluster][name] != nil {
		return nil, &BroodError{Code: bus.CodeConflict}
	}
	var binding v1alpha1.JenkinsRoleBinding
	_ = json.Unmarshal(obj, &binding)
	f.bindings[cluster][name] = &binding
	f.ops = append(f.ops, "create-binding:"+cluster+":"+name)
	return obj, nil
}

func (f *fakeConfigBrood) UpdateJenkinsRoleBinding(_ context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	f.ensure(cluster)
	if f.bindings[cluster][name] == nil {
		return nil, &BroodError{Code: bus.CodeNotFound}
	}
	var binding v1alpha1.JenkinsRoleBinding
	_ = json.Unmarshal(obj, &binding)
	f.bindings[cluster][name] = &binding
	f.ops = append(f.ops, "update-binding:"+cluster+":"+name)
	return obj, nil
}

func (f *fakeConfigBrood) DeleteJenkinsRoleBinding(_ context.Context, cluster, name string) error {
	f.ensure(cluster)
	delete(f.bindings[cluster], name)
	f.ops = append(f.ops, "delete-binding:"+cluster+":"+name)
	return nil
}

func marshalValues[T any](m map[string]*T) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(m))
	for _, item := range m {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func TestRBACFederationReconcilerConvergesAndIsIdempotent(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "core"}, {Name: "dev-cluster"}})
	brood.ensure("dev-cluster")
	brood.roles["dev-cluster"]["stale"] = &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "stale", Labels: map[string]string{rbac.LabelFederatedFrom: "old"}}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Job/Delete"}}}
	brood.bindings["dev-cluster"]["stale-binding"] = &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "stale-binding", Labels: map[string]string{rbac.LabelFederatedFrom: "old"}}, Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "stale"}}

	warnings := reconciler.Reconcile(context.Background())
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if brood.roles["core"] != nil || brood.bindings["core"] != nil {
		t.Fatalf("core was written: roles=%v bindings=%v", brood.roles["core"], brood.bindings["core"])
	}
	if got := brood.roles["dev-cluster"]["jenkins-admin"]; got == nil || got.Spec.Description != "core copy" {
		t.Fatalf("role not created/updated: %#v", got)
	}
	if len(brood.bindings["dev-cluster"]) != 1 {
		t.Fatalf("bindings after GC/create = %#v", brood.bindings["dev-cluster"])
	}
	if brood.roles["dev-cluster"]["stale"] != nil || brood.bindings["dev-cluster"]["stale-binding"] != nil {
		t.Fatalf("stale federated objects were not garbage collected")
	}

	brood.ops = nil
	warnings = reconciler.Reconcile(context.Background())
	if len(warnings) != 0 || len(brood.ops) != 0 {
		t.Fatalf("repeat reconcile warnings=%v ops=%v", warnings, brood.ops)
	}
}

func TestRBACFederationReconcilerSkipsUnlabeledCollisions(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	brood.ensure("dev-cluster")
	desiredBindings := reconciler.desiredBindingsForTest()
	brood.roles["dev-cluster"]["jenkins-admin"] = &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Job/Read"}}}
	brood.bindings["dev-cluster"][desiredBindings[0].Name] = &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: desiredBindings[0].Name}, Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "other"}}

	warnings := reconciler.Reconcile(context.Background())
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "unlabeled JenkinsRole collision") || !strings.Contains(strings.Join(warnings, "\n"), "unlabeled JenkinsRoleBinding collision") {
		t.Fatalf("warnings = %v", warnings)
	}
	if brood.roles["dev-cluster"]["jenkins-admin"].Spec.Permissions[0] != "Job/Read" || brood.bindings["dev-cluster"][desiredBindings[0].Name].Spec.RoleRef != "other" {
		t.Fatalf("unlabeled objects were overwritten")
	}
}

func TestRBACFederationReconcilerAcceptsMatchingBuiltinCreateRace(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	builtin := configureBuiltinDesiredRole(reconciler)
	brood.ensure("dev-cluster")
	actual := builtin.DeepCopy()
	brood.roles["dev-cluster"][actual.Name] = actual
	// Hide the builtin from the list so the reconciler attempts a create and
	// hits the conflict path (role created between list and create).
	brood.hiddenFromList = map[string]bool{actual.Name: true}
	desiredBindings := reconciler.desiredBindingsForTest()
	brood.bindings["dev-cluster"][desiredBindings[0].Name] = desiredBindings[0].DeepCopy()
	brood.ops = nil

	warnings := reconciler.Reconcile(context.Background())
	if len(warnings) != 0 {
		t.Fatalf("matching builtin create race must stay silent, got warnings = %v", warnings)
	}
}

func TestRBACFederationReconcilerAcceptsMatchingBuiltinCollision(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	builtin := configureBuiltinDesiredRole(reconciler)
	brood.ensure("dev-cluster")
	actual := builtin.DeepCopy()
	brood.roles["dev-cluster"][actual.Name] = actual
	desiredBindings := reconciler.desiredBindingsForTest()
	brood.bindings["dev-cluster"][desiredBindings[0].Name] = desiredBindings[0].DeepCopy()
	brood.ops = nil

	warnings := reconciler.Reconcile(context.Background())
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if len(brood.ops) != 0 {
		t.Fatalf("matching builtin was modified: ops=%v", brood.ops)
	}
}

func TestRBACFederationReconcilerWarnsAndPreservesBuiltinSpecDrift(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	builtin := configureBuiltinDesiredRole(reconciler)
	brood.ensure("dev-cluster")
	actual := builtin.DeepCopy()
	actual.Spec.Permissions = []string{"Job/Read"}
	brood.roles["dev-cluster"][actual.Name] = actual
	desiredBindings := reconciler.desiredBindingsForTest()
	brood.bindings["dev-cluster"][desiredBindings[0].Name] = desiredBindings[0].DeepCopy()
	brood.ops = nil

	for i := 0; i < 2; i++ {
		warnings := reconciler.Reconcile(context.Background())
		if len(warnings) != 1 || !strings.Contains(warnings[0], "builtin spec drift") {
			t.Fatalf("warnings = %v", warnings)
		}
	}
	if len(brood.ops) != 0 || !reflect.DeepEqual(brood.roles["dev-cluster"][actual.Name].Spec, actual.Spec) {
		t.Fatalf("drifted builtin was modified: ops=%v role=%#v", brood.ops, brood.roles["dev-cluster"][actual.Name])
	}
}

func TestRBACFederationReconcilerDeduplicatesCollisionWarnings(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	writer := &bytes.Buffer{}
	reconciler.logger = slog.New(slog.NewTextHandler(writer, nil))
	brood.ensure("dev-cluster")
	desiredBindings := reconciler.desiredBindingsForTest()
	brood.roles["dev-cluster"]["jenkins-admin"] = &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Job/Read"}}}
	brood.bindings["dev-cluster"][desiredBindings[0].Name] = &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: desiredBindings[0].Name}, Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "other"}}

	reconciler.reconcileAndLog(context.Background())
	reconciler.reconcileAndLog(context.Background())
	if got := strings.Count(writer.String(), "rbac federation warning"); got != 2 {
		t.Fatalf("warning count after repeated pass = %d, want 2", got)
	}
	delete(brood.roles["dev-cluster"], "jenkins-admin")
	delete(brood.bindings["dev-cluster"], desiredBindings[0].Name)
	reconciler.reconcileAndLog(context.Background())
	brood.roles["dev-cluster"]["jenkins-admin"] = &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Job/Read"}}}
	brood.bindings["dev-cluster"][desiredBindings[0].Name] = &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: desiredBindings[0].Name}, Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "other"}}
	reconciler.reconcileAndLog(context.Background())
	if got := strings.Count(writer.String(), "rbac federation warning"); got != 4 {
		t.Fatalf("warning count after recurrence = %d, want 4", got)
	}
}

func TestRBACFederationReconcilerWarnsForBuiltinLabelOnNonBuiltinName(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	writer := &bytes.Buffer{}
	reconciler.logger = slog.New(slog.NewTextHandler(writer, nil))
	brood.ensure("dev-cluster")
	brood.roles["dev-cluster"]["jenkins-admin"] = &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin", Labels: map[string]string{v1alpha1.LabelBuiltin: "true"}}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Job/Read"}}}
	reconciler.reconcileAndLog(context.Background())
	reconciler.reconcileAndLog(context.Background())
	if got := strings.Count(writer.String(), "rbac federation warning"); got != 1 || !strings.Contains(writer.String(), "builtin-labeled") {
		t.Fatalf("deduped builtin-label warning output = %q", writer.String())
	}
}

func configureBuiltinDesiredRole(reconciler *RBACFederationReconciler) *v1alpha1.JenkinsRole {
	role := rbac.BuiltinJenkinsRoles()[0]
	reconciler.roles.(*fakeFederationStore).items["team-admin"].(*v1alpha1.VarroaRole).Spec.JenkinsRoleRef = role.Name
	reconciler.coreJenkinsRoles.(*fakeFederationStore).items[role.Name] = role
	return role
}

func TestRBACFederationReconcilerUnreachableHiveDoesNotAbortOthers(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "edge"}, {Name: "dev-cluster"}})
	brood.unreachable["edge"] = true
	warnings := reconciler.Reconcile(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "edge") {
		t.Fatalf("warnings = %v", warnings)
	}
	if brood.roles["dev-cluster"]["jenkins-admin"] == nil {
		t.Fatalf("reachable cluster did not converge")
	}
}

func TestRBACFederationEnqueueIsDebounced(t *testing.T) {
	reconciler, _ := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	reconciler.Enqueue()
	reconciler.Enqueue()
	if got := len(reconciler.signal); got != 1 {
		t.Fatalf("queued signals = %d, want 1", got)
	}
}

func TestRBACFederationTickerTriggersResync(t *testing.T) {
	reconciler, _ := testFederationReconciler(nil)
	reconciler.debounceInterval = time.Millisecond
	reconciler.resyncInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := 0
	reconciler.clusterLister = func() ([]bus.ClusterInfo, error) {
		count++
		if count >= 2 {
			cancel()
		}
		return nil, nil
	}
	reconciler.Run(ctx)
	if count < 2 {
		t.Fatalf("reconcile count = %d, want initial + ticker", count)
	}
}

func testFederationReconciler(clusters []bus.ClusterInfo) (*RBACFederationReconciler, *fakeConfigBrood) {
	roles := &fakeFederationStore{items: map[string]interface{}{
		"team-admin": &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: "team-admin"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "jenkins-admin"}},
	}}
	bindings := &fakeFederationStore{items: map[string]interface{}{
		"bind-admin": &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "bind-admin"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}}},
	}}
	jenkinsRoles := &fakeFederationStore{items: map[string]interface{}{
		"jenkins-admin": &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Overall/Read"}, Description: "core copy"}},
	}}
	brood := newFakeConfigBrood()
	reconciler := NewRBACFederationReconciler(roles, bindings, jenkinsRoles, brood, nil, "core", testLogger())
	reconciler.clusterLister = func() ([]bus.ClusterInfo, error) { return clusters, nil }
	return reconciler, brood
}

func (r *RBACFederationReconciler) desiredBindingsForTest() []*v1alpha1.JenkinsRoleBinding {
	_, bindings, _ := r.desired()
	return bindings
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(testWriter{}, nil)) }

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestRoleDriftedIgnoresResourceVersion(t *testing.T) {
	desired := &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "role", Labels: map[string]string{rbac.LabelFederatedFrom: "vr"}}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Job/Read"}}}
	actual := desired.DeepCopy()
	actual.ResourceVersion = "123"
	if !reflect.DeepEqual(actual.Spec, desired.Spec) || roleDrifted(actual, desired) {
		t.Fatalf("resourceVersion should not cause drift")
	}
}

func (f *fakeConfigBrood) ListComposedBundles(context.Context, string, string) ([]json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) GetComposedBundle(context.Context, string, string, string) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) CreateComposedBundle(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) UpdateComposedBundle(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) DeleteComposedBundle(context.Context, string, string, string) error {
	panic("not implemented")
}
func (f *fakeConfigBrood) PauseComposedBundle(context.Context, string, string, string, bool) error {
	panic("not implemented")
}
func (f *fakeConfigBrood) ComposeBundle(context.Context, string, string, json.RawMessage) (*bus.BundleComposePreview, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) ListCatalogItems(context.Context, string, string, CatalogItemFilter) ([]json.RawMessage, string, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) GetCatalogItem(context.Context, string, string, string) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) ListCatalogSources(context.Context, string, string) ([]json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) GetCatalogSource(context.Context, string, string, string) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) CreateCatalogSource(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) UpdateCatalogSource(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) DeleteCatalogSource(context.Context, string, string, string) error {
	panic("not implemented")
}
func (f *fakeConfigBrood) SyncCatalogSource(context.Context, string, string, string) error {
	panic("not implemented")
}
func (f *fakeConfigBrood) GetProvisioningDefaults(context.Context, string, string) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) UpdateProvisioningDefaults(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) ListVersionProfiles(context.Context, string) ([]json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) GetVersionProfile(context.Context, string, string) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) CreateVersionProfile(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) UpdateVersionProfile(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
	panic("not implemented")
}
func (f *fakeConfigBrood) DeleteVersionProfile(context.Context, string, string) error {
	panic("not implemented")
}
func (f *fakeConfigBrood) ViewVersionProfiles(context.Context, string) ([]bus.VersionProfileView, error) {
	panic("not implemented")
}
