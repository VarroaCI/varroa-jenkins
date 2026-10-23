package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// devRole grants pipelines read+write; readerRole grants pipelines read
// only. Both are used across the tests below.
func pipelinesRoles() []*v1alpha1.VarroaRole {
	return []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "dev"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"pipelines"}, Verbs: []string{"read", "write"}},
				// Carried so the selector-scoped test can prove the same
				// binding still satisfies a controllers gate.
				{Resources: []string{"controllers"}, Verbs: []string{"read"}},
			},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "reader"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "writer-only"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"write"}}},
		}},
	}
}

// --------------------------------------------------------------------------
// Step 1: nil claims deny.
// --------------------------------------------------------------------------

func TestCanAccessPipeline_Step1_NilClaimsDenies(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}
	if a.CanReadPipeline(nil, ref, "") {
		t.Error("expected nil claims to deny CanReadPipeline")
	}
	if a.CanWritePipeline(nil, ref, "") {
		t.Error("expected nil claims to deny CanWritePipeline")
	}
	if a.CanReadRun(nil, RunTenantRow{Tenant: "team-a"}) {
		t.Error("expected nil claims to deny CanReadRun")
	}
}

// --------------------------------------------------------------------------
// Step 2: the tenant must parse, and its parsed folder must equal the
// supplied folder. This also covers the acceptance item "an inconsistent
// TenantRef denied".
// --------------------------------------------------------------------------

func TestCanAccessPipeline_Step2_UnparsableTenantDenies(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}

	// "a/b/c" has two "/" and does not parse per rbac.ParseTenant.
	ref := TenantRef{Tenant: "a/b/c", Folder: ""}
	if a.CanReadPipeline(claims, ref, "") {
		t.Error("expected an unparsable tenant value to deny")
	}
}

func TestCanAccessPipeline_Step2_InconsistentTenantRefDenied(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}

	// Tenant "team-a" parses to Folder "team-a", not "team-b": the caller
	// supplied a reference that disagrees with itself.
	ref := TenantRef{Tenant: "team-a", Folder: "team-b"}
	if a.CanReadPipeline(claims, ref, "") {
		t.Error("expected an inconsistent TenantRef (Folder mismatch) to deny")
	}
}

// --------------------------------------------------------------------------
// Step 3: a non-empty job path must lie inside the tenant's folder.
// --------------------------------------------------------------------------

func TestCanAccessPipeline_Step3_JobPathOutsideFolderDenies(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	if a.CanReadPipeline(claims, ref, "team-b/checkout-api/main") {
		t.Error("expected a job path outside the tenant's folder to deny")
	}
}

func TestCanAccessPipeline_Step3_EmptyFolderPrefixesEveryPath(t *testing.T) {
	// A TenantController's folder is "": the stripped path equals the input.
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}
	ref := TenantRef{Tenant: "ns-a/ctrl-a", Folder: ""}

	if !a.CanReadPipeline(claims, ref, "whatever/job") {
		t.Error("expected an empty folder to prefix every job path")
	}
}

// --------------------------------------------------------------------------
// Step 4: grants come only from EffectiveTenantCapabilities. A caller with
// no matching binding holds no grants and is denied.
// --------------------------------------------------------------------------

func TestCanAccessPipeline_Step4_NoGrantsDenies(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "someone-else"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"} // does not match the binding's subject
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	if a.CanReadPipeline(claims, ref, "") {
		t.Error("expected a caller with no matching binding to hold no grants")
	}
}

// --------------------------------------------------------------------------
// Step 5: verb + glob, tested on one grant. Covers the acceptance items "a
// narrowed caller allowed on the empty job path and refused on a
// non-matching path", the wildcard-globs branch, and the "entries tested
// one at a time and never unioned" rule.
// --------------------------------------------------------------------------

func TestCanAccessPipeline_Step5_NarrowedGrantAllowsEmptyJobPath(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "team-a", Jobs: []string{"release/*"}}},
			},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	if !a.CanReadPipeline(claims, ref, "") {
		t.Error("expected a job-narrowed grant to admit the empty job path (renders the tenant page)")
	}
}

