package rbac

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestDesiredFederatedCRsRefPath(t *testing.T) {
	scope := &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}}
	roles, bindings, warnings, _ := DesiredFederatedCRs(
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
	roles, bindings, warnings, _ := DesiredFederatedCRs(
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
	roles, bindings, warnings, _ := DesiredFederatedCRs(
		[]*v1alpha1.VarroaRoleBinding{{Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "item-role", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}}}},
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
	roles, bindings, warnings, _ := DesiredFederatedCRs(
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
	roles, bindings, warnings, _ := DesiredFederatedCRs(
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
		roles, _, _, _ := DesiredFederatedCRs(
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

// A VarroaRole whose only rule grants a pipelines verb and sets neither
// jenkinsRoleRef nor jenkinsPermissions has no Jenkins projection: D12 states
// spec.apiRules is never read by this function.
func TestDesiredFederatedCRsPipelinesOnlyRoleFederatesNothing(t *testing.T) {
	roles, bindings, _, _ := DesiredFederatedCRs(
		[]*v1alpha1.VarroaRoleBinding{{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-pipelines"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "pipelines-only", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}},
		}},
		func(name string) (*v1alpha1.VarroaRole, bool) {
			return &v1alpha1.VarroaRole{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}}},
			}, true
		},
		func(string) (*v1alpha1.JenkinsRole, bool) { return nil, false },
	)
	if len(roles) != 0 || len(bindings) != 0 {
		t.Fatalf("roles=%#v bindings=%#v, want no federated objects", roles, bindings)
	}
}

// D12: the federation reconciler skips a VarroaRoleBinding whose scope
// carries tenants and records a warning, matching the existing skip for a
// non-Global referenced role. A sibling controller-scoped binding on the same
// role still federates.
func TestDesiredFederatedCRsSkipsTenantScopedBinding(t *testing.T) {
	tenantScoped := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "bind-tenant"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef:  "team-admin",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		},
	}
	controllerScoped := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "bind-controller"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef:  "team-admin",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "bob"}},
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		},
	}
	roles, bindings, warnings, _ := DesiredFederatedCRs(
		[]*v1alpha1.VarroaRoleBinding{tenantScoped, controllerScoped},
		func(name string) (*v1alpha1.VarroaRole, bool) {
			return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "jenkins-admin"}}, true
		},
		func(name string) (*v1alpha1.JenkinsRole, bool) {
			return &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}}, true
		},
	)
	if len(roles) != 1 || roles[0].Name != "jenkins-admin" {
		t.Fatalf("roles = %#v", roles)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %#v, want exactly the controller-scoped binding", bindings)
	}
	for _, b := range bindings {
		for _, s := range b.Spec.Subjects {
			if s.Name == "alice" {
				t.Fatalf("tenant-scoped binding was federated: %#v", b)
			}
		}
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "bind-tenant") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning naming bind-tenant, got %v", warnings)
	}
}

// A VarroaRoleBinding with no subjects grants nothing in the generator, and
// the JenkinsRoleBinding CRD rejects a binding with no subjects field. The
// projector skips it, and a role that only such a binding references stays
// off the hive.
func TestDesiredFederatedCRsSkipsEmptySubjects(t *testing.T) {
	vrbs := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-empty"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "developer", Subjects: []v1alpha1.SubjectRef{}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "team-nil"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "developer"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "team-tenant"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "developer", Scope: &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "bind-admin"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}}},
	}
	roles, bindings, warnings, _ := DesiredFederatedCRs(
		vrbs,
		func(name string) (*v1alpha1.VarroaRole, bool) {
			return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "jenkins-" + name}}, true
		},
		func(name string) (*v1alpha1.JenkinsRole, bool) {
			return &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}}, true
		},
	)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for a binding with no subjects", warnings)
	}
	if len(roles) != 1 || roles[0].Name != "jenkins-team-admin" {
		t.Fatalf("roles = %#v, want only jenkins-team-admin", roles)
	}
	if len(bindings) != 1 || bindings[0].Spec.RoleRef != "jenkins-team-admin" {
		t.Fatalf("bindings = %#v, want only the team-admin binding", bindings)
	}
}

