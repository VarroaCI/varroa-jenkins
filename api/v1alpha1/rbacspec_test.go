package v1alpha1_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestRBACSpecBindingName_Golden(t *testing.T) {
	cases := []struct{ ns, name, role, want string }{
		{"ns", "test", "developer", "ctrl-cf4a83dbe7472025-developer"},
		{"a-b", "c", "admin", "ctrl-4e84717d75bd540d-admin"},
		{"a", "b-c", "admin", "ctrl-b88f83c840abe581-admin"},
		{"prod-a", "jenkins-1", "admin", "ctrl-fbff6b43b023e716-admin"},
	}
	for _, tc := range cases {
		if got := v1alpha1.RBACSpecBindingName(tc.ns, tc.name, tc.role); got != tc.want {
			t.Errorf("RBACSpecBindingName(%q, %q, %q) = %q, want %q", tc.ns, tc.name, tc.role, got, tc.want)
		}
	}
}

func TestRBACSpecBindingName_ValidForLongNames(t *testing.T) {
	ns := strings.Repeat("a", 63)
	name := strings.Repeat("b", 253)
	for _, role := range v1alpha1.RBACSpecRoles() {
		n := v1alpha1.RBACSpecBindingName(ns, name, role)
		if errs := validation.IsDNS1123Subdomain(n); len(errs) != 0 {
			t.Errorf("RBACSpecBindingName for role %q is not a valid name: %v", role, errs)
		}
		if len(n) > 31 {
			t.Errorf("RBACSpecBindingName for role %q has %d characters, want at most 31", role, len(n))
		}
	}
}

// binding returns a JenkinsRoleBinding named name that carries the three
// operator ownership labels for ns/ctrl.
func binding(name, ns, ctrl string) *v1alpha1.JenkinsRoleBinding {
	return &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("uid-" + name),
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:           v1alpha1.ManagedByOperator,
				v1alpha1.LabelControllerNamespace: ns,
				v1alpha1.LabelControllerName:      ctrl,
			},
		},
	}
}

func TestRBACSpecBindingOwner(t *testing.T) {
	derived := v1alpha1.RBACSpecBindingName("prod-a", "jenkins-1", "admin")
	noLabels := binding(derived, "prod-a", "jenkins-1")
	noLabels.Labels = nil
	local := binding(derived, "prod-a", "jenkins-1")
	local.Labels[v1alpha1.LabelManagedBy] = "local"
	noNS := binding(derived, "", "jenkins-1")
	cases := []struct {
		name   string
		b      *v1alpha1.JenkinsRoleBinding
		ns, n  string
		wantOK bool
	}{
		{"owned", binding(derived, "prod-a", "jenkins-1"), "prod-a", "jenkins-1", true},
		{"renamed copy keeps labels", binding("payments-admins", "prod-a", "jenkins-1"), "", "", false},
		{"derived name without labels", noLabels, "", "", false},
		{"suffix not an rbacSpec role", binding(v1alpha1.RBACSpecBindingName("prod-a", "jenkins-1", "superuser"), "prod-a", "jenkins-1"), "", "", false},
		{"managed-by is not the operator", local, "", "", false},
		{"labels of another controller", binding(derived, "prod-a", "jenkins-2"), "", "", false},
		{"hash of a-b/c with labels of a/b-c", binding(v1alpha1.RBACSpecBindingName("a-b", "c", "admin"), "a", "b-c"), "", "", false},
		{"dash-joined name with labels", binding("ctrl-prod-a-jenkins-1-admin", "prod-a", "jenkins-1"), "", "", false},
		{"empty namespace label", noNS, "", "", false},
		{"nil binding", nil, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, n, ok := v1alpha1.RBACSpecBindingOwner(tc.b)
			if ok != tc.wantOK || ns != tc.ns || n != tc.n {
				t.Errorf("RBACSpecBindingOwner() = (%q, %q, %v), want (%q, %q, %v)", ns, n, ok, tc.ns, tc.n, tc.wantOK)
			}
		})
	}
}

// TestRBACSpecRoles_MatchControllerCRDEnum keeps RBACSpecRoles equal to the
// enum on RBACGroupBinding.Role in the Controller CRD.
func TestRBACSpecRoles_MatchControllerCRDEnum(t *testing.T) {
	crd := loadCRD(t, filepath.Join(crdDir, "varroa.dev_controllers.yaml"))
	role := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].
		Properties["rbacSpec"].Properties["groups"].Items.Schema.Properties["role"]
	enum := make([]string, 0, len(role.Enum))
	for _, raw := range role.Enum {
		var s string
		if err := json.Unmarshal(raw.Raw, &s); err != nil {
			t.Fatalf("decode enum value %s: %v", raw.Raw, err)
		}
		enum = append(enum, s)
	}
	got := slices.Clone(v1alpha1.RBACSpecRoles())
	slices.Sort(got)
	slices.Sort(enum)
	if !slices.Equal(got, enum) {
		t.Errorf("RBACSpecRoles() = %v, CRD enum = %v", got, enum)
	}
}
