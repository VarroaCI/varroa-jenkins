package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestTeamDeepCopyIsolatesStaleNamespaces mutates a deep copy of a populated
// Team and asserts the original stays unchanged. DeepCopy for this kind is
// hand-written, so this round trip is its only acceptance.
func TestTeamDeepCopyIsolatesStaleNamespaces(t *testing.T) {
	orig := Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status: TeamStatus{
			Placement: &TeamPlacementStatus{
				Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1,
			},
			Pipelines: &TeamPipelinesStatus{
				WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns",
			},
			Conditions: []TeamCondition{{
				Type: TeamConditionPlaced, Status: metav1.ConditionTrue, Reason: TeamReasonTenantPlaced,
			}},
			StaleAgentServiceNamespaces: []string{"ns-a", "ns-b"},
		},
	}

	copied := orig.DeepCopy()
	copied.Status.StaleAgentServiceNamespaces[0] = "changed"
	copied.Status.StaleAgentServiceNamespaces = append(copied.Status.StaleAgentServiceNamespaces, "ns-c")
	copied.Status.Placement.Cell = "cell-b"

	if orig.Status.StaleAgentServiceNamespaces[0] != "ns-a" {
		t.Errorf("the copy changed the original entry: %v", orig.Status.StaleAgentServiceNamespaces)
	}
	if len(orig.Status.StaleAgentServiceNamespaces) != 2 {
		t.Errorf("the copy changed the original length: %v", orig.Status.StaleAgentServiceNamespaces)
	}
	if orig.Status.Placement.Cell != "cell-a" {
		t.Errorf("the copy changed the original placement: %+v", orig.Status.Placement)
	}
}

// TestTeamDeepCopyKeepsNilStaleNamespaces asserts that an absent list stays
// absent, so a copy never writes an empty list into a status patch.
func TestTeamDeepCopyKeepsNilStaleNamespaces(t *testing.T) {
	orig := Team{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	if got := orig.DeepCopy().Status.StaleAgentServiceNamespaces; got != nil {
		t.Errorf("the copy holds %v, want nil", got)
	}
}
