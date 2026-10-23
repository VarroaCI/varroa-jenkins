package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

var federationTestNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// statusFederationReconciler builds the standard test reconciler with a
// crdstore.Fake seeded from the informer store and a fixed clock.
func statusFederationReconciler(t *testing.T, clusters []bus.ClusterInfo) (*RBACFederationReconciler, *fakeConfigBrood, *crdstore.Fake) {
	t.Helper()
	reconciler, brood := testFederationReconciler(clusters)
	store := crdstore.NewFake()
	for _, obj := range reconciler.bindings.(*fakeFederationStore).items {
		crdstore.MustSeed(store, obj.(*v1alpha1.VarroaRoleBinding).DeepCopy())
	}
	for _, obj := range reconciler.roles.(*fakeFederationStore).items {
		crdstore.MustSeed(store, obj.(*v1alpha1.VarroaRole).DeepCopy())
	}
	reconciler.store = store
	reconciler.now = func() time.Time { return federationTestNow }
	return reconciler, brood, store
}

// addSourceBinding adds a binding to the informer store and to the Fake.
func addSourceBinding(t *testing.T, r *RBACFederationReconciler, store *crdstore.Fake, b *v1alpha1.VarroaRoleBinding) {
	t.Helper()
	r.bindings.(*fakeFederationStore).items[b.Name] = b
	crdstore.MustSeed(store, b.DeepCopy())
}

func storedStatus(t *testing.T, store *crdstore.Fake, name string) v1alpha1.VarroaRoleBindingStatus {
	t.Helper()
	got, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), store, name, "")
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return got.Status
}

// syncInformer copies the stored status into the informer copy, as the
// watch does after a status write.
func syncInformer(t *testing.T, r *RBACFederationReconciler, store *crdstore.Fake, name string) {
	t.Helper()
	r.bindings.(*fakeFederationStore).items[name].(*v1alpha1.VarroaRoleBinding).Status = storedStatus(t, store, name)
}

func entryFor(t *testing.T, status v1alpha1.VarroaRoleBindingStatus, cluster string) v1alpha1.BindingFederationStatus {
	t.Helper()
	for _, e := range status.Federation {
		if e.Cluster == cluster {
			return e
		}
	}
	t.Fatalf("no federation entry for %s in %#v", cluster, status.Federation)
	return v1alpha1.BindingFederationStatus{}
}

func TestFederationStatusReportsFederatedPerHive(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "core"}, {Name: "edge"}, {Name: "dev-cluster"}})
	r.bindings.(*fakeFederationStore).items["bind-admin"].(*v1alpha1.VarroaRoleBinding).Generation = 3

	if w := r.Reconcile(context.Background()); len(w) != 0 {
		t.Fatalf("warnings = %v", w)
	}
	status := storedStatus(t, store, "bind-admin")
	if status.ObservedGeneration != 3 {
		t.Fatalf("observedGeneration = %d, want 3", status.ObservedGeneration)
	}
	if len(status.Federation) != 2 || status.Federation[0].Cluster != "dev-cluster" || status.Federation[1].Cluster != "edge" {
		t.Fatalf("federation = %#v, want dev-cluster then edge, no core entry", status.Federation)
	}
	desired := r.desiredBindingsForTest()[0].Name
	for _, e := range status.Federation {
		if e.State != v1alpha1.BindingFederated || e.Reason != "Federated" || !strings.Contains(e.Message, desired) {
			t.Fatalf("entry = %#v, want federated naming %s", e, desired)
		}
		if !e.LastTransitionTime.Time.Equal(federationTestNow) {
			t.Fatalf("lastTransitionTime = %v, want %v", e.LastTransitionTime, federationTestNow)
		}
	}
}

