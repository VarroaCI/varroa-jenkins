package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestCanReadFleetLocks_BuiltinRoleMatrix proves the fleet-lock gate
// against a cluster-wide binding for each built-in role. operator holds
// the verb, admin holds it through its wildcard, and developer and viewer
// hold neither: one fleet-wide lock read discloses every tenant's lock
// names and job paths at once.
func TestCanReadFleetLocks_BuiltinRoleMatrix(t *testing.T) {
	cases := []struct {
		role  string
		rules []v1alpha1.APIRule
		want  bool
	}{
		{
			role:  "admin",
			rules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
			want:  true,
		},
		{
			role:  "operator",
			rules: []v1alpha1.APIRule{{Resources: []string{"fleetlocks"}, Verbs: []string{"read"}}},
			want:  true,
		},
		{
			role:  "developer",
			rules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read", "write"}}},
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

			if got := a.CanReadFleetLocks(claims); got != tc.want {
				t.Errorf("CanReadFleetLocks = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCanReadFleetLocks_NamespaceScopedBindingDenied proves the
// cluster-scoped class: varroa.fleet_locks carries no CRD, and the gate
// still refuses a binding narrowed to a namespace allowlist.
func TestCanReadFleetLocks_NamespaceScopedBindingDenied(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"fleetlocks"}, Verbs: []string{"read"}}},
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

	if a.CanReadFleetLocks(claims) {
		t.Error("a namespace-scoped binding must be denied CanReadFleetLocks")
	}

	// The same grant on a cluster-wide binding is allowed.
	wide := *binding
	wide.Name = "wide-operator"
	wide.Spec.Scope = nil
	wideAuth := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{&wide}), false)
	if !wideAuth.CanReadFleetLocks(claims) {
		t.Error("a cluster-wide binding granting fleetlocks:read must be allowed")
	}
}

// TestCanReadFleetLocks_NilClaimsDenied proves the gate denies an
// unauthenticated caller, matching every other typed Can* method.
func TestCanReadFleetLocks_NilClaimsDenied(t *testing.T) {
	a := NewAuthorizer(testResolver(nil, nil), false)
	if a.CanReadFleetLocks(nil) {
		t.Error("CanReadFleetLocks(nil) must be false")
	}
}
