package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
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
	walk, err := s.readablePipelineTenants(ctx, claims)
	if err != nil {
		return PipelinesOverview{}, nil, err
	}
	// A degraded placement read stops the overview here. The run read would
	// pay a second connect timeout against the same store.
	if walk.Degraded != nil {
		return PipelinesOverview{}, nil, walk.Degraded
	}
	readable := walk.Items
	scopes, names := s.runScopes(claims, readable)
	ov, err := s.deps.RecordStore.RunsOverview(ctx, recordstore.RunsOverviewQuery{Scopes: scopes, Now: now})
	if err != nil {
		return PipelinesOverview{}, nil, err
	}
	attn, err := s.overviewAttention(ctx, claims, scopes, names, now)
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
		Hourly:            overviewHours(ov.WindowStart, ov.Hourly),
		WaitingInputs:     attn.waitingInputs,
		MoreWaitingInputs: attn.moreWaitingInputs,
		FailingJobs:       attn.failingJobs,
		MoreFailingJobs:   attn.moreFailingJobs,
		Running:           attn.running,
		NextScheduled:     attn.nextScheduled,
	}
	if s.deps.Brood != nil {
		out.LocalCluster = s.deps.Brood.LocalCluster()
	}
	return out, nil, nil
}

// overviewTotals maps the store totals onto the response, and derives the pass
// rate. The rate divides the passed runs by the counted runs, which leave out
// INTERRUPTED and NOT_BUILT. The rate is nil when no run of the window counts.
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
	if t.Counted > 0 {
		rate := float64(t.Passed) / float64(t.Counted)
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

// Caps of the attention reads. The overview lists at most this many runs
// waiting for input and this many failing jobs, and it sends a more flag when
// the store has more.
const (
	overviewWaitingInputLimit = 20
	overviewFailingJobLimit   = 20
	overviewRunningLimit      = 10
	overviewRunningShown      = 3
	overviewStageErrorRunes   = 200
)

// overviewAttention holds the attention reads of the overview: the runs that
// wait for input, the failing jobs, the runs in progress, and the next cron run.
type overviewAttention struct {
	waitingInputs     []PipelinesOverviewWaitingInput
	moreWaitingInputs bool
	failingJobs       []PipelinesOverviewFailingJob
	moreFailingJobs   bool
	running           []PipelinesOverviewRunningRun
	nextScheduled     *PipelinesOverviewScheduledRun
}

// overviewAttention runs the attention reads in order: the waiting inputs, the
// running runs, the failing jobs, and the next cron run. An error from any of
// them fails the overview, because a silently dropped item would mislead the
// reader. With no scope it reads nothing and answers empty lists.
func (s *Server) overviewAttention(ctx context.Context, claims *auth.Claims, scopes []recordstore.RunScope, names map[string][]string, now time.Time) (overviewAttention, error) {
	out := overviewAttention{
		waitingInputs: []PipelinesOverviewWaitingInput{},
		failingJobs:   []PipelinesOverviewFailingJob{},
		running:       []PipelinesOverviewRunningRun{},
	}
	if len(scopes) == 0 {
		return out, nil
	}
	grants := make(map[string]fleetTenantGrants)

	waiting, err := s.deps.RecordStore.ListRunsAcross(ctx, recordstore.RunsAcrossQuery{
		Scopes: scopes, Status: recordstore.RunStatusWaitingInput,
		Sort: recordstore.RunSortStarted, Ascending: true, Limit: overviewWaitingInputLimit,
	})
	if err != nil {
		return overviewAttention{}, err
	}
	for _, run := range waiting.Runs {
		if run.Status != "RUNNING" {
			continue
		}
		var inputs []recordstore.StoredRunInput
		if json.Unmarshal(run.Inputs, &inputs) != nil || len(inputs) == 0 {
			continue
		}
		first := inputs[0]
		out.waitingInputs = append(out.waitingInputs, PipelinesOverviewWaitingInput{
			Run: s.fleetRowFor(claims, run, names, nil, grants),
			Input: PipelinesOverviewInput{
				ID: first.ID, Message: first.Message, OK: first.OK, Submitter: first.Submitter,
				ParameterCount: len(first.Parameters),
			},
			InputCount: len(inputs),
		})
	}
	out.moreWaitingInputs = waiting.NextCursor != ""

	running, err := s.overviewRunning(ctx, claims, scopes, names, grants)
	if err != nil {
		return overviewAttention{}, err
	}
	out.running = running

	failing, err := s.deps.RecordStore.FailingJobs(ctx, recordstore.FailingJobsQuery{
		Scopes: scopes, Since: now.Add(-recordstore.FailingJobsWindow), Limit: overviewFailingJobLimit,
	})
	if err != nil {
		return overviewAttention{}, err
	}
	for _, job := range failing.Jobs {
		out.failingJobs = append(out.failingJobs, s.overviewFailingJob(claims, job, names, grants))
	}
	out.moreFailingJobs = failing.More

	next, ok, err := s.deps.RecordStore.NextCronRun(ctx, scopes, now)
	if err != nil {
		return overviewAttention{}, err
	}
	if ok {
		folder := ""
		if parsed, err := rbac.ParseTenant(next.Tenant); err == nil {
			folder = parsed.Folder
		}
		out.nextScheduled = &PipelinesOverviewScheduledRun{
			Tenant:       railNameFor(names, next.Cluster, next.Tenant),
			TenantFolder: folder,
			Cluster:      next.Cluster,
			JobPath:      next.JobPath,
			At:           next.At,
		}
	}
	return out, nil
}

// overviewRunning answers up to overviewRunningShown runs in progress that wait
// for no answer, with the top-level stage of each one when the store knows it.
func (s *Server) overviewRunning(ctx context.Context, claims *auth.Claims, scopes []recordstore.RunScope, names map[string][]string, grants map[string]fleetTenantGrants) ([]PipelinesOverviewRunningRun, error) {
	page, err := s.deps.RecordStore.ListRunsAcross(ctx, recordstore.RunsAcrossQuery{
		Scopes: scopes, Status: "RUNNING", Sort: recordstore.RunSortStarted, Limit: overviewRunningLimit,
	})
	if err != nil {
		return nil, err
	}
	var rows []FleetRunRow
	for _, run := range page.Runs {
		row := s.fleetRowFor(claims, run, names, nil, grants)
		if row.Status != "RUNNING" || row.PendingInput {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) > overviewRunningShown {
		rows = rows[:overviewRunningShown]
	}
	out := make([]PipelinesOverviewRunningRun, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RunID)
	}
	stages, err := s.deps.RecordStore.CurrentStages(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out = append(out, PipelinesOverviewRunningRun{Run: row, Stage: stages[row.RunID]})
	}
	return out, nil
}

// overviewFailingJob maps one failing job of the store onto the overview row.
// The stage is the digest's failed stage. StageError is the first line of that
// stage's error, without ANSI escapes and capped in runes.
func (s *Server) overviewFailingJob(claims *auth.Claims, job recordstore.FailingJob, names map[string][]string, grants map[string]fleetTenantGrants) PipelinesOverviewFailingJob {
	out := PipelinesOverviewFailingJob{
		Run:               s.fleetRowFor(claims, job.Failed, names, nil, grants),
		TestsFailed:       0,
		FailuresSincePass: job.FailuresSincePass,
	}
	if job.Failed.TestsFailed != nil {
		out.TestsFailed = *job.Failed.TestsFailed
	}
	if failed := digestFailedStage(job.FailedStages); failed != nil {
		out.Stage = digestStageName(failed)
		if failed.Error != nil {
			out.StageError = firstErrorLine(*failed.Error)
		}
	}
	if job.LastPassed != nil {
		passed := &PipelinesOverviewPassedRun{
			RunID:     job.LastPassed.RunID,
			CreatedAt: job.LastPassed.CreatedAt,
			EndedAt:   job.LastPassed.EndedAt,
		}
		if job.LastPassed.BuildNumber != nil {
			passed.BuildNumber = *job.LastPassed.BuildNumber
		}
		out.LastPassed = passed
	}
	return out
}

// firstErrorLine answers the first line of a stage error, without ANSI escapes,
// trimmed, and capped at overviewStageErrorRunes runes with an ellipsis.
func firstErrorLine(raw string) string {
	line, _, _ := strings.Cut(stripANSIEscapes(raw), "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) <= overviewStageErrorRunes {
		return line
	}
	return string([]rune(line)[:overviewStageErrorRunes]) + "…"
}

// overviewHours answers the OverviewHours buckets of the window, oldest first.
// It copies the store's buckets by position, and a missing bucket stays zero.
func overviewHours(windowStart time.Time, hours []recordstore.HourCount) []PipelinesOverviewHour {
	out := make([]PipelinesOverviewHour, recordstore.OverviewHours)
	for i := range out {
		out[i].Start = windowStart.Add(time.Duration(i) * time.Hour)
	}
	for i, h := range hours {
		if i < len(out) {
			out[i].Runs = h.Runs
			out[i].Failed = h.Failed
		}
	}
	return out
}
