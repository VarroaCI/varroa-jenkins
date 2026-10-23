package controller

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

var updateCellNetworkPolicyGolden = flag.Bool("update", false, "regenerate the cell NetworkPolicy golden fixture")

const cellNetworkPolicyGoldenPath = "testdata/cell-networkpolicy.golden.yaml"

// TestBuildCellNetworkPolicyGolden asserts design D2's exact shape: the
// podSelector, the fixed ingress/egress rules in order, agent-namespace
// peers sorted regardless of input order, and the extra rules appended
// verbatim. Run with -update to regenerate the fixture after a deliberate
// shape change.
func TestBuildCellNetworkPolicyGolden(t *testing.T) {
	spec := CellPolicySpec{
		ControllerName:   "ci-00000000",
		Namespace:        "tenant-a",
		ReleaseNamespace: "varroa-system",
		GatewayGRPCPort:  9090,
		AgentNamespaces:  []string{"agents-b", "agents-a"}, // deliberately unsorted
		APIServerEgress: APIServerEgress{
			CIDRs: []string{"10.0.0.0/16"},
			Ports: []int32{443},
		},
		ExtraIngress: []networkingv1.NetworkPolicyIngressRule{
			{
				From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "monitoring-agent"}}},
				},
				Ports: []networkingv1.NetworkPolicyPort{{Port: intOrStrPtr(9100)}},
			},
		},
		ExtraEgress: []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.0/24"}},
				},
			},
		},
	}

	got := buildCellNetworkPolicy(spec)
	gotYAML, err := sigsyaml.Marshal(got)
	if err != nil {
		t.Fatalf("marshal built NetworkPolicy: %v", err)
	}

	if *updateCellNetworkPolicyGolden {
		if err := os.WriteFile(cellNetworkPolicyGoldenPath, gotYAML, 0o644); err != nil {
			t.Fatalf("write golden fixture: %v", err)
		}
		return
	}

	want, err := os.ReadFile(cellNetworkPolicyGoldenPath)
	if err != nil {
		t.Fatalf("read golden fixture: %v (run with -update to create it)", err)
	}
	if string(gotYAML) != string(want) {
		t.Errorf("built NetworkPolicy does not match golden fixture %s\ngot:\n%s\nwant:\n%s",
			cellNetworkPolicyGoldenPath, gotYAML, want)
	}
}

func intOrStrPtr(port int) *intstr.IntOrString {
	p := intstr.FromInt(port)
	return &p
}

// mustRunnerContribution reads cr's runner contribution the same way one
// reconcile tick does (design D2, D11), for a test that drives
// reconcileCellNetworkPolicy directly instead of through the full tick.
func mustRunnerContribution(t *testing.T, rec *Reconciler, cr *v1alpha1.Controller) runners.RunnerContribution {
	t.Helper()
	contrib, err := rec.runnerContribution(context.Background(), cr)
	if err != nil {
		t.Fatalf("runnerContribution: %v", err)
	}
	return contrib
}

// TestBuildCellNetworkPolicyAPIServerEgressDefaults asserts the fallback
// design D2 names: no configured CIDR renders 0.0.0.0/0, and no configured
// port renders an empty port list (matches every port, same as the chart's
// own apiServerEgress template).
func TestBuildCellNetworkPolicyAPIServerEgressDefaults(t *testing.T) {
	np := buildCellNetworkPolicy(CellPolicySpec{ControllerName: "ci-1", Namespace: "ns"})

	var apiServerRule *networkingv1.NetworkPolicyEgressRule
	for i := range np.Spec.Egress {
		rule := &np.Spec.Egress[i]
		if len(rule.To) == 1 && rule.To[0].IPBlock != nil {
			apiServerRule = rule
		}
	}
	if apiServerRule == nil {
		t.Fatal("no apiserver egress rule found (expected exactly one ipBlock-only peer)")
	}
	if got := apiServerRule.To[0].IPBlock.CIDR; got != "0.0.0.0/0" {
		t.Errorf("default apiserver CIDR = %q, want 0.0.0.0/0", got)
	}
	if len(apiServerRule.Ports) != 0 {
		t.Errorf("default apiserver ports = %+v, want empty (matches every port)", apiServerRule.Ports)
	}
}

