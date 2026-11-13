package controller

import (
	"net"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// assistantEgressPolicyName names the NetworkPolicy that opens the assistant's
// model-provider addresses. It lives in the release namespace.
const assistantEgressPolicyName = "varroa-assistant-egress"

// assistantEgressComponents are the pods that call a model provider: the BFF
// and the Tenant agent runner.
var assistantEgressComponents = []string{"varroa-bff", "varroa-agent"}

// buildAssistantEgressPolicy renders the egress policy of the assistant. It
// holds one rule per provider with at least one valid CIDR, on the provider's
// effective port. The policy selects only the BFF and the agent runner, and it
// restricts egress only. It returns nil when no rule remains. The second
// result names each provider that has no valid CIDR.
func buildAssistantEgressPolicy(namespace string, providers []v1alpha1.AssistantProvider) (*networkingv1.NetworkPolicy, []string) {
	ordered := append([]v1alpha1.AssistantProvider(nil), providers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	tcp := corev1.ProtocolTCP
	var rules []networkingv1.NetworkPolicyEgressRule
	var missing []string
	for _, p := range ordered {
		var peers []networkingv1.NetworkPolicyPeer
		cidrs := append([]string(nil), p.CIDRs...)
		sort.Strings(cidrs)
		for _, cidr := range cidrs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				continue
			}
			peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
		}
		if len(peers) == 0 {
			missing = append(missing, p.Name)
			continue
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To:    peers,
			Ports: []networkingv1.NetworkPolicyPort{portRule(&tcp, p.EffectivePort())},
		})
	}
	if len(rules) == 0 {
		return nil, missing
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      assistantEgressPolicyName,
			Namespace: namespace,
			Labels:    assistantKeyLabels(),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "app.kubernetes.io/component",
					Operator: metav1.LabelSelectorOpIn,
					Values:   append([]string(nil), assistantEgressComponents...),
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      rules,
		},
	}
	return np, missing
}
