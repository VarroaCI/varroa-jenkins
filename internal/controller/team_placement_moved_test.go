package controller

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestTeamReconciler_PlacementMovedPublishesOnMoveOnly checks that a first bind
// yields no event, that a move to another cell yields one, and that an
// unchanged cell yields none.
func TestTeamReconciler_PlacementMovedPublishesOnMoveOnly(t *testing.T) {
	bound := &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell-b", CellNamespace: "varroa-system"}

	if _, ok := placementMovedEvent("team-a", "", bound); ok {
		t.Fatal("a first bind must publish nothing")
	}
	if _, ok := placementMovedEvent("team-a", "cell-b", bound); ok {
		t.Fatal("an unchanged cell must publish nothing")
	}
	ev, ok := placementMovedEvent("team-a", "cell-a", bound)
	if !ok {
		t.Fatal("a move must publish one event")
	}
	if ev.Type != "tenant.placement_moved" || ev.Source != "operator" || ev.Reason != "cell-a" ||
		ev.Controller != "cell-b" || ev.Namespace != "varroa-system" || ev.ItemPath != "team-a" {
		t.Errorf("event = %+v", ev)
	}
}
