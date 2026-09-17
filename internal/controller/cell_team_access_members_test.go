package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// drainingTeam builds a Team whose Placed condition reports TenantMoving.
func drainingTeam(members []v1alpha1.TeamMemberStatus) *v1alpha1.Team {
	return &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status: v1alpha1.TeamStatus{
			Conditions: []v1alpha1.TeamCondition{{
				Type:   v1alpha1.TeamConditionPlaced,
				Status: metav1.ConditionFalse,
				Reason: v1alpha1.TeamReasonTenantMoving,
			}},
			Placement: &v1alpha1.TeamPlacementStatus{
				Cell: "cell-a", CellNamespace: "cells-ns", Members: members,
			},
		},
	}
}

// TestTenantIsDrainingFromCellCoversEveryMember asserts a draining tenant
// loses its folder on every member's cell, so the voluntary move's idle
// test converges on all of them.
func TestTenantIsDrainingFromCellCoversEveryMember(t *testing.T) {
	slot0, slot1 := int32(0), int32(1)
	team := drainingTeam([]v1alpha1.TeamMemberStatus{
		{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TeamMemberRoleActive, Slot: &slot0},
		{Cell: "cell-b", CellNamespace: "cells-ns", Role: v1alpha1.TeamMemberRoleActive, Slot: &slot1},
		{Cell: "cell-c", CellNamespace: "cells-ns", Role: v1alpha1.TeamMemberRoleStandby},
	})

	for _, name := range []string{"cell-a", "cell-b", "cell-c"} {
		cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cells-ns"}}
		if !tenantIsDrainingFromCell(team, cr) {
			t.Errorf("%s does not read the tenant as draining", name)
		}
	}
	other := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-z", Namespace: "cells-ns"}}
	if tenantIsDrainingFromCell(team, other) {
		t.Error("a cell holding no member reads the tenant as draining")
	}
}

// TestTenantIsDrainingFromCellReadsTheScalarForAnEmptyList asserts a
// tenant with no stored member list still drains from its scalar cell.
func TestTenantIsDrainingFromCellReadsTheScalarForAnEmptyList(t *testing.T) {
	team := drainingTeam(nil)
	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "cells-ns"}}
	if !tenantIsDrainingFromCell(team, cr) {
		t.Error("an empty member list does not read the scalar cell")
	}
}
