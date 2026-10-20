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
	// failBindingCreate makes CreateJenkinsRoleBinding answer an invalid
	// error for the named binding, as the API server does for a spec that
	// fails CRD validation.
	failBindingCreate map[string]bool
	// failBindingDelete makes DeleteJenkinsRoleBinding answer an internal
	// error for the named binding and keep the object.
	failBindingDelete map[string]bool
	// failRoleDelete makes DeleteJenkinsRole answer an internal error for
	// the named role and keep the object.
	failRoleDelete map[string]bool
	// goneRoleDelete makes DeleteJenkinsRole answer not found for the named
	// role, as the API server does after another writer deleted it.
	goneRoleDelete map[string]bool
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
	if f.failRoleDelete[name] {
		return &BroodError{Code: bus.CodeInternal, Msg: "etcd timeout"}
	}
	if f.goneRoleDelete[name] {
		delete(f.roles[cluster], name)
		return &BroodError{Code: bus.CodeNotFound, Msg: "not found"}
	}
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
	if f.failBindingCreate[name] {
		return nil, &BroodError{Code: bus.CodeInvalid, Msg: "spec.subjects: Required value"}
	}
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
	if f.failBindingDelete[name] {
		return &BroodError{Code: bus.CodeInternal, Msg: "etcd timeout"}
	}
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

// A source binding that loses its last subject stops federating. The child
// that an earlier pass created on the hive is garbage-collected, and so is a
// role that only that binding referenced.
func TestRBACFederationReconcilerDeletesChildWhenSubjectsEmpty(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	if warnings := reconciler.Reconcile(context.Background()); len(warnings) != 0 {
		t.Fatalf("first pass warnings = %v", warnings)
	}
	federated := reconciler.desiredBindingsForTest()
	if len(federated) != 1 || brood.bindings["dev-cluster"][federated[0].Name] == nil {
		t.Fatalf("first pass did not federate the binding")
	}

	source := reconciler.bindings.(*fakeFederationStore).items["bind-admin"].(*v1alpha1.VarroaRoleBinding)
	source.Spec.Subjects = []v1alpha1.SubjectRef{}
	if warnings := reconciler.Reconcile(context.Background()); len(warnings) != 0 {
		t.Fatalf("second pass warnings = %v", warnings)
	}
	if len(brood.bindings["dev-cluster"]) != 0 {
		t.Fatalf("hive bindings = %v, want none", brood.bindings["dev-cluster"])
	}
	if brood.roles["dev-cluster"]["jenkins-admin"] != nil {
		t.Fatalf("role that only the empty binding referenced stayed on the hive")
	}
}

// A create that the hive rejects on one binding is a warning. The pass
// continues: a later binding is created and the garbage-collect loop runs.
func TestRBACFederationReconcilerCreateFailureDoesNotAbortCluster(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	store := reconciler.bindings.(*fakeFederationStore)
	store.items["bind-zeta"] = &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "bind-zeta"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "zed"}}}}
	desired := reconciler.desiredBindingsForTest()
	if len(desired) != 2 {
		t.Fatalf("desired bindings = %d, want 2", len(desired))
	}
	brood.failBindingCreate = map[string]bool{desired[0].Name: true}
	brood.ensure("dev-cluster")
	brood.bindings["dev-cluster"]["varroa-fed-team-admin-stale"] = &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "varroa-fed-team-admin-stale", Labels: map[string]string{rbac.LabelFederatedFrom: "team-admin"}}}

	warnings := reconciler.Reconcile(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], desired[0].Name) || !strings.Contains(warnings[0], "dev-cluster") {
		t.Fatalf("warnings = %v, want one warning naming %s", warnings, desired[0].Name)
	}
	if brood.bindings["dev-cluster"][desired[1].Name] == nil {
		t.Fatalf("binding after the failed one was not created")
	}
	if brood.bindings["dev-cluster"]["varroa-fed-team-admin-stale"] != nil {
		t.Fatalf("garbage-collect loop did not run after a create failure")
	}
}

