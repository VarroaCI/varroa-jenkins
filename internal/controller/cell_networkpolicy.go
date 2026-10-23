package controller

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// defaultCellGatewayGRPCPort is the gateway's built-in gRPC listen port,
// used when the operator has not learned a different one from its own
// gateway endpoint (SetVarroaEndpoint). It matches the gateway's own
// "-grpc-port" flag default.
const defaultCellGatewayGRPCPort int32 = 9090

// jenkinsAPIKeyVerifyPort is the port the Jenkins JVM calls on the gateway
// to verify an API key (design D2). It shares the pod network with the
// mite container, so the cell NetworkPolicy admits both ports on the same
// gateway peer.
const jenkinsAPIKeyVerifyPort int32 = 9092

// updateCenterPort is the in-cluster update center's HTTP listen port,
// the one the chart's varroa-updatecenter Service exposes.
const updateCenterPort int32 = 8080

// CellPolicySpec carries everything buildCellNetworkPolicy needs to render
// one cell's NetworkPolicy (design D2). The caller resolves every field
// from live state before calling.
type CellPolicySpec struct {
	// ControllerName is the cell's controllerPrefix. It names both the
	// NetworkPolicy object ("<ControllerName>-cell") and the pod label the
	// StatefulSet already sets ("app": ControllerName).
	ControllerName string
	// Namespace is the cell's own namespace.
	Namespace string
	// ReleaseNamespace is the namespace the BFF, operator, and gateway run
	// in.
	ReleaseNamespace string
	// GatewayGRPCPort is the gateway's mite gRPC listen port.
	GatewayGRPCPort int32
	// AgentNamespaces lists the agent namespace of every tenant currently
	// placed on this cell.
	AgentNamespaces []string
	// APIServerEgress names the apiserver egress CIDRs and ports, mirroring
	// chart value networkPolicy.apiServerEgress.
	APIServerEgress APIServerEgress
	// RunnerPeers is one resolved network destination per delivered ssh
	// Runner of a placed tenant (design D11), from the same
	// runners.BuildContribution read that produced the cell's node list.
	// One egress rule renders per entry.
	RunnerPeers []runners.RunnerEgressPeer
	// LibraryPeers is one resolved address per pool library remote,
	// rendered as a separate rule set from RunnerPeers.
	LibraryPeers []runners.RunnerEgressPeer
	// ExtraIngress and ExtraEgress render verbatim after the fixed rules,
	// mirroring chart value networkPolicy.cellIsolation.extraIngress and
	// networkPolicy.cellIsolation.extraEgress. The operator never writes a
	// runner rule into ExtraEgress: RunnerPeers is a separate field, and
	// the two render as separate egress entries.
	ExtraIngress []networkingv1.NetworkPolicyIngressRule
	ExtraEgress  []networkingv1.NetworkPolicyEgressRule
}