// TestBuildCellNetworkPolicyOmitsAgentNamespaceRulesWhenEmpty asserts that a
// cell with no placed tenant renders no agent-namespace ingress/egress
// rule at all — an empty NetworkPolicyPeer list would deny all traffic for
// that rule anyway, so omitting it keeps the object shape unambiguous.
func TestBuildCellNetworkPolicyOmitsAgentNamespaceRulesWhenEmpty(t *testing.T) {
	np := buildCellNetworkPolicy(CellPolicySpec{ControllerName: "ci-1", Namespace: "ns"})

	if len(np.Spec.Ingress) != 1 {
		t.Errorf("ingress rule count = %d, want 1 (BFF/operator only, no agent-namespace rule)", len(np.Spec.Ingress))
	}
	if len(np.Spec.Egress) != 4 {
		t.Errorf("egress rule count = %d, want 4 (kube-dns, gateway, update center, apiserver — no agent-namespace rule)", len(np.Spec.Egress))
	}
}

// TestReconcileCellNetworkPolicySkipsNonCell asserts that a Controller
// without spec.cell.enabled never gets a NetworkPolicy created or deleted,
// even with both flags on.
func TestReconcileCellNetworkPolicySkipsNonCell(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, mustRunnerContribution(t, rec, cr)); err != nil {
		t.Fatalf("reconcileCellNetworkPolicy: %v", err)
	}
	if len(client.createdCellNetworkPolicies) != 0 || len(client.deletedCellNetworkPolicies) != 0 {
		t.Errorf("non-cell controller must never touch the cell NetworkPolicy; created=%+v deleted=%v",
			client.createdCellNetworkPolicies, client.deletedCellNetworkPolicies)
	}
}

// TestReconcileCellNetworkPolicyFlagFlip is task 4.3's first acceptance: it
// flips each of the two gating flags and asserts create then delete. Both
// VARROA_NETWORKPOLICY_ENABLED and VARROA_CELL_ISOLATION.enabled must be
// true for the policy to exist at all.
func TestReconcileCellNetworkPolicyFlagFlip(t *testing.T) {
	cases := []struct {
		name                 string
		networkPolicyEnabled bool
		cellIsolationEnabled bool
		wantCreate           bool
	}{
		{"both disabled", false, false, false},
		{"only networkPolicy enabled", true, false, false},
		{"only cellIsolation enabled", false, true, false},
		{"both enabled", true, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient()
			rec := newTestReconciler(client)
			rec.cellsConfig = CellsConfig{
				NetworkPolicyEnabled: tc.networkPolicyEnabled,
				CellIsolation:        CellIsolation{Enabled: tc.cellIsolationEnabled},
			}

			cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
			cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}

			if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, mustRunnerContribution(t, rec, cr)); err != nil {
				t.Fatalf("reconcileCellNetworkPolicy: %v", err)
			}

			if tc.wantCreate {
				if len(client.createdCellNetworkPolicies) != 1 {
					t.Errorf("createdCellNetworkPolicies = %+v, want exactly 1 create", client.createdCellNetworkPolicies)
				}
				if len(client.deletedCellNetworkPolicies) != 0 {
					t.Errorf("deletedCellNetworkPolicies = %v, want none", client.deletedCellNetworkPolicies)
				}
			} else {
				if len(client.createdCellNetworkPolicies) != 0 {
					t.Errorf("createdCellNetworkPolicies = %+v, want none", client.createdCellNetworkPolicies)
				}
				pre := controllerPrefix(cr)
				if len(client.deletedCellNetworkPolicies) != 1 || client.deletedCellNetworkPolicies[0] != pre+"-cell" {
					t.Errorf("deletedCellNetworkPolicies = %v, want [%q]", client.deletedCellNetworkPolicies, pre+"-cell")
				}
			}
		})
	}
}

// TestReconcileCellNetworkPolicyAddsTenantNamespaceOnNextTick is task 4.3's
// second acceptance: placing a tenant on a Connected cell adds its
// agentNamespace to the rebuilt policy on the next tick. The controller
// reconciler rebuilds the policy fresh on every call, so "next tick" is
// modeled here as two successive calls to reconcileCellNetworkPolicy
// against the same Connected cell — exactly what the per-tick block does.
func TestReconcileCellNetworkPolicyAddsTenantNamespaceOnNextTick(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	crdstore.MustSeed(client.store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"agents-a"},
			Pipelines:  &v1alpha1.TenantPipelinesSpec{Profile: "lts", AgentNamespace: "agents-a"},
		},
	})

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{}

	// Tick 1: no tenant placed yet.
	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, mustRunnerContribution(t, rec, cr)); err != nil {
		t.Fatalf("reconcileCellNetworkPolicy (tick 1): %v", err)
	}
	if len(client.createdCellNetworkPolicies) != 1 {
		t.Fatalf("tick 1: createdCellNetworkPolicies = %+v, want exactly 1", client.createdCellNetworkPolicies)
	}
	if got := client.createdCellNetworkPolicies[0].AgentNamespaces; len(got) != 0 {
		t.Errorf("tick 1: AgentNamespaces = %v, want none (no tenant placed)", got)
	}

	// Placement pass places team-a on this cell between ticks.
	cr.Status.Cell.Tenants = []string{"team-a"}

	// Tick 2: same cell, tenant now placed.
	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, mustRunnerContribution(t, rec, cr)); err != nil {
		t.Fatalf("reconcileCellNetworkPolicy (tick 2): %v", err)
	}
	if len(client.createdCellNetworkPolicies) != 2 {
		t.Fatalf("tick 2: createdCellNetworkPolicies = %+v, want exactly 2", client.createdCellNetworkPolicies)
	}
	got := client.createdCellNetworkPolicies[1].AgentNamespaces
	if len(got) != 1 || got[0] != "agents-a" {
		t.Errorf("tick 2: AgentNamespaces = %v, want [agents-a]", got)
	}
}

