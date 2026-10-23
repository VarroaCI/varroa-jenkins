package v1alpha1_test

import (
	"path/filepath"
	"strings"
	"testing"

	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
)

// tenantAgentObject returns a valid TenantAgent with spec overrides merged in.
func tenantAgentObject(spec map[string]interface{}) map[string]interface{} {
	base := map[string]interface{}{
		"tenantRef": map[string]interface{}{"name": "team-a"},
		"triggers":  []interface{}{map[string]interface{}{"type": "run.failed"}},
	}
	for k, v := range spec {
		base[k] = v
	}
	return map[string]interface{}{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "TenantAgent",
		"metadata":   map[string]interface{}{"name": "agent-a"},
		"spec":       base,
	}
}

// TestTenantAgentSchemaRejects runs the OpenAPI schema of the generated CRD
// over objects that the schema alone must refuse. The enum on
// unattendedWrites comes from the policy package through the marker.
func TestTenantAgentSchemaRejects(t *testing.T) {
	crd := loadCRD(t, filepath.Join(crdDir, "varroa.dev_tenantagents.yaml"))
	sv, _, err := apiservervalidation.NewSchemaValidator(internalSchema(t, crd))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		spec map[string]interface{}
		want string // substring of the refused field; "" = accepted
	}{
		{
			name: "valid object accepted",
			spec: map[string]interface{}{
				"unattendedWrites": []interface{}{"run_pipeline", "restart_run"},
				"dataSharing":      "metadata",
			},
		},
		{
			name: "release_fleet_lock refused as unattended",
			spec: map[string]interface{}{"unattendedWrites": []interface{}{"release_fleet_lock"}},
			want: "unattendedWrites",
		},
		{
			name: "forge outside the enum refused",
			spec: map[string]interface{}{"outputs": map[string]interface{}{
				"issues": map[string]interface{}{"repo": "https://git.example.org/o/r", "forge": "jira"},
			}},
			want: "forge",
		},
		{
			name: "dataSharing all refused",
			spec: map[string]interface{}{"dataSharing": "all"},
			want: "dataSharing",
		},
		{
			name: "unknown trigger type refused",
			spec: map[string]interface{}{"triggers": []interface{}{map[string]interface{}{"type": "run.bogus"}}},
			want: "type",
		},
		{
			name: "zero session budget refused",
			spec: map[string]interface{}{"budgets": map[string]interface{}{"session": 0}},
			want: "session",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := apiservervalidation.ValidateCustomResource(nil, tenantAgentObject(c.spec), sv)
			if c.want == "" {
				if len(errs) > 0 {
					t.Fatalf("want accept, got %v", errs)
				}
				return
			}
			for _, e := range errs {
				if strings.Contains(e.Field, c.want) {
					return
				}
			}
			t.Fatalf("want refusal at a field containing %q, got %v", c.want, errs)
		})
	}
}
