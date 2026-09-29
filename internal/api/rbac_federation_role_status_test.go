package api

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

func storedRoleStatus(t *testing.T, store *crdstore.Fake, name string) v1alpha1.VarroaRoleStatus {
	t.Helper()
	got, err := crdstore.Get[v1alpha1.VarroaRole](context.Background(), store, name, "")
	if err != nil {
		t.Fatalf("get varroarole %s: %v", name, err)
	}
	return got.Status
}

// syncRoleInformer copies the stored status of team-admin into the informer
// copy, as the watch does after a status write.
func syncRoleInformer(t *testing.T, r *RBACFederationReconciler, store *crdstore.Fake) {
	t.Helper()
	r.roles.(*fakeFederationStore).items["team-admin"].(*v1alpha1.VarroaRole).Status = storedRoleStatus(t, store, "team-admin")
}

func rolePatches(t *testing.T, store *crdstore.Fake) []crdstore.StatusPatch {
	t.Helper()
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRole]()
	return store.StatusPatches(gvr)
}

func addSourceRole(t *testing.T, r *RBACFederationReconciler, store *crdstore.Fake, role *v1alpha1.VarroaRole) {
	t.Helper()
	r.roles.(*fakeFederationStore).items[role.Name] = role
	crdstore.MustSeed(store, role.DeepCopy())
}

// addJenkinsOps adds the core JenkinsRole jenkins-ops.
func addJenkinsOps(r *RBACFederationReconciler) {
	r.coreJenkinsRoles.(*fakeFederationStore).items["jenkins-ops"] = &v1alpha1.JenkinsRole{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-ops"},
		Spec:       v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Overall/Read"}},
	}
}

// repointToJenkinsOps sets the jenkinsRoleRef of role to jenkins-ops.
func repointToJenkinsOps(r *RBACFederationReconciler, role string) {
	r.roles.(*fakeFederationStore).items[role].(*v1alpha1.VarroaRole).Spec.JenkinsRoleRef = "jenkins-ops"
}

// onlyRoleEntry returns the one entry of status, which must be for dev-cluster.
func onlyRoleEntry(t *testing.T, status v1alpha1.VarroaRoleStatus) v1alpha1.BindingFederationStatus {
	t.Helper()
	if len(status.Federation) != 1 || status.Federation[0].Cluster != "dev-cluster" {
		t.Fatalf("role federation = %#v, want one entry for dev-cluster", status.Federation)
	}
	return status.Federation[0]
}

// A jenkinsRoleRef change makes the earlier role copy stale. A failed delete
// reports on the VarroaRole, and a repeated fault writes nothing.
func TestRoleStatusReportsFailedStaleDelete(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "core"}, {Name: "dev-cluster"}})
	addJenkinsOps(r)
	r.roles.(*fakeFederationStore).items["team-admin"].(*v1alpha1.VarroaRole).Generation = 4
	r.Reconcile(context.Background())
	if len(rolePatches(t, store)) != 0 {
		t.Fatalf("role patches = %v, want none on a healthy pass", rolePatches(t, store))
	}

	repointToJenkinsOps(r, "team-admin")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	warnings := r.Reconcile(context.Background())
	if len(warnings) != 1 || warnings[0] != "cluster dev-cluster: delete JenkinsRole jenkins-admin: etcd timeout" {
		t.Fatalf("warnings = %v, want one delete warning", warnings)
	}
	status := storedRoleStatus(t, store, "team-admin")
	if status.ObservedGeneration != 4 {
		t.Fatalf("observedGeneration = %d, want 4", status.ObservedGeneration)
	}
	e := onlyRoleEntry(t, status)
	if e.State != v1alpha1.BindingFailed || e.Reason != "StaleJenkinsRoleDeleteFailed" || e.Message != "delete of earlier copy JenkinsRole jenkins-admin failed: etcd timeout" {
		t.Fatalf("entry = %#v, want failed StaleJenkinsRoleDeleteFailed", e)
	}
	if !e.LastTransitionTime.Time.Equal(federationTestNow) {
		t.Fatalf("lastTransitionTime = %v, want %v", e.LastTransitionTime, federationTestNow)
	}
	if got := len(rolePatches(t, store)); got != 1 {
		t.Fatalf("role patches = %d, want 1", got)
	}
}

// A role copy outlives its last binding. The delete fails, and the owner
// annotation still names the VarroaRole.
func TestRoleStatusReportsStaleDeleteWithNoBindings(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	r.Reconcile(context.Background())

	delete(r.bindings.(*fakeFederationStore).items, "bind-admin")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	r.Reconcile(context.Background())
	e := onlyRoleEntry(t, storedRoleStatus(t, store, "team-admin"))
	if e.Reason != "StaleJenkinsRoleDeleteFailed" || !strings.Contains(e.Message, "jenkins-admin") {
		t.Fatalf("entry = %#v, want the stale fault on jenkins-admin", e)
	}
}