// TestHandleProvisioningCreatesCellNetworkPolicy asserts the first call
// site: handleProvisioning creates a fresh cell's NetworkPolicy next to the
// Service, so a cell is never briefly unprotected between provisioning and
// its first Running/Connected tick.
func TestHandleProvisioningCreatesCellNetworkPolicy(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}

	if len(client.createdCellNetworkPolicies) != 1 {
		t.Fatalf("createdCellNetworkPolicies = %+v, want exactly 1", client.createdCellNetworkPolicies)
	}
	pre := controllerPrefix(cr)
	if got := client.createdCellNetworkPolicies[0].ControllerName; got != pre {
		t.Errorf("ControllerName = %q, want %q", got, pre)
	}
}

// TestReconcileControllerTickRebuildsCellNetworkPolicy asserts the second
// call site: the Running/Connected per-tick block rebuilds the policy on
// every pass, alongside reconcileService and reconcileIngress.
func TestReconcileControllerTickRebuildsCellNetworkPolicy(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	rec.miteTransport = &captureTransport{}
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}

	// handleConnected may return an error in this minimal fixture (no live
	// mite); the tick block that rebuilds the NetworkPolicy runs before that
	// dispatch regardless, so only the created-policy side effect is
	// asserted here.
	_ = rec.reconcileController(context.Background(), cr)

	if len(client.createdCellNetworkPolicies) != 1 {
		t.Fatalf("createdCellNetworkPolicies = %+v, want exactly 1", client.createdCellNetworkPolicies)
	}
}

// TestGatewayGRPCPortFallsBackToDefault asserts gatewayGRPCPort's fallback
// when SetVarroaEndpoint was never called or carries an unparsable value.
func TestGatewayGRPCPortFallsBackToDefault(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	if got := rec.gatewayGRPCPort(); got != defaultCellGatewayGRPCPort {
		t.Errorf("gatewayGRPCPort() = %d, want default %d", got, defaultCellGatewayGRPCPort)
	}

	rec.SetVarroaEndpoint("varroa-gateway.varroa-system.svc.cluster.local:9191")
	if got := rec.gatewayGRPCPort(); got != 9191 {
		t.Errorf("gatewayGRPCPort() = %d, want 9191", got)
	}
}

