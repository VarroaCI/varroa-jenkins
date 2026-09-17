package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// listPlacedTeams reads each Team named in cr.Status.Cell.Tenants, once per
// tick (design D7). A missing Team is skipped and logged: it does not fail
// the tick, and it leaves every other tenant unaffected. The controller
// reconciler needs each Team's spec.pipelines for the bundle reference and
// the agent namespace, and each Team's Placed condition for the drain
// filter (design D4).
func (r *Reconciler) listPlacedTeams(ctx context.Context, cr *v1alpha1.Controller) []*v1alpha1.Team {
	if cr.Status.Cell == nil {
		return nil
	}
	teams := make([]*v1alpha1.Team, 0, len(cr.Status.Cell.Tenants))
	for _, name := range cr.Status.Cell.Tenants {
		team, err := crdstore.Get[v1alpha1.Team](ctx, r.store, name, "")
		if err != nil {
			if r.Logger != nil {
				r.Logger.Warn("cell content: tenant Team not found", "team", name,
					"controller", cr.Namespace+"/"+cr.Name, "error", err)
			}
			continue
		}
		teams = append(teams, team)
	}
	return teams
}

// findTeamCondition returns the named condition, or nil when absent.
func findTeamCondition(conds []v1alpha1.TeamCondition, condType string) *v1alpha1.TeamCondition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// tenantIsDrainingFromCell reports whether team is draining away from cr:
// its Placed condition is False with reason TenantMoving, and its
// placement still names this cell (design D4, tenant-isolation spec). A
// draining tenant's folder is omitted from the merged manifest, which is
// what makes the voluntary move's idle test converge.
func tenantIsDrainingFromCell(team *v1alpha1.Team, cr *v1alpha1.Controller) bool {
	cond := findTeamCondition(team.Status.Conditions, v1alpha1.TeamConditionPlaced)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TeamReasonTenantMoving {
		return false
	}
	// A draining tenant loses its folder on every member's cell, so the
	// voluntary move's idle test converges on all of them.
	p := team.Status.Placement
	if p == nil {
		return false
	}
	for _, m := range p.Members {
		if m.Cell == cr.Name && m.CellNamespace == cr.Namespace {
			return true
		}
	}
	return p.Cell == cr.Name && p.CellNamespace == cr.Namespace
}