func TestFederationStatusRecordsSkipOnEveryHive(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}, {Name: "edge"}})
	addSourceBinding(t, r, store, &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "tenant-empty"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{}}})
	addSourceBinding(t, r, store, &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}, Spec: v1alpha1.VarroaRoleBindingSpec{
		RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "bob"}},
		Scope: &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
	}})

	r.Reconcile(context.Background())
	for name, reason := range map[string]string{"tenant-empty": rbac.SkipNoSubjects, "tenant": rbac.SkipNoTenantWrite} {
		status := storedStatus(t, store, name)
		if len(status.Federation) != 2 {
			t.Fatalf("%s federation = %#v, want one entry per hive", name, status.Federation)
		}
		for _, e := range status.Federation {
			if e.State != v1alpha1.BindingSkipped || e.Reason != reason || e.Message == "" {
				t.Fatalf("%s entry = %#v, want skipped %s", name, e, reason)
			}
		}
	}
}

func TestFederationStatusReportsHiveFaults(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}, {Name: "edge"}})
	brood.unreachable["edge"] = true
	desired := r.desiredBindingsForTest()[0].Name
	brood.failBindingCreate = map[string]bool{desired: true}

	r.Reconcile(context.Background())
	status := storedStatus(t, store, "bind-admin")
	if e := entryFor(t, status, "edge"); e.State != v1alpha1.BindingFailed || e.Reason != "HiveUnavailable" || !strings.Contains(e.Message, "offline") {
		t.Fatalf("edge entry = %#v, want failed HiveUnavailable", e)
	}
	if e := entryFor(t, status, "dev-cluster"); e.State != v1alpha1.BindingFailed || e.Reason != "JenkinsRoleBindingWriteFailed" || !strings.Contains(e.Message, "subjects") {
		t.Fatalf("dev-cluster entry = %#v, want failed JenkinsRoleBindingWriteFailed", e)
	}
}

func TestFederationStatusReportsCollisions(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	brood.ensure("dev-cluster")
	brood.roles["dev-cluster"]["jenkins-admin"] = &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}}

	r.Reconcile(context.Background())
	if e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster"); e.State != v1alpha1.BindingFailed || e.Reason != "JenkinsRoleCollision" {
		t.Fatalf("entry = %#v, want failed JenkinsRoleCollision", e)
	}

	delete(brood.roles["dev-cluster"], "jenkins-admin")
	desired := r.desiredBindingsForTest()[0].Name
	brood.bindings["dev-cluster"][desired] = &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: desired}, Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "other"}}
	r.Reconcile(context.Background())
	if e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster"); e.State != v1alpha1.BindingFailed || e.Reason != "JenkinsRoleBindingCollision" {
		t.Fatalf("entry = %#v, want failed JenkinsRoleBindingCollision", e)
	}
}

// A matching builtin role on the hive is accepted, so the binding reports
// federated.
func TestFederationStatusAcceptsMatchingBuiltin(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	builtin := configureBuiltinDesiredRole(r)
	brood.ensure("dev-cluster")
	brood.roles["dev-cluster"][builtin.Name] = builtin.DeepCopy()

	r.Reconcile(context.Background())
	if e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster"); e.State != v1alpha1.BindingFederated {
		t.Fatalf("entry = %#v, want federated", e)
	}
}

// A fault clears on the next pass, and the entry moves back to federated
// with a new transition time.
func TestFederationStatusRecoversAfterFaultClears(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	brood.unreachable["dev-cluster"] = true
	r.Reconcile(context.Background())
	if e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster"); e.State != v1alpha1.BindingFailed {
		t.Fatalf("entry = %#v, want failed", e)
	}
	syncInformer(t, r, store, "bind-admin")

	later := federationTestNow.Add(5 * time.Minute)
	r.now = func() time.Time { return later }
	brood.unreachable["dev-cluster"] = false
	r.Reconcile(context.Background())
	e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster")
	if e.State != v1alpha1.BindingFederated || !e.LastTransitionTime.Time.Equal(later) {
		t.Fatalf("entry = %#v, want federated at %v", e, later)
	}
}