// Two VarroaRoles share one jenkinsRoleRef. Both are named in the annotation,
// so both report a failed delete of the shared copy.
func TestRoleStatusStaleDeleteReachesAnnotatedOwners(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	addSourceRole(t, r, store, &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: "team-ops"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "jenkins-admin"}})
	addSourceBinding(t, r, store, &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "bind-ops"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-ops", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "bob"}}}})
	r.Reconcile(context.Background())
	if got := brood.roles["dev-cluster"]["jenkins-admin"].Annotations[rbac.AnnotationFederatedRoles]; got != "team-admin,team-ops" {
		t.Fatalf("role annotation = %q, want team-admin,team-ops", got)
	}

	addJenkinsOps(r)
	repointToJenkinsOps(r, "team-admin")
	repointToJenkinsOps(r, "team-ops")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	r.Reconcile(context.Background())
	for _, name := range []string{"team-admin", "team-ops"} {
		if e := onlyRoleEntry(t, storedRoleStatus(t, store, name)); e.Reason != "StaleJenkinsRoleDeleteFailed" {
			t.Fatalf("%s entry = %#v, want the stale fault", name, e)
		}
	}
}

// A role copy with no annotation reports through its label, and the desired
// copy gets the annotation on the same pass.
func TestRoleStatusStaleDeleteFallsBackToLabel(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	brood.ensure("dev-cluster")
	brood.roles["dev-cluster"]["jenkins-old"] = &v1alpha1.JenkinsRole{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-old", Labels: map[string]string{rbac.LabelFederatedFrom: "team-admin"}},
		Spec:       v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Overall/Administer"}},
	}
	brood.failRoleDelete = map[string]bool{"jenkins-old": true}

	r.Reconcile(context.Background())
	e := onlyRoleEntry(t, storedRoleStatus(t, store, "team-admin"))
	if e.Message != "delete of earlier copy JenkinsRole jenkins-old failed: etcd timeout" {
		t.Fatalf("entry = %#v, want the stale fault on jenkins-old", e)
	}
	if got := brood.roles["dev-cluster"]["jenkins-admin"].Annotations[rbac.AnnotationFederatedRoles]; got != "team-admin" {
		t.Fatalf("role annotation = %q, want team-admin", got)
	}
}

// A role copy whose VarroaRole is gone from the core changes no status. Its
// warning carries the orphan prefix.
func TestRoleStatusOrphanStaleDeleteIsWarningOnly(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	brood.ensure("dev-cluster")
	brood.roles["dev-cluster"]["jenkins-gone"] = &v1alpha1.JenkinsRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "jenkins-gone",
			Labels:      map[string]string{rbac.LabelFederatedFrom: "team-admin"},
			Annotations: map[string]string{rbac.AnnotationFederatedRoles: "role-gone"},
		},
		Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"},
	}
	brood.roles["dev-cluster"]["jenkins-unlabeled-owner"] = &v1alpha1.JenkinsRole{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-unlabeled-owner", Labels: map[string]string{rbac.LabelFederatedFrom: "role-gone"}},
		Spec:       v1alpha1.JenkinsRoleSpec{RoleType: "Global"},
	}
	brood.failRoleDelete = map[string]bool{"jenkins-gone": true, "jenkins-unlabeled-owner": true}

	warnings := r.Reconcile(context.Background())
	want := []string{
		"cluster dev-cluster: delete orphaned JenkinsRole jenkins-gone: etcd timeout",
		"cluster dev-cluster: delete orphaned JenkinsRole jenkins-unlabeled-owner: etcd timeout",
	}
	if len(warnings) != 2 || warnings[0] != want[0] || warnings[1] != want[1] {
		t.Fatalf("warnings = %v, want %v", warnings, want)
	}
	if got := len(rolePatches(t, store)); got != 0 {
		t.Fatalf("role patches = %d, want 0", got)
	}
}

// A delete that succeeds clears the entry. The patch sends an empty
// federation list, which the registry clear rule turns into a null.
func TestRoleStatusClearsAfterDeleteSucceeds(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	addJenkinsOps(r)
	r.Reconcile(context.Background())
	repointToJenkinsOps(r, "team-admin")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	r.Reconcile(context.Background())
	syncRoleInformer(t, r, store)

	brood.failRoleDelete = nil
	r.Reconcile(context.Background())
	patches := rolePatches(t, store)
	if len(patches) != 2 {
		t.Fatalf("role patches = %d, want 2", len(patches))
	}
	last, ok := patches[1].Status.(v1alpha1.VarroaRoleStatus)
	if !ok || len(last.Federation) != 0 {
		t.Fatalf("last patch = %#v, want a status with no federation entries", patches[1].Status)
	}
	if brood.roles["dev-cluster"]["jenkins-admin"] != nil {
		t.Fatalf("earlier copy jenkins-admin stayed on the hive")
	}
}

