package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// drainingTenant builds a Tenant whose Placed condition reports TenantMoving.
func drainingTenant(members []v1alpha1.TenantMemberStatus) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status: v1alpha1.TenantStatus{
			Conditions: []v1alpha1.TenantCondition{{
				Type:   v1alpha1.TenantConditionPlaced,
				Status: metav1.ConditionFalse,
				Reason: v1alpha1.TenantReasonTenantMoving,
			}},
			Placement: &v1alpha1.TenantPlacementStatus{
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
	tenant := drainingTenant([]v1alpha1.TenantMemberStatus{
		{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot0},
		{Cell: "cell-b", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: &slot1},
		{Cell: "cell-c", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleStandby},
	})

	for _, name := range []string{"cell-a", "cell-b", "cell-c"} {
		cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cells-ns"}}
		if !tenantIsDrainingFromCell(tenant, cr) {
			t.Errorf("%s does not read the tenant as draining", name)
		}
	}
	other := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-z", Namespace: "cells-ns"}}
	if tenantIsDrainingFromCell(tenant, other) {
		t.Error("a cell holding no member reads the tenant as draining")
	}
}

// TestTenantIsDrainingFromCellReadsTheScalarForAnEmptyList asserts a
// tenant with no stored member list still drains from its scalar cell.
func TestTenantIsDrainingFromCellReadsTheScalarForAnEmptyList(t *testing.T) {
	tenant := drainingTenant(nil)
	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "cells-ns"}}
	if !tenantIsDrainingFromCell(tenant, cr) {
		t.Error("an empty member list does not read the scalar cell")
	}
}
