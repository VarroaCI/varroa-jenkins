package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestCanRunners_BuiltinRoleMatrix proves the four typed runners
// authorizer methods (design D12) against a cluster-wide binding for each
// built-in role's runners verb set (internal/controller/role_controller.go,
// TestBuiltinRoles_RunnersVerbMatrix): admin and operator hold all four
// verbs, developer and viewer hold read only.
func TestCanRunners_BuiltinRoleMatrix(t *testing.T) {
	cases := []struct {
		role       string
		rules      []v1alpha1.APIRule
		wantRead   bool
		wantCreate bool
		wantUpdate bool
		wantDelete bool
	}{
		{
			role:       "admin",
			rules:      []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
			wantRead:   true,
			wantCreate: true,
			wantUpdate: true,
			wantDelete: true,
		},
		{
			role:       "operator",
			rules:      []v1alpha1.APIRule{{Resources: []string{"runners"}, Verbs: []string{"read", "create", "update", "delete"}}},
			wantRead:   true,
			wantCreate: true,
			wantUpdate: true,
			wantDelete: true,
		},
		{
			role:     "developer",
			rules:    []v1alpha1.APIRule{{Resources: []string{"runners"}, Verbs: []string{"read"}}},
			wantRead: true,
		},
		{
			role:     "viewer",
			rules:    []v1alpha1.APIRule{{Resources: []string{"runners"}, Verbs: []string{"read"}}},
			wantRead: true,
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
					// Scope: nil = cluster-wide, matching how the built-in
					// roles are normally bound.
				},
			}
			a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
			claims := &auth.Claims{Subject: "user-" + tc.role}

			if got := a.CanReadRunners(claims); got != tc.wantRead {
				t.Errorf("CanReadRunners = %v, want %v", got, tc.wantRead)
			}
			if got := a.CanCreateRunners(claims); got != tc.wantCreate {
				t.Errorf("CanCreateRunners = %v, want %v", got, tc.wantCreate)
			}
			if got := a.CanUpdateRunners(claims); got != tc.wantUpdate {
				t.Errorf("CanUpdateRunners = %v, want %v", got, tc.wantUpdate)
			}
			if got := a.CanDeleteRunners(claims); got != tc.wantDelete {
				t.Errorf("CanDeleteRunners = %v, want %v", got, tc.wantDelete)
			}
		})
	}
}

// TestCanRunners_NilClaimsDenied proves every typed method denies nil
// claims, matching every other typed Can* method on Authorizer.
func TestCanRunners_NilClaimsDenied(t *testing.T) {
	a := NewAuthorizer(testResolver(nil, nil), false)
	if a.CanReadRunners(nil) {
		t.Error("CanReadRunners(nil) must be false")
	}
	if a.CanCreateRunners(nil) {
		t.Error("CanCreateRunners(nil) must be false")
	}
	if a.CanUpdateRunners(nil) {
		t.Error("CanUpdateRunners(nil) must be false")
	}
	if a.CanDeleteRunners(nil) {
		t.Error("CanDeleteRunners(nil) must be false")
	}
}

// TestCanRunners_NamespaceScopedBindingDenied proves the cluster-scope leak
// fix that CanReadJenkinsRoles etc. already carry: a namespace-scoped
// runners binding must not satisfy the cluster-wide runners gate. Runner is
// cluster-scoped (design D12: "the verb grants the whole fleet"), so this
// mirrors TestGlobalOnlyGates_NamespaceScopedOperatorDenied.
func TestCanRunners_NamespaceScopedBindingDenied(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "runners-role"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"runners"}, Verbs: []string{"read", "create", "update", "delete"}}}},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-runners"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "runners-role",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
	claims := &auth.Claims{Subject: "user-a"}

	if a.CanReadRunners(claims) {
		t.Error("namespace-scoped runners binding must be denied CanReadRunners")
	}
	if a.CanCreateRunners(claims) {
		t.Error("namespace-scoped runners binding must be denied CanCreateRunners")
	}
}
