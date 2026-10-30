package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// overviewRecentEntry pairs a recent run with its start time, so the rows
// of several scopes merge in time order.
type overviewRecentEntry struct {
	at  time.Time
	run RecentRunResult
}

// PipelinesOverview answers the body of GET /pipelines/overview. It reads the
// tenants the caller may read, the window counts of their scopes, and the held
// fleet locks that the caller may read. A failed lock read logs a warning and
// counts no lock. It does not fail the overview. A *ServiceError answers 503
// when no record store is configured.
func (s *Server) PipelinesOverview(ctx context.Context, claims *auth.Claims, now time.Time) (PipelinesOverview, *ServiceError, error) {
	if s.deps.RecordStore == nil {
		return PipelinesOverview{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}, nil
	}
	readable, _, err := s.readablePipelineTenants(ctx, claims)
	if err != nil {
		return PipelinesOverview{}, nil, err
	}
	scopes, names := s.runScopes(claims, readable)
	ov, err := s.deps.RecordStore.RunsOverview(ctx, recordstore.RunsOverviewQuery{Scopes: scopes, Now: now})
	if err != nil {
		return PipelinesOverview{}, nil, err
	}

	rail := make(map[string]*PipelinesOverviewTenant)
	clusterSets := make(map[string]map[string]bool)
	recent := make(map[string][]overviewRecentEntry)
	row := func(name string) *PipelinesOverviewTenant {
		r, ok := rail[name]
		if !ok {
			r = &PipelinesOverviewTenant{Name: name, Clusters: []string{}, Recent: []RecentRunResult{}}
			rail[name] = r
			clusterSets[name] = make(map[string]bool)
		}
		return r
	}
	for _, t := range readable {
		r := row(t.Row.Name)
		if t.Placement == placementPlaced {
			r.Placement = placementPlaced
		} else if r.Placement == "" {
			r.Placement = t.Placement
		}
	}
	for _, so := range ov.Scopes {
		for _, name := range names[so.Cluster+"/"+so.Tenant] {
			r := row(name)
			r.Runs24h += so.Runs24h
			r.Failed24h += so.Failed24h
			r.Running += so.Running
			r.WaitingInput += so.WaitingInput
			clusterSets[name][so.Cluster] = true
			for _, rr := range so.Recent {
				recent[name] = append(recent[name], overviewRecentEntry{
					at:  rr.CreatedAt,
					run: RecentRunResult{RunID: rr.RunID, Status: rr.Status, Result: rr.Result},
				})
			}
		}
	}

	locks := s.overviewLocksHeld(ctx, claims, names)
	allClusters := make(map[string]bool)
	tenants := make([]PipelinesOverviewTenant, 0, len(rail))
	var attention PipelinesOverviewAttention
	for name, r := range rail {
		for cluster := range clusterSets[name] {
			r.Clusters = append(r.Clusters, cluster)
			allClusters[cluster] = true
		}
		sort.Strings(r.Clusters)
		entries := recent[name]
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })
		for i := 0; i < len(entries) && i < recordstore.RecentResultsSize; i++ {
			r.Recent = append(r.Recent, entries[i].run)
		}
		r.LocksHeld = locks[name]
		attention.Failed24h += r.Failed24h
		attention.WaitingInput += r.WaitingInput
		attention.LocksHeld += r.LocksHeld
		if r.Placement == placementAwaitingCell {
			attention.AwaitingCell++
		}
		tenants = append(tenants, *r)
	}
	sort.SliceStable(tenants, func(i, j int) bool {
		a, b := tenants[i], tenants[j]
		if a.Failed24h != b.Failed24h {
			return a.Failed24h > b.Failed24h
		}
		if a.Running != b.Running {
			return a.Running > b.Running
		}
		if a.Runs24h != b.Runs24h {
			return a.Runs24h > b.Runs24h
		}
		return a.Name < b.Name
	})

	out := PipelinesOverview{
		WindowStart:       ov.WindowStart,
		Clusters:          len(allClusters),
		CanReadFleetLocks: s.deps.Authorizer.CanReadFleetLocks(claims),
		Totals:            overviewTotals(ov.Totals),
		Attention:         attention,
		Tenants:           tenants,
	}
	if s.deps.Brood != nil {
		out.LocalCluster = s.deps.Brood.LocalCluster()
	}
	return out, nil, nil
}

// overviewTotals maps the store totals onto the response, and derives the pass
// rate. The pass rate is nil when no run of the window has finished.
func overviewTotals(t recordstore.RunsOverviewTotals) PipelinesOverviewTotals {
	out := PipelinesOverviewTotals{
		Runs24h:           t.Runs24h,
		RunsPrior24h:      t.RunsPrior24h,
		Passed:            t.Passed,
		Failed:            t.Failed,
		Interrupted:       t.Interrupted,
		Finished:          t.Finished,
		Running:           t.Running,
		Queued:            t.Queued,
		MedianQueueWaitMs: t.MedianQueueWaitMs,
		MedianDurationMs:  t.MedianDurationMs,
		P90DurationMs:     t.P90DurationMs,
	}
	if t.Finished > 0 {
		rate := float64(t.Passed) / float64(t.Finished)
		out.PassRate = &rate
	}
	return out
}

// overviewLocksHeld counts the held fleet locks per rail name. A lock counts
// only when the caller may read its job path, and only when its tenant is one
// of the readable scopes. A failed lock read logs a warning and counts none.
func (s *Server) overviewLocksHeld(ctx context.Context, claims *auth.Claims, names map[string][]string) map[string]int64 {
	counts := make(map[string]int64)
	locks, err := s.deps.RecordStore.ListAllFleetLocks(ctx)
	if err != nil {
		s.deps.Logger.Warn("pipelines overview: list fleet locks", "error", err)
		return counts
	}
	grants := make(map[string]fleetTenantGrants)
	for _, lock := range locks {
		if lock.State != "held" {
			continue
		}
		key := lock.Cluster + "/" + lock.Tenant
		owners := names[key]
		if len(owners) == 0 {
			continue
		}
		tg, seen := grants[key]
		if !seen {
			tg = s.fleetTenantGrantsOf(claims, recordstore.Run{Cluster: lock.Cluster, Tenant: lock.Tenant})
			grants[key] = tg
		}
		if tg.ok && grantsAllow(tg.grants, tg.folder, lock.JobPath, "read") {
			for _, name := range owners {
				counts[name]++
			}
		}
	}
	return counts
}

// handlePipelinesOverview serves GET /pipelines/overview.
func (s *Server) handlePipelinesOverview(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	body, serviceErr, err := s.PipelinesOverview(r.Context(), claims, time.Now().UTC())
	if serviceErr != nil {
		s.writeJSONError(w, serviceErr.Status, serviceErr.Message)
		return
	}
	if s.handleReaderError(w, err, "pipelines overview") {
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}