// A pass that computes the stored status writes nothing, so the status write
// and the informer update it causes do not loop.
func TestFederationStatusSkipsUnchangedWrite(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	r.Reconcile(context.Background())
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	if got := len(store.StatusPatches(gvr)); got != 1 {
		t.Fatalf("status patches after first pass = %d, want 1", got)
	}
	syncInformer(t, r, store, "bind-admin")

	r.now = func() time.Time { return federationTestNow.Add(time.Hour) }
	r.Reconcile(context.Background())
	if got := len(store.StatusPatches(gvr)); got != 1 {
		t.Fatalf("status patches after unchanged pass = %d, want 1", got)
	}
	if e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster"); !e.LastTransitionTime.Time.Equal(federationTestNow) {
		t.Fatalf("lastTransitionTime moved without a change: %v", e.LastTransitionTime)
	}
}

// A hive that leaves membership loses its entry.
func TestFederationStatusDropsDepartedHive(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}, {Name: "edge"}})
	r.Reconcile(context.Background())
	syncInformer(t, r, store, "bind-admin")

	r.clusterLister = func() ([]bus.ClusterInfo, error) { return []bus.ClusterInfo{{Name: "dev-cluster"}}, nil }
	r.Reconcile(context.Background())
	status := storedStatus(t, store, "bind-admin")
	if len(status.Federation) != 1 || status.Federation[0].Cluster != "dev-cluster" {
		t.Fatalf("federation = %#v, want only dev-cluster", status.Federation)
	}
}

// A failed cluster list gives no hive set, so the pass writes no status.
func TestFederationStatusNotWrittenWhenClusterListFails(t *testing.T) {
	r, _, store := statusFederationReconciler(t, nil)
	r.clusterLister = func() ([]bus.ClusterInfo, error) { return nil, errors.New("kv down") }
	r.Reconcile(context.Background())
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	if got := len(store.StatusPatches(gvr)); got != 0 {
		t.Fatalf("status patches = %d, want 0", got)
	}
}

// A failed status write is a warning, and the pass still writes the other
// bindings.
func TestFederationStatusWriteFailureIsWarning(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	addSourceBinding(t, r, store, &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "bind-other"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{}}})
	store.FailNext("patchstatus", gvr, errors.New("apiserver busy"))
	warnings := r.Reconcile(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "status varroarolebinding") {
		t.Fatalf("warnings = %v, want one status write warning", warnings)
	}
	// The Fake records only a patch that succeeds. One binding failed, so the
	// other one must still hold its status.
	written := 0
	for _, name := range []string{"bind-admin", "bind-other"} {
		if len(storedStatus(t, store, name).Federation) == 1 {
			written++
		}
	}
	if got := len(store.StatusPatches(gvr)); got != 1 || written != 1 {
		t.Fatalf("patches = %d, written = %d; want 1 and 1: the pass continues after a failed write", got, written)
	}
}

// A builtin-labeled role with a different spec is a collision, and the
// message names the builtin mismatch.
func TestFederationStatusReportsBuiltinSpecDrift(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	builtin := configureBuiltinDesiredRole(r)
	brood.ensure("dev-cluster")
	drifted := builtin.DeepCopy()
	drifted.Spec.Permissions = []string{"Job/Read"}
	brood.roles["dev-cluster"][drifted.Name] = drifted

	r.Reconcile(context.Background())
	e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster")
	if e.State != v1alpha1.BindingFailed || e.Reason != "JenkinsRoleCollision" || !strings.Contains(e.Message, "builtin role with a different spec") {
		t.Fatalf("entry = %#v, want failed JenkinsRoleCollision naming the builtin mismatch", e)
	}
}

