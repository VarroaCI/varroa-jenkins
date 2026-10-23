package v1alpha1_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
)

// enumMarker matches the generated enum marker on the UnattendedWrites field.
var enumMarker = regexp.MustCompile(`(?m)^\s*// \+kubebuilder:validation:items:Enum=(.*)$`)

// TestUnattendedEnumMatchesPolicy keeps the unattendedWrites enum in step
// with the policy package. The marker in the Go source and the CRD schema
// must both equal policy.UnattendedEligible, and neither may name a tool of
// policy.NeverUnattended.
func TestUnattendedEnumMatchesPolicy(t *testing.T) {
	src, err := os.ReadFile("tenantagent_types.go")
	if err != nil {
		t.Fatal(err)
	}
	m := enumMarker.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("tenantagent_types.go has no items:Enum marker on UnattendedWrites")
	}
	marker := strings.Split(m[1], ";")
	if !reflect.DeepEqual(marker, policy.UnattendedEligible) {
		t.Errorf("marker enum = %v, want policy.UnattendedEligible %v (run go generate ./api/v1alpha1/...)", marker, policy.UnattendedEligible)
	}

	crd := loadCRD(t, filepath.Join(crdDir, "varroa.dev_tenantagents.yaml"))
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	field, ok := spec.Properties["unattendedWrites"]
	if !ok || field.Items == nil {
		t.Fatal("CRD has no spec.unattendedWrites items schema")
	}
	crdEnum := make([]string, 0, len(field.Items.Schema.Enum))
	for _, v := range field.Items.Schema.Enum {
		var s string
		if err := json.Unmarshal(v.Raw, &s); err != nil {
			t.Fatal(err)
		}
		crdEnum = append(crdEnum, s)
	}
	if !reflect.DeepEqual(crdEnum, policy.UnattendedEligible) {
		t.Errorf("CRD enum = %v, want policy.UnattendedEligible %v (run make check-crds)", crdEnum, policy.UnattendedEligible)
	}
	for _, name := range policy.NeverUnattended {
		if slices.Contains(crdEnum, name) || slices.Contains(marker, name) {
			t.Errorf("unattendedWrites names %q, which policy.NeverUnattended forbids", name)
		}
	}
}
