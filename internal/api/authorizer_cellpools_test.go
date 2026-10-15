package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestCanCellPools_BuiltinRoleMatrix proves the two cell-pool gates against a
// cluster-wide binding for each built-in role. operator holds both verbs,
// admin holds both through its wildcard, and developer and viewer hold
// neither: CellPool.status.tenants lists every tenant of the pool, and the
// Teams API is admin-only.
func TestCanCellPools_BuiltinRoleMatrix(t *testing.T) {
	cases := []struct {
		role       string
		rules      []v1alpha1.APIRule
		wantRead   bool
		wantUpdate bool
	}{
		{
			role:       "admin",
			rules:      []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
			wantRead:   true,
			wantUpdate: true,
		},
		{
			role:       "operator",
			rules:      []v1alpha1.APIRule{{Resources: []string{"cellpools"}, Verbs: []string{"read", "update"}}},
			wantRead:   true,
			wantUpdate: true,
		},
		{
			role:  "developer",
			rules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		},
		{
			role:  "viewer",
			rules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			role := &v1alpha1.VarroaRole{
				ObjectMeta: metav1.ObjectMeta{Name: tc.role},
				Spec:       v1alpha1.VarroaRoleSpec{APIRules: tc.rules},
			}
			binding := &v1alpha1.VarroaRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: tc.role + "-binding"},
				Spec: v1alpha1.VarroaRoleBindingSpec{
					Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-" + tc.role}},
					RoleRef:  tc.role,
				},
			}
			a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
			claims := &auth.Claims{Subject: "user-" + tc.role}

			if got := a.CanReadCellPools(claims); got != tc.wantRead {
				t.Errorf("CanReadCellPools = %v, want %v", got, tc.wantRead)
			}
			if got := a.CanUpdateCellPools(claims); got != tc.wantUpdate {
				t.Errorf("CanUpdateCellPools = %v, want %v", got, tc.wantUpdate)
			}
		})
	}
}

// TestCanCellPools_NamespaceScopedBindingDenied proves the cluster-scoped
// class: CellPool is cluster-scoped, so a namespace-scoped binding satisfies
// neither gate.
func TestCanCellPools_NamespaceScopedBindingDenied(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"cellpools"}, Verbs: []string{"read", "update"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-operator"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "operator",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
	claims := &auth.Claims{Subject: "user-a"}

	if a.CanReadCellPools(claims) {
		t.Error("a namespace-scoped binding must be denied CanReadCellPools")
	}
	if a.CanUpdateCellPools(claims) {
		t.Error("a namespace-scoped binding must be denied CanUpdateCellPools")
	}
}

// TestCanCellPools_NilClaimsDenied proves both gates deny an unauthenticated
// caller, matching every other typed Can* method.
func TestCanCellPools_NilClaimsDenied(t *testing.T) {
	a := NewAuthorizer(testResolver(nil, nil), false)
	if a.CanReadCellPools(nil) {
		t.Error("CanReadCellPools(nil) must be false")
	}
	if a.CanUpdateCellPools(nil) {
		t.Error("CanUpdateCellPools(nil) must be false")
	}
}