// A create failure that repeats on every pass logs one warning line.
func TestRBACFederationReconcilerRepeatedCreateFailureLogsOnce(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	writer := &bytes.Buffer{}
	reconciler.logger = slog.New(slog.NewTextHandler(writer, nil))
	desired := reconciler.desiredBindingsForTest()
	brood.failBindingCreate = map[string]bool{desired[0].Name: true}

	reconciler.reconcileAndLog(context.Background())
	reconciler.reconcileAndLog(context.Background())
	if got := strings.Count(writer.String(), "rbac federation warning"); got != 1 {
		t.Fatalf("warning count after two passes = %d, want 1; log = %s", got, writer.String())
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
	reconciler := NewRBACFederationReconciler(roles, bindings, jenkinsRoles, brood, nil, nil, "core", testLogger())
	reconciler.clusterLister = func() ([]bus.ClusterInfo, error) { return clusters, nil }
	return reconciler, brood
}

func (r *RBACFederationReconciler) desiredBindingsForTest() []*v1alpha1.JenkinsRoleBinding {
	_, bindings, _, _ := r.desired(r.sourceBindings(), r.sourceRoles())
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

// roleDrifted sees a change of the source annotation and ignores an
// annotation that another writer adds.
func TestRoleDriftedComparesSourceAnnotation(t *testing.T) {
	desired := &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{
		Name:        "role",
		Labels:      map[string]string{rbac.LabelFederatedFrom: "vr"},
		Annotations: map[string]string{rbac.AnnotationFederatedRoles: "vr,vr-two"},
	}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}}
	actual := desired.DeepCopy()
	actual.Annotations["example.com/other"] = "x"
	if roleDrifted(actual, desired) {
		t.Fatalf("an unrelated annotation caused drift")
	}
	actual.Annotations[rbac.AnnotationFederatedRoles] = "vr"
	if !roleDrifted(actual, desired) {
		t.Fatalf("a changed source annotation caused no drift")
	}
}

// An existing role copy with no source annotation gets it on one update, and
// the next pass writes nothing.
func TestReconcileAnnotatesExistingRoleOnce(t *testing.T) {
	r, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	r.Reconcile(context.Background())
	delete(brood.roles["dev-cluster"]["jenkins-admin"].Annotations, rbac.AnnotationFederatedRoles)
	brood.ops = nil

	r.Reconcile(context.Background())
	if len(brood.ops) != 1 || brood.ops[0] != "update-role:dev-cluster:jenkins-admin" {
		t.Fatalf("ops = %v, want one role update", brood.ops)
	}
	if got := brood.roles["dev-cluster"]["jenkins-admin"].Annotations[rbac.AnnotationFederatedRoles]; got != "team-admin" {
		t.Fatalf("role annotation = %q, want team-admin", got)
	}
	brood.ops = nil
	r.Reconcile(context.Background())
	if len(brood.ops) != 0 {
		t.Fatalf("ops = %v, want none after the annotation settles", brood.ops)
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
func (f *fakeConfigBrood) ListCatalogItems(context.Context, string, string, CatalogItemFilter) ([]json.RawMessage, error) {
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

// addPipelinesWriteRole adds a VarroaRole that grants pipelines write to the
// reconciler's role snapshot.
func addPipelinesWriteRole(r *RBACFederationReconciler) {
	r.roles.(*fakeFederationStore).items["pipelines-writer"] = &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "pipelines-writer"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"pipelines"}, Verbs: []string{"write"}},
		}},
	}
}

// tenantWriteBindingFixture returns a tenant-scoped binding with pipelines write.
func tenantWriteBindingFixture(jobs []string) *v1alpha1.VarroaRoleBinding {
	return &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-write"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef:  "pipelines-writer",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "team-a", Jobs: jobs}},
			},
		},
	}
}

// addTenantWriteBinding adds the tenant-scoped binding to the reconciler's
// binding snapshot only.
func addTenantWriteBinding(r *RBACFederationReconciler, jobs []string) {
	r.bindings.(*fakeFederationStore).items["tenant-write"] = tenantWriteBindingFixture(jobs)
}

func TestRBACFederationReconcilerWritesAndDeletesTenantChildren(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	addPipelinesWriteRole(reconciler)
	addTenantWriteBinding(reconciler, []string{"a/*"})
	if warnings := reconciler.Reconcile(context.Background()); len(warnings) != 0 {
		t.Fatalf("first pass warnings = %v", warnings)
	}
	first := reconciler.desiredBindingsForTest()
	var firstTenant []string
	for _, b := range first {
		if b.Labels[rbac.LabelFederatedTenantMember] == "true" {
			firstTenant = append(firstTenant, b.Name)
		}
	}
	if len(firstTenant) != 1 || brood.bindings["dev-cluster"][firstTenant[0]] == nil {
		t.Fatalf("first pass did not write one tenant copy: %v", brood.bindings["dev-cluster"])
	}

	// A change to the job globs gives the copy a new name. The pass writes
	// the new copy and deletes the old one.
	source := reconciler.bindings.(*fakeFederationStore).items["tenant-write"].(*v1alpha1.VarroaRoleBinding)
	source.Spec.Scope.Tenants[0].Jobs = []string{"a/*", "b/*"}
	if warnings := reconciler.Reconcile(context.Background()); len(warnings) != 0 {
		t.Fatalf("second pass warnings = %v", warnings)
	}
	if len(brood.bindings["dev-cluster"]) != 2 {
		t.Fatalf("hive bindings = %v, want the team-admin copy and one tenant copy", brood.bindings["dev-cluster"])
	}
	if brood.bindings["dev-cluster"][firstTenant[0]] != nil {
		t.Fatalf("the earlier tenant copy %s was not deleted", firstTenant[0])
	}

	// Removing the grant deletes the copy.
	delete(reconciler.bindings.(*fakeFederationStore).items, "tenant-write")
	if warnings := reconciler.Reconcile(context.Background()); len(warnings) != 0 {
		t.Fatalf("third pass warnings = %v", warnings)
	}
	if len(brood.bindings["dev-cluster"]) != 1 {
		t.Fatalf("hive bindings = %v, want only the team-admin copy", brood.bindings["dev-cluster"])
	}
}

