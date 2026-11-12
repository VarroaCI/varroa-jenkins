package v1alpha1_test

import (
	"path/filepath"
	"strings"
	"testing"

	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
)

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
