package rbac

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// federatedGrantCopy returns a tenant-member copy with the source-binding
// annotation the federation projection writes.
func federatedGrantCopy(name, team, jobs, from string, subjects ...v1alpha1.SubjectRef) *v1alpha1.JenkinsRoleBinding {
	b := federatedTenantCopy(name, team, jobs, subjects...)
	b.Annotations[AnnotationFederatedBindings] = from
	return b
}

func TestTenantGrantSources_ReportsFederatedCopy(t *testing.T) {
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, nil, nil)
	r = withFederatedCopies(r, federatedGrantCopy("varroa-fed-tenant-narrow", "sbsoftware", `["repos/projgen/*"]`,
		"alpha-writers,beta-writers", v1alpha1.SubjectRef{Kind: "User", Name: "alice"}))

	grants := r.TenantGrantSources(mustParseTenant(t, "sbsoftware"))
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d: %+v", len(grants), grants)
	}
	g := grants[0]
	if g.Source != GrantSourceFederatedBinding {
		t.Errorf("Source = %q, want %q", g.Source, GrantSourceFederatedBinding)
	}
	if g.Binding != "varroa-fed-tenant-narrow" {
		t.Errorf("Binding = %q, want the copy name", g.Binding)
	}
	if g.RoleRef != "pipelines-writer" {
		t.Errorf("RoleRef = %q, want the source VarroaRole pipelines-writer", g.RoleRef)
	}
	if len(g.Subjects) != 1 || g.Subjects[0].Name != "alice" {
		t.Errorf("Subjects = %+v, want alice", g.Subjects)
	}
	if !reflect.DeepEqual(g.Verbs, []string{"write"}) {
		t.Errorf("Verbs = %v, want [write]", g.Verbs)
	}
	if !reflect.DeepEqual(g.Jobs, []string{"repos/projgen/*"}) {
		t.Errorf("Jobs = %v, want the job glob", g.Jobs)
	}
	if !reflect.DeepEqual(g.FederatedFrom, []string{"alpha-writers", "beta-writers"}) {
		t.Errorf("FederatedFrom = %v, want the two source bindings", g.FederatedFrom)
	}
}

func TestTenantGrantSources_FederatedWholeFolderCopyHasNilJobs(t *testing.T) {
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, nil, nil)
	r = withFederatedCopies(r, federatedGrantCopy("varroa-fed-tenant-whole", "sbsoftware", "", "alpha-writers",
		v1alpha1.SubjectRef{Kind: "User", Name: "alice"}))

	grants := r.TenantGrantSources(mustParseTenant(t, "sbsoftware"))
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	if grants[0].Jobs != nil {
		t.Errorf("Jobs = %v, want nil for a whole-folder copy", grants[0].Jobs)
	}
	if !reflect.DeepEqual(grants[0].FederatedFrom, []string{"alpha-writers"}) {
		t.Errorf("FederatedFrom = %v, want [alpha-writers]", grants[0].FederatedFrom)
	}
}

func TestTenantGrantSources_SkipsCopyOfOtherTenantOrUnreadableJobs(t *testing.T) {
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, nil, nil)
	r = withFederatedCopies(r,
		federatedGrantCopy("varroa-fed-tenant-other", "other-team", "", "alpha-writers",
			v1alpha1.SubjectRef{Kind: "User", Name: "bob"}),
		federatedGrantCopy("varroa-fed-tenant-bad", "sbsoftware", `not-json`, "alpha-writers",
			v1alpha1.SubjectRef{Kind: "User", Name: "carol"}),
		federatedGrantCopy("varroa-fed-tenant-empty", "sbsoftware", `[]`, "alpha-writers",
			v1alpha1.SubjectRef{Kind: "User", Name: "dave"}),
	)

	if grants := r.TenantGrantSources(mustParseTenant(t, "sbsoftware")); len(grants) != 0 {
		t.Errorf("grants = %+v, want none: a copy of another tenant or with an unreadable job list fails closed", grants)
	}
}

func TestTenantGrantSources_OrdersFederatedBetweenTenantScopedAndClusterWide(t *testing.T) {
	cluster := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-writers"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "everyone"}},
			RoleRef:  "developer",
		},
	}
	scoped := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-scoped"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "acme"}},
			RoleRef:  "developer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "sbsoftware"}}},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{cluster, scoped}, nil, nil)
	r = withFederatedCopies(r, federatedGrantCopy("varroa-fed-tenant-whole", "sbsoftware", "", "alpha-writers",
		v1alpha1.SubjectRef{Kind: "User", Name: "alice"}))

	grants := r.TenantGrantSources(mustParseTenant(t, "sbsoftware"))
	got := make([]string, 0, len(grants))
	for _, g := range grants {
		got = append(got, g.Source)
	}
	want := []string{GrantSourceTenantScopedBinding, GrantSourceFederatedBinding, GrantSourceClusterWideBinding}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
}

func TestTenantMemberRoles_FederatedCopyCountedOnce(t *testing.T) {
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, nil, nil)
	r = withFederatedCopies(r, federatedGrantCopy("varroa-fed-tenant-whole", "sbsoftware", "", "alpha-writers",
		v1alpha1.SubjectRef{Kind: "User", Name: "alice"}))

	roles := r.TenantMemberRoles(mustParseTenant(t, "sbsoftware"))
	if len(roles) != 1 {
		t.Fatalf("roles = %+v, want one subtree role", roles)
	}
	if len(roles[0].Subjects) != 1 || roles[0].Subjects[0].Name != "alice" {
		t.Errorf("subjects = %+v, want alice once", roles[0].Subjects)
	}
}