func TestCanAccessPipeline_Step5_NarrowedGrantRefusesNonMatchingPath(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "team-a", Jobs: []string{"release/*"}}},
			},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	if a.CanReadPipeline(claims, ref, "team-a/other-job/main") {
		t.Error("expected a job-narrowed grant to refuse a path outside its globs")
	}
	if !a.CanReadPipeline(claims, ref, "team-a/release/eu") {
		t.Error("expected a job-narrowed grant to allow a path matching its globs (folder-relative)")
	}
}

func TestCanAccessPipeline_Step5_NilGlobsCoverEveryJob(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	if !a.CanReadPipeline(claims, ref, "team-a/anything/at/all") {
		t.Error("expected a grant with nil Globs to cover every job")
	}
}

// TestCanAccessPipeline_Step5_GrantsNeverUnioned is the subtle unit: one
// grant holds the verb with a non-matching glob, and a second grant matches
// the glob but lacks the verb. Neither grant alone satisfies both tests, so
// the caller must be denied. A union-of-grants bug (checking hasVerb across
// the whole list separately from the glob match) would wrongly allow this.
func TestCanAccessPipeline_Step5_GrantsNeverUnioned(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "read-narrowed"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader", // holds pipelines:read, not write
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "team-a", Jobs: []string{"other/*"}}}, // does not match "release/eu"
			},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "write-unnarrowed"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "writer-only", // holds pipelines:write, not read
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	if a.CanReadPipeline(claims, ref, "team-a/release/eu") {
		t.Error("expected the read check to deny: no single grant holds both pipelines:read and a matching glob")
	}
}

// --------------------------------------------------------------------------
// D7: a run row whose controller is absent from the lister still authorizes
// by the tenant stored on the row.
// --------------------------------------------------------------------------

func TestCanReadRun_ControllerAbsentFromListerStillAuthorizes(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	// testResolver's controller lister is always empty: "ctrl-gone" is never
	// added, modeling a deleted cell.
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}

	row := RunTenantRow{Tenant: "ns-a/ctrl-gone", JobPath: "main"}
	if !a.CanReadRun(claims, row) {
		t.Error("expected a run row to authorize by its stored tenant even when the cell no longer exists")
	}
}

// TestCanReadRun_MalformedTenantDenies proves CanReadRun denies explicitly
// on a row whose stored tenant does not parse, rather than falling through
// to canAccessPipeline with a zero-value folder.
func TestCanReadRun_MalformedTenantDenies(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
		}},
	}
	a := NewAuthorizer(testResolver(pipelinesRoles(), bindings), false)
	claims := &auth.Claims{Subject: "u"}

	row := RunTenantRow{Tenant: "a/b/c", JobPath: "main"}
	if a.CanReadRun(claims, row) {
		t.Error("expected a malformed row tenant to deny CanReadRun")
	}
}

// --------------------------------------------------------------------------
// D7: a selector-scoped ("colocated") binding grants no pipelines access,
// on a live request, even when it would satisfy a controllers gate for the
// same target.
// --------------------------------------------------------------------------

func TestCanReadPipeline_SelectorScopedBindingRefused(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "selector-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "dev",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "team-a"}},
			},
		}},
	}
	ctrl := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a", Labels: map[string]string{"tenant": "team-a"}},
	}
	a := NewAuthorizer(testResolverWithController(pipelinesRoles(), bindings, ctrl), false)
	claims := &auth.Claims{Subject: "u"}
	ref := TenantRef{Tenant: "ns-a/ctrl-a", Folder: ""}

	if a.CanReadPipeline(claims, ref, "") {
		t.Error("expected a selector-scoped binding to grant no pipelines access")
	}
	// The same binding still grants a controllers verb for the co-located
	// controller: this proves the refusal is specific to the pipelines gate,
	// not a general binding failure.
	if !a.CanReadController(claims, "ns-a", "ctrl-a") {
		t.Error("expected the selector-scoped binding to still satisfy a controllers gate")
	}
}

