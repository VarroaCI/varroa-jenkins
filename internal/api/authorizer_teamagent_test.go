package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// teamAgentRoleRules mirrors the Team agent rules of the built-in roles. The
// controller test TestBuiltinRoles_TeamAgentVerbs keeps the two in step.
func teamAgentRoleRules(role string) []v1alpha1.APIRule {
	switch role {
	case "admin":
		return []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}
	case "operator":
		return []v1alpha1.APIRule{
			{Resources: []string{"teamagents"}, Verbs: []string{"unattended"}},
			{Resources: []string{"agentsessions"}, Verbs: []string{"read"}},
		}
	default:
		return []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}}
	}
}

// teamAgentAuthorizer binds the subject to role at cluster scope, or at the
// namespace scope when namespaced is true.
func teamAgentAuthorizer(role string, namespaced bool) (*Authorizer, *auth.Claims) {
	r := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: role},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: teamAgentRoleRules(role)},
	}
	b := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role + "-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-" + role}},
			RoleRef:  role,
		},
	}
	if namespaced {
		b.Spec.Scope = &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}}
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{r}, []*v1alpha1.VarroaRoleBinding{b}), false)
	return a, &auth.Claims{Subject: "user-" + role}
}

// TestCanGrantUnattended_Matrix proves the unattended grant sits on admin and
// operator, and that a namespace-scoped operator and a developer lack it.
func TestCanGrantUnattended_Matrix(t *testing.T) {
	cases := []struct {
		name       string
		role       string
		namespaced bool
		want       bool
	}{
		{"operator", "operator", false, true},
		{"admin", "admin", false, true},
		{"developer", "developer", false, false},
		{"namespace-scoped operator", "operator", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, claims := teamAgentAuthorizer(c.role, c.namespaced)
			if got := a.CanGrantUnattended(claims); got != c.want {
				t.Errorf("CanGrantUnattended = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCanReadAgentSessions_Matrix mirrors the unattended matrix for the
// session read.
func TestCanReadAgentSessions_Matrix(t *testing.T) {
	cases := []struct {
		name       string
		role       string
		namespaced bool
		want       bool
	}{
		{"operator", "operator", false, true},
		{"admin", "admin", false, true},
		{"developer", "developer", false, false},
		{"namespace-scoped operator", "operator", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, claims := teamAgentAuthorizer(c.role, c.namespaced)
			if got := a.CanReadAgentSessions(claims); got != c.want {
				t.Errorf("CanReadAgentSessions = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCanGrantUnattended_NilClaims denies a missing identity.
func TestCanGrantUnattended_NilClaims(t *testing.T) {
	a, _ := teamAgentAuthorizer("operator", false)
	if a.CanGrantUnattended(nil) || a.CanReadAgentSessions(nil) {
		t.Fatal("a nil identity must hold no Team agent grant")
	}
}