// The status patch carries only status: the stored spec stays as the user
// wrote it.
func TestFederationStatusLeavesSpecAlone(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	// The informer copy lags a user edit. The status write must not revert it.
	updated, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), store, "bind-admin", "")
	if err != nil {
		t.Fatal(err)
	}
	updated.Spec.Subjects = append(updated.Spec.Subjects, v1alpha1.SubjectRef{Kind: "User", Name: "carol"})
	if err := crdstore.Update[v1alpha1.VarroaRoleBinding](context.Background(), store, updated); err != nil {
		t.Fatal(err)
	}
	r.Reconcile(context.Background())
	got, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), store, "bind-admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Subjects) != 2 || len(got.Status.Federation) != 1 {
		t.Fatalf("stored binding = %#v, want the user edit and one entry", got)
	}
}

// A lagged informer copy does not reset lastTransitionTime or repeat an
// unchanged write: the pass reads the stored status before it writes.
func TestFederationStatusToleratesLaggedInformer(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	r.Reconcile(context.Background())
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()

	r.now = func() time.Time { return federationTestNow.Add(time.Minute) }
	r.Reconcile(context.Background())
	if got := len(store.StatusPatches(gvr)); got != 1 {
		t.Fatalf("status patches = %d, want 1: the lagged copy caused a repeat write", got)
	}
	if e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster"); !e.LastTransitionTime.Time.Equal(federationTestNow) {
		t.Fatalf("lastTransitionTime = %v, want %v", e.LastTransitionTime, federationTestNow)
	}
}

// A failed read of the stored status is a warning, and the pass writes
// nothing for that binding: a write built from the informer copy could reset
// lastTransitionTime.
func TestFederationStatusSkipsWriteWhenReadFails(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	store.FailNext("get", gvr, errors.New("apiserver busy"))
	warnings := r.Reconcile(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "read status varroarolebinding bind-admin") {
		t.Fatalf("warnings = %v, want one read warning", warnings)
	}
	if got := len(store.StatusPatches(gvr)); got != 0 {
		t.Fatalf("status patches = %d, want 0", got)
	}
}

func TestFederationStatusEqualIgnoresEntryOrder(t *testing.T) {
	ts := metav1.NewTime(federationTestNow)
	a := v1alpha1.BindingFederationStatus{Cluster: "a", State: v1alpha1.BindingFederated, LastTransitionTime: ts}
	b := v1alpha1.BindingFederationStatus{Cluster: "b", State: v1alpha1.BindingFailed, Reason: "HiveUnavailable", LastTransitionTime: ts}
	x := []v1alpha1.BindingFederationStatus{a, b}
	y := []v1alpha1.BindingFederationStatus{b, a}
	if !statusEqual(1, x, 1, y) {
		t.Fatalf("statusEqual depends on entry order")
	}
	if statusEqual(1, x, 2, y) {
		t.Fatalf("statusEqual missed a generation change")
	}
	y[0].State = v1alpha1.BindingFederated
	if statusEqual(1, x, 1, y) {
		t.Fatalf("statusEqual missed a state change")
	}
}

// An informer update during a pass does not pair the old outcome with the new
// generation. The pass writes the snapshot it projected, and the next pass
// writes the new spec.
func TestFederationStatusPairsOutcomeWithSnapshotGeneration(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	items := r.bindings.(*fakeFederationStore).items
	items["bind-admin"].(*v1alpha1.VarroaRoleBinding).Generation = 1
	r.clusterLister = func() ([]bus.ClusterInfo, error) {
		updated := items["bind-admin"].(*v1alpha1.VarroaRoleBinding).DeepCopy()
		updated.Generation = 2
		updated.Spec.Scope = &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}}
		items["bind-admin"] = updated
		return []bus.ClusterInfo{{Name: "dev-cluster"}}, nil
	}

	r.Reconcile(context.Background())
	status := storedStatus(t, store, "bind-admin")
	if status.ObservedGeneration != 1 || status.Federation[0].State != v1alpha1.BindingFederated {
		t.Fatalf("status = %#v, want generation 1 federated", status)
	}

	r.Reconcile(context.Background())
	status = storedStatus(t, store, "bind-admin")
	if status.ObservedGeneration != 2 || status.Federation[0].Reason != rbac.SkipNoTenantWrite {
		t.Fatalf("status = %#v, want generation 2 skipped TenantScope", status)
	}
}

