package api

import (
	"context"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestSearchRecords_RunUnreadableTenantAbsent proves a "run" search row is
// absent when the caller holds no grant on the row's tenant.
func TestSearchRecords_RunUnreadableTenantAbsent(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "team-a", JobPath: "svc/build"},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "other-tenant"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-b"}}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	server := NewServer(&Dependencies{Logger: slog.Default(), Authorizer: a, RecordStore: reader})
	claims := &auth.Claims{Subject: "u"}

	got := server.searchRecords(context.Background(), claims, "query", "")
	for _, r := range got {
		if r.Type == "run" {
			t.Fatalf("got a run result for a tenant the caller cannot read: %+v", r)
		}
	}
}

// TestSearchRecords_RunColocatedTenantAbsent proves a "run" search row from
// a selector-scoped ("colocated") binding is absent, even when the same
// binding grants a controllers verb for the live, label-matched controller.
func TestSearchRecords_RunColocatedTenantAbsent(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "ctrl-a", Tenant: "ns-a/ctrl-a", JobPath: "svc/build"},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "selector-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "team-a"}},
			},
		}},
	}
	ctrl := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a", Labels: map[string]string{"team": "team-a"}},
	}
	a := NewAuthorizer(testResolverWithController(pipelinesRoles(), bindings, ctrl), false)
	server := NewServer(&Dependencies{Logger: slog.Default(), Authorizer: a, RecordStore: reader})
	claims := &auth.Claims{Subject: "u"}

	got := server.searchRecords(context.Background(), claims, "query", "")
	for _, r := range got {
		if r.Type == "run" {
			t.Fatalf("got a run result from a selector-scoped (colocated) binding, want it absent: %+v", r)
		}
	}
	// The same binding still grants a controllers verb for the co-located
	// controller. This proves the refusal is specific to the pipelines
	// gate, not a general binding failure.
	if !a.CanReadController(claims, "ns-a", "ctrl-a") {
		t.Fatal("want the selector-scoped binding to still satisfy a controllers gate")
	}
}

// TestSearchRecords_RunDeletedCellPresentForPermittedCaller proves a "run"
// search row stays present for a permitted caller even when the row's cell
// no longer exists as a live Controller. CanReadRun reads no controller or
// placement, only the row's tenant.
func TestSearchRecords_RunDeletedCellPresentForPermittedCaller(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "ctrl-gone", Tenant: "ns-a/ctrl-gone", JobPath: "svc/build"},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-a-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	// No Controller is seeded: "ctrl-gone" names a cell that no longer
	// exists.
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	server := NewServer(&Dependencies{Logger: slog.Default(), Authorizer: a, RecordStore: reader})
	claims := &auth.Claims{Subject: "u"}

	got := server.searchRecords(context.Background(), claims, "query", "")
	for _, r := range got {
		if r.Type == "run" {
			return
		}
	}
	t.Fatal("want the run present for a permitted caller even though its cell no longer exists")
}