// buildCellNetworkPolicy renders one cell's NetworkPolicy exactly as design
// D2 lists it. AgentNamespaces is sorted before use so the result is
// deterministic regardless of the caller's tenant order.
func buildCellNetworkPolicy(spec CellPolicySpec) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP

	agentNamespaces := append([]string(nil), spec.AgentNamespaces...)
	sort.Strings(agentNamespaces)

	ingress := []networkingv1.NetworkPolicyIngressRule{
		{
			From: []networkingv1.NetworkPolicyPeer{
				componentPeer("varroa-bff", spec.ReleaseNamespace),
				componentPeer("varroa-operator", spec.ReleaseNamespace),
			},
			Ports: []networkingv1.NetworkPolicyPort{
				portRule(&tcp, 8080),
			},
		},
	}
	if len(agentNamespaces) > 0 {
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
			From: agentNamespacePeers(agentNamespaces),
			Ports: []networkingv1.NetworkPolicyPort{
				portRule(&tcp, 8080),
				portRule(&tcp, 50000),
			},
		})
	}
	ingress = append(ingress, spec.ExtraIngress...)

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: namespaceNameSelector("kube-system"),
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
				},
			},
			Ports: []networkingv1.NetworkPolicyPort{
				portRule(&udp, 53),
				portRule(&tcp, 53),
			},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{
				componentPeer("varroa-gateway", spec.ReleaseNamespace),
			},
			Ports: []networkingv1.NetworkPolicyPort{
				portRule(&tcp, spec.GatewayGRPCPort),
				portRule(&tcp, jenkinsAPIKeyVerifyPort),
			},
		},
		// The in-cluster update center (ruling (ii)): the operator itself
		// points the cell's plugins-init at that service, so a cell can
		// never boot without this peer while updateCenter.enabled is on.
		// The rule is harmless when the component is absent.
		{
			To: []networkingv1.NetworkPolicyPeer{
				componentPeer("varroa-updatecenter", spec.ReleaseNamespace),
			},
			Ports: []networkingv1.NetworkPolicyPort{
				portRule(&tcp, updateCenterPort),
			},
		},
		{
			To:    apiServerEgressPeers(spec.APIServerEgress.CIDRs),
			Ports: apiServerEgressPorts(spec.APIServerEgress.Ports),
		},
	}
	if len(agentNamespaces) > 0 {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: agentNamespacePeers(agentNamespaces),
			Ports: []networkingv1.NetworkPolicyPort{
				portRule(&tcp, 8080),
				portRule(&tcp, 50000),
			},
		})
	}
	egress = append(egress, addressEgressRules(spec.RunnerPeers)...)
	egress = append(egress, addressEgressRules(spec.LibraryPeers)...)
	egress = append(egress, spec.ExtraEgress...)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.ControllerName + "-cell",
			Namespace: spec.Namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": spec.ControllerName}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

// componentPeer matches a pod carrying app.kubernetes.io/component=name in
// the given namespace.
func componentPeer(component, namespace string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": component}},
		NamespaceSelector: namespaceNameSelector(namespace),
	}
}

// namespaceNameSelector matches a namespace by its immutable
// kubernetes.io/metadata.name label.
func namespaceNameSelector(namespace string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}}
}

// agentNamespacePeers builds one peer per agent namespace, matching that
// namespace only (no pod selector): every pod in a placed tenant's agent
// namespace may reach the cell, and the cell may reach any of them.
func agentNamespacePeers(namespaces []string) []networkingv1.NetworkPolicyPeer {
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(namespaces))
	for _, ns := range namespaces {
		peers = append(peers, networkingv1.NetworkPolicyPeer{NamespaceSelector: namespaceNameSelector(ns)})
	}
	return peers
}

// apiServerEgressPeers builds one ipBlock peer per CIDR. The chart guard
// refuses an empty list, so the 0.0.0.0/0 fallback covers only an unset or
// unparsable env var.
func apiServerEgressPeers(cidrs []string) []networkingv1.NetworkPolicyPeer {
	if len(cidrs) == 0 {
		cidrs = []string{"0.0.0.0/0"}
	}
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(cidrs))
	for _, cidr := range cidrs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	return peers
}

// apiServerEgressPorts builds one TCP port entry per configured apiserver
// egress port. The chart guard refuses an empty ports list, because Kubernetes
// reads an empty list as every port.
func apiServerEgressPorts(ports []int32) []networkingv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	rules := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, p := range ports {
		rules = append(rules, portRule(&tcp, p))
	}
	return rules
}

// portRule builds one NetworkPolicyPort for a single numbered port.
func portRule(protocol *corev1.Protocol, port int32) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt(int(port))
	return networkingv1.NetworkPolicyPort{Protocol: protocol, Port: &p}
}

// addressEgressRules builds one egress rule per resolved peer, sorted by
// address then port so the result is deterministic regardless of the
// caller's read order. A peer absent from the list opens no rule: this is
// how a deleted or moved runner's rule disappears on the next assembly.
// Runner and pool library peers pass through this function. Its rules are
// a separate rule set from networkPolicy.cellIsolation.extraEgress: the two
// never merge.
func addressEgressRules(peers []runners.RunnerEgressPeer) []networkingv1.NetworkPolicyEgressRule {
	sorted := append([]runners.RunnerEgressPeer(nil), peers...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].IP != sorted[j].IP {
			return sorted[i].IP < sorted[j].IP
		}
		return sorted[i].Port < sorted[j].Port
	})

	tcp := corev1.ProtocolTCP
	rules := make([]networkingv1.NetworkPolicyEgressRule, 0, len(sorted))
	for _, peer := range sorted {
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: singleAddressCIDR(peer.IP)}},
			},
			Ports: []networkingv1.NetworkPolicyPort{portRule(&tcp, int32(peer.Port))},
		})
	}
	return rules
}