func statusPatchCount(t *testing.T, store *crdstore.Fake) int {
	t.Helper()
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	return len(store.StatusPatches(gvr))
}

func setSubjects(r *RBACFederationReconciler, name string, subjects ...string) {
	b := r.bindings.(*fakeFederationStore).items[name].(*v1alpha1.VarroaRoleBinding)
	b.Spec.Subjects = []v1alpha1.SubjectRef{}
	for _, s := range subjects {
		b.Spec.Subjects = append(b.Spec.Subjects, v1alpha1.SubjectRef{Kind: "User", Name: s})
	}
}

// A subject change renames the child. A failed delete of the earlier child
// reports failed until the delete succeeds, and a repeated fault writes
// nothing.
func TestFederationStatusReportsFailedStaleDelete(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	r.Reconcile(context.Background())
	syncInformer(t, r, store, "bind-admin")
	old := r.desiredBindingsForTest()[0].Name

	setSubjects(r, "bind-admin", "bob")
	brood.failBindingDelete = map[string]bool{old: true}
	before := statusPatchCount(t, store)
	warnings := r.Reconcile(context.Background())
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "cluster dev-cluster: delete JenkinsRoleBinding "+old) {
		t.Fatalf("warnings = %v, want one delete warning for %s", warnings, old)
	}
	current := r.desiredBindingsForTest()[0].Name
	if current == old || brood.bindings["dev-cluster"][current] == nil || brood.bindings["dev-cluster"][old] == nil {
		t.Fatalf("hive bindings = %v, want the new copy %s and the earlier copy %s", brood.bindings["dev-cluster"], current, old)
	}
	e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster")
	if e.State != v1alpha1.BindingFailed || e.Reason != "StaleJenkinsRoleBindingDeleteFailed" {
		t.Fatalf("entry = %#v, want failed StaleJenkinsRoleBindingDeleteFailed", e)
	}
	want := "JenkinsRoleBinding " + current + " applied; delete of earlier copy " + old + " failed: etcd timeout"
	if e.Message != want {
		t.Fatalf("message = %q, want %q", e.Message, want)
	}
	if got := statusPatchCount(t, store); got != before+1 {
		t.Fatalf("status patches = %d, want %d", got, before+1)
	}
	syncInformer(t, r, store, "bind-admin")

	r.now = func() time.Time { return federationTestNow.Add(time.Minute) }
	r.Reconcile(context.Background())
	if got := statusPatchCount(t, store); got != before+1 {
		t.Fatalf("status patches after a repeated fault = %d, want %d", got, before+1)
	}

	later := federationTestNow.Add(time.Hour)
	r.now = func() time.Time { return later }
	brood.failBindingDelete = nil
	r.Reconcile(context.Background())
	e = entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster")
	if e.State != v1alpha1.BindingFederated || !e.LastTransitionTime.Time.Equal(later) {
		t.Fatalf("entry = %#v, want federated at %v", e, later)
	}
	if got := statusPatchCount(t, store); got != before+2 {
		t.Fatalf("status patches after the fault clears = %d, want %d", got, before+2)
	}
	if brood.bindings["dev-cluster"][old] != nil {
		t.Fatalf("earlier copy %s stayed on the hive", old)
	}
}