// TestBuildCellNetworkPolicy_RunnerPeerAddsOneEgressRule is task 6.1's
// "one peer, a default port" acceptance at the builder level: one
// RunnerPeers entry renders exactly one egress rule, an ipBlock with a
// single-address CIDR, on the peer's port.
func TestBuildCellNetworkPolicy_RunnerPeerAddsOneEgressRule(t *testing.T) {
	np := buildCellNetworkPolicy(CellPolicySpec{
		ControllerName: "ci-1",
		Namespace:      "ns",
		RunnerPeers:    []runners.RunnerEgressPeer{{IP: "10.20.0.5", Port: 22}},
	})

	if len(np.Spec.Egress) != 5 {
		t.Fatalf("egress rule count = %d, want 5 (kube-dns, gateway, update center, apiserver, runner)", len(np.Spec.Egress))
	}
	rule := np.Spec.Egress[4]
	if len(rule.To) != 1 || rule.To[0].IPBlock == nil || rule.To[0].IPBlock.CIDR != "10.20.0.5/32" {
		t.Fatalf("runner rule peer = %+v, want ipBlock 10.20.0.5/32", rule.To)
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 22 {
		t.Fatalf("runner rule ports = %+v, want [22]", rule.Ports)
	}
}

// TestBuildCellNetworkPolicy_MultipleRunnerPeersSortDeterministically
// proves several peers each open their own rule, ordered independent of
// the caller's input order.
func TestBuildCellNetworkPolicy_MultipleRunnerPeersSortDeterministically(t *testing.T) {
	spec := CellPolicySpec{
		ControllerName: "ci-1",
		Namespace:      "ns",
		RunnerPeers: []runners.RunnerEgressPeer{
			{IP: "10.20.0.9", Port: 22},
			{IP: "10.20.0.5", Port: 2222},
		},
	}
	first := buildCellNetworkPolicy(spec)
	spec.RunnerPeers[0], spec.RunnerPeers[1] = spec.RunnerPeers[1], spec.RunnerPeers[0]
	second := buildCellNetworkPolicy(spec)

	if len(first.Spec.Egress) != 6 || len(second.Spec.Egress) != 6 {
		t.Fatalf("egress rule counts = %d, %d, want 6 each", len(first.Spec.Egress), len(second.Spec.Egress))
	}
	for i := 3; i <= 4; i++ {
		if first.Spec.Egress[i].To[0].IPBlock.CIDR != second.Spec.Egress[i].To[0].IPBlock.CIDR {
			t.Errorf("rule %d peer differs by input order: %q vs %q", i,
				first.Spec.Egress[i].To[0].IPBlock.CIDR, second.Spec.Egress[i].To[0].IPBlock.CIDR)
		}
	}
}

// TestBuildCellNetworkPolicy_NoRunnerPeersAddsNoRule covers both of task
// 6.1's "no rule" acceptances at the builder level: an empty RunnerPeers
// slice (an inbound-only cell, or a cell whose runners all left) adds no
// egress rule at all.
func TestBuildCellNetworkPolicy_NoRunnerPeersAddsNoRule(t *testing.T) {
	np := buildCellNetworkPolicy(CellPolicySpec{ControllerName: "ci-1", Namespace: "ns"})
	if len(np.Spec.Egress) != 4 {
		t.Fatalf("egress rule count = %d, want 4 (kube-dns, gateway, update center, apiserver — no runner rule)", len(np.Spec.Egress))
	}
}

// TestBuildCellNetworkPolicy_RunnerPeersAndExtraEgressAppearSeparately is
// task 6.3's acceptance: the operator never writes a runner rule into
// networkPolicy.cellIsolation.extraEgress. Both sets render as their own
// rules, in order, and the extraEgress rule passes through untouched.
func TestBuildCellNetworkPolicy_RunnerPeersAndExtraEgressAppearSeparately(t *testing.T) {
	extra := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.0/24"}},
		},
	}
	np := buildCellNetworkPolicy(CellPolicySpec{
		ControllerName: "ci-1",
		Namespace:      "ns",
		RunnerPeers:    []runners.RunnerEgressPeer{{IP: "10.20.0.5", Port: 22}},
		ExtraEgress:    []networkingv1.NetworkPolicyEgressRule{extra},
	})

	if len(np.Spec.Egress) != 6 {
		t.Fatalf("egress rule count = %d, want 6 (kube-dns, gateway, update center, apiserver, runner, extra)", len(np.Spec.Egress))
	}
	runnerRule := np.Spec.Egress[4]
	if runnerRule.To[0].IPBlock.CIDR != "10.20.0.5/32" {
		t.Errorf("runner rule = %+v, want ipBlock 10.20.0.5/32", runnerRule)
	}
	extraRule := np.Spec.Egress[5]
	if extraRule.To[0].IPBlock.CIDR != "203.0.113.0/24" {
		t.Errorf("extraEgress rule = %+v, want unchanged ipBlock 203.0.113.0/24", extraRule)
	}
	if len(extraRule.Ports) != 0 {
		t.Errorf("extraEgress rule ports = %+v, want none (renders verbatim)", extraRule.Ports)
	}
}

// TestSingleAddressCIDR asserts the /32 and /128 suffix rule.
func TestSingleAddressCIDR(t *testing.T) {
	if got := singleAddressCIDR("10.20.0.5"); got != "10.20.0.5/32" {
		t.Errorf("singleAddressCIDR(IPv4) = %q, want 10.20.0.5/32", got)
	}
	if got := singleAddressCIDR("2001:db8::1"); got != "2001:db8::1/128" {
		t.Errorf("singleAddressCIDR(IPv6) = %q, want 2001:db8::1/128", got)
	}
}

// runnerPlacedTenant and runnerSSHRunner build the minimal Tenant/Runner pair
// reconcileCellNetworkPolicy's runner-contribution read needs, mirroring
// the runners package's own private test fixtures (unexported there, so
// duplicated here rather than imported).
func runnerPlacedTenant(cell, cellNamespace string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Generation: 1},
		Spec:       v1alpha1.TenantSpec{Pipelines: &v1alpha1.TenantPipelinesSpec{AgentNamespace: "agents"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: cell, CellNamespace: cellNamespace},
		},
	}
}

