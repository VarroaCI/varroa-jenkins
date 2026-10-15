package runners

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func cellFor(name string) *v1alpha1.Controller {
	return &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "varroa-system"}}
}

// TestBuildContribution_ReplicaWithNoTickerStillRenders is task 3.6's first
// acceptance: BuildContribution is a free function with no dependency on any
// Reconciler ticker state, so a replica that never ran the ticker still
// renders the full node list straight from the CRDs.
func TestBuildContribution_ReplicaWithNoTickerStillRenders(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))
	crdstore.MustSeed(f, inboundRunner())

	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.Nodes) != 2 {
		t.Fatalf("Nodes = %d entries, want 2", len(contrib.Nodes))
	}
}

// TestBuildContribution_DeletedRunnerDisappears is task 3.6's second
// acceptance: a deleted runner disappears from the next call with no
// bookkeeping.
func TestBuildContribution_DeletedRunnerDisappears(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	cell := cellFor("cell-a")
	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cell)
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.Nodes) != 1 {
		t.Fatalf("Nodes before delete = %d, want 1", len(contrib.Nodes))
	}

	if err := crdstore.Delete[v1alpha1.Runner](context.Background(), f, "win-1", ""); err != nil {
		t.Fatalf("delete runner: %v", err)
	}

	contrib, err = BuildContribution(context.Background(), f, sshSecrets(), cell)
	if err != nil {
		t.Fatalf("BuildContribution after delete: %v", err)
	}
	if len(contrib.Nodes) != 0 {
		t.Fatalf("Nodes after delete = %d, want 0", len(contrib.Nodes))
	}
}

// TestBuildContribution_OnlyThisCell proves a runner placed on a different
// cell contributes nothing.
func TestBuildContribution_OnlyThisCell(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cellFor("cell-b"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.Nodes) != 0 {
		t.Fatalf("Nodes for the wrong cell = %d, want 0", len(contrib.Nodes))
	}
}

// TestTick_EmptyHintMap_StillRendersCorrectNodeList is task 3.7's third
// acceptance: a Reconciler whose enqueue-hint map is empty (a fresh
// Reconciler, or one that just lost the map on a restart) still enqueues
// every cell that currently hosts a runner, and the resulting
// BuildContribution call — which never reads the hint map at all — renders
// the correct node list regardless. The hint map only ever adds extra
// enqueues for a cell a runner just left; losing it costs a reconcile
// interval of delay on that departure, never a wrong node list.
func TestTick_EmptyHintMap_StillRendersCorrectNodeList(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	rec := newTestReconciler(f) // lastCellForRunner is nil: an empty hint map
	rec.probeOffline(t)
	result := rec.Tick(context.Background())
	if !containsCell(result.TouchedCells, CellKey{Cluster: "core", Namespace: "varroa-system", Name: "cell-a"}) {
		t.Fatalf("TouchedCells with an empty hint map = %v, want cell-a present", result.TouchedCells)
	}

	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.Nodes) != 1 {
		t.Fatalf("Nodes = %d, want 1", len(contrib.Nodes))
	}
}

// TestBuildContribution_UndeliveredRunnerContributesNoNode proves the same
// delivery rule the ticker applies also gates BuildContribution.
func TestBuildContribution_UndeliveredRunnerContributesNoNode(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	broken := sshRunner("win-1", "payments")
	broken.Spec.Launcher = "winrm"
	crdstore.MustSeed(f, broken)

	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.Nodes) != 0 {
		t.Fatalf("Nodes for an undelivered runner = %d, want 0", len(contrib.Nodes))
	}
}

// TestBuildContribution_SSHRunnerContributesEgressPeer proves
// BuildContribution fills EgressPeers for a delivered ssh runner (task
// 6.2), from the same read that produced its node.
func TestBuildContribution_SSHRunnerContributesEgressPeer(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	runner := sshRunner("win-1", "payments")
	runner.Spec.Host = "10.20.0.5"
	crdstore.MustSeed(f, runner)

	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	want := []RunnerEgressPeer{{IP: "10.20.0.5", Port: defaultPort}}
	if len(contrib.EgressPeers) != 1 || contrib.EgressPeers[0] != want[0] {
		t.Fatalf("EgressPeers = %+v, want %+v", contrib.EgressPeers, want)
	}
}

// TestBuildContribution_InboundOnlyContributesNoEgressPeer is task 6.1's
// "a cell with only inbound runners producing no rule" acceptance.
func TestBuildContribution_InboundOnlyContributesNoEgressPeer(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, inboundRunner())

	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.EgressPeers) != 0 {
		t.Fatalf("EgressPeers = %+v, want none for an inbound-only cell", contrib.EgressPeers)
	}
}

// TestBuildContribution_DeletedRunnerLosesItsEgressPeer is task 6.1's "a
// cell whose runners were all deleted producing no rule" acceptance.
func TestBuildContribution_DeletedRunnerLosesItsEgressPeer(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	runner := sshRunner("win-1", "payments")
	runner.Spec.Host = "10.20.0.5"
	crdstore.MustSeed(f, runner)

	cell := cellFor("cell-a")
	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cell)
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.EgressPeers) != 1 {
		t.Fatalf("EgressPeers before delete = %+v, want 1", contrib.EgressPeers)
	}

	if err := crdstore.Delete[v1alpha1.Runner](context.Background(), f, "win-1", ""); err != nil {
		t.Fatalf("delete runner: %v", err)
	}

	contrib, err = BuildContribution(context.Background(), f, sshSecrets(), cell)
	if err != nil {
		t.Fatalf("BuildContribution after delete: %v", err)
	}
	if len(contrib.EgressPeers) != 0 {
		t.Fatalf("EgressPeers after delete = %+v, want none", contrib.EgressPeers)
	}
}

// TestBuildContribution_OfflineRunnerKeepsItsNode proves that a runner's
// probed offline status (as the ticker would have written it) never enters
// the delivery decision BuildContribution recomputes fresh from the CRDs —
// design D2's "delivery is independent of liveness".
func TestBuildContribution_OfflineRunnerKeepsItsNode(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	// Simulate what the ticker would have written for an offline probe:
	// Connected false, Ready=False/Offline. BuildContribution must not read
	// any of this.
	if err := crdstore.PatchStatus[v1alpha1.Runner](context.Background(), f, "win-1", "", &v1alpha1.RunnerStatus{
		Connected: false,
		Conditions: []v1alpha1.RunnerCondition{
			{Type: ConditionTypeReady, Status: metav1.ConditionFalse, Reason: ReasonOffline},
		},
	}); err != nil {
		t.Fatalf("simulate offline status: %v", err)
	}

	contrib, err := BuildContribution(context.Background(), f, sshSecrets(), cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.Nodes) != 1 {
		t.Fatalf("Nodes for a delivered-but-offline runner = %d, want 1 (still rendered)", len(contrib.Nodes))
	}
}