func TestBindingDriftedComparesTenantAnnotations(t *testing.T) {
	base := func() *v1alpha1.JenkinsRoleBinding {
		return &v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "varroa-fed-tenant-x",
				Labels:      map[string]string{rbac.LabelFederatedFrom: "pipelines-writer", rbac.LabelFederatedTenantMember: "true"},
				Annotations: map[string]string{rbac.AnnotationFederatedBindings: "tw", rbac.AnnotationFederatedTenant: "team-a"},
			},
			Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "varroa:tenant-member:team-a", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}},
		}
	}
	desired := base()
	if bindingDrifted(base(), desired) {
		t.Fatalf("identical copies drift")
	}
	moved := base()
	moved.Annotations[rbac.AnnotationFederatedTenant] = "team-b"
	if !bindingDrifted(moved, desired) {
		t.Fatalf("a changed tenant annotation does not drift")
	}
	narrowed := base()
	narrowed.Annotations[rbac.AnnotationFederatedTenantJobs] = `["a/*"]`
	if !bindingDrifted(narrowed, desired) {
		t.Fatalf("a changed jobs annotation does not drift")
	}
	empty := base()
	empty.Annotations[rbac.AnnotationFederatedTenantJobs] = ""
	if !bindingDrifted(empty, desired) {
		t.Fatalf("an empty jobs annotation matches an absent one (absent is the whole folder)")
	}
}

// addClusterWideWriteBinding adds a binding with no scope and the
// pipelines-writer role to the reconciler's binding snapshot.
func addClusterWideWriteBinding(r *RBACFederationReconciler) {
	r.bindings.(*fakeFederationStore).items["bind-writer"] = &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "bind-writer"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef:  "pipelines-writer",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
		},
	}
}

// allTenantsCopyName returns the name of the desired all-tenants copy.
func allTenantsCopyName(t *testing.T, r *RBACFederationReconciler) string {
	t.Helper()
	for _, b := range r.desiredBindingsForTest() {
		if b.Labels[rbac.LabelFederatedTenantMember] == "true" {
			return b.Name
		}
	}
	t.Fatalf("no all-tenants copy in the desired state")
	return ""
}

func TestRBACFederationReconcilerWritesAndDeletesAllTenantsCopy(t *testing.T) {
	reconciler, brood := testFederationReconciler([]bus.ClusterInfo{{Name: "dev-cluster"}})
	addPipelinesWriteRole(reconciler)
	addClusterWideWriteBinding(reconciler)
	if warnings := reconciler.Reconcile(context.Background()); len(warnings) != 0 {
		t.Fatalf("first pass warnings = %v", warnings)
	}
	copyName := allTenantsCopyName(t, reconciler)
	written := brood.bindings["dev-cluster"][copyName]
	if written == nil || written.Annotations[rbac.AnnotationFederatedTenant] != rbac.FederatedAllTenants {
		t.Fatalf("first pass did not write the all-tenants copy: %v", brood.bindings["dev-cluster"])
	}

	// A pass with no change writes nothing.
	opsBefore := len(brood.ops)
	if warnings := reconciler.Reconcile(context.Background()); len(warnings) != 0 {
		t.Fatalf("second pass warnings = %v", warnings)
	}
	if len(brood.ops) != opsBefore {
		t.Fatalf("second pass wrote %v", brood.ops[opsBefore:])
	}

	// Removing the source binding deletes the copy. The bind-admin child of
	// the default fixture stays.
	delete(reconciler.bindings.(*fakeFederationStore).items, "bind-writer")
	if warnings := reconciler.Reconcile(context.Background()); len(warnings) != 0 {
		t.Fatalf("third pass warnings = %v", warnings)
	}
	if brood.bindings["dev-cluster"][copyName] != nil {
		t.Fatalf("the all-tenants copy %s was not deleted", copyName)
	}
	if len(brood.bindings["dev-cluster"]) != 1 {
		t.Fatalf("hive bindings = %v, want only the bind-admin child", brood.bindings["dev-cluster"])
	}
}