// singleAddressCIDR renders one resolved address as the single-address
// CIDR a NetworkPolicy ipBlock peer requires: /32 for an IPv4 address, /128
// for an IPv6 address. The caller always passes an address
// runners.BuildContribution already parsed or resolved, never a raw
// hostname.
func singleAddressCIDR(ip string) string {
	if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
		return fmt.Sprintf("%s/128", ip)
	}
	return fmt.Sprintf("%s/32", ip)
}

// gatewayGRPCPort returns the port component of the operator's own gateway
// endpoint (SetVarroaEndpoint), falling back to defaultCellGatewayGRPCPort
// when unset or unparsable.
func (r *Reconciler) gatewayGRPCPort() int32 {
	if r.varroaEndpoint != "" {
		if _, portStr, err := net.SplitHostPort(r.varroaEndpoint); err == nil {
			if p, err := strconv.Atoi(portStr); err == nil {
				return int32(p)
			}
		}
	}
	return defaultCellGatewayGRPCPort
}

// cellTenantAgentNamespaces resolves a cell's placed tenants
// (cr.Status.Cell.Tenants) to their spec.pipelines.agentNamespace,
// deduplicated and sorted. A tenant whose Tenant no longer exists, or that
// carries no pipelines tenancy, is skipped: the admitted namespace set
// must never include one a tenant does not actually use.
func (r *Reconciler) cellTenantAgentNamespaces(ctx context.Context, cr *v1alpha1.Controller) []string {
	if cr.Status.Cell == nil {
		return nil
	}
	seen := make(map[string]bool)
	var namespaces []string
	for _, tenant := range cr.Status.Cell.Tenants {
		tenant, err := crdstore.Get[v1alpha1.Tenant](ctx, r.store, tenant, "")
		if err != nil || tenant == nil || tenant.Spec.Pipelines == nil {
			continue
		}
		ns := tenant.Spec.Pipelines.AgentNamespace
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return namespaces
}

// reconcileCellNetworkPolicy creates, updates, or deletes a cell's
// NetworkPolicy (design D2). A non-cell Controller never gets one. Rebuilt
// on every call so a tenant placed or removed between ticks converges
// without a re-provision. Non-fatal: the caller logs a failure and
// continues, matching reconcileService and reconcileIngress in the same
// tick block.
//
// contrib is the caller's own single runners.BuildContribution read for
// this tick, the same value the caller passes into buildDesiredStateCommand
// (design D11). Reading it again here would let a Runner or Secret change
// between the two reads render a NetworkPolicy and a pushed node list that
// describe different runner sets.
func (r *Reconciler) reconcileCellNetworkPolicy(ctx context.Context, cr *v1alpha1.Controller, contrib runners.RunnerContribution) error {
	if !v1alpha1.IsCell(cr) {
		return nil
	}
	name := controllerPrefix(cr) + "-cell"
	if !r.cellsConfig.NetworkPolicyEnabled || !r.cellsConfig.CellIsolation.Enabled {
		return r.client.DeleteCellNetworkPolicy(ctx, name, cr.Namespace)
	}
	libs, err := r.cellPoolLibraries(ctx, cr)
	if err != nil {
		return fmt.Errorf("read cell pool libraries: %w", err)
	}
	spec := CellPolicySpec{
		ControllerName:   controllerPrefix(cr),
		Namespace:        cr.Namespace,
		ReleaseNamespace: r.getOperatorNamespace(),
		GatewayGRPCPort:  r.gatewayGRPCPort(),
		AgentNamespaces:  r.cellTenantAgentNamespaces(ctx, cr),
		APIServerEgress:  r.cellsConfig.APIServerEgress,
		RunnerPeers:      contrib.EgressPeers,
		LibraryPeers:     poolLibraryEgressPeers(ctx, libs),
		ExtraIngress:     r.cellsConfig.CellIsolation.ExtraIngress,
		ExtraEgress:      r.cellsConfig.CellIsolation.ExtraEgress,
	}
	return r.client.CreateCellNetworkPolicy(ctx, spec)
}
