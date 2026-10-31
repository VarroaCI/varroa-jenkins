package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// FleetRunsParams holds the query of GET /pipelines/runs. Tenants names
// rail names. An empty list reads every readable tenant. Status is QUEUED,
// RUNNING, COMPLETED, or WAITING_INPUT.
type FleetRunsParams struct {
	Tenants                                             []string
	Job, Trigger, Status, Result, Branch, Actor, Search string
	Since                                               time.Time
	Sort                                                recordstore.RunSort
	Ascending                                           bool
	Limit                                               int
	Cursor                                              string
}

// maxTriggerRefillPages bounds the store pages that one trigger-filtered
// feed page may read. A rare trigger over a long history then answers a
// short page with a cursor, never a long scan.
const maxTriggerRefillPages = 5

// fleetSorts maps each accepted sort name to its key.
var fleetSorts = map[string]recordstore.RunSort{
	"started":  recordstore.RunSortStarted,
	"duration": recordstore.RunSortDuration,
	"tenant":   recordstore.RunSortTenant,
	"job":      recordstore.RunSortJob,
	"status":   recordstore.RunSortStatus,
	"actor":    recordstore.RunSortActor,
}

// fleetStatuses lists the accepted status filters.
var fleetStatuses = map[string]bool{"QUEUED": true, "RUNNING": true, "COMPLETED": true, recordstore.RunStatusWaitingInput: true}

// fleetTriggers lists the accepted trigger filters.
var fleetTriggers = map[string]bool{
	triggerKindPullRequest: true, triggerKindPush: true, triggerKindCron: true,
	triggerKindManual: true, triggerKindUpstream: true, triggerKindWebhook: true,
	triggerKindReplay: true, triggerKindRestart: true, triggerKindOther: true,
}

// parseFleetRunsParams reads the query of GET /pipelines/runs. An unknown
// sort, order, status, or trigger is an error, and so is a since value that
// is neither a positive duration nor an RFC 3339 time. The limit uses the
// same rule as the per-tenant run list.
func parseFleetRunsParams(r *http.Request, now time.Time) (FleetRunsParams, error) {
	return ParseFleetRunsQuery(r.URL.Query(), now)
}

// ParseFleetRunsQuery reads the fleet run query from its values. The HTTP route
// and the list_runs tool share it, so both refuse the same values.
func ParseFleetRunsQuery(q url.Values, now time.Time) (FleetRunsParams, error) {
	p := FleetRunsParams{
		Tenants: q["tenant"],
		Job:     q.Get("job"),
		Trigger: q.Get("trigger"),
		Status:  q.Get("status"),
		Result:  q.Get("result"),
		Branch:  q.Get("branch"),
		Actor:   q.Get("actor"),
		Search:  q.Get("q"),
		Cursor:  q.Get("cursor"),
	}
	if p.Trigger != "" && !fleetTriggers[p.Trigger] {
		return FleetRunsParams{}, fmt.Errorf("unknown trigger %q", p.Trigger)
	}
	if p.Status != "" && !fleetStatuses[p.Status] {
		return FleetRunsParams{}, fmt.Errorf("unknown status %q", p.Status)
	}
	sortName := q.Get("sort")
	if sortName == "" {
		sortName = "started"
	}
	sortKey, ok := fleetSorts[sortName]
	if !ok {
		return FleetRunsParams{}, fmt.Errorf("unknown sort %q", sortName)
	}
	p.Sort = sortKey
	switch q.Get("order") {
	case "", "desc":
		p.Ascending = false
	case "asc":
		p.Ascending = true
	default:
		return FleetRunsParams{}, fmt.Errorf("unknown order %q", q.Get("order"))
	}
	if raw := q.Get("since"); raw != "" {
		since, err := parseFleetSince(raw, now)
		if err != nil {
			return FleetRunsParams{}, err
		}
		p.Since = since
	}
	limit, err := runsLimitValue(q.Get("limit"))
	if err != nil {
		return FleetRunsParams{}, err
	}
	p.Limit = limit
	return p, nil
}

// parseFleetSince answers the lower bound that a since value names. A positive
// Go duration counts back from now. Otherwise the value is an RFC 3339 time.
func parseFleetSince(raw string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return time.Time{}, errors.New("since must be a positive duration or an RFC 3339 time")
		}
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errors.New("since must be a positive duration or an RFC 3339 time")
	}
	return t, nil
}

