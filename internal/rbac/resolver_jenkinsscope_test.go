package rbac

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestLowerScope(t *testing.T) {
	cases := []struct {
		name     string
		roleType string
		js       *v1alpha1.JenkinsScope
		wantType string
		wantPat  string
		wantOK   bool
	}{
		{name: "nil is Global", roleType: "Global", js: nil, wantType: "Global", wantOK: true},
		{name: "Global", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Global"}, wantType: "Global", wantOK: true},
		{name: "Pattern", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Pattern", Pattern: "payments/.*"}, wantType: "Item", wantPat: "payments/.*", wantOK: true},
		{name: "Folder None", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments", Propagate: "None"}, wantType: "Item", wantPat: "^payments$", wantOK: true},
		{name: "Folder empty propagate", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments"}, wantType: "Item", wantPat: "^payments$", wantOK: true},
		{name: "Folder Children", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments", Propagate: "Children"}, wantType: "Item", wantPat: "^payments/[^/]*$", wantOK: true},
		{name: "Folder Subtree", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments", Propagate: "Subtree"}, wantType: "Item", wantPat: "^payments($|/.*)$", wantOK: true},
		{name: "Agent role with Folder", roleType: "Agent", js: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments"}, wantType: "Agent", wantOK: true},
		{name: "lowercase folder refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "folder", Folder: "payments"}, wantOK: false},
		{name: "Cluster refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Cluster"}, wantOK: false},
		{name: "empty type refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: ""}, wantOK: false},
		{name: "Agent role with Cluster refused", roleType: "Agent", js: &v1alpha1.JenkinsScope{Type: "Cluster"}, wantOK: false},
		{name: "Pattern with no pattern refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Pattern"}, wantOK: false},
		{name: "Folder with no folder refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Folder"}, wantOK: false},
		{name: "Folder with leading slash refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "/payments"}, wantOK: false},
		{name: "Global with propagate refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Global", Propagate: "Subtree"}, wantOK: false},
		{name: "Folder with pattern refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments", Pattern: "x"}, wantOK: false},
		{name: "Agent role with Pattern and no pattern refused", roleType: "Agent", js: &v1alpha1.JenkinsScope{Type: "Pattern"}, wantOK: false},
		{name: "Pattern that Java refuses refused", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Pattern", Pattern: "(payments"}, wantOK: false},
		{name: "Agent role with Pattern that Java refuses refused", roleType: "Agent", js: &v1alpha1.JenkinsScope{Type: "Pattern", Pattern: "(payments"}, wantOK: false},
		{name: "Pattern with Java lookahead", roleType: "Global", js: &v1alpha1.JenkinsScope{Type: "Pattern", Pattern: "(?=payments/).*"}, wantType: "Item", wantPat: "(?=payments/).*", wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotPat, gotOK := lowerScope(tc.roleType, tc.js)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if gotType != tc.wantType || gotPat != tc.wantPat {
				t.Fatalf("lowerScope() = (%q, %q), want (%q, %q)", gotType, gotPat, tc.wantType, tc.wantPat)
			}
		})
	}
}

func TestJenkinsRoleAssignments_UnknownJenkinsScopeTypeGrantsNothing(t *testing.T) {
	jenkinsRoles := []*v1alpha1.JenkinsRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "builder"},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    "Item",
				Permissions: []string{"hudson.model.Item.Build"},
			},
		},
	}
	bindings := []*v1alpha1.JenkinsRoleBinding{
		jenkinsBindingFor("lower", "folder", "payments"),
		jenkinsBindingFor("cluster", "Cluster", ""),
		jenkinsBindingFor("empty", "", ""),
		jenkinsBindingFor("valid", "Folder", "payments"),
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"}},
	}
	r := testResolverWithJenkins(nil, nil, jenkinsRoles, bindings, controllers, false)

	assignments, err := r.JenkinsRoleAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var validFound bool
	for _, a := range assignments {
		for _, s := range a.Subjects {
			switch s.Name {
			case "lower", "cluster", "empty":
				t.Errorf("binding with a wrong scope type granted %q: %+v", s.Name, a)
			case "valid":
				if a.RoleType == "Item" && a.Pattern == "^payments$" {
					validFound = true
				}
			}
		}
	}
	if !validFound {
		t.Fatalf("valid Folder binding missing an Item assignment with pattern ^payments$: %+v", assignments)
	}
}

// TestJenkinsRoleAssignments_InvalidJenkinsScopeShapeGrantsNothing covers a
// stored scope whose shape the CRD refuses. A Pattern scope with no pattern
// must not reach role-strategy, because role-strategy reads it as ".*" and the
// grant would then cover every item.
func TestJenkinsRoleAssignments_InvalidJenkinsScopeShapeGrantsNothing(t *testing.T) {
	jenkinsRoles := []*v1alpha1.JenkinsRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "builder"},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    "Item",
				Permissions: []string{"hudson.model.Item.Build"},
			},
		},
	}
	bindings := []*v1alpha1.JenkinsRoleBinding{
		jenkinsBindingFor("nopattern", "Pattern", ""),
		jenkinsBindingFor("nofolder", "Folder", ""),
		jenkinsBindingFor("slash", "Folder", "/payments"),
		jenkinsBindingFor("valid", "Folder", "payments"),
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"}},
	}
	r := testResolverWithJenkins(nil, nil, jenkinsRoles, bindings, controllers, false)

	assignments, err := r.JenkinsRoleAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var validFound bool
	for _, a := range assignments {
		if a.RoleType == "Item" && a.Pattern == "" {
			t.Errorf("item assignment with an empty pattern would cover every item: %+v", a)
		}
		for _, s := range a.Subjects {
			switch s.Name {
			case "nopattern", "nofolder", "slash":
				t.Errorf("binding with an invalid scope shape granted %q: %+v", s.Name, a)
			case "valid":
				if a.RoleType == "Item" && a.Pattern == "^payments$" {
					validFound = true
				}
			}
		}
	}
	if !validFound {
		t.Fatalf("valid Folder binding missing an Item assignment with pattern ^payments$: %+v", assignments)
	}
}

