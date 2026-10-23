package rbac

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func mustParseTenant(t *testing.T, name string) ParsedTenant {
	t.Helper()
	ref, err := ParseTenant(name)
	if err != nil {
		t.Fatalf("ParseTenant(%q): %v", name, err)
	}
	return ref
}

// readyTenant builds a Tenant whose managed Group counts: status.groupRef names
// it and TenantRBACReady is true.
func readyTenant(roleRef string, subjects ...v1alpha1.SubjectRef) *v1alpha1.Tenant {
	const name = "payments"
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.TenantSpec{RoleRef: roleRef, Subjects: subjects},
		Status: v1alpha1.TenantStatus{
			GroupRef:   "tenant-" + name,
			Conditions: []v1alpha1.TenantCondition{{Type: v1alpha1.TenantConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
}

func TestTenantGrantSources_TenantMembershipPath(t *testing.T) {
	tenant := readyTenant("developer", v1alpha1.SubjectRef{Kind: "User", Name: "alice"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Tenant{tenant}, nil)

	grants := r.TenantGrantSources(mustParseTenant(t, "payments"))
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d: %+v", len(grants), grants)
	}
	g := grants[0]
	if g.Source != GrantSourceTenantMembership {
		t.Errorf("Source = %q, want %q", g.Source, GrantSourceTenantMembership)
	}
	if g.Binding != "" {
		t.Errorf("Binding = %q, want empty for the membership path", g.Binding)
	}
	if g.RoleRef != "developer" {
		t.Errorf("RoleRef = %q, want developer", g.RoleRef)
	}
	if len(g.Subjects) != 2 || g.Subjects[0].Name != "tenant-payments" || g.Subjects[1].Name != "alice" {
		t.Errorf("Subjects = %+v, want the managed Group and alice", g.Subjects)
	}
	if len(g.Verbs) != 2 || g.Verbs[0] != "read" || g.Verbs[1] != "write" {
		t.Errorf("Verbs = %v, want read and write", g.Verbs)
	}
	if g.Jobs != nil {
		t.Errorf("Jobs = %v, want nil for an unnarrowed grant", g.Jobs)
	}
}

func TestTenantGrantSources_TenantMembershipSkipsAnUnreadyGroup(t *testing.T) {
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{RoleRef: "developer"},
		Status:     v1alpha1.TenantStatus{GroupRef: "tenant-payments"},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Tenant{tenant}, nil)

	// TenantRBACReady is absent, so the managed Group is not an effective
	// subject and the Tenant names no other subject.
	if grants := r.TenantGrantSources(mustParseTenant(t, "payments")); len(grants) != 0 {
		t.Fatalf("expected no grant, got %+v", grants)
	}
}

func TestTenantGrantSources_AdminTenantRoleYieldsNoGrant(t *testing.T) {
	tenant := readyTenant("admin")
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Tenant{tenant}, nil)

	if grants := r.TenantGrantSources(mustParseTenant(t, "payments")); len(grants) != 0 {
		t.Fatalf("an admin roleRef contributed %d grants", len(grants))
	}
}

func TestTenantGrantSources_EmptyTenantRoleMeansDeveloper(t *testing.T) {
	tenant := readyTenant("")
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Tenant{tenant}, nil)

	grants := r.TenantGrantSources(mustParseTenant(t, "payments"))
	if len(grants) != 1 || grants[0].RoleRef != "developer" {
		t.Fatalf("grants = %+v, want one developer grant", grants)
	}
}