// A child with no owner annotation reports through its role label, and the
// desired child gets the annotation on the same pass.
func TestFederationStatusStaleDeleteFallsBackToRoleLabel(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	brood.ensure("dev-cluster")
	desired := r.desiredBindingsForTest()[0].DeepCopy()
	desired.Annotations = nil
	brood.bindings["dev-cluster"][desired.Name] = desired
	stale := "varroa-fed-team-admin-000000000000"
	brood.bindings["dev-cluster"][stale] = &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: stale, Labels: map[string]string{rbac.LabelFederatedFrom: "team-admin"}},
		Spec:       v1alpha1.JenkinsRoleBindingSpec{RoleRef: "jenkins-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "mallory"}}},
	}
	brood.failBindingDelete = map[string]bool{stale: true}
	addSourceBinding(t, r, store, &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}, Spec: v1alpha1.VarroaRoleBindingSpec{
		RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "bob"}},
		Scope: &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
	}})

	r.Reconcile(context.Background())
	if e := entryFor(t, storedStatus(t, store, "tenant"), "dev-cluster"); e.Reason != rbac.SkipNoTenantWrite {
		t.Fatalf("tenant entry = %#v, want skipped TenantScope: a skipped binding is no fallback owner", e)
	}
	if got := brood.bindings["dev-cluster"][desired.Name].Annotations[rbac.AnnotationFederatedBindings]; got != "bind-admin" {
		t.Fatalf("owner annotation = %q, want bind-admin", got)
	}
	e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster")
	if e.Reason != "StaleJenkinsRoleBindingDeleteFailed" || !strings.Contains(e.Message, stale) {
		t.Fatalf("entry = %#v, want the stale fault on %s", e, stale)
	}
}

// A stale child whose owners are all gone from the core changes no status.
// Its warning carries the orphan prefix.
func TestFederationStatusOrphanStaleDeleteIsWarningOnly(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	r.Reconcile(context.Background())
	syncInformer(t, r, store, "bind-admin")
	before := statusPatchCount(t, store)

	orphan := "varroa-fed-team-admin-111111111111"
	brood.bindings["dev-cluster"][orphan] = &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        orphan,
			Labels:      map[string]string{rbac.LabelFederatedFrom: "team-admin"},
			Annotations: map[string]string{rbac.AnnotationFederatedBindings: "bind-gone"},
		},
		Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "jenkins-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "mallory"}}},
	}
	brood.failBindingDelete = map[string]bool{orphan: true}

	warnings := r.Reconcile(context.Background())
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "cluster dev-cluster: delete orphaned JenkinsRoleBinding "+orphan) {
		t.Fatalf("warnings = %v, want the orphan warning", warnings)
	}
	if got := statusPatchCount(t, store); got != before {
		t.Fatalf("status patches = %d, want %d", got, before)
	}
}

// A stale child reports only on the owners its annotation names, not on
// every binding on the same role.
func TestFederationStatusStaleDeleteReachesListedOwnersOnly(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	for name, subject := range map[string]string{"b-one": "carol", "b-two": "carol", "b-three": "dave"} {
		addSourceBinding(t, r, store, &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: subject}}}})
	}
	r.Reconcile(context.Background())
	var shared string
	for name, b := range brood.bindings["dev-cluster"] {
		if b.Annotations[rbac.AnnotationFederatedBindings] == "b-one,b-two" {
			shared = name
		}
	}
	if shared == "" {
		t.Fatalf("no hive binding names b-one,b-two: %v", brood.bindings["dev-cluster"])
	}
	for _, name := range []string{"bind-admin", "b-one", "b-two", "b-three"} {
		syncInformer(t, r, store, name)
	}

	setSubjects(r, "b-one", "erin")
	setSubjects(r, "b-two", "frank")
	brood.failBindingDelete = map[string]bool{shared: true}
	r.Reconcile(context.Background())
	for name, want := range map[string]string{"b-one": "StaleJenkinsRoleBindingDeleteFailed", "b-two": "StaleJenkinsRoleBindingDeleteFailed", "b-three": "Federated", "bind-admin": "Federated"} {
		if e := entryFor(t, storedStatus(t, store, name), "dev-cluster"); e.Reason != want {
			t.Fatalf("%s entry = %#v, want reason %s", name, e, want)
		}
	}
}

