package mcp

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// rbacReaderClaims is the caller of the RBAC read-gate tests.
var rbacReaderClaims = &auth.Claims{Subject: "rbac-reader"}

// rbacReadTools is the guard table of the RBAC read gates. Each row names one
// read tool, its arguments, the resource whose read verb it needs, and whether
// it takes a cluster argument.
var rbacReadTools = []struct {
	tool     string
	args     map[string]any
	resource string
	cluster  bool
}{
	{tool: "list_jenkins_roles", args: map[string]any{}, resource: "jenkinsroles"},
	{tool: "get_jenkins_role", args: map[string]any{"name": "r1"}, resource: "jenkinsroles", cluster: true},
	{tool: "list_jenkins_role_bindings", args: map[string]any{}, resource: "jenkinsrolebindings"},
	{tool: "get_jenkins_role_binding", args: map[string]any{"name": "b1"}, resource: "jenkinsrolebindings", cluster: true},
	{tool: "list_varroa_roles", args: map[string]any{}, resource: "roles"},
	{tool: "get_varroa_role", args: map[string]any{"name": "vr1"}, resource: "roles"},
	{tool: "list_varroa_role_bindings", args: map[string]any{}, resource: "rolebindings"},
	{tool: "get_varroa_role_binding", args: map[string]any{"name": "vb1"}, resource: "rolebindings"},
}

// rbacReadDenied is the refusal that a caller without resource:read gets.
func rbacReadDenied(resource string) string {
	return "access denied: missing " + resource + ":read permission"
}

// rbacReadResources returns each resource of rbacReadTools once, in table order.
func rbacReadResources() []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range rbacReadTools {
		if !seen[row.resource] {
			seen[row.resource] = true
			out = append(out, row.resource)
		}
	}
	return out
}

// rbacReadDeps binds rbac-reader to one role that holds rules. A nil scope makes
// the binding cluster-wide. The local store and the member member-b each hold
// the Jenkins role r1 and the Jenkins role binding b1. The local store also
// holds the Varroa role vr1 and the Varroa role binding vb1.
func rbacReadDeps(t *testing.T, rules []v1alpha1.APIRule, scope *v1alpha1.VarroaRoleBindingScope) *api.Dependencies {
	t.Helper()
	seed := func(s *crdstore.Fake) {
		crdstore.MustSeed(s,
			&v1alpha1.JenkinsRole{
				ObjectMeta: metav1.ObjectMeta{Name: "r1"},
				Spec: v1alpha1.JenkinsRoleSpec{
					RoleType:    "Global",
					Permissions: []string{"hudson.model.Hudson.Read"},
				},
			},
			&v1alpha1.JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "b1"},
				Spec: v1alpha1.JenkinsRoleBindingSpec{
					RoleRef:  "r1",
					Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
				},
			},
		)
	}
	local := crdstore.NewFake()
	seed(local)
	crdstore.MustSeed(local,
		&v1alpha1.VarroaRole{
			ObjectMeta: metav1.ObjectMeta{Name: "vr1"},
			Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}}},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vb1"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				RoleRef:  "vr1",
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			},
		},
	)
	member := &memberConfig{store: crdstore.NewFake()}
	seed(member.store)

	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "reader"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: rules},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "reader-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "rbac-reader"}},
			RoleRef:  "reader",
			Scope:    scope,
		},
	}}
	return &api.Dependencies{
		Client:      &stubClient{},
		Store:       local,
		Authorizer:  api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
		Brood:       memberBrood{},
		ConfigBrood: member,
	}
}

