package api

import (
	"context"
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// ListTenantFleetRuns answers one page of the runs of one job of one tenant, as
// fleet rows. It resolves the tenant and admits its refs the way ListRuns does,
// and it merges the pages through the same cross-tenant cursor. Each run maps
// through buildFleetRunRow, and every row carries the tenant argument as its
// rail name.
func (s *Server) ListTenantFleetRuns(ctx context.Context, claims *auth.Claims, tenant, jobPath, branch, result, actor, cause string, limit int, cursor string) (FleetRunPage, *ServiceError, error) {
	if s.deps.RecordStore == nil {
		return FleetRunPage{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}, nil
	}
	refs, serr := s.resolveTenantTenants(ctx, claims, tenant)
	if serr != nil {
		return FleetRunPage{}, serr, nil
	}
	var admitted []TenantRef
	for _, ref := range refs {
		if s.canReadPipeline(claims, ref, jobPath) {
			admitted = append(admitted, ref)
		}
	}
	if len(admitted) == 0 {
		return FleetRunPage{}, &ServiceError{Status: http.StatusNotFound, Message: "job not found"}, nil
	}
	filters := listRunsFilters{branch: branch, result: result, actor: actor, cause: cause}
	merged, next, serr, err := s.mergeRunPages(ctx, admitted, jobPath, filters, limit, cursor)
	if serr != nil || err != nil {
		return FleetRunPage{}, serr, err
	}
	grants := make(map[string]fleetTenantGrants)
	items := make([]FleetRunRow, 0, len(merged))
	for _, run := range merged {
		key := run.Cluster + "/" + run.Tenant
		tg, seen := grants[key]
		if !seen {
			tg = s.fleetTenantGrantsOf(claims, run)
			grants[key] = tg
		}
		row := buildFleetRunRow(run, tenant, tg.ok && grantsAllow(tg.grants, tg.folder, run.JobPath, "write"))
		s.redactFleetUpstream(claims, run, &row)
		items = append(items, row)
	}
	return FleetRunPage{Items: items, NextCursor: next}, nil, nil
}