// jenkinsBindingFor returns a JenkinsRoleBinding that binds one group to the
// builder role under the given scope type.
func jenkinsBindingFor(group, scopeType, folder string) *v1alpha1.JenkinsRoleBinding {
	return &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "jrb-" + group},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			RoleRef:  "builder",
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: group}},
			JenkinsScope: &v1alpha1.JenkinsScope{
				Type:   scopeType,
				Folder: folder,
			},
		},
	}
}

// TestGenerate_UncompilablePatternNeverRendered checks that a binding whose
// Pattern scope Java refuses never reaches the rendered role-strategy YAML.
// The valid binding in the same controller still renders.
func TestGenerate_UncompilablePatternNeverRendered(t *testing.T) {
	jenkinsRoles := []*v1alpha1.JenkinsRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "builder"},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    "Item",
				Permissions: []string{"hudson.model.Item.Build"},
			},
		},
	}
	bad := jenkinsBindingFor("bad", "Pattern", "")
	bad.Spec.JenkinsScope.Pattern = "(payments"
	good := jenkinsBindingFor("good", "Pattern", "")
	good.Spec.JenkinsScope.Pattern = "payments/.*"
	bindings := []*v1alpha1.JenkinsRoleBinding{bad, good}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"}},
	}
	r := testResolverWithJenkins(nil, nil, jenkinsRoles, bindings, controllers, false)

	out, err := NewGenerator(r).Generate(controllers[0])
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Contains(out, "(payments") {
		t.Errorf("rendered YAML holds the refused pattern:\n%s", out)
	}
	if strings.Contains(out, "bad") {
		t.Errorf("rendered YAML grants the refused binding:\n%s", out)
	}
	if !strings.Contains(out, "payments/.*") {
		t.Errorf("rendered YAML lacks the valid pattern:\n%s", out)
	}
}
