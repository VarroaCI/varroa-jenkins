package runners

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func placedTenant(name, cell, cellNamespace string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metaObj(name),
		Spec:       v1alpha1.TenantSpec{Pipelines: &v1alpha1.TenantPipelinesSpec{AgentNamespace: "agents"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{
				Cluster:       "core",
				Cell:          cell,
				CellNamespace: cellNamespace,
			},
		},
	}
}

func sshRunner(name, tenant string) *v1alpha1.Runner {
	return &v1alpha1.Runner{
		ObjectMeta: metaObj(name),
		Spec: v1alpha1.RunnerSpec{
			Tenant:        tenant,
			Launcher:      launcherSSH,
			RemoteFS:      "/home/jenkins",
			Host:          "10.0.0.1",
			CredentialRef: "ssh-key",
		},
	}
}

// sshSecrets answers sshRunner's credential in the agents namespace, so a
// contribution test that is not about credential faults renders its nodes.
func sshSecrets() *fakeSecretReader {
	return &fakeSecretReader{
		namespace: "agents",
		records: []map[string][]byte{
			secretRecord("ssh-key", map[string]string{"username": "jenkins", "privateKey": "PEMDATA"}),
		},
	}
}

func inboundRunner() *v1alpha1.Runner {
	return &v1alpha1.Runner{
		ObjectMeta: metaObj("agent-1"),
		Spec: v1alpha1.RunnerSpec{
			Tenant:   "payments",
			Launcher: launcherInbound,
			RemoteFS: "/home/jenkins",
		},
	}
}

// TestEvaluateRunner_Reasons is task 3.2's table-driven test: one case per
// early Ready=False reason, over evaluateRunner directly.
func TestEvaluateRunner_Reasons(t *testing.T) {
	tests := []struct {
		name       string
		runner     *v1alpha1.Runner
		tenant     *v1alpha1.Tenant
		wantReason string
	}{
		{
			name:       "tenant not found",
			runner:     sshRunner("win-1", "payments"),
			tenant:     nil,
			wantReason: ReasonTenantNotFound,
		},
		{
			name:   "tenant not pipelines",
			runner: sshRunner("win-1", "payments"),
			tenant: &v1alpha1.Tenant{
				ObjectMeta: metaObj("payments"),
				Spec:       v1alpha1.TenantSpec{},
			},
			wantReason: ReasonTenantNotPipelines,
		},
		{
			name: "launcher unsupported",
			runner: &v1alpha1.Runner{
				ObjectMeta: metaObj("win-1"),
				Spec:       v1alpha1.RunnerSpec{Tenant: "payments", Launcher: "winrm", RemoteFS: "/x"},
			},
			tenant:     placedTenant("payments", "cell-a", "varroa-system"),
			wantReason: ReasonLauncherUnsupported,
		},
		{
			name:       "tenant name unsupported",
			runner:     sshRunner("win-1", "pay.roll"),
			tenant:     placedTenant("pay.roll", "cell-a", "varroa-system"),
			wantReason: ReasonTenantNameUnsupported,
		},
		{
			name:       "runner name unsupported",
			runner:     sshRunner("win.1", "payments"),
			tenant:     placedTenant("payments", "cell-a", "varroa-system"),
			wantReason: ReasonRunnerNameUnsupported,
		},
		{
			name:       "awaiting placement",
			runner:     sshRunner("win-1", "payments"),
			tenant:     &v1alpha1.Tenant{ObjectMeta: metaObj("payments"), Spec: v1alpha1.TenantSpec{Pipelines: &v1alpha1.TenantPipelinesSpec{}}},
			wantReason: ReasonAwaitingPlacement,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := evaluateRunner(tt.runner, tt.tenant)
			if eval.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", eval.Reason, tt.wantReason)
			}
			if eval.Delivered {
				t.Fatal("Delivered = true for a faulted runner")
			}
		})
	}
}

// TestEvaluateRunner_MissingAndNonPipelinesDiffer proves the two tenant faults
// carry distinct reasons, per the spec's "A missing Tenant and a non-pipelines
// Tenant differ" scenario.
func TestEvaluateRunner_MissingAndNonPipelinesDiffer(t *testing.T) {
	missing := evaluateRunner(sshRunner("a", "ghost"), nil)
	nonPipelines := evaluateRunner(sshRunner("b", "search"),
		&v1alpha1.Tenant{ObjectMeta: metaObj("search")})

	if missing.Reason != ReasonTenantNotFound {
		t.Fatalf("missing tenant reason = %q, want %q", missing.Reason, ReasonTenantNotFound)
	}
	if nonPipelines.Reason != ReasonTenantNotPipelines {
		t.Fatalf("non-pipelines tenant reason = %q, want %q", nonPipelines.Reason, ReasonTenantNotPipelines)
	}
}

// TestEvaluateRunner_Delivered proves a fully valid, placed runner delivers
// with no reason.
func TestEvaluateRunner_Delivered(t *testing.T) {
	eval := evaluateRunner(sshRunner("win-1", "payments"), placedTenant("payments", "cell-a", "varroa-system"))
	if !eval.Delivered {
		t.Fatalf("Delivered = false, reason %q", eval.Reason)
	}
	if eval.Reason != "" {
		t.Fatalf("Reason = %q, want empty for a delivered runner", eval.Reason)
	}
	if eval.NodeName != "payments-win-1" {
		t.Fatalf("NodeName = %q, want payments-win-1", eval.NodeName)
	}
	if eval.Cell != "cell-a" || eval.CellNamespace != "varroa-system" {
		t.Fatalf("Cell/CellNamespace = %q/%q, want cell-a/varroa-system", eval.Cell, eval.CellNamespace)
	}
}

// TestEvaluateRunner_NodeNameAlwaysComputed proves status.nodeName is
// derived even for a runner that names no Tenant (tick step 3 runs before the
// fault chain).
func TestEvaluateRunner_NodeNameAlwaysComputed(t *testing.T) {
	eval := evaluateRunner(sshRunner("win-1", "ghost"), nil)
	if eval.NodeName != "ghost-win-1" {
		t.Fatalf("NodeName = %q, want ghost-win-1", eval.NodeName)
	}
}