// --------------------------------------------------------------------------
// Nil Authorizer denies all three checks.
// --------------------------------------------------------------------------

func TestCanAccessPipeline_NilAuthorizerDeniesAll(t *testing.T) {
	var a *Authorizer
	claims := &auth.Claims{Subject: "u"}
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	if a.CanReadPipeline(claims, ref, "") {
		t.Error("expected a nil Authorizer to deny CanReadPipeline")
	}
	if a.CanWritePipeline(claims, ref, "") {
		t.Error("expected a nil Authorizer to deny CanWritePipeline")
	}
	if a.CanReadRun(claims, RunTenantRow{Tenant: "team-a"}) {
		t.Error("expected a nil Authorizer to deny CanReadRun")
	}
}

// --------------------------------------------------------------------------
// TenantRef constructors.
// --------------------------------------------------------------------------

func TestTenantRefFromTenant_TenantTenant(t *testing.T) {
	ref, err := TenantRefFromTenant("team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := TenantRef{Tenant: "team-a", TenantName: "team-a", Folder: "team-a"}
	if ref != want {
		t.Errorf("got %+v, want %+v", ref, want)
	}
}

func TestTenantRefFromTenant_ControllerTenant(t *testing.T) {
	ref, err := TenantRefFromTenant("ns-a/ctrl-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := TenantRef{
		Namespace:  "ns-a",
		Controller: "ctrl-a",
		Tenant:     "ns-a/ctrl-a",
		TenantName: "ctrl-a",
		Folder:     "",
	}
	if ref != want {
		t.Errorf("got %+v, want %+v", ref, want)
	}
}

func TestTenantRefFromTenant_MalformedDenies(t *testing.T) {
	if _, err := TenantRefFromTenant(""); err == nil {
		t.Error("expected an empty tenant value to error")
	}
	if _, err := TenantRefFromTenant("a/b/c"); err == nil {
		t.Error("expected a doubly-nested tenant value to error")
	}
}

func TestTenantRefFromRow_StampsRowIdentityColumns(t *testing.T) {
	ref, err := TenantRefFromRow("core", "ns-a", "ctrl-a", "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := TenantRef{
		Cluster:    "core",
		Namespace:  "ns-a",
		Controller: "ctrl-a",
		Tenant:     "team-a",
		TenantName: "team-a",
		Folder:     "team-a",
	}
	if ref != want {
		t.Errorf("got %+v, want %+v", ref, want)
	}
}

func TestTenantRefFromRow_MalformedTenantDenies(t *testing.T) {
	if _, err := TenantRefFromRow("core", "ns-a", "ctrl-a", ""); err == nil {
		t.Error("expected an empty tenant value to error")
	}
}

// testResolverWithController mirrors testResolver, but seeds the controller
// lister with one Controller, for tests that must prove a selector-scoped
// binding still satisfies a controllers gate against a live target.
func testResolverWithController(
	roles []*v1alpha1.VarroaRole,
	bindings []*v1alpha1.VarroaRoleBinding,
	ctrl *v1alpha1.Controller,
) *rbac.Resolver {
	roleIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range roles {
		_ = roleIdx.Add(r)
	}
	roleInf := &fakeInformer{indexer: roleIdx}

	bindingIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{rbac.BySubjectIndex: rbac.SubjectIndexFunc})
	for _, b := range bindings {
		_ = bindingIdx.Add(b)
	}
	bindingInf := &fakeInformer{indexer: bindingIdx}

	jrIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	jrInf := &fakeInformer{indexer: jrIdx}

	jrbIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{rbac.JenkinsBySubjectIndex: rbac.JenkinsSubjectIndexFunc})
	jrbInf := &fakeInformer{indexer: jrbIdx}

	ctrlIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	_ = ctrlIdx.Add(ctrl)
	ctrlInf := &fakeInformer{indexer: ctrlIdx}

	return rbac.NewResolver(roleInf, bindingInf, jrInf, jrbInf, ctrlInf, nil, false, []string{"sub", "preferred_username"})
}