// TestSearchRecords_JobOutsideGlobsAbsent proves a "job" search row is
// absent when the row's job path lies outside the caller's tenant-scoped
// job globs.
func TestSearchRecords_JobOutsideGlobsAbsent(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Jobs: []recordstore.JobSearchResult{
			{Cluster: "core", Namespace: "ns-a", Cell: "ctrl-a", Tenant: "team-a", JobPath: "svc-b/build"},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "narrowed"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a", Jobs: []string{"svc-a/*"}}}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	server := NewServer(&Dependencies{Logger: slog.Default(), Authorizer: a, RecordStore: reader})
	claims := &auth.Claims{Subject: "u"}

	got := server.searchRecords(context.Background(), claims, "query", "")
	for _, r := range got {
		if r.Type == "job" {
			t.Fatalf("got a job result outside the caller's job globs: %+v", r)
		}
	}
}

// TestSearchRecords_RunTenantScopedNoControllersReadPushesDown proves a
// caller with only a tenant-scoped pipelines:read grant (no
// controllers:read) still sees a "run" row tenanted on that tenant: the
// D5 scope alone admits no row for this caller, so the row reaches the
// response only through the tenant pushdown (ruling y).
func TestSearchRecords_RunTenantScopedNoControllersReadPushesDown(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "team-a", JobPath: "team-a/svc/build"},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-a-pipelines"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		}},
	}
	// EffectivePermissions' Tenants partition (Resolver.TenantGrants)
	// enumerates existing Team CRDs, so the test needs one named
	// "team-a" for the tenant-scoped binding above to surface.
	teams := []*v1alpha1.Team{{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}}
	a := NewAuthorizer(testResolverWithTeams(pipelinesRoles(), bindings, teams), false)
	server := NewServer(&Dependencies{Logger: slog.Default(), Authorizer: a, RecordStore: reader})
	claims := &auth.Claims{Subject: "u"}

	got := server.searchRecords(context.Background(), claims, "query", "")
	found := false
	for _, r := range got {
		if r.Type == "run" && r.Namespace == "ns-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("got %+v, want the run row tenanted team-a present", got)
	}

	tenantSeen := false
	for _, tn := range reader.lastQuery.Tenants {
		if tn == "team-a" {
			tenantSeen = true
		}
	}
	if !tenantSeen {
		t.Fatalf("reader.lastQuery.Tenants = %v, want it to contain %q", reader.lastQuery.Tenants, "team-a")
	}
	if !a.CanReadRun(claims, RunTenantRow{Tenant: "team-a", JobPath: "team-a/svc/build"}) {
		t.Fatal("want CanReadRun to admit the row directly")
	}
}

// TestSearchRecords_RunDeletedTeamTenantScopedBindingStillPushesDown
// proves a "run" row tenanted on "team-gone" stays present when the
// caller holds a tenant-scoped binding naming "team-gone" but no Team
// object of that name exists. Resolver.TenantGrants enumerates existing
// Team objects only, so it cannot see this binding's tenant; the
// pushdown must still carry the name through perms.Scopes, since
// CanReadRun's binding path (path 2 of D4) authorizes the row directly
// regardless of whether the Team exists.
func TestSearchRecords_RunDeletedTeamTenantScopedBindingStillPushesDown(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "team-gone", JobPath: "team-gone/svc/build"},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-gone-pipelines"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-gone"}}},
		}},
	}
	// No Team named "team-gone" is seeded: the Team was deleted, but the
	// binding and the historical run row it authorized both remain.
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	server := NewServer(&Dependencies{Logger: slog.Default(), Authorizer: a, RecordStore: reader})
	claims := &auth.Claims{Subject: "u"}

	if !a.CanReadRun(claims, RunTenantRow{Tenant: "team-gone", JobPath: "team-gone/svc/build"}) {
		t.Fatal("want CanReadRun to admit the row directly through the binding path, Team or not")
	}

	got := server.searchRecords(context.Background(), claims, "query", "")
	found := false
	for _, r := range got {
		if r.Type == "run" && r.Namespace == "ns-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("got %+v, want the run row tenanted team-gone present even with no Team object", got)
	}

	tenantSeen := false
	for _, tn := range reader.lastQuery.Tenants {
		if tn == "team-gone" {
			tenantSeen = true
		}
	}
	if !tenantSeen {
		t.Fatalf("reader.lastQuery.Tenants = %v, want it to contain %q", reader.lastQuery.Tenants, "team-gone")
	}
}

// TestSearchRecords_ClusterWidePipelinesGrantPushesDownAllTenants proves
// a caller whose only grant is a cluster-wide custom role with
// pipelines:read (and no controllers:read) sees a "run" row tenanted
// with a synthetic tenant, and that the pushdown reaches the reader as
// AllTenants: a cluster-wide binding's role never appears in the D5
// scope (which reflects controllers:read) or in Tenants (a cluster-wide
// binding names no tenant), so AllTenants is the only path that carries
// it (ruling y).
func TestSearchRecords_ClusterWidePipelinesGrantPushesDownAllTenants(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "ns-a/cell-a", JobPath: "svc/build"},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-pipelines"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			// No Scope: a cluster-wide binding.
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	server := NewServer(&Dependencies{Logger: slog.Default(), Authorizer: a, RecordStore: reader})
	claims := &auth.Claims{Subject: "u"}

	if !a.CanReadRun(claims, RunTenantRow{Tenant: "ns-a/cell-a", JobPath: "svc/build"}) {
		t.Fatal("want CanReadRun to admit the row through the cluster-wide binding's pipelines:read")
	}

	got := server.searchRecords(context.Background(), claims, "query", "")
	found := false
	for _, r := range got {
		if r.Type == "run" && r.Namespace == "ns-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("got %+v, want the run row tenanted ns-a/cell-a present", got)
	}
	if !reader.lastQuery.AllTenants {
		t.Fatal("reader.lastQuery.AllTenants = false, want true for a cluster-wide pipelines:read grant")
	}
}

// TestSearchRecords_NamespaceScopedPipelinesGrantPushesDownNamespace
// proves a caller whose only grant is a namespace-scoped binding on
// "ns-a" with pipelines:read sees a "run" row in "ns-a", and that the
// pushdown reaches the reader's job/run namespace list.
func TestSearchRecords_NamespaceScopedPipelinesGrantPushesDownNamespace(t *testing.T) {
	reader := &fixedSearchReader{results: recordstore.SearchResults{
		Runs: []recordstore.RunSearchResult{
			{RunID: "r-a", Cluster: "core", Namespace: "ns-a", Cell: "cell-a", Tenant: "ns-a/cell-a", JobPath: "svc/build"},
		},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-a-pipelines"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	server := NewServer(&Dependencies{Logger: slog.Default(), Authorizer: a, RecordStore: reader})
	claims := &auth.Claims{Subject: "u"}

	if !a.CanReadRun(claims, RunTenantRow{Tenant: "ns-a/cell-a", JobPath: "svc/build"}) {
		t.Fatal("want CanReadRun to admit the row through the namespace-scoped binding's pipelines:read")
	}

	got := server.searchRecords(context.Background(), claims, "query", "")
	found := false
	for _, r := range got {
		if r.Type == "run" && r.Namespace == "ns-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("got %+v, want the run row in ns-a present", got)
	}
	if reader.lastQuery.AllTenants {
		t.Fatal("reader.lastQuery.AllTenants = true, want false for a namespace-scoped grant")
	}
	nsSeen := false
	for _, ns := range reader.lastQuery.JobRunNamespaces {
		if ns == "ns-a" {
			nsSeen = true
		}
	}
	if !nsSeen {
		t.Fatalf("reader.lastQuery.JobRunNamespaces = %v, want it to contain %q", reader.lastQuery.JobRunNamespaces, "ns-a")
	}
}
