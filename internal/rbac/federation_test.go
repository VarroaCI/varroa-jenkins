package rbac

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestDesiredFederatedCRsRefPath(t *testing.T) {
	scope := &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}}
	roles, bindings, warnings := DesiredFederatedCRs(
		[]*v1alpha1.VarroaRoleBinding{{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-admin"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				RoleRef:  "team-admin",
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
				Scope:    scope,
			},
		}},
		func(name string) (*v1alpha1.VarroaRole, bool) {
			return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "jenkins-admin"}}, true
		},
		func(name string) (*v1alpha1.JenkinsRole, bool) {
			return &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Overall/Administer"}, Description: "copied"}}, true
		},
	)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if len(roles) != 1 || roles[0].Name != "jenkins-admin" {
		t.Fatalf("roles = %#v", roles)
	}
	if got := roles[0].Labels[LabelFederatedFrom]; got != "team-admin" {
		t.Fatalf("federated label = %q", got)
	}
	if roles[0].Spec.Description != "copied" || roles[0].Spec.Permissions[0] != "Overall/Administer" {
		t.Fatalf("role spec not fully copied: %#v", roles[0].Spec)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %#v", bindings)
	}
	if !strings.HasPrefix(bindings[0].Name, "varroa-fed-team-admin-") {
		t.Fatalf("binding name = %q", bindings[0].Name)
	}
	if bindings[0].Spec.RoleRef != "jenkins-admin" {
		t.Fatalf("binding RoleRef = %q", bindings[0].Spec.RoleRef)
	}
	if bindings[0].Spec.ControllerScope != scope {
		t.Fatalf("ControllerScope was not copied verbatim")
	}
}

func TestDesiredFederatedCRsLegacyPath(t *testing.T) {
	roles, bindings, warnings := DesiredFederatedCRs(
		[]*v1alpha1.VarroaRoleBinding{{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "legacy-role", Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "devs"}}}}},
		func(name string) (*v1alpha1.VarroaRole, bool) {
			return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsPermissions: []string{"Job/Build"}}}, true
		},
		func(string) (*v1alpha1.JenkinsRole, bool) { return nil, false },
	)
	if len(warnings) != 0 || len(roles) != 1 || len(bindings) != 1 {
		t.Fatalf("roles=%#v bindings=%#v warnings=%v", roles, bindings, warnings)
	}
	if roles[0].Name != "legacy-role" || roles[0].Spec.RoleType != "Global" || roles[0].Spec.Permissions[0] != "Job/Build" {
		t.Fatalf("legacy role = %#v", roles[0])
	}
	if bindings[0].Spec.RoleRef != "legacy-role" {
		t.Fatalf("legacy binding RoleRef = %q", bindings[0].Spec.RoleRef)
	}
}

func TestDesiredFederatedCRsSkipsNonGlobalRef(t *testing.T) {
	roles, bindings, warnings := DesiredFederatedCRs(
		[]*v1alpha1.VarroaRoleBinding{{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "item-role"}}},
		func(name string) (*v1alpha1.VarroaRole, bool) {
			return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "folder-role"}}, true
		},
		func(name string) (*v1alpha1.JenkinsRole, bool) {
			return &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Item", Permissions: []string{"Job/Read"}}}, true
		},
	)
	if len(roles) != 0 || len(bindings) != 0 || len(warnings) != 1 {
		t.Fatalf("roles=%#v bindings=%#v warnings=%v", roles, bindings, warnings)
	}
}

func TestDesiredFederatedCRsDedupsRolesAndSkipsMite(t *testing.T) {
	vrbs := []*v1alpha1.VarroaRoleBinding{
		{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}}},
		{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "bob"}}}},
		// A VarroaRole whose Jenkins projection resolves to the reserved
		// system-mite role must be skipped (guard is on the projected roleName,
		// not the VarroaRole's own name).
		{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "mite-ref", Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "attackers"}}}},
	}
	roles, bindings, warnings := DesiredFederatedCRs(
		vrbs,
		func(name string) (*v1alpha1.VarroaRole, bool) {
			ref := "jenkins-admin"
			if name == "mite-ref" {
				ref = "system-mite"
			}
			return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: ref}}, true
		},
		func(name string) (*v1alpha1.JenkinsRole, bool) {
			return &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Overall/Read"}}}, true
		},
	)
	if len(roles) != 1 || roles[0].Name != "jenkins-admin" {
		t.Fatalf("roles = %#v", roles)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings = %#v", bindings)
	}
	for _, b := range bindings {
		if b.Spec.RoleRef == "system-mite" {
			t.Fatalf("system-mite binding was federated: %#v", b)
		}
	}
	if len(warnings) == 0 {
		t.Fatalf("expected a warning for the skipped system-mite projection")
	}
}

// A VarroaRole whose Jenkins projection resolves to the reserved
// system-operator role must be skipped for the same reason as system-mite: the
// resolver synthesizes varroa:system-operator (carrying Administer, bound to the
// operator's machine-identity group) and a user projection to that name would
// collide with it. Guard is on the projected roleName, not the VarroaRole's name.
func TestDesiredFederatedCRsSkipsSystemOperator(t *testing.T) {
	vrbs := []*v1alpha1.VarroaRoleBinding{
		{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}}},
		{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "op-ref", Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "attackers"}}}},
	}
	roles, bindings, warnings := DesiredFederatedCRs(
		vrbs,
		func(name string) (*v1alpha1.VarroaRole, bool) {
			ref := "jenkins-admin"
			if name == "op-ref" {
				ref = "system-operator"
			}
			return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: ref}}, true
		},
		func(name string) (*v1alpha1.JenkinsRole, bool) {
			return &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Overall/Read"}}}, true
		},
	)
	if len(roles) != 1 || roles[0].Name != "jenkins-admin" {
		t.Fatalf("roles = %#v", roles)
	}
	for _, b := range bindings {
		if b.Spec.RoleRef == "system-operator" {
			t.Fatalf("system-operator binding was federated: %#v", b)
		}
	}
	if len(warnings) == 0 {
		t.Fatalf("expected a warning for the skipped system-operator projection")
	}
}

func TestDesiredFederatedCRsLabelIsDeterministic(t *testing.T) {
	// Two VarroaRoles map to the same roleName via a shared jenkinsRoleRef; the
	// federated role's federated-from label must be the smallest source name
	// regardless of binding order (no reflect.DeepEqual drift churn).
	mk := func(order []string) map[string]string {
		vrbs := make([]*v1alpha1.VarroaRoleBinding, 0, len(order))
		for _, n := range order {
			vrbs = append(vrbs, &v1alpha1.VarroaRoleBinding{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: n, Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: n}}}})
		}
		roles, _, _ := DesiredFederatedCRs(
			vrbs,
			func(name string) (*v1alpha1.VarroaRole, bool) {
				return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "shared-role"}}, true
			},
			func(name string) (*v1alpha1.JenkinsRole, bool) {
				return &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}}, true
			},
		)
		if len(roles) != 1 {
			t.Fatalf("roles = %#v", roles)
		}
		return roles[0].Labels
	}
	a := mk([]string{"zeta", "alpha"})
	b := mk([]string{"alpha", "zeta"})
	if a[LabelFederatedFrom] != "alpha" || b[LabelFederatedFrom] != "alpha" {
		t.Fatalf("label not deterministic: %q vs %q", a[LabelFederatedFrom], b[LabelFederatedFrom])
	}
}