// A binding that loses its last subject skips. On a hive that keeps its
// earlier child, the stale fault replaces the skip.
func TestFederationStatusStaleDeleteReplacesSkip(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}, {Name: "edge"}})
	brood.unreachable["edge"] = true
	r.Reconcile(context.Background())
	syncInformer(t, r, store, "bind-admin")
	old := r.desiredBindingsForTest()[0].Name

	setSubjects(r, "bind-admin")
	brood.failBindingDelete = map[string]bool{old: true}
	r.Reconcile(context.Background())
	status := storedStatus(t, store, "bind-admin")
	e := entryFor(t, status, "dev-cluster")
	if e.State != v1alpha1.BindingFailed || e.Reason != "StaleJenkinsRoleBindingDeleteFailed" || e.Message != "delete of earlier copy "+old+" failed: etcd timeout" {
		t.Fatalf("dev-cluster entry = %#v, want the stale fault on %s", e, old)
	}
	if e := entryFor(t, status, "edge"); e.State != v1alpha1.BindingSkipped || e.Reason != rbac.SkipNoSubjects {
		t.Fatalf("edge entry = %#v, want skipped NoSubjects", e)
	}
}

func TestFederationStatusTenantBindingFederated(t *testing.T) {
	r, _, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	addPipelinesWriteRole(r)
	addSourceBinding(t, r, store, tenantWriteBindingFixture([]string{"a/*"}))
	r.Reconcile(context.Background())

	status := storedStatus(t, store, "tenant-write")
	e := entryFor(t, status, "dev-cluster")
	if e.State != v1alpha1.BindingFederated || !strings.Contains(e.Message, "varroa-fed-tenant-") {
		t.Fatalf("entry = %#v, want federated with a tenant copy name", e)
	}
}

func TestFederationStatusTenantChildFaultReported(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	addPipelinesWriteRole(r)
	addSourceBinding(t, r, store, tenantWriteBindingFixture([]string{"a/*"}))
	for _, b := range r.desiredBindingsForTest() {
		if b.Labels[rbac.LabelFederatedTenantMember] == "true" {
			brood.failBindingCreate = map[string]bool{b.Name: true}
		}
	}

	r.Reconcile(context.Background())
	e := entryFor(t, storedStatus(t, store, "tenant-write"), "dev-cluster")
	if e.State != v1alpha1.BindingFailed || e.Reason != "JenkinsRoleBindingWriteFailed" {
		t.Fatalf("entry = %#v, want failed JenkinsRoleBindingWriteFailed", e)
	}
}

// A cluster-wide write binding whose role has a Global projection reports
// both children on the hive. A write fault on the all-tenants copy is
// reported as a fault of the binding.
func TestFederationStatusClusterWideWriteReportsBothChildren(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	legacy := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "writer-legacy"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules:           []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"write"}}},
			JenkinsPermissions: []string{"Overall/Read"},
		},
	}
	r.roles.(*fakeFederationStore).items[legacy.Name] = legacy
	crdstore.MustSeed(store, legacy.DeepCopy())
	addSourceBinding(t, r, store, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "bind-writer"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "writer-legacy", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}},
	})

	r.Reconcile(context.Background())
	e := entryFor(t, storedStatus(t, store, "bind-writer"), "dev-cluster")
	if e.State != v1alpha1.BindingFederated || !strings.Contains(e.Message, "varroa-fed-tenant-all-") {
		t.Fatalf("entry = %#v, want federated naming the all-tenants copy", e)
	}
	if !strings.Contains(e.Message, "varroa-fed-writer-legacy-") {
		t.Fatalf("entry = %#v, want federated naming the Global child", e)
	}

	copyName := allTenantsCopyName(t, r)
	brood.failBindingCreate = map[string]bool{copyName: true}
	delete(brood.bindings["dev-cluster"], copyName)
	syncInformer(t, r, store, "bind-writer")
	r.Reconcile(context.Background())
	if e := entryFor(t, storedStatus(t, store, "bind-writer"), "dev-cluster"); e.State != v1alpha1.BindingFailed || e.Reason != "JenkinsRoleBindingWriteFailed" {
		t.Fatalf("entry = %#v, want failed JenkinsRoleBindingWriteFailed", e)
	}
}