func runnerSSHRunner() *v1alpha1.Runner {
	const name, tenant, host = "win-1", "payments", "10.20.0.5"
	return &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: v1alpha1.RunnerSpec{
			Tenant:        tenant,
			Launcher:      "ssh",
			RemoteFS:      "/home/jenkins",
			Host:          host,
			CredentialRef: "ssh-key",
		},
	}
}

// seedRunnerCredentialSecret gives runnerSSHRunner's credentialRef a labelled
// Secret in the agents namespace, so the runner is deliverable.
func seedRunnerCredentialSecret(client *testClient) {
	client.labeledSecrets = append(client.labeledSecrets, labeledSecretFixture{
		name: "ssh-key", namespace: "agents",
		labels: map[string]string{"varroa.dev/runner-credential": "true"},
	})
	if client.existingSecrets == nil {
		client.existingSecrets = map[string]map[string][]byte{}
	}
	client.existingSecrets["ssh-key"] = map[string][]byte{"username": []byte("jenkins"), "privateKey": []byte("PEMDATA")}
}

func runnerInboundRunner(name, tenant string) *v1alpha1.Runner {
	return &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: v1alpha1.RunnerSpec{
			Tenant:   tenant,
			Launcher: "inbound",
			RemoteFS: "/home/jenkins",
		},
	}
}

// TestReconcileCellNetworkPolicy_SSHRunnerContributesPeer is task 6.1/6.2's
// end-to-end wiring proof: a placed tenant's ssh Runner reaches the built
// policy as one egress rule to its host on its (defaulted) port, through
// the same runners.BuildContribution read the caller hands
// reconcileCellNetworkPolicy.
func TestReconcileCellNetworkPolicy_SSHRunnerContributesPeer(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}

	crdstore.MustSeed(client.store, runnerPlacedTenant("test", "ns1"))
	seedRunnerCredentialSecret(client)
	crdstore.MustSeed(client.store, runnerSSHRunner())

	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, mustRunnerContribution(t, rec, cr)); err != nil {
		t.Fatalf("reconcileCellNetworkPolicy: %v", err)
	}
	if len(client.createdCellNetworkPolicies) != 1 {
		t.Fatalf("createdCellNetworkPolicies = %+v, want exactly 1", client.createdCellNetworkPolicies)
	}
	spec := client.createdCellNetworkPolicies[0]
	want := []runners.RunnerEgressPeer{{IP: "10.20.0.5", Port: 22}}
	if len(spec.RunnerPeers) != 1 || spec.RunnerPeers[0] != want[0] {
		t.Fatalf("RunnerPeers = %+v, want %+v (default port 22)", spec.RunnerPeers, want)
	}

	np := buildCellNetworkPolicy(spec)
	found := false
	for _, rule := range np.Spec.Egress {
		if len(rule.To) == 1 && rule.To[0].IPBlock != nil && rule.To[0].IPBlock.CIDR == "10.20.0.5/32" {
			found = true
			if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 22 {
				t.Errorf("runner rule ports = %+v, want [22]", rule.Ports)
			}
		}
	}
	if !found {
		t.Errorf("built policy carries no egress rule to 10.20.0.5/32; egress = %+v", np.Spec.Egress)
	}
}

// TestReconcileCellNetworkPolicy_InboundOnlyContributesNoPeer is task
// 6.1's "a cell with only inbound runners producing no rule" acceptance,
// run through the reconciler.
func TestReconcileCellNetworkPolicy_InboundOnlyContributesNoPeer(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}

	crdstore.MustSeed(client.store, runnerPlacedTenant("test", "ns1"))
	crdstore.MustSeed(client.store, runnerInboundRunner("agent-1", "payments"))

	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, mustRunnerContribution(t, rec, cr)); err != nil {
		t.Fatalf("reconcileCellNetworkPolicy: %v", err)
	}
	spec := client.createdCellNetworkPolicies[0]
	if len(spec.RunnerPeers) != 0 {
		t.Fatalf("RunnerPeers = %+v, want none for an inbound-only cell", spec.RunnerPeers)
	}
}

