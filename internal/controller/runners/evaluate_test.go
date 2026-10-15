package runners

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func placedTeam(name, cell, cellNamespace string) *v1alpha1.Team {
	return &v1alpha1.Team{
		ObjectMeta: metaObj(name),
		Spec:       v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{AgentNamespace: "agents"}},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{
				Cluster:       "core",
				Cell:          cell,
				CellNamespace: cellNamespace,
			},
		},
	}
}

func sshRunner(name, team string) *v1alpha1.Runner {
	return &v1alpha1.Runner{
		ObjectMeta: metaObj(name),
		Spec: v1alpha1.RunnerSpec{
			Team:          team,
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
			Team:     "payments",
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
		team       *v1alpha1.Team
		wantReason string
	}{
		{
			name:       "team not found",
			runner:     sshRunner("win-1", "payments"),
			team:       nil,
			wantReason: ReasonTeamNotFound,
		},
		{
			name:   "team not pipelines",
			runner: sshRunner("win-1", "payments"),
			team: &v1alpha1.Team{
				ObjectMeta: metaObj("payments"),
				Spec:       v1alpha1.TeamSpec{},
			},
			wantReason: ReasonTeamNotPipelines,
		},
		{
			name: "launcher unsupported",
			runner: &v1alpha1.Runner{
				ObjectMeta: metaObj("win-1"),
				Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "winrm", RemoteFS: "/x"},
			},
			team:       placedTeam("payments", "cell-a", "varroa-system"),
			wantReason: ReasonLauncherUnsupported,
		},
		{
			name:       "team name unsupported",
			runner:     sshRunner("win-1", "pay.roll"),
			team:       placedTeam("pay.roll", "cell-a", "varroa-system"),
			wantReason: ReasonTeamNameUnsupported,
		},
		{
			name:       "runner name unsupported",
			runner:     sshRunner("win.1", "payments"),
			team:       placedTeam("payments", "cell-a", "varroa-system"),
			wantReason: ReasonRunnerNameUnsupported,
		},
		{
			name:       "awaiting placement",
			runner:     sshRunner("win-1", "payments"),
			team:       &v1alpha1.Team{ObjectMeta: metaObj("payments"), Spec: v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{}}},
			wantReason: ReasonAwaitingPlacement,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := evaluateRunner(tt.runner, tt.team)
			if eval.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", eval.Reason, tt.wantReason)
			}
			if eval.Delivered {
				t.Fatal("Delivered = true for a faulted runner")
			}
		})
	}
}

// TestEvaluateRunner_MissingAndNonPipelinesDiffer proves the two team faults
// carry distinct reasons, per the spec's "A missing Team and a non-pipelines
// Team differ" scenario.
func TestEvaluateRunner_MissingAndNonPipelinesDiffer(t *testing.T) {
	missing := evaluateRunner(sshRunner("a", "ghost"), nil)
	nonPipelines := evaluateRunner(sshRunner("b", "search"),
		&v1alpha1.Team{ObjectMeta: metaObj("search")})

	if missing.Reason != ReasonTeamNotFound {
		t.Fatalf("missing team reason = %q, want %q", missing.Reason, ReasonTeamNotFound)
	}
	if nonPipelines.Reason != ReasonTeamNotPipelines {
		t.Fatalf("non-pipelines team reason = %q, want %q", nonPipelines.Reason, ReasonTeamNotPipelines)
	}
}

// TestEvaluateRunner_Delivered proves a fully valid, placed runner delivers
// with no reason.
func TestEvaluateRunner_Delivered(t *testing.T) {
	eval := evaluateRunner(sshRunner("win-1", "payments"), placedTeam("payments", "cell-a", "varroa-system"))
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
// derived even for a runner that names no Team (tick step 3 runs before the
// fault chain).
func TestEvaluateRunner_NodeNameAlwaysComputed(t *testing.T) {
	eval := evaluateRunner(sshRunner("win-1", "ghost"), nil)
	if eval.NodeName != "ghost-win-1" {
		t.Fatalf("NodeName = %q, want ghost-win-1", eval.NodeName)
	}
}
