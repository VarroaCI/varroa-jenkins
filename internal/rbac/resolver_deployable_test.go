package rbac

import (
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// --------------------------------------------------------------------------
// Tests for CreateScopeNamespaces
// --------------------------------------------------------------------------

func TestCreateScopeNamespaces_NilClaims(t *testing.T) {
	r := testResolver(nil, nil, nil, false)
	ns, unrestricted := r.CreateScopeNamespaces(nil)
	if ns != nil {
		t.Errorf("expected nil namespaces for nil claims, got %v", ns)
	}
	if unrestricted {
		t.Error("expected unrestricted=false for nil claims")
	}
}

func TestCreateScopeNamespaces_ClusterWideCreate(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"create"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "platform-eng"}},
				RoleRef:  "admin",
				// Scope: nil = cluster-wide
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"platform-eng"}}

	ns, unrestricted := r.CreateScopeNamespaces(claims)
	if len(ns) != 0 {
		t.Errorf("expected empty namespaces for cluster-wide binding, got %v", ns)
	}
	if !unrestricted {
		t.Error("expected unrestricted=true for cluster-wide binding")
	}
}

func TestCreateScopeNamespaces_ExplicitNamespaces(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"create"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-ns-a-b"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "developer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"ns-a", "ns-b"},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "dev-user"}

	ns, unrestricted := r.CreateScopeNamespaces(claims)
	if unrestricted {
		t.Error("expected unrestricted=false for namespace-scoped binding")
	}
	sort.Strings(ns)
	if len(ns) != 2 || ns[0] != "ns-a" || ns[1] != "ns-b" {
		t.Errorf("expected [ns-a ns-b], got %v", ns)
	}
}

func TestCreateScopeNamespaces_EmptyNamespacesUnrestricted(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "creator"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"create"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-ns-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "devs"}},
				RoleRef:  "creator",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"devs"}}

	ns, unrestricted := r.CreateScopeNamespaces(claims)
	if len(ns) != 0 {
		t.Errorf("expected empty namespaces, got %v", ns)
	}
	if !unrestricted {
		t.Error("expected unrestricted=true for scope with empty Namespaces")
	}
}

func TestCreateScopeNamespaces_SelectorOnly(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "selector-role"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"create"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "selector-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "sel-user"}},
				RoleRef:  "selector-role",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					ControllerSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"env": "prod"},
					},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "sel-user"}

	ns, unrestricted := r.CreateScopeNamespaces(claims)
	if len(ns) != 0 {
		t.Errorf("expected empty namespaces for selector-only, got %v", ns)
	}
	if unrestricted {
		t.Error("expected unrestricted=false for selector-only scope")
	}
}

func TestCreateScopeNamespaces_SelectorPlusNamespaces(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "combo-role"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "combo-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "combo-user"}},
				RoleRef:  "combo-role",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"ns-x"},
					ControllerSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"tier": "frontend"},
					},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "combo-user"}

	ns, unrestricted := r.CreateScopeNamespaces(claims)
	if len(ns) != 0 {
		t.Errorf("expected empty namespaces for selector+namespaces scope, got %v", ns)
	}
	if unrestricted {
		t.Error("expected unrestricted=false for selector+namespaces scope")
	}
}

func TestCreateScopeNamespaces_WildcardRoleWithNamespaces(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "super-admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "sa-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "admins"}},
				RoleRef:  "super-admin",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"ns-admin"},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "admin-user", Groups: []string{"admins"}}

	ns, unrestricted := r.CreateScopeNamespaces(claims)
	if unrestricted {
		t.Error("expected unrestricted=false for namespace-scoped binding")
	}
	if len(ns) != 1 || ns[0] != "ns-admin" {
		t.Errorf("expected [ns-admin], got %v", ns)
	}
}

func TestCreateScopeNamespaces_ReadOnlyRole(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "viewer-user"}},
				RoleRef:  "viewer",
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "viewer-user"}

	ns, unrestricted := r.CreateScopeNamespaces(claims)
	if len(ns) != 0 {
		t.Errorf("expected empty namespaces for read-only role, got %v", ns)
	}
	if unrestricted {
		t.Error("expected unrestricted=false for read-only role")
	}
}

func TestCreateScopeNamespaces_MixedUnrestrictedAndExplicit(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-admin"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "multi-user"}},
				RoleRef:  "admin",
				// Scope: nil = cluster-wide
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "multi-user"}},
				RoleRef:  "admin",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"ns-scoped"},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "multi-user"}

	ns, unrestricted := r.CreateScopeNamespaces(claims)
	if !unrestricted {
		t.Error("expected unrestricted=true (one binding is cluster-wide)")
	}
	// Should have ns-scoped from the explicit binding
	if len(ns) != 1 || ns[0] != "ns-scoped" {
		t.Errorf("expected [ns-scoped], got %v", ns)
	}
}