// A repeated fault keeps lastTransitionTime and writes nothing. A changed
// message moves lastTransitionTime.
func TestRoleStatusKeepsTransitionTime(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	addJenkinsOps(r)
	r.Reconcile(context.Background())
	repointToJenkinsOps(r, "team-admin")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	r.Reconcile(context.Background())
	syncRoleInformer(t, r, store)

	r.now = func() time.Time { return federationTestNow.Add(time.Hour) }
	r.Reconcile(context.Background())
	e := onlyRoleEntry(t, storedRoleStatus(t, store, "team-admin"))
	if !e.LastTransitionTime.Time.Equal(federationTestNow) {
		t.Fatalf("lastTransitionTime = %v, want %v", e.LastTransitionTime, federationTestNow)
	}
	if got := len(rolePatches(t, store)); got != 1 {
		t.Fatalf("role patches = %d, want 1", got)
	}
}

// A lagged informer copy that already matches the stored status on the
// next read writes nothing.
func TestRoleStatusSkipsUnchangedWrite(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	addJenkinsOps(r)
	r.Reconcile(context.Background())
	repointToJenkinsOps(r, "team-admin")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	r.Reconcile(context.Background())

	r.Reconcile(context.Background())
	if got := len(rolePatches(t, store)); got != 1 {
		t.Fatalf("role patches = %d, want 1: the stored status already matches", got)
	}
}

// A hive whose list fails keeps its stored entry: the pass did not see its
// stale copies.
func TestRoleStatusKeepsEntryOnHiveListFailure(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}, {Name: "edge"}})
	addJenkinsOps(r)
	r.Reconcile(context.Background())
	repointToJenkinsOps(r, "team-admin")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	r.Reconcile(context.Background())
	syncRoleInformer(t, r, store)
	status := storedRoleStatus(t, store, "team-admin")
	if len(status.Federation) != 2 {
		t.Fatalf("role federation = %#v, want one entry per hive", status.Federation)
	}

	brood.unreachable["edge"] = true
	brood.failRoleDelete = nil
	r.Reconcile(context.Background())
	patches := rolePatches(t, store)
	last := patches[len(patches)-1].Status.(v1alpha1.VarroaRoleStatus)
	if len(last.Federation) != 1 || last.Federation[0].Cluster != "edge" || last.Federation[0].Reason != "StaleJenkinsRoleDeleteFailed" {
		t.Fatalf("last patch federation = %#v, want only the kept edge entry", last.Federation)
	}
}

// A hive that leaves the fleet loses its entry.
func TestRoleStatusDropsDepartedHive(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}, {Name: "edge"}})
	addJenkinsOps(r)
	r.Reconcile(context.Background())
	repointToJenkinsOps(r, "team-admin")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	r.Reconcile(context.Background())
	syncRoleInformer(t, r, store)

	r.clusterLister = func() ([]bus.ClusterInfo, error) { return []bus.ClusterInfo{{Name: "dev-cluster"}}, nil }
	r.Reconcile(context.Background())
	patches := rolePatches(t, store)
	last := patches[len(patches)-1].Status.(v1alpha1.VarroaRoleStatus)
	if len(last.Federation) != 1 || last.Federation[0].Cluster != "dev-cluster" {
		t.Fatalf("last patch federation = %#v, want only dev-cluster", last.Federation)
	}
}

// A delete that answers not found is a success: the copy is gone.
func TestRoleStatusNotFoundDeleteIsClean(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	addJenkinsOps(r)
	r.Reconcile(context.Background())
	repointToJenkinsOps(r, "team-admin")
	brood.goneRoleDelete = map[string]bool{"jenkins-admin": true}
	if w := r.Reconcile(context.Background()); len(w) != 0 {
		t.Fatalf("warnings = %v, want none", w)
	}
	if got := len(rolePatches(t, store)); got != 0 {
		t.Fatalf("role patches = %d, want 0", got)
	}
}

// A stale role fault changes no binding status: the binding still reports
// its own outcome.
func TestRoleStatusLeavesBindingStatusAlone(t *testing.T) {
	r, brood, store := statusFederationReconciler(t, []bus.ClusterInfo{{Name: "dev-cluster"}})
	addJenkinsOps(r)
	r.Reconcile(context.Background())
	repointToJenkinsOps(r, "team-admin")
	brood.failRoleDelete = map[string]bool{"jenkins-admin": true}
	r.Reconcile(context.Background())
	if e := entryFor(t, storedStatus(t, store, "bind-admin"), "dev-cluster"); e.State != v1alpha1.BindingFederated {
		t.Fatalf("binding entry = %#v, want federated", e)
	}
}