// TestReconcileCellNetworkPolicy_DeletedRunnerLosesItsPeer is task 6.1's
// "a cell whose runners were all deleted producing no rule" acceptance,
// modeled as two successive ticks the same way
// TestReconcileCellNetworkPolicyAddsTenantNamespaceOnNextTick models a
// placement change.
func TestReconcileCellNetworkPolicy_DeletedRunnerLosesItsPeer(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}

	crdstore.MustSeed(client.store, runnerPlacedTenant("test", "ns1"))
	seedRunnerCredentialSecret(client)
	crdstore.MustSeed(client.store, runnerSSHRunner())

	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, mustRunnerContribution(t, rec, cr)); err != nil {
		t.Fatalf("reconcileCellNetworkPolicy (tick 1): %v", err)
	}
	if got := client.createdCellNetworkPolicies[0].RunnerPeers; len(got) != 1 {
		t.Fatalf("tick 1: RunnerPeers = %+v, want 1", got)
	}

	if err := crdstore.Delete[v1alpha1.Runner](context.Background(), client.store, "win-1", ""); err != nil {
		t.Fatalf("delete runner: %v", err)
	}

	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, mustRunnerContribution(t, rec, cr)); err != nil {
		t.Fatalf("reconcileCellNetworkPolicy (tick 2): %v", err)
	}
	if got := client.createdCellNetworkPolicies[1].RunnerPeers; len(got) != 0 {
		t.Fatalf("tick 2: RunnerPeers = %+v, want none after the runner is deleted", got)
	}
}

// countingRunnerListBackend counts ListObjects calls for the Runner GVR,
// delegating every call to the wrapped Backend unchanged. It proves a
// reconcile tick reads the Runner set once and shares that one read with
// every consumer (design D2, D11).
type countingRunnerListBackend struct {
	crdstore.Backend
	runnerGVR schema.GroupVersionResource
	calls     int
}

func (b *countingRunnerListBackend) ListObjects(ctx context.Context, gvr schema.GroupVersionResource, namespace, labelSelector string) ([]unstructured.Unstructured, error) {
	if gvr == b.runnerGVR {
		b.calls++
	}
	return b.Backend.ListObjects(ctx, gvr, namespace, labelSelector)
}

// TestReconcileController_RunnerContributionReadOnceEachTick proves a
// Connected-phase tick reads the Runner set exactly once and hands that
// one result to both the cell NetworkPolicy and the pushed desired state
// (design D2, D11): a divergent second read can never happen, because
// there is only one.
func TestReconcileController_RunnerContributionReadOnceEachTick(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec, registry := newTestReconcilerForGate(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	cr := newGateCtrl("cell-a", 0, "", nil)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}

	crdstore.MustSeed(client.store, runnerPlacedTenant("cell-a", "ns"))
	seedRunnerCredentialSecret(client)
	crdstore.MustSeed(client.store, runnerSSHRunner())

	runnerGVR, err := crdstore.GVRFor[v1alpha1.Runner]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	counting := &countingRunnerListBackend{Backend: client.store, runnerGVR: runnerGVR}
	rec.store = counting

	registry.Register(cr.Name, cr.Namespace, nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	// Send fails (no real gRPC stream in test); the read count below is
	// what this test proves, not a successful push.
	_ = rec.reconcileController(context.Background(), cr)

	if counting.calls != 1 {
		t.Fatalf("Runner list calls in one tick = %d, want exactly 1 (the NetworkPolicy and the desired state must share one read)", counting.calls)
	}
	if len(client.createdCellNetworkPolicies) != 1 {
		t.Fatalf("createdCellNetworkPolicies = %+v, want exactly 1", client.createdCellNetworkPolicies)
	}
	want := runners.RunnerEgressPeer{IP: "10.20.0.5", Port: 22}
	got := client.createdCellNetworkPolicies[0].RunnerPeers
	if len(got) != 1 || got[0] != want {
		t.Fatalf("RunnerPeers = %+v, want %+v", got, want)
	}
}

// failingRunnerListBackend fails every Runner list, as a transient API
// outage would.
type failingRunnerListBackend struct {
	crdstore.Backend
	runnerGVR schema.GroupVersionResource
}

func (b *failingRunnerListBackend) ListObjects(ctx context.Context, gvr schema.GroupVersionResource, namespace, labelSelector string) ([]unstructured.Unstructured, error) {
	if gvr == b.runnerGVR {
		return nil, errors.New("runner list: transient failure")
	}
	return b.Backend.ListObjects(ctx, gvr, namespace, labelSelector)
}

// TestReconcileController_RunnerContributionErrorWritesNothing proves a
// failed contribution read never stands in for an empty runner set: the
// tick writes no cell NetworkPolicy and returns the error before any
// desired-state publish (design D2).
func TestReconcileController_RunnerContributionErrorWritesNothing(t *testing.T) {
	client := newTestClientWithGateBundle()
	rec := newTestReconciler(client)
	cr := newGateCtrl("cell-a", 0, "", nil)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Phase = v1alpha1.ControllerPhaseConnected
	crdstore.MustSeed(client.store, cr)
	crdstore.MustSeed(client.store, runnerPlacedTenant("cell-a", "ns"))
	seedRunnerCredentialSecret(client)
	crdstore.MustSeed(client.store, runnerSSHRunner())

	runnerGVR, err := crdstore.GVRFor[v1alpha1.Runner]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	rec.store = &failingRunnerListBackend{Backend: client.store, runnerGVR: runnerGVR}

	err = rec.reconcileController(context.Background(), cr)
	if err == nil || !strings.Contains(err.Error(), "build runner contribution") {
		t.Fatalf("reconcile error = %v, want the contribution read failure", err)
	}
	if len(client.createdCellNetworkPolicies) != 0 {
		t.Fatalf("createdCellNetworkPolicies = %+v, want none after a failed read", client.createdCellNetworkPolicies)
	}
}

// TestBuildCellNetworkPolicyAdmitsUpdateCenter asserts ruling (ii): the
// cell may reach the in-cluster update center pods in the release
// namespace on TCP 8080, the port the operator wires into JENKINS_UC.
func TestBuildCellNetworkPolicyAdmitsUpdateCenter(t *testing.T) {
	np := buildCellNetworkPolicy(CellPolicySpec{ControllerName: "ci-1", Namespace: "ns", ReleaseNamespace: "varroa-system"})
	var found bool
	for _, rule := range np.Spec.Egress {
		if len(rule.To) != 1 || rule.To[0].PodSelector == nil {
			continue
		}
		if rule.To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "varroa-updatecenter" {
			continue
		}
		found = true
		if rule.To[0].NamespaceSelector == nil || rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "varroa-system" {
			t.Errorf("update center peer namespace = %+v, want the release namespace", rule.To[0].NamespaceSelector)
		}
		if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 8080 {
			t.Errorf("update center ports = %+v, want exactly TCP 8080", rule.Ports)
		}
	}
	if !found {
		t.Fatal("no egress rule admits the varroa-updatecenter component")
	}
}

// seedCellPoolWithLibrary registers pool-a with one library at remote and
// labels cr as a member of that pool.
func seedCellPoolWithLibrary(client *testClient, cr *v1alpha1.Controller, remote string) {
	crdstore.MustSeed(client.store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef: "lts-2-555",
			Libraries: []v1alpha1.CellPoolLibrary{{
				Name: "platform-lib", Remote: remote, DefaultVersion: "main",
			}},
		},
	})
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
}