func TestTenantGrantSources_TenantScopedBindingPath(t *testing.T) {
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-release"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-idp-group"}},
			RoleRef:  "developer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"release/*"}}},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, nil, nil)

	grants := r.TenantGrantSources(mustParseTenant(t, "payments"))
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d: %+v", len(grants), grants)
	}
	g := grants[0]
	if g.Source != GrantSourceTenantScopedBinding || g.Binding != "payments-release" {
		t.Errorf("Source = %q, Binding = %q", g.Source, g.Binding)
	}
	if len(g.Jobs) != 1 || g.Jobs[0] != "release/*" {
		t.Errorf("Jobs = %v, want release/*", g.Jobs)
	}
	if len(g.Subjects) != 1 || g.Subjects[0].Name != "pay-idp-group" {
		t.Errorf("Subjects = %+v", g.Subjects)
	}

	// The same binding reaches no other tenant.
	if other := r.TenantGrantSources(mustParseTenant(t, "search")); len(other) != 0 {
		t.Errorf("the scoped binding reached search: %+v", other)
	}
}

func TestTenantGrantSources_ClusterWideBindingReachesEveryTenant(t *testing.T) {
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-viewers"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "everyone"}},
			RoleRef:  "viewer",
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, nil, nil)

	for _, tenant := range []string{"payments", "search"} {
		grants := r.TenantGrantSources(mustParseTenant(t, tenant))
		if len(grants) != 1 {
			t.Fatalf("%s: expected 1 grant, got %d", tenant, len(grants))
		}
		if grants[0].Source != GrantSourceClusterWideBinding {
			t.Errorf("%s: Source = %q", tenant, grants[0].Source)
		}
		if grants[0].Jobs != nil {
			t.Errorf("%s: an unnarrowed grant carries Jobs %v", tenant, grants[0].Jobs)
		}
	}
}

func TestTenantGrantSources_TwoGrantsStaySeparate(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "wide-write"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "writers"}},
				RoleRef:  "writer",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "narrow-read"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "readers"}},
				RoleRef:  "viewer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"release/*"}}},
				},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole(), writerRole()}, bindings, nil, nil)

	grants := r.TenantGrantSources(mustParseTenant(t, "payments"))
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %d: %+v", len(grants), grants)
	}
	// The narrowing grant comes first, so a reader sees the restriction
	// before the wide grant.
	if grants[0].Source != GrantSourceTenantScopedBinding || grants[1].Source != GrantSourceClusterWideBinding {
		t.Fatalf("order = %q, %q", grants[0].Source, grants[1].Source)
	}
	if len(grants[0].Verbs) != 1 || grants[0].Verbs[0] != "read" {
		t.Errorf("narrowed grant verbs = %v, want read alone", grants[0].Verbs)
	}
	if len(grants[1].Verbs) != 1 || grants[1].Verbs[0] != "write" {
		t.Errorf("wide grant verbs = %v, want write alone", grants[1].Verbs)
	}
}

func TestTenantGrantSources_StableOrder(t *testing.T) {
	tenant := readyTenant("developer")
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "zz-wide"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "viewer"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "aa-wide"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "viewer"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "bb-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				RoleRef: "viewer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
				},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole(), viewerRole()}, bindings, []*v1alpha1.Tenant{tenant}, nil)

	grants := r.TenantGrantSources(mustParseTenant(t, "payments"))
	want := []struct{ source, binding string }{
		{GrantSourceTenantMembership, ""},
		{GrantSourceTenantScopedBinding, "bb-scoped"},
		{GrantSourceClusterWideBinding, "aa-wide"},
		{GrantSourceClusterWideBinding, "zz-wide"},
	}
	if len(grants) != len(want) {
		t.Fatalf("expected %d grants, got %d: %+v", len(want), len(grants), grants)
	}
	for i, w := range want {
		if grants[i].Source != w.source || grants[i].Binding != w.binding {
			t.Errorf("grants[%d] = %q/%q, want %q/%q", i, grants[i].Source, grants[i].Binding, w.source, w.binding)
		}
	}
}

func TestTenantGrantSources_TruncationCap(t *testing.T) {
	var bindings []*v1alpha1.VarroaRoleBinding
	for i := 0; i < MaxTenantGrants+5; i++ {
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wide-%03d", i)},
			Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "viewer"},
		})
	}
	// One narrowing grant must survive the cut, because the order puts the
	// narrowing rows first.
	bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "narrow"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef: "viewer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"release/*"}}},
			},
		},
	})
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, bindings, nil, nil)

	grants, truncated := r.TenantGrantSourcesTruncated(mustParseTenant(t, "payments"))
	if !truncated {
		t.Error("expected truncated true")
	}
	if len(grants) != MaxTenantGrants {
		t.Fatalf("expected %d grants, got %d", MaxTenantGrants, len(grants))
	}
	if grants[0].Source != GrantSourceTenantScopedBinding {
		t.Errorf("the cut dropped the narrowing grant: first source = %q", grants[0].Source)
	}
}

func TestTenantGrantSources_ExactCapIsNotTruncated(t *testing.T) {
	var bindings []*v1alpha1.VarroaRoleBinding
	for i := 0; i < MaxTenantGrants; i++ {
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wide-%03d", i)},
			Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "viewer"},
		})
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, bindings, nil, nil)

	grants, truncated := r.TenantGrantSourcesTruncated(mustParseTenant(t, "payments"))
	if truncated {
		t.Error("a list of exactly the cap reported a cut")
	}
	if len(grants) != MaxTenantGrants {
		t.Fatalf("expected %d grants, got %d", MaxTenantGrants, len(grants))
	}
}

func TestTenantGrantSources_NothingReachesTheTenant(t *testing.T) {
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, nil, nil)

	grants, truncated := r.TenantGrantSourcesTruncated(mustParseTenant(t, "payments"))
	if len(grants) != 0 || truncated {
		t.Fatalf("grants = %+v, truncated = %v", grants, truncated)
	}
}

func TestTenantGrantSources_ControllerTenantResolvesNothing(t *testing.T) {
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-viewers"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "viewer"},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, nil, nil)

	// A cell hosts pipelines tenants only, so a synthetic controller tenant
	// resolves nothing.
	if grants := r.TenantGrantSources(mustParseTenant(t, "tenant-a/ci")); len(grants) != 0 {
		t.Fatalf("a controller tenant resolved %d grants", len(grants))
	}
}

func TestTenantItemRole(t *testing.T) {
	if got := TenantItemRoleName("payments"); got != "varroa:tenant:payments" {
		t.Errorf("TenantItemRoleName = %q", got)
	}
	if got := TenantItemRolePattern("payments"); got != "^payments($|/.*)$" {
		t.Errorf("TenantItemRolePattern = %q", got)
	}
}
