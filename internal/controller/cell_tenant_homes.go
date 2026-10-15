package controller

import (
	"context"
	"sort"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// attachTenantHomes stamps one entry for each tenant this cell hosts onto
// the desired state: the slot this cell holds for the tenant, the role,
// and the tenant's job-path to slot map.
//
// The operator computes no home. It reads the map from the recorder over
// the tenant.sync "homes" kind, which the recorder owns. A failed call
// leaves the previous map in place for that tick and retries on the next
// assembly.
func (r *Reconciler) attachTenantHomes(ctx context.Context, cr *v1alpha1.Controller,
	teams []*v1alpha1.Team, desired *mitev1.DesiredStateCommand) {
	if desired == nil || !isCellController(cr) || len(teams) == 0 {
		return
	}

	var entries []*mitev1.TenantHomes
	for _, team := range teams {
		if team.Spec.Pipelines == nil {
			continue
		}
		result := r.tenantHomes(ctx, cr, team.Name)
		entry := &mitev1.TenantHomes{
			Tenant:   team.Name,
			Slot:     int(tenantSlotOnCell(team, cr)),
			Role:     tenantRoleOnCell(team, cr),
			JobHomes: intJobHomes(result.JobHomes),
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Tenant < entries[j].Tenant })
	desired.TenantHomes = entries
}

// tenantHomes answers the recorder's newest home map for one tenant, and
// the cached previous answer when the call fails.
func (r *Reconciler) tenantHomes(ctx context.Context, cr *v1alpha1.Controller, tenant string) cells.HomesResult {
	key := cr.Namespace + "/" + cr.Name + "/" + tenant
	if r.homesFunc == nil {
		return r.cachedHomes(key)
	}
	result, err := r.homesFunc(ctx, cells.HomesRequest{Cluster: r.clusterName, Tenant: tenant})
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("read the tenant home map",
				"controller", cr.Namespace+"/"+cr.Name, "tenant", tenant, "error", err)
		}
		return r.cachedHomes(key)
	}
	r.storeHomes(key, result)
	return result
}

// cachedHomes answers the last successful reply for key, or the zero
// value when none was stored.
func (r *Reconciler) cachedHomes(key string) cells.HomesResult {
	r.homesMu.Lock()
	defer r.homesMu.Unlock()
	return r.homesCache[key]
}

// storeHomes saves one successful reply under key.
func (r *Reconciler) storeHomes(key string, result cells.HomesResult) {
	r.homesMu.Lock()
	defer r.homesMu.Unlock()
	if r.homesCache == nil {
		r.homesCache = map[string]cells.HomesResult{}
	}
	r.homesCache[key] = result
}

// tenantRoleOnCell answers the role the tenant holds on cr: standby for a
// standby member, and active for every other case, including a tenant
// with no member list.
func tenantRoleOnCell(team *v1alpha1.Team, cr *v1alpha1.Controller) string {
	if team.Status.Placement == nil {
		return v1alpha1.TeamMemberRoleActive
	}
	for _, m := range team.Status.Placement.Members {
		if m.Cell == cr.Name && m.CellNamespace == cr.Namespace {
			if m.Role == v1alpha1.TeamMemberRoleStandby {
				return v1alpha1.TeamMemberRoleStandby
			}
			return v1alpha1.TeamMemberRoleActive
		}
	}
	return v1alpha1.TeamMemberRoleActive
}

// intJobHomes converts the recorder's int32 slot indexes to the generated
// proto map's int values.
func intJobHomes(homes map[string]int32) map[string]int {
	if len(homes) == 0 {
		return nil
	}
	out := make(map[string]int, len(homes))
	for path, slot := range homes {
		out[path] = int(slot)
	}
	return out
}
