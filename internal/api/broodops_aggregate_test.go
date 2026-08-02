package api

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestAggregatePhase(t *testing.T) {
	tests := []struct {
		name   string
		phases []v1alpha1.BroodOperationPhase
		want   v1alpha1.BroodOperationPhase
		wantOK bool
	}{
		// §2 Rule 1: all Succeeded
		{"all Succeeded", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhaseSucceeded,
			v1alpha1.BroodOperationPhaseSucceeded,
		}, v1alpha1.BroodOperationPhaseSucceeded, true},

		// §2 Rule 2: all terminal ∧ ≥1 Failed
		{"all terminal with Failed", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhaseSucceeded,
			v1alpha1.BroodOperationPhaseFailed,
		}, v1alpha1.BroodOperationPhaseFailed, true},
		{"all Failed", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhaseFailed,
			v1alpha1.BroodOperationPhaseFailed,
		}, v1alpha1.BroodOperationPhaseFailed, true},

		// §2 Rule 3: all terminal (Succeeded/Canceled mix)
		{"Succeeded/Canceled mix", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhaseSucceeded,
			v1alpha1.BroodOperationPhaseCanceled,
		}, v1alpha1.BroodOperationPhaseCanceled, true},

		// §2 Rule 4: every non-terminal child Suspended (≥1 Suspended)
		{"all Suspended", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhaseSuspended,
			v1alpha1.BroodOperationPhaseSuspended,
		}, v1alpha1.BroodOperationPhaseSuspended, true},
		{"Suspended + Succeeded (non-terminal—terminal mix, but non-terminal is Suspended)",
			[]v1alpha1.BroodOperationPhase{
				v1alpha1.BroodOperationPhaseSuspended,
				v1alpha1.BroodOperationPhaseSucceeded,
			}, v1alpha1.BroodOperationPhaseSuspended, true},
		{"Suspended + Canceled",
			[]v1alpha1.BroodOperationPhase{
				v1alpha1.BroodOperationPhaseSuspended,
				v1alpha1.BroodOperationPhaseCanceled,
			}, v1alpha1.BroodOperationPhaseSuspended, true},

		// §2 Rule 5: all Pending
		{"all Pending", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhasePending,
			v1alpha1.BroodOperationPhasePending,
		}, v1alpha1.BroodOperationPhasePending, true},

		// §2 Rule 6: otherwise Running
		{"Pending + Succeeded → Running", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhasePending,
			v1alpha1.BroodOperationPhaseSucceeded,
		}, v1alpha1.BroodOperationPhaseRunning, true},
		{"Suspended + Pending → Running", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhaseSuspended,
			v1alpha1.BroodOperationPhasePending,
		}, v1alpha1.BroodOperationPhaseRunning, true},
		{"Running + Succeeded → Running", []v1alpha1.BroodOperationPhase{
			v1alpha1.BroodOperationPhaseRunning,
			v1alpha1.BroodOperationPhaseSucceeded,
		}, v1alpha1.BroodOperationPhaseRunning, true},

		// Empty-string phase counts as Pending.
		{"empty-string phase → Pending", []v1alpha1.BroodOperationPhase{""}, v1alpha1.BroodOperationPhasePending, true},
		{"empty + Succeeded → Running", []v1alpha1.BroodOperationPhase{"", v1alpha1.BroodOperationPhaseSucceeded}, v1alpha1.BroodOperationPhaseRunning, true},
		{"empty + empty → Pending", []v1alpha1.BroodOperationPhase{"", ""}, v1alpha1.BroodOperationPhasePending, true},

		// Empty-set guard.
		{"empty slice → ok=false", []v1alpha1.BroodOperationPhase{}, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := aggregatePhase(tt.phases)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("phase = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSumSummaries(t *testing.T) {
	summaries := []v1alpha1.BroodSummary{
		{Total: 5, Succeeded: 3, Failed: 1, Skipped: 1},
		{Total: 3, Succeeded: 2, Failed: 1, Skipped: 0},
	}
	got := sumSummaries(summaries)
	want := v1alpha1.BroodSummary{Total: 8, Succeeded: 5, Failed: 2, Skipped: 1}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestGroupRuns(t *testing.T) {
	ops := []ClusterBroodOp{
		{Cluster: "dev-cluster", Op: &v1alpha1.BroodOperation{}},
		{Cluster: "core", Op: &v1alpha1.BroodOperation{}},
	}
	// Set namespace/name on the ops.
	ops[0].Op.Namespace = "ns1"
	ops[0].Op.Name = "run-a"
	ops[1].Op.Namespace = "ns1"
	ops[1].Op.Name = "run-a"

	groups := groupRuns(ops)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].Children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(groups[0].Children))
	}
	// Member clusters should be sorted.
	if groups[0].Children[0].Cluster != "core" || groups[0].Children[1].Cluster != "dev-cluster" {
		t.Errorf("expected sorted clusters core,dev-cluster, got %s,%s",
			groups[0].Children[0].Cluster, groups[0].Children[1].Cluster)
	}
}