// TestBuildCellNetworkPolicy_LibraryPeersRenderAfterRunnersBeforeExtra
// checks the rule order: runner rules, then library rules, then extra
// egress.
func TestBuildCellNetworkPolicy_LibraryPeersRenderAfterRunnersBeforeExtra(t *testing.T) {
	spec := CellPolicySpec{
		ControllerName:   "test",
		Namespace:        "ns1",
		ReleaseNamespace: "varroa-system",
		GatewayGRPCPort:  9090,
		RunnerPeers:      []runners.RunnerEgressPeer{{IP: "10.20.0.5", Port: 22}},
		LibraryPeers:     []runners.RunnerEgressPeer{{IP: "10.30.0.9", Port: 2222}},
		ExtraEgress: []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "192.0.2.0/24"}}},
		}},
	}
	np := buildCellNetworkPolicy(spec)
	order := []string{"10.20.0.5/32", "10.30.0.9/32", "192.0.2.0/24"}
	last := -1
	for _, cidr := range order {
		idx := -1
		for i, rule := range np.Spec.Egress {
			if len(rule.To) == 1 && rule.To[0].IPBlock != nil && rule.To[0].IPBlock.CIDR == cidr {
				idx = i
			}
		}
		if idx <= last {
			t.Fatalf("rule %s at index %d, want after index %d; egress = %+v", cidr, idx, last, np.Spec.Egress)
		}
		last = idx
	}
}

