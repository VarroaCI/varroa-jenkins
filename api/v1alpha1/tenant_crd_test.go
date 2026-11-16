package v1alpha1_test

import (
	"path/filepath"
	"strings"
	"testing"

	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
)

// TestTenantSchemaRefusesEmptyNamespaces runs the generated Tenant schema over
// a Tenant with one and with no namespace. The Tenant reconciler copies the
// namespaces into the scope of the tenant binding, so an empty list would
// store a binding with no scope part.
func TestTenantSchemaRefusesEmptyNamespaces(t *testing.T) {
	crd := loadCRD(t, filepath.Join(crdDir, "varroa.dev_tenants.yaml"))
	sv, _, err := apiservervalidation.NewSchemaValidator(internalSchema(t, crd))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		namespaces []interface{}
		want       string // substring of the refused field; "" = accepted
	}{
		{name: "one namespace accepted", namespaces: []interface{}{"tenant-a"}},
		{name: "empty namespaces list refused", namespaces: []interface{}{}, want: "spec.namespaces"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := map[string]interface{}{
				"apiVersion": "varroa.dev/v1alpha1",
				"kind":       "Tenant",
				"metadata":   map[string]interface{}{"name": "tenant-a"},
				"spec":       map[string]interface{}{"namespaces": tc.namespaces},
			}
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
