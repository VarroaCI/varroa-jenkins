package controller

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestBuildAssistantEgressPolicyOneRulePerProvider(t *testing.T) {
	a := keyProvider("alpha")
	a.CIDRs = []string{"192.0.2.10/32", "192.0.2.0/24"}
	b := keyProvider("beta")
	b.CIDRs = []string{"198.51.100.7/32"}
	np, missing := buildAssistantEgressPolicy(testNamespace, []v1alpha1.AssistantProvider{b, a})
	if np == nil || len(missing) != 0 {
		t.Fatalf("policy %v, missing %v", np, missing)
	}
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("egress rules %d, want one per provider", len(np.Spec.Egress))
	}
	if len(np.Spec.Egress[0].To) != 2 || np.Spec.Egress[0].To[0].IPBlock.CIDR != "192.0.2.0/24" {
		t.Fatalf("first rule holds %+v, want alpha's CIDRs sorted", np.Spec.Egress[0].To)
	}
	if np.Name != assistantEgressPolicyName || np.Namespace != testNamespace {
		t.Fatalf("policy %s/%s, want %s/%s", np.Namespace, np.Name, testNamespace, assistantEgressPolicyName)
	}
}

func TestBuildAssistantEgressPolicySkipsProviderWithoutCIDRs(t *testing.T) {
	bare := keyProvider("bare")
	bare.CIDRs = nil
	np, missing := buildAssistantEgressPolicy(testNamespace, []v1alpha1.AssistantProvider{keyProvider("smoke"), bare})
	if np == nil || len(np.Spec.Egress) != 1 {
		t.Fatalf("want one rule for the provider with CIDRs, got %v", np)
	}
	if len(missing) != 1 || missing[0] != "bare" {
		t.Fatalf("missing %v, want [bare]", missing)
	}
	if np, missing := buildAssistantEgressPolicy(testNamespace, []v1alpha1.AssistantProvider{bare}); np != nil || len(missing) != 1 {
		t.Fatalf("no rule must give no policy, got %v and %v", np, missing)
	}
}

func TestBuildAssistantEgressPolicyUsesEffectivePort(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		port    int32
		want    int32
	}{
		{"https default", "https://model.test/v1", 0, 443},
		{"http default", "http://model.test/v1", 0, 80},
		{"explicit port in URL", "https://model.test:8443/v1", 0, 8443},
		{"explicit port wins", "https://model.test:8443/v1", 11434, 11434},
	}
	for _, tc := range cases {
		p := keyProvider("p")
		p.BaseURL = tc.baseURL
		p.Port = tc.port
		np, _ := buildAssistantEgressPolicy(testNamespace, []v1alpha1.AssistantProvider{p})
		if np == nil {
			t.Fatalf("%s: no policy", tc.name)
		}
		got := np.Spec.Egress[0].Ports[0].Port.IntVal
		if got != tc.want {
			t.Fatalf("%s: port %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestBuildAssistantEgressPolicySelectsBFFAndAgent(t *testing.T) {
	np, _ := buildAssistantEgressPolicy(testNamespace, []v1alpha1.AssistantProvider{keyProvider("smoke")})
	if np == nil {
		t.Fatal("no policy")
	}
	reqs := np.Spec.PodSelector.MatchExpressions
	if len(reqs) != 1 || reqs[0].Key != "app.kubernetes.io/component" || reqs[0].Operator != "In" {
		t.Fatalf("pod selector %+v", reqs)
	}
	got := map[string]bool{}
	for _, v := range reqs[0].Values {
		got[v] = true
	}
	if len(got) != 2 || !got["varroa-bff"] || !got["varroa-agent"] {
		t.Fatalf("selector values %v, want varroa-bff and varroa-agent only", reqs[0].Values)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("policy types %v, want Egress only", np.Spec.PolicyTypes)
	}
	if np.Spec.Ingress != nil {
		t.Fatalf("policy restricts ingress: %+v", np.Spec.Ingress)
	}
}

func TestBuildAssistantEgressPolicySkipsInvalidCIDR(t *testing.T) {
	bad := keyProvider("bad")
	bad.CIDRs = []string{"not-a-cidr", "192.0.2.300/24"}
	np, missing := buildAssistantEgressPolicy(testNamespace, []v1alpha1.AssistantProvider{bad})
	if np != nil {
		t.Fatalf("a provider with only invalid CIDRs opened a rule: %+v", np.Spec.Egress)
	}
	if len(missing) != 1 || missing[0] != "bad" {
		t.Fatalf("missing %v, want [bad]", missing)
	}
	mixed := keyProvider("mixed")
	mixed.CIDRs = []string{"not-a-cidr", "192.0.2.0/24"}
	np, _ = buildAssistantEgressPolicy(testNamespace, []v1alpha1.AssistantProvider{mixed})
	if np == nil || len(np.Spec.Egress[0].To) != 1 {
		t.Fatalf("the valid CIDR of a mixed list was dropped")
	}
}
