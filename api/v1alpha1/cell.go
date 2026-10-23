package v1alpha1

import (
	"sort"

	"k8s.io/apimachinery/pkg/types"
)

// IsCell reports whether cr runs as a cell: one disposable Jenkins JVM that
// hosts several pipelines tenants as top-level folders.
//
// This is the single answer to that question. A second copy of the
// spec.cell.enabled test drifts from this one, the same way a direct read of
// spec.headless drifts from IsHeadless.
func IsCell(cr *Controller) bool {
	if cr == nil {
		return false
	}
	return cr.Spec.Cell != nil && cr.Spec.Cell.Enabled
}

// CellHoldsTenant reports whether the cell cr holds tenant: its
// status.cell.tenantWarnings carries an ItemsHeld entry for tenant.
// A held tenant's declared jobs are not the cell's jobs, so a read
// of the tenant's definition declares no job while this is true.
// It answers for one cell. A reader of a Tenant checks every member cell
// that PlacementCells lists.
func CellHoldsTenant(cr *Controller, tenant string) bool {
	if cr == nil || cr.Status.Cell == nil || tenant == "" {
		return false
	}
	for _, w := range cr.Status.Cell.TenantWarnings {
		if w.Tenant == tenant && w.Reason == TenantReasonItemsHeld {
			return true
		}
	}
	return false
}

// PlacementCells lists the cells that a tenant placement names, in reading
// order: the scalar slot-0 cell, then the active members by slot, then the
// standby members by namespace/name. Each cell appears once. Every reader
// of a Tenant's member cells uses this order.
func PlacementCells(p *TenantPlacementStatus) []types.NamespacedName {
	if p == nil {
		return nil
	}
	var out []types.NamespacedName
	seen := map[string]bool{}
	add := func(namespace, name string) {
		key := namespace + "/" + name
		if name == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, types.NamespacedName{Namespace: namespace, Name: name})
	}
	add(p.CellNamespace, p.Cell)
	var active, standby []TenantMemberStatus
	for _, m := range p.Members {
		if m.Slot != nil {
			active = append(active, m)
		} else {
			standby = append(standby, m)
		}
	}
	sort.SliceStable(active, func(i, j int) bool { return *active[i].Slot < *active[j].Slot })
	sort.SliceStable(standby, func(i, j int) bool {
		return standby[i].CellNamespace+"/"+standby[i].Cell < standby[j].CellNamespace+"/"+standby[j].Cell
	})
	for _, m := range active {
		add(m.CellNamespace, m.Cell)
	}
	for _, m := range standby {
		add(m.CellNamespace, m.Cell)
	}
	return out
}