// TestRBACReadTools_RefuseACallerWithoutTheGrant proves that the gate runs
// before any read and before the cluster lookup. A caller with no read grant
// gets the same refusal on every cluster, including an unknown one.
func TestRBACReadTools_RefuseACallerWithoutTheGrant(t *testing.T) {
	deps := rbacReadDeps(t, []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}}, nil)
	for _, row := range rbacReadTools {
		variants := []map[string]any{row.args}
		if row.cluster {
			variants = append(variants,
				withArgs(row.args, map[string]any{"cluster": "member-b"}),
				withArgs(row.args, map[string]any{"cluster": "member-z"}),
			)
		}
		for _, args := range variants {
			res := callToolAs(t, deps, rbacReaderClaims, row.tool, args)
			if !res.IsError || contentText(res) != rbacReadDenied(row.resource) {
				t.Errorf("%s %v: got isError=%v text=%q, want %q", row.tool, args, res.IsError, contentText(res), rbacReadDenied(row.resource))
			}
		}
	}
}

// TestRBACReadTools_NamespaceScopedGrantIsRefused covers the spec scenario of a
// namespace-scoped grant. The RBAC objects are cluster-scoped, so such a grant
// does not pass the gate.
func TestRBACReadTools_NamespaceScopedGrantIsRefused(t *testing.T) {
	scope := &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}}
	deps := rbacReadDeps(t, []v1alpha1.APIRule{{Resources: rbacReadResources(), Verbs: []string{"read"}}}, scope)
	for _, row := range rbacReadTools {
		res := callToolAs(t, deps, rbacReaderClaims, row.tool, row.args)
		if !res.IsError || contentText(res) != rbacReadDenied(row.resource) {
			t.Errorf("%s: got isError=%v text=%q, want %q", row.tool, res.IsError, contentText(res), rbacReadDenied(row.resource))
		}
	}
}

// TestRBACReadTools_EachToolChecksItsOwnVerb grants one resource at a time,
// cluster-wide. Only the tools of that resource may succeed.
func TestRBACReadTools_EachToolChecksItsOwnVerb(t *testing.T) {
	for _, res := range rbacReadResources() {
		t.Run(res, func(t *testing.T) {
			deps := rbacReadDeps(t, []v1alpha1.APIRule{{Resources: []string{res}, Verbs: []string{"read"}}}, nil)
			for _, row := range rbacReadTools {
				if row.resource == res {
					out := callToolAs(t, deps, rbacReaderClaims, row.tool, row.args)
					if out.IsError {
						t.Errorf("%s: granted %s:read, got error %q", row.tool, res, contentText(out))
					}
					if row.cluster {
						out = callToolAs(t, deps, rbacReaderClaims, row.tool, withArgs(row.args, map[string]any{"cluster": "member-b"}))
						if out.IsError {
							t.Errorf("%s on member-b: granted %s:read, got error %q", row.tool, res, contentText(out))
						}
					}
					continue
				}
				out := callToolAs(t, deps, rbacReaderClaims, row.tool, row.args)
				if !out.IsError || contentText(out) != rbacReadDenied(row.resource) {
					t.Errorf("%s: granted only %s:read, got isError=%v text=%q", row.tool, res, out.IsError, contentText(out))
				}
			}
		})
	}
}

// TestRBACReadTools_NeedAnAuthorizerAndAnIdentity checks the two guards that
// run before the grant check: an authenticated caller and a configured
// authorizer.
func TestRBACReadTools_NeedAnAuthorizerAndAnIdentity(t *testing.T) {
	deps := rbacReadDeps(t, []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}}, nil)
	for _, row := range rbacReadTools {
		res := callToolAs(t, deps, nil, row.tool, row.args)
		if !res.IsError || contentText(res) != "authentication required" {
			t.Errorf("%s without claims: got isError=%v text=%q, want %q", row.tool, res.IsError, contentText(res), "authentication required")
		}
	}
	deps.Authorizer = nil
	for _, row := range rbacReadTools {
		res := callToolAs(t, deps, rbacReaderClaims, row.tool, row.args)
		if !res.IsError || contentText(res) != "authorizer not configured" {
			t.Errorf("%s without authorizer: got isError=%v text=%q, want %q", row.tool, res.IsError, contentText(res), "authorizer not configured")
		}
	}
}