// The JenkinsRoleBinding subjects field has no omitempty. A nil slice encodes
// as null, and the API server treats null as a missing required field.
func TestDesiredFederatedCRsSubjectsNeverNull(t *testing.T) {
	_, bindings, _, _ := DesiredFederatedCRs(
		[]*v1alpha1.VarroaRoleBinding{{ObjectMeta: metav1.ObjectMeta{Name: "bind-admin"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}}}},
		func(name string) (*v1alpha1.VarroaRole, bool) {
			return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsPermissions: []string{"Overall/Read"}}}, true
		},
		func(string) (*v1alpha1.JenkinsRole, bool) { return nil, false },
	)
	if len(bindings) != 1 {
		t.Fatalf("bindings = %#v", bindings)
	}
	raw, err := json.Marshal(bindings[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	subjects, ok := decoded.Spec["subjects"]
	if !ok || !strings.HasPrefix(string(subjects), "[") {
		t.Fatalf("spec.subjects is not a list: %s", raw)
	}
}

// A projected binding never encodes a null subjects list, even when a caller
// hands the projector a subject list that it then copies.
func TestFederatedSubjectsCopyIsNonNil(t *testing.T) {
	if got := copySubjects(nil); got == nil {
		t.Fatalf("copySubjects(nil) = nil, want an empty non-nil slice")
	}
}

// Each child names every source binding that projects onto it, sorted, so
// the annotation does not depend on the informer order.
func TestDesiredFederatedCRsAnnotatesOwners(t *testing.T) {
	subjects := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	mk := func(names ...string) []*v1alpha1.JenkinsRoleBinding {
		vrbs := make([]*v1alpha1.VarroaRoleBinding, 0, 1+len(names))
		vrbs = append(vrbs, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "b-solo"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "bob"}}},
		})
		for _, n := range names {
			vrbs = append(vrbs, &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: n}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "team-admin", Subjects: subjects}})
		}
		_, bindings, _, _ := DesiredFederatedCRs(vrbs,
			func(name string) (*v1alpha1.VarroaRole, bool) {
				return &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.VarroaRoleSpec{JenkinsPermissions: []string{"Overall/Read"}}}, true
			},
			func(string) (*v1alpha1.JenkinsRole, bool) { return nil, false },
		)
		return bindings
	}
	for _, order := range [][]string{{"b-two", "b-one"}, {"b-one", "b-two"}} {
		bindings := mk(order...)
		if len(bindings) != 2 {
			t.Fatalf("bindings = %#v, want one shared child and one solo child", bindings)
		}
		got := map[string]bool{}
		for _, b := range bindings {
			got[b.Annotations[AnnotationFederatedBindings]] = true
		}
		if !got["b-one,b-two"] || !got["b-solo"] {
			t.Fatalf("order %v annotations = %v, want b-one,b-two and b-solo", order, got)
		}
	}
}

// Each federated JenkinsRole names every VarroaRole that projects onto it
// through at least one projected binding, sorted, so the annotation does not
// depend on the informer order. A VarroaRole whose bindings are all skipped
// is not named.
func TestDesiredFederatedCRsAnnotatesRoleSources(t *testing.T) {
	varroaRoles := map[string]*v1alpha1.VarroaRole{
		"zeta":  {ObjectMeta: metav1.ObjectMeta{Name: "zeta"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "shared"}},
		"alpha": {ObjectMeta: metav1.ObjectMeta{Name: "alpha"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "shared"}},
		"quiet": {ObjectMeta: metav1.ObjectMeta{Name: "quiet"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "shared"}},
	}
	subjects := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b-zeta"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "zeta", Subjects: subjects}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b-alpha"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "alpha", Subjects: subjects}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b-quiet"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "quiet"}},
	}
	run := func(in []*v1alpha1.VarroaRoleBinding) []*v1alpha1.JenkinsRole {
		roles, _, _, _ := DesiredFederatedCRs(in,
			func(name string) (*v1alpha1.VarroaRole, bool) { vr, ok := varroaRoles[name]; return vr, ok },
			func(name string) (*v1alpha1.JenkinsRole, bool) {
				return &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: []string{"Overall/Read"}}}, true
			},
		)
		return roles
	}
	reversed := []*v1alpha1.VarroaRoleBinding{vrbs[2], vrbs[1], vrbs[0]}
	for _, in := range [][]*v1alpha1.VarroaRoleBinding{vrbs, reversed} {
		roles := run(in)
		if len(roles) != 1 {
			t.Fatalf("roles = %#v, want one shared role", roles)
		}
		if got := roles[0].Annotations[AnnotationFederatedRoles]; got != "alpha,zeta" {
			t.Fatalf("role annotation = %q, want alpha,zeta", got)
		}
		if got := roles[0].Labels[LabelFederatedFrom]; got != "alpha" {
			t.Fatalf("role label = %q, want alpha", got)
		}
	}
}
