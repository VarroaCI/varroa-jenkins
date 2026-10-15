package rbac

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestTenantGrants_OrdersByTenantNameThenSortedJobs proves the ordering
// contract of TenantGrants: entries sort by tenant name, then by each
// entry's sorted Jobs list.
func TestTenantGrants_OrdersByTenantNameThenSortedJobs(t *testing.T) {
	teams := []*v1alpha1.Team{
		{ObjectMeta: metav1.ObjectMeta{Name: "payments"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "billing"}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b1"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"z/*"}}}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b2"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"a/*"}}}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b3"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "billing"}}},
		}},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, bindings, teams, nil)
	claims := &auth.Claims{Subject: "u"}

	got := r.TenantGrants(claims)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(got), got)
	}
	if got[0].Tenant != "billing" {
		t.Errorf("expected billing first, got %q", got[0].Tenant)
	}
	if got[1].Tenant != "payments" || len(got[1].Jobs) != 1 || got[1].Jobs[0] != "a/*" {
		t.Errorf("expected payments/[a/*] second, got %+v", got[1])
	}
	if got[2].Tenant != "payments" || len(got[2].Jobs) != 1 || got[2].Jobs[0] != "z/*" {
		t.Errorf("expected payments/[z/*] third, got %+v", got[2])
	}
}

// TestTenantGrants_TeamsWithNoPipelinesVerbDropped proves a Team the caller
// can access, but whose role grants no pipelines verb, produces no entry.
func TestTenantGrants_TeamsWithNoPipelinesVerbDropped(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{RoleRef: "controllers-only"},
		Status: v1alpha1.TeamStatus{
			GroupRef:   "team-payments",
			Conditions: []v1alpha1.TeamCondition{{Type: v1alpha1.TeamConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "controllers-only"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{role}, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"team-payments"}}

	got := r.TenantGrants(claims)
	if len(got) != 0 {
		t.Fatalf("expected no entries, got %+v", got)
	}
}

// TestTenantGrants_NilClaimsReturnsEmptyNotNil proves the result is always
// a non-nil slice.
func TestTenantGrants_NilClaimsReturnsEmptyNotNil(t *testing.T) {
	r := testTenantResolver(nil, nil, nil, nil)
	got := r.TenantGrants(nil)
	if got == nil {
		t.Fatal("expected a non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected no entries, got %+v", got)
	}
}
