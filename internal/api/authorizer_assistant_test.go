package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestCanUseAssistant_BuiltinRoleMatrix proves the assistant gates against a
// cluster-wide binding for each built-in role. Admin holds both verbs through
// its wildcard. Operator, developer, and viewer hold use only.
func TestCanUseAssistant_BuiltinRoleMatrix(t *testing.T) {
	cases := []struct {
		role    string
		rules   []v1alpha1.APIRule
		wantUse bool
		wantAdm bool
	}{
		{
			role:    "admin",
			rules:   []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
			wantUse: true, wantAdm: true,
		},
		{
			role:    "operator",
			rules:   []v1alpha1.APIRule{{Resources: []string{"assistant"}, Verbs: []string{"use"}}},
			wantUse: true,
		},
		{
			role:    "developer",
			rules:   []v1alpha1.APIRule{{Resources: []string{"assistant"}, Verbs: []string{"use"}}},
			wantUse: true,
		},
		{
			role:    "viewer",
			rules:   []v1alpha1.APIRule{{Resources: []string{"assistant"}, Verbs: []string{"use"}}},
			wantUse: true,
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

			if got := a.CanUseAssistant(claims); got != tc.wantUse {
				t.Errorf("CanUseAssistant = %v, want %v", got, tc.wantUse)
			}
			if got := a.CanAdminAssistant(claims); got != tc.wantAdm {
				t.Errorf("CanAdminAssistant = %v, want %v", got, tc.wantAdm)
			}
		})
	}
}

// TestCanUseAssistant_NamespaceScopedViewerDenied proves a namespace-scoped
// binding gains no assistant access, even for a role that grants use.
func TestCanUseAssistant_NamespaceScopedViewerDenied(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"assistant"}, Verbs: []string{"use"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-viewer"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
	if a.CanUseAssistant(&auth.Claims{Subject: "user-a"}) {
		t.Error("a namespace-scoped binding must be denied CanUseAssistant")
	}
	if a.CanUseAssistant(nil) || a.CanAdminAssistant(nil) {
		t.Error("nil claims must be denied both assistant gates")
	}
}