// TestReconcileCellNetworkPolicy_PoolLibraryContributesPeer checks that a
// pool library's resolved host and port reach the built policy.
func TestReconcileCellNetworkPolicy_PoolLibraryContributesPeer(t *testing.T) {
	withPoolLibraryLookup(t, func(context.Context, string) ([]string, error) {
		return []string{"10.30.0.9"}, nil
	})
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	seedCellPoolWithLibrary(client, cr, "ssh://git@git.example:2222/lib.git")

	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, runners.RunnerContribution{}); err != nil {
		t.Fatalf("reconcileCellNetworkPolicy: %v", err)
	}
	if len(client.createdCellNetworkPolicies) != 1 {
		t.Fatalf("createdCellNetworkPolicies = %+v, want exactly 1", client.createdCellNetworkPolicies)
	}
	spec := client.createdCellNetworkPolicies[0]
	want := runners.RunnerEgressPeer{IP: "10.30.0.9", Port: 2222}
	if len(spec.LibraryPeers) != 1 || spec.LibraryPeers[0] != want {
		t.Fatalf("LibraryPeers = %+v, want [%+v]", spec.LibraryPeers, want)
	}

	np := buildCellNetworkPolicy(spec)
	found := false
	for _, rule := range np.Spec.Egress {
		if len(rule.To) == 1 && rule.To[0].IPBlock != nil && rule.To[0].IPBlock.CIDR == "10.30.0.9/32" {
			found = true
			if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 2222 {
				t.Errorf("library rule ports = %+v, want [2222]", rule.Ports)
			}
		}
	}
	if !found {
		t.Errorf("built policy carries no egress rule to 10.30.0.9/32; egress = %+v", np.Spec.Egress)
	}
}

// TestReconcileCellNetworkPolicy_PoolWithoutLibrariesAddsNoPeer covers a
// cell whose pool is missing and a pool that declares no library.
func TestReconcileCellNetworkPolicy_PoolWithoutLibrariesAddsNoPeer(t *testing.T) {
	t.Run("missing pool", func(t *testing.T) {
		client := newTestClient()
		rec := newTestReconciler(client)
		rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}
		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
		cr.Labels = map[string]string{cells.LabelCellPool: "pool-missing"}
		if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, runners.RunnerContribution{}); err != nil {
			t.Fatalf("reconcileCellNetworkPolicy: %v", err)
		}
		if got := client.createdCellNetworkPolicies[0].LibraryPeers; len(got) != 0 {
			t.Fatalf("LibraryPeers = %+v, want none", got)
		}
	})
	t.Run("empty library list", func(t *testing.T) {
		client := newTestClient()
		rec := newTestReconciler(client)
		rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}
		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
		crdstore.MustSeed(client.store, &v1alpha1.CellPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
			Spec:       v1alpha1.CellPoolSpec{ProfileRef: "lts-2-555"},
		})
		cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
		if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, runners.RunnerContribution{}); err != nil {
			t.Fatalf("reconcileCellNetworkPolicy: %v", err)
		}
		if got := client.createdCellNetworkPolicies[0].LibraryPeers; len(got) != 0 {
			t.Fatalf("LibraryPeers = %+v, want none", got)
		}
	})
}

// TestReconcileCellNetworkPolicy_RemovedLibraryLosesItsPeer models two
// ticks: the pool is deleted between them, so its library peer drops.
func TestReconcileCellNetworkPolicy_RemovedLibraryLosesItsPeer(t *testing.T) {
	withPoolLibraryLookup(t, func(context.Context, string) ([]string, error) {
		return []string{"10.30.0.9"}, nil
	})
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}
	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	seedCellPoolWithLibrary(client, cr, "https://git.example/lib.git")

	ctx := context.Background()
	if err := rec.reconcileCellNetworkPolicy(ctx, cr, runners.RunnerContribution{}); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if got := client.createdCellNetworkPolicies[0].LibraryPeers; len(got) != 1 {
		t.Fatalf("tick 1 LibraryPeers = %+v, want one peer", got)
	}

	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	if err := client.store.DeleteObject(ctx, gvr, "", "pool-a"); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	if err := rec.reconcileCellNetworkPolicy(ctx, cr, runners.RunnerContribution{}); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := client.createdCellNetworkPolicies[1].LibraryPeers; len(got) != 0 {
		t.Fatalf("tick 2 LibraryPeers = %+v, want none after removal", got)
	}
}

// TestReconcileCellNetworkPolicy_PoolReadErrorWritesNothing checks that a
// failed pool read returns an error and writes no policy.
func TestReconcileCellNetworkPolicy_PoolReadErrorWritesNothing(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.cellsConfig = CellsConfig{NetworkPolicyEnabled: true, CellIsolation: CellIsolation{Enabled: true}}
	cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	client.store.FailAlways("get", gvr, errors.New("apiserver unavailable"))

	if err := rec.reconcileCellNetworkPolicy(context.Background(), cr, runners.RunnerContribution{}); err == nil {
		t.Fatal("a pool read failure must fail the reconcile")
	}
	if len(client.createdCellNetworkPolicies) != 0 {
		t.Fatalf("createdCellNetworkPolicies = %+v, want none", client.createdCellNetworkPolicies)
	}
}
