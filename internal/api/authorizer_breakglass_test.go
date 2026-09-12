package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// --------------------------------------------------------------------------
// Tests for CanBreakGlass (D6, task 7.1/7.2)
// --------------------------------------------------------------------------

// TestCanBreakGlass_BuiltinRoles mirrors the four built-in role verb sets
// from internal/controller/role_controller.go's builtinRoles(). Only
// varroa:admin holds break-glass, and it holds the verb through its
// wildcard "*"/"*" grant: the role literal below carries no explicit
// "break-glass" entry, which is task 7.2's acceptance.
func TestCanBreakGlass_BuiltinRoles(t *testing.T) {
	tests := []struct {
		name string
		role *v1alpha1.VarroaRole
		want bool
	}{
		{
			name: "admin",
			role: &v1alpha1.VarroaRole{
				ObjectMeta: metav1.ObjectMeta{Name: "admin"},
				Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				}},
			},
			want: true,
		},
		{
			name: "operator",
			role: &v1alpha1.VarroaRole{
				ObjectMeta: metav1.ObjectMeta{Name: "operator"},
				Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read", "create", "update", "delete", "approve-restart", "approve-deletion", "manage"}},
				}},
			},
			want: false,
		},
		{
			name: "developer",
			role: &v1alpha1.VarroaRole{
				ObjectMeta: metav1.ObjectMeta{Name: "developer"},
				Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read", "approve-restart"}},
				}},
			},
			want: false,
		},
		{
			name: "viewer",
			role: &v1alpha1.VarroaRole{
				ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
				Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				}},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bindings := []*v1alpha1.VarroaRoleBinding{
				{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-" + tc.name},
					Spec: v1alpha1.VarroaRoleBindingSpec{
						Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
						RoleRef:  tc.role.Name,
						// Scope: nil = cluster-wide
					}},
			}
			a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{tc.role}, bindings), false)
			claims := &auth.Claims{Subject: "user-a"}

			got := a.CanBreakGlass(claims, "team-a", "c1")
			if got != tc.want {
				t.Errorf("CanBreakGlass(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestCanBreakGlass_NilClaims mirrors every other controllers-verb check:
// an unauthenticated caller is denied regardless of defaultRead.
func TestCanBreakGlass_NilClaims(t *testing.T) {
	a := NewAuthorizer(testResolver(nil, nil), true)
	if a.CanBreakGlass(nil, "team-a", "c1") {
		t.Error("nil claims must be denied CanBreakGlass")
	}
}

// --------------------------------------------------------------------------
// Tests for verb independence (D6, task 7.3)
// --------------------------------------------------------------------------

// TestCanBreakGlass_IndependentOfApproveVerbs proves break-glass grants and
// is granted independently of approve-restart and approve-deletion, in both
// directions.
func TestCanBreakGlass_IndependentOfApproveVerbs(t *testing.T) {
	approveOnlyRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "approve-only"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"controllers"}, Verbs: []string{"approve-restart", "approve-deletion"}},
		}},
	}
	breakGlassOnlyRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "break-glass-only"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"controllers"}, Verbs: []string{"break-glass"}},
		}},
	}

	t.Run("approve-restart and approve-deletion do not grant break-glass", func(t *testing.T) {
		bindings := []*v1alpha1.VarroaRoleBinding{
			{ObjectMeta: metav1.ObjectMeta{Name: "approve-only-binding"},
				Spec: v1alpha1.VarroaRoleBindingSpec{
					Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
					RoleRef:  "approve-only",
				}},
		}
		a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{approveOnlyRole}, bindings), false)
		claims := &auth.Claims{Subject: "user-a"}

		if !a.CanApproveRestart(claims, "team-a", "c1") {
			t.Error("approve-only role must still be allowed CanApproveRestart")
		}
		if !a.CanApproveDeletion(claims, "team-a", "c1") {
			t.Error("approve-only role must still be allowed CanApproveDeletion")
		}
		if a.CanBreakGlass(claims, "team-a", "c1") {
			t.Error("approve-restart and approve-deletion must NOT grant CanBreakGlass")
		}
	})

	t.Run("break-glass does not grant approve-restart or approve-deletion", func(t *testing.T) {
		bindings := []*v1alpha1.VarroaRoleBinding{
			{ObjectMeta: metav1.ObjectMeta{Name: "break-glass-only-binding"},
				Spec: v1alpha1.VarroaRoleBindingSpec{
					Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-b"}},
					RoleRef:  "break-glass-only",
				}},
		}
		a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{breakGlassOnlyRole}, bindings), false)
		claims := &auth.Claims{Subject: "user-b"}

		if !a.CanBreakGlass(claims, "team-a", "c1") {
			t.Error("break-glass-only role must still be allowed CanBreakGlass")
		}
		if a.CanApproveRestart(claims, "team-a", "c1") {
			t.Error("break-glass must NOT grant CanApproveRestart")
		}
		if a.CanApproveDeletion(claims, "team-a", "c1") {
			t.Error("break-glass must NOT grant CanApproveDeletion")
		}
	})
}