// ListFleetRuns answers one page of the fleet run feed. It reads only the
// scopes of the tenants the caller may read, and a tenant filter names rail
// names within that set. A filter that names no readable tenant answers an
// empty page and reads nothing. A trigger filter refills the page from up to
// maxTriggerRefillPages store pages. A *ServiceError answers a client error.
// A non-nil error is a store failure.
func (s *Server) ListFleetRuns(ctx context.Context, claims *auth.Claims, p FleetRunsParams) (FleetRunPage, *ServiceError, error) {
	page := FleetRunPage{Items: []FleetRunRow{}}
	if s.deps.RecordStore == nil {
		return page, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}, nil
	}
	walk, err := s.readablePipelineTenants(ctx, claims)
	if err != nil {
		return page, nil, err
	}
	// A degraded placement read stops the feed here. A run read would pay a
	// second connect timeout against the same store.
	if walk.Degraded != nil {
		return page, nil, walk.Degraded
	}
	scopes, names := s.runScopes(claims, walk.Items)
	wanted := make(map[string]bool, len(p.Tenants))
	for _, name := range p.Tenants {
		wanted[name] = true
	}
	if len(p.Tenants) > 0 {
		var kept []recordstore.RunScope
		for _, scope := range scopes {
			if anyWanted(wanted, names[scope.Cluster+"/"+scope.Tenant]) {
				kept = append(kept, scope)
			}
		}
		scopes = kept
	}
	if len(scopes) == 0 {
		return page, nil, nil
	}

	query := recordstore.RunsAcrossQuery{
		Scopes:    scopes,
		JobPath:   p.Job,
		Branch:    p.Branch,
		Actor:     p.Actor,
		Result:    p.Result,
		Status:    p.Status,
		Since:     p.Since,
		Search:    p.Search,
		Sort:      p.Sort,
		Ascending: p.Ascending,
		Limit:     p.Limit,
		Cursor:    p.Cursor,
	}
	grants := make(map[string]fleetTenantGrants)
	cursor := p.Cursor
	var lastNext string
	for pages := 0; pages < maxTriggerRefillPages; pages++ {
		query.Cursor = cursor
		rp, err := s.deps.RecordStore.ListRunsAcross(ctx, query)
		if errors.Is(err, recordstore.ErrInvalidCursor) {
			return FleetRunPage{}, &ServiceError{Status: http.StatusBadRequest, Message: "invalid cursor"}, nil
		}
		if err != nil {
			return FleetRunPage{}, nil, err
		}
		lastNext = rp.NextCursor
		for i, run := range rp.Runs {
			key := run.Cluster + "/" + run.Tenant
			tg, seen := grants[key]
			if !seen {
				tg = s.fleetTenantGrantsOf(claims, run)
				grants[key] = tg
			}
			railName := run.Tenant
			if owners := names[key]; len(owners) > 0 {
				railName = owners[0]
				// A requested rail name labels its own rows.
				for _, o := range owners {
					if wanted[o] {
						railName = o
						break
					}
				}
			}
			row := buildFleetRunRow(run, railName, tg.ok && grantsAllow(tg.grants, tg.folder, run.JobPath, "write"))
			s.redactFleetUpstream(claims, run, &row)
			if p.Trigger != "" && row.Trigger.Kind != p.Trigger {
				continue
			}
			page.Items = append(page.Items, row)
			if len(page.Items) == p.Limit {
				if i < len(rp.Runs)-1 || rp.NextCursor != "" {
					page.NextCursor = recordstore.EncodeRunsAcrossCursor(p.Sort, p.Ascending, run)
				}
				return page, nil, nil
			}
		}
		if rp.NextCursor == "" {
			return page, nil, nil
		}
		cursor = rp.NextCursor
	}
	page.NextCursor = lastNext
	return page, nil, nil
}

// anyWanted reports whether one of owners is in wanted.
func anyWanted(wanted map[string]bool, owners []string) bool {
	for _, o := range owners {
		if wanted[o] {
			return true
		}
	}
	return false
}

// redactFleetUpstream applies the upstream redaction of GET /runs/{run} to a
// fleet row. A caller who may not read the upstream run sees its tenant only,
// and the trigger label drops the job path with it.
func (s *Server) redactFleetUpstream(claims *auth.Claims, run recordstore.Run, row *FleetRunRow) {
	link := s.upstreamRunLink(claims, run)
	if link == nil {
		return
	}
	if link.JobPath == "" {
		row.Upstream = &FleetRunUpstream{Tenant: link.Tenant}
		row.Trigger.Label = "Upstream"
		return
	}
	row.Upstream = &FleetRunUpstream{Tenant: link.Tenant, Job: link.JobPath, BuildNumber: link.BuildNumber, RunID: link.RunID}
}

// fleetTenantGrants holds the grants of the caller on one run's tenant, and
// the folder that grantsAllow tests job paths against. ok is false when the
// tenant value does not resolve.
type fleetTenantGrants struct {
	grants []rbac.TenantGrant
	folder string
	ok     bool
}

// fleetTenantGrantsOf resolves the caller's grants on the run's tenant once.
// Each run of the tenant then tests its job with grantsAllow, as the
// per-tenant run routes do.
func (s *Server) fleetTenantGrantsOf(claims *auth.Claims, run recordstore.Run) fleetTenantGrants {
	ref, err := TenantRefFromRow(run.Cluster, run.Namespace, run.Cell, run.Tenant)
	if err != nil {
		return fleetTenantGrants{}
	}
	grants, ok := s.deps.Authorizer.tenantGrants(claims, ref.Tenant, ref.Folder)
	return fleetTenantGrants{grants: grants, folder: ref.Folder, ok: ok}
}

// handleListFleetRuns serves GET /pipelines/runs.
func (s *Server) handleListFleetRuns(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	p, err := parseFleetRunsParams(r, time.Now().UTC())
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, serviceErr, err := s.ListFleetRuns(r.Context(), claims, p)
	if serviceErr != nil {
		s.writeJSONError(w, serviceErr.Status, serviceErr.Message)
		return
	}
	if s.handleReaderError(w, err, "list fleet runs") {
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}
