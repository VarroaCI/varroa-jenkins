package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestTenantAgentBindingName_Golden(t *testing.T) {
	cases := []struct{ ns, name, want string }{
		{"ns-a", "agent-a", "tenantagent-0eb3bb0eec318ce4"},
		{"a-b", "c", "tenantagent-4e84717d75bd540d"},
		{"a", "b-c", "tenantagent-b88f83c840abe581"},
	}
	for _, tc := range cases {
		if got := TenantAgentBindingName(tc.ns, tc.name); got != tc.want {
			t.Errorf("TenantAgentBindingName(%q, %q) = %q, want %q", tc.ns, tc.name, got, tc.want)
		}
	}
}

func TestBroodScheduleBindingName_Golden(t *testing.T) {
	cases := []struct{ ns, name, want string }{
		{"ns-a", "nightly", "broodschedule-de59698b5963b41984ed191b495eeae7fd55f8d8a1eb7876"},
		{"a-b", "c", "broodschedule-4e84717d75bd540d02715af25a29b419f8c59fb0f5166490"},
	}
	for _, tc := range cases {
		if got := BroodScheduleBindingName(tc.ns, tc.name); got != tc.want {
			t.Errorf("BroodScheduleBindingName(%q, %q) = %q, want %q", tc.ns, tc.name, got, tc.want)
		}
	}
}

func TestOperatorBindingNames_ValidForLongNames(t *testing.T) {
	ns := strings.Repeat("a", 63)
	name := strings.Repeat("b", 253)
	for _, got := range []string{TenantAgentBindingName(ns, name), BroodScheduleBindingName(ns, name)} {
		if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 {
			t.Errorf("name %q is not a valid DNS subdomain: %v", got, errs)
		}
		if len(got) > 63 {
			t.Errorf("name %q has %d characters, want at most 63", got, len(got))
		}
	}
}

func ownedTenantAgentBinding() *VarroaRoleBinding {
	return &VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: TenantAgentBindingName("ns-a", "agent-a"),
			Labels: map[string]string{
				LabelTenantAgent:          "true",
				LabelTenantAgentNamespace: "ns-a",
				LabelTenantAgentName:      "agent-a",
			},
		},
	}
}

func ownedBroodScheduleBinding() *VarroaRoleBinding {
	return &VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: BroodScheduleBindingName("ns-a", "nightly"),
			Annotations: map[string]string{
				AnnotationBroodScheduleNamespace: "ns-a",
				AnnotationBroodScheduleName:      "nightly",
			},
		},
	}
}

func TestVarroaRoleBindingOwner(t *testing.T) {
	renamed := ownedTenantAgentBinding()
	renamed.Name = "copy-of-agent"

	renamedBS := ownedBroodScheduleBinding()
	renamedBS.Name = "copy-of-schedule"

	noLabels := &VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: TenantAgentBindingName("ns-a", "agent-a")}}

	falseLabel := ownedTenantAgentBinding()
	falseLabel.Labels[LabelTenantAgent] = "false"

	otherAgent := ownedTenantAgentBinding()
	otherAgent.Labels[LabelTenantAgentName] = "agent-b"

	hashOfAB := &VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: TenantAgentBindingName("a-b", "c"),
			Labels: map[string]string{
				LabelTenantAgent:          "true",
				LabelTenantAgentNamespace: "a",
				LabelTenantAgentName:      "b-c",
			},
		},
	}

	emptyNS := ownedTenantAgentBinding()
	emptyNS.Labels[LabelTenantAgentNamespace] = ""

	bsAsLabels := &VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: BroodScheduleBindingName("ns-a", "nightly"),
			Labels: map[string]string{
				AnnotationBroodScheduleNamespace: "ns-a",
				AnnotationBroodScheduleName:      "nightly",
			},
		},
	}

	taNameWithBSAnnotations := &VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: TenantAgentBindingName("ns-a", "agent-a"),
			Annotations: map[string]string{
				AnnotationBroodScheduleNamespace: "ns-a",
				AnnotationBroodScheduleName:      "agent-a",
			},
		},
	}

	cases := []struct {
		name string
		b    *VarroaRoleBinding
		want BindingOwner
		ok   bool
	}{
		{"owned TenantAgent", ownedTenantAgentBinding(), BindingOwner{BindingOwnerTenantAgent, "ns-a", "agent-a"}, true},
		{"owned BroodSchedule", ownedBroodScheduleBinding(), BindingOwner{BindingOwnerBroodSchedule, "ns-a", "nightly"}, true},
		{"renamed copy with TenantAgent labels", renamed, BindingOwner{}, false},
		{"renamed copy with BroodSchedule annotations", renamedBS, BindingOwner{}, false},
		{"derived name without labels", noLabels, BindingOwner{}, false},
		{"tenant-agent false", falseLabel, BindingOwner{}, false},
		{"labels of another agent", otherAgent, BindingOwner{}, false},
		{"hash of a-b/c with labels of a/b-c", hashOfAB, BindingOwner{}, false},
		{"empty namespace label", emptyNS, BindingOwner{}, false},
		{"BroodSchedule keys written as labels", bsAsLabels, BindingOwner{}, false},
		{"TenantAgent name with BroodSchedule annotations", taNameWithBSAnnotations, BindingOwner{}, false},
		{"nil binding", nil, BindingOwner{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := VarroaRoleBindingOwner(tc.b)
			if ok != tc.ok || got != tc.want {
				t.Errorf("VarroaRoleBindingOwner() = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
