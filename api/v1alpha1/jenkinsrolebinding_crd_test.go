package v1alpha1_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestJenkinsScopeGoValidationMatchesCRD runs one table through the Go
// ValidateJenkinsScope and the CRD admission harness, so the two copies of
// the rules cannot drift. Every entry uses a type and propagate value that
// the enums accept, so only the field rules decide the result.
func TestJenkinsScopeGoValidationMatchesCRD(t *testing.T) {
	crdFile := filepath.Join(crdDir, "varroa.dev_jenkinsrolebindings.yaml")
	cases := []struct {
		name      string
		scope     v1alpha1.JenkinsScope
		wantValid bool
	}{
		{name: "Global", scope: v1alpha1.JenkinsScope{Type: "Global"}, wantValid: true},
		{name: "Folder", scope: v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments"}, wantValid: true},
		{name: "Folder with Subtree", scope: v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments", Propagate: "Subtree"}, wantValid: true},
		{name: "Pattern", scope: v1alpha1.JenkinsScope{Type: "Pattern", Pattern: "payments/.*"}, wantValid: true},
		{name: "Folder without folder", scope: v1alpha1.JenkinsScope{Type: "Folder"}},
		{name: "Folder with leading slash", scope: v1alpha1.JenkinsScope{Type: "Folder", Folder: "/payments"}},
		{name: "Pattern without pattern", scope: v1alpha1.JenkinsScope{Type: "Pattern"}},
		{name: "Global with propagate", scope: v1alpha1.JenkinsScope{Type: "Global", Propagate: "None"}},
		{name: "Global with folder", scope: v1alpha1.JenkinsScope{Type: "Global", Folder: "payments"}},
		{name: "Pattern with folder", scope: v1alpha1.JenkinsScope{Type: "Pattern", Pattern: "x", Folder: "payments"}},
		{name: "Folder with pattern", scope: v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments", Pattern: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]interface{}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			errs := newHarness(t, crdFile).admit(t, jenkinsRoleBindingObject(m), nil)
			crdValid := len(errs) == 0
			goValid := v1alpha1.ValidateJenkinsScope(&tc.scope) == nil
			if crdValid != tc.wantValid {
				t.Errorf("CRD valid = %v, want %v: %v", crdValid, tc.wantValid, errs)
			}
			if goValid != tc.wantValid {
				t.Errorf("Go valid = %v, want %v", goValid, tc.wantValid)
			}
		})
	}
}

// jenkinsRoleBindingObject returns a valid JenkinsRoleBinding with the given
// jenkinsScope merged in. A nil scope omits the field.
func jenkinsRoleBindingObject(scope map[string]interface{}) map[string]interface{} {
	spec := map[string]interface{}{
		"roleRef":  "builder",
		"subjects": []interface{}{map[string]interface{}{"kind": "Group", "name": "devs"}},
	}
	if scope != nil {
		spec["jenkinsScope"] = scope
	}
	return map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "JenkinsRoleBinding",
		"metadata":   map[string]interface{}{"name": "binding-a"},
		"spec":       spec,
	}
}

// TestJenkinsRoleBindingSchemaRejects runs the generated CRD schema over
// jenkinsScope values. The enums on type and propagate must refuse a value
// outside the list, so a stored object can never hold a wrong scope type.
func TestJenkinsRoleBindingSchemaRejects(t *testing.T) {
	crd := loadCRD(t, filepath.Join(crdDir, "varroa.dev_jenkinsrolebindings.yaml"))
	sv, _, err := apiservervalidation.NewSchemaValidator(internalSchema(t, crd))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		scope map[string]interface{} // nil omits jenkinsScope
		want  string                 // substring of the refused field; "" = accepted
	}{
		{name: "no jenkinsScope accepted"},
		{name: "Global accepted", scope: map[string]interface{}{"type": "Global"}},
		{
			name:  "Folder with Subtree accepted",
			scope: map[string]interface{}{"type": "Folder", "folder": "payments", "propagate": "Subtree"},
		},
		{
			name:  "Pattern accepted",
			scope: map[string]interface{}{"type": "Pattern", "pattern": "payments/.*"},
		},
		{name: "lowercase folder type refused", scope: map[string]interface{}{"type": "folder", "folder": "payments"}, want: "jenkinsScope.type"},
		{name: "Cluster type refused", scope: map[string]interface{}{"type": "Cluster"}, want: "jenkinsScope.type"},
		{name: "empty type refused", scope: map[string]interface{}{"type": ""}, want: "jenkinsScope.type"},
		{
			name:  "lowercase propagate refused",
			scope: map[string]interface{}{"type": "Folder", "folder": "payments", "propagate": "subtree"},
			want:  "jenkinsScope.propagate",
		},
		{
			name:  "All propagate refused",
			scope: map[string]interface{}{"type": "Folder", "folder": "payments", "propagate": "All"},
			want:  "jenkinsScope.propagate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := jenkinsRoleBindingObject(tc.scope)
			errs := apiservervalidation.ValidateCustomResource(nil, obj, sv)
			if tc.want == "" {
				if len(errs) > 0 {
					t.Fatalf("expected accept, got %v", errs)
				}
				return
			}
			for _, e := range errs {
				if strings.Contains(e.Field, tc.want) {
					return
				}
			}
			t.Fatalf("expected refusal at a field containing %q, got %v", tc.want, errs)
		})
	}
}
