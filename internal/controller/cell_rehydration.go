package controller

import (
	"context"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/rehydrate"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// cellJenkinsHome is the JENKINS_HOME value every cell's Jenkins
// container carries. review-log.md's task 5.1-5.6 note records that no
// shared operator-side constant exists yet for it: every write site
// (clientset_client.go's StatefulSet builder) hardcodes the literal
// inline. This constant is this batch's own copy of that same literal.
const cellJenkinsHome = "/var/jenkins_home"

// findCellRehydrationEntry finds tenant's own entry in cr's current
// Controller.status.cell.rehydration list (the value from before this
// tick's rebuild), or nil when absent.
func findCellRehydrationEntry(cr *v1alpha1.Controller, tenant string) *v1alpha1.CellRehydrationEntry {
	if cr.Status.Cell == nil {
		return nil
	}
	for i := range cr.Status.Cell.Rehydration {
		if cr.Status.Cell.Rehydration[i].Tenant == tenant {
			return &cr.Status.Cell.Rehydration[i]
		}
	}
	return nil
}

// rehydrationPendingSinceFor answers task 6.3's deadline anchor for
// cellKey/tenant: the first time this generation/cellInstance pairing
// was seen pending. It resets the stored anchor whenever the pairing
// changes, so a fresh placement or a rolled pod starts a fresh clock.
func (r *Reconciler) rehydrationPendingSinceFor(cellKey, tenant string, generation int64, cellInstance string, now time.Time) time.Time {
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	if r.rehydrationPendingSince == nil {
		r.rehydrationPendingSince = make(map[string]rehydrationPendingMark)
	}
	key := cellKey + "/" + tenant
	mark, ok := r.rehydrationPendingSince[key]
	if !ok || mark.Generation != generation || mark.CellInstance != cellInstance {
		mark = rehydrationPendingMark{Generation: generation, CellInstance: cellInstance, Since: now}
		r.rehydrationPendingSince[key] = mark
	}
	return mark.Since
}

// clearRehydrationPendingSince drops cellKey/tenant's deadline anchor
// once its entry counts as applied.
func (r *Reconciler) clearRehydrationPendingSince(cellKey, tenant string) {
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	delete(r.rehydrationPendingSince, cellKey+"/"+tenant)
}

// numberSeededJobsLocked answers cellKey's own in-memory number-seeded-job
// set (design.md D4 order 4, task 5.7), creating it on first use. A
// path enters this set only through commitNumberOnlyProgress, once a
// matching, successful DesiredStateResult confirms the mite wrote it —
// never merely because a tick selected it for sending. The caller holds
// numberCacheMu. The returned map is the live set: a caller that reads
// it after the lock is released must copy it first.
func (r *Reconciler) numberSeededJobsLocked(cellKey string) map[string]bool {
	if r.numberSeededJobs == nil {
		r.numberSeededJobs = make(map[string]map[string]bool)
	}
	seeded, ok := r.numberSeededJobs[cellKey]
	if !ok {
		seeded = make(map[string]bool)
		r.numberSeededJobs[cellKey] = seeded
	}
	return seeded
}

// numberSeededJobsCopy returns a copy of cellKey's confirmed seeded set,
// safe to read and mutate outside numberCacheMu.
func (r *Reconciler) numberSeededJobsCopy(cellKey string) map[string]bool {
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	seeded := r.numberSeededJobsLocked(cellKey)
	out := make(map[string]bool, len(seeded))
	for p := range seeded {
		out[p] = true
	}
	return out
}

// commitNumberOnlyProgress implements task 5.7's per-block commit rule:
// a number-only block's job paths join the seeded cache only once the
// DesiredStateResult of the push that carried the block reports
// rehydration success with no error. The caller resolves the block from
// the cell's in-flight push (takeRehydrationResult), so a failed or
// unmatched result leaves the cache untouched, and the next tick's
// AdvanceNumberOnlyRound call recomputes the same remaining set and
// resends the same block. Design D5's write rule 3 makes a repeat
// number-only write of an already-numbered job a harmless no-op. A job
// the mite deferred (write rule 1: its folder did not exist yet) never
// counts as seeded even on a success result, because its
// nextBuildNumber file was not written.
func (r *Reconciler) commitNumberOnlyProgress(cellKey string, jobPaths []string, success bool, rehydrationErr string, deferredJobs []string) {
	if len(jobPaths) == 0 || !success || rehydrationErr != "" {
		return
	}
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	deferred := make(map[string]bool, len(deferredJobs))
	for _, p := range deferredJobs {
		deferred[p] = true
	}
	seeded := r.numberSeededJobsLocked(cellKey)
	for _, p := range jobPaths {
		if !deferred[p] {
			seeded[p] = true
		}
	}
}

// rehydrationTenantState is one placed tenant's state at the start of a
// tick: its entry from before this tick's rebuild and its placement.
type rehydrationTenantState struct {
	team      *v1alpha1.Team
	existing  *v1alpha1.CellRehydrationEntry
	placement rehydrationPlacement
	applied   bool
	expired   bool
	builds    int32
}

// attachCellRehydration builds and attaches this cell's rehydration
// bundle onto desired (design D3/D4/D13, tasks 5.7/5.8/6.0a/6.1/6.3/6.4).
// It mutates cr.Status.Cell.Rehydration in place; the caller's own
// end-of-tick status patch persists it, matching every other status
// field the controller reconciler writes on cr directly.
//
// It answers nil when the tick attaches no bundle and holds none. Field
// 17 stays out of the composite desired-state hash (design D3), so the
// caller bypasses the idle-converged short-circuit only when
// desired.Rehydration is set: a push with no bundle only sends the
// delivered state again.
//
// While a delivered push has no result and its window is open, the tick
// holds: it keeps the entries, attaches nothing, and answers a held plan.
// A new bundle would replace the stamped command ID, and the result of
// the slower push would then match no entry.
//
// The pre-send entries carry no command ID. The caller stamps it with
// markRehydrationSent after a delivered push.
func (r *Reconciler) attachCellRehydration(ctx context.Context, cr *v1alpha1.Controller, desired *mitev1.DesiredStateCommand, hasArtifactManagerS3 bool, now time.Time) *rehydrationSendPlan {
	if !isCellController(cr) {
		return nil
	}
	cellKey := cr.Namespace + "/" + cr.Name
	teams := r.listPlacedTeams(ctx, cr)
	if len(teams) == 0 {
		r.dropRehydrationInFlight(cellKey)
		return nil
	}

	cellInstance := ""
	if cr.Status.Cell != nil {
		cellInstance = cr.Status.Cell.Instance
	}
	opts := rehydrate.Options{HasArtifactManagerS3: hasArtifactManagerS3}

	// First pass: decide for each tenant whether it is applied, past its
	// deadline, or pending. The pending set decides whether the tenants
	// of an in-flight push still hold it.
	states := make([]rehydrationTenantState, 0, len(teams))
	pendingSet := make(map[string]rehydrationPlacement)
	existingByTenant := make(map[string]*v1alpha1.CellRehydrationEntry, len(teams))
	for _, team := range teams {
		tenant := team.Name
		generation := int64(0)
		if team.Status.Placement != nil {
			generation = team.Status.Placement.Generation
		}
		st := rehydrationTenantState{
			team:      team,
			existing:  findCellRehydrationEntry(cr, tenant),
			placement: rehydrationPlacement{Generation: generation, CellInstance: cellInstance},
		}
		existingByTenant[tenant] = st.existing
		if rehydrationApplied(st.existing, generation, cellInstance) {
			r.clearRehydrationPendingSince(cellKey, tenant)
			st.applied = true
			states = append(states, st)
			continue
		}
		st.builds = v1alpha1.DefaultRehydrateBuilds
		rehydrateDeadline := v1alpha1.DefaultRehydrateDeadline().Duration
		if team.Spec.Pipelines != nil {
			st.builds = team.Spec.Pipelines.EffectiveRehydrateBuilds()
			rehydrateDeadline = team.Spec.Pipelines.EffectiveRehydrateDeadline()
		}
		pendingSince := r.rehydrationPendingSinceFor(cellKey, tenant, generation, cellInstance, now)
		st.expired = rehydrationExpired(pendingSince, rehydrateDeadline, now)
		if !st.expired {
			pendingSet[tenant] = st.placement
		}
		states = append(states, st)
	}

	entries := make([]v1alpha1.CellRehydrationEntry, 0, len(teams))
	if cr.Status.Cell == nil {
		cr.Status.Cell = &v1alpha1.CellStatus{}
	}

	// outcomes caches each pending tenant's seed outcome, so a hold check
	// and the rebuild after it fetch each tenant's records only once.
	outcomes := make(map[string]tenantSeedOutcome)
	outcomeFor := func(st rehydrationTenantState) tenantSeedOutcome {
		o, ok := outcomes[st.team.Name]
		if !ok {
			o = r.gatherTenantSeeds(ctx, cellKey, st, opts)
			outcomes[st.team.Name] = o
		}
		return o
	}

	// A tenant outside the held push ends the hold only when it has a
	// seed to send. A tenant whose fetch or render fails has nothing to
	// send, and ending the hold for it would replace the stamped command
	// ID on every tick.
	// A hold that ends only because another tenant has a seed leaves the
	// in-flight push valid. Its record and its stamped command ID stay
	// until a delivered push replaces them, so its result still counts
	// when the new push is held back or not delivered.
	var keep *rehydrationPush
	inFlight := r.rehydrationInFlightFor(cellKey)
	if held := heldRehydrationPlan(inFlight, pendingSet, existingByTenant, now); held != nil {
		holds := true
		for _, st := range states {
			if _, covered := held.Tenants[st.team.Name]; covered || st.applied || st.expired {
				continue
			}
			if len(outcomeFor(st).seeds) > 0 {
				holds = false
				break
			}
		}
		if holds {
			for _, st := range states {
				_, covered := held.Tenants[st.team.Name]
				switch {
				case st.applied || covered:
					entries = append(entries, *st.existing)
				case st.expired:
					entries = append(entries, expiredEntry(st, cellInstance, now))
				default:
					entries = r.appendUnseededEntry(entries, cellKey, st, outcomeFor(st), now)
				}
			}
			cr.Status.Cell.Rehydration = entries
			return held
		}
		keep = inFlight
	}
	if keep == nil {
		r.dropRehydrationInFlight(cellKey)
	}

	var allSeeds []rehydrate.JobSeed
	tenantJobPaths := make(map[string][]string)
	// tenantFailedJobPaths holds, per tenant, every enumerated job path
	// whose BuildJobSeed call errored this tick (design D4: every
	// enumerated job gets a seed or a pending/unseeded representation
	// — a render failure is never silently dropped from the fence
	// accounting). The selection pass never sees these paths, since
	// they never reach allSeeds, so they are folded into the unseeded
	// set by hand once the cross-tenant selection is known.
	tenantFailedJobPaths := make(map[string][]string)
	sentTenants := make(map[string]rehydrationPlacement)

	for _, st := range states {
		tenant := st.team.Name
		if st.applied {
			entries = append(entries, *st.existing)
			continue
		}
		if st.expired {
			entries = append(entries, expiredEntry(st, cellInstance, now))
			continue
		}
		o := outcomeFor(st)
		if len(o.seeds) == 0 {
			entries = r.appendUnseededEntry(entries, cellKey, st, o, now)
			continue
		}

		paths := make([]string, len(o.seeds), len(o.seeds)+len(o.failedPaths))
		count := 0
		for i, seed := range o.seeds {
			paths[i] = seed.JobPath
			count += seed.RecordCount()
		}
		paths = append(paths, o.failedPaths...)
		tenantJobPaths[tenant] = paths
		tenantFailedJobPaths[tenant] = o.failedPaths
		allSeeds = append(allSeeds, o.seeds...)
		sentTenants[tenant] = st.placement
		// Task 6.1: written before the send, Applied false, Deferred
		// empty, and no command ID until a delivered push stamps it.
		// Truncated and UnseededJobs are filled below, once the whole
		// cell's cross-tenant selection is known — the failed paths
		// already count toward it here, in case a later error return
		// leaves this initial value standing.
		entries = append(entries, preSendEntry(tenant, "", st.placement.Generation, cellInstance, int32(count), false, int32(len(o.failedPaths)), now))
	}

	if len(allSeeds) == 0 {
		publishRehydrationEntries(cr, entries, keep, existingByTenant)
		return nil
	}

	result, err := rehydrate.SelectForBundle(allSeeds)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("rehydration selection failed", "controller", cellKey, "error", err)
		}
		publishRehydrationEntries(cr, entries, keep, existingByTenant)
		return nil
	}

	var numberOnlyJobPaths []string
	if result.NumberOnly {
		// Order 4 triggers only when the bare NumberFiles for every job
		// of the cell already exceed the budget (design.md D4): job
		// count, not record volume, is what forced this branch, and
		// allSeeds' job count does not shrink from one tick to the
		// next. Re-measuring the full, record-bearing result computed
		// above would therefore report the exact same overflow every
		// tick, forever — it can never converge. AdvanceNumberOnlyRound
		// is the one authority the cache-aware, cumulative view of
		// what still needs a number, whether this tick is a
		// mid-progression block or the block that finally clears
		// UnseededJobs to zero.
		//
		// AdvanceNumberOnlyRound is called against a copy of the
		// confirmed seeded set, never the set itself: computing this
		// tick's candidate block must not commit it. A block counts as
		// seeded only once commitNumberOnlyProgress sees the successful
		// DesiredStateResult of the push that carried it (task 5.7).
		candidate := r.numberSeededJobsCopy(cellKey)
		blockResult, _, blockErr := rehydrate.AdvanceNumberOnlyRound(allSeeds, candidate, rehydrate.BundleBudgetBytes)
		if blockErr != nil {
			if r.Logger != nil {
				r.Logger.Warn("rehydration number-only round failed", "controller", cellKey, "error", blockErr)
			}
		} else {
			result = blockResult
			numberOnlyJobPaths = append([]string(nil), blockResult.JobPaths...)
		}
	}

	buildBundle := r.buildBundle
	if buildBundle == nil {
		buildBundle = rehydrate.BuildBundle
	}
	bundle, err := buildBundle(result)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("rehydration bundle build failed", "controller", cellKey, "error", err)
		}
		publishRehydrationEntries(cr, entries, keep, existingByTenant)
		return nil
	}

	// Task 6.4: the field is set only while at least one tenant above
	// is not applied — attachCellRehydration returns early with no
	// mutation of desired when every tenant already is.
	desired.Rehydration = bundle

	unseeded := make(map[string]bool, len(result.UnseededJobPaths))
	for _, p := range result.UnseededJobPaths {
		unseeded[p] = true
	}
	// A job whose seed failed to render never reaches allSeeds, so the
	// selection pass has no way to name it: fold it in here, by hand,
	// so it counts as unseeded rather than silently vanishing from the
	// fence accounting (design D4).
	for _, paths := range tenantFailedJobPaths {
		for _, p := range paths {
			unseeded[p] = true
		}
	}
	covered := make(map[string]bool, len(result.JobPaths))
	for _, p := range result.JobPaths {
		covered[p] = true
	}

	for i := range entries {
		tenant := entries[i].Tenant
		if _, ok := sentTenants[tenant]; !ok {
			continue
		}
		var tenantUnseeded int32
		var tenantCovered int
		for _, p := range tenantJobPaths[tenant] {
			if unseeded[p] {
				tenantUnseeded++
			}
			if covered[p] {
				tenantCovered++
			}
		}
		entries[i].BundleID = bundle.BundleId
		entries[i].Truncated = result.Truncated
		entries[i].UnseededJobs = tenantUnseeded
		if result.NumberOnly || tenantCovered == 0 {
			entries[i].Records = 0
		}
	}

	deferredEntries := publishRehydrationEntries(cr, entries, keep, existingByTenant)
	return &rehydrationSendPlan{Bundle: bundle, Tenants: sentTenants, NumberOnlyJobPaths: numberOnlyJobPaths, Entries: deferredEntries}
}

// tenantSeedOutcome is one pending tenant's seeds for a tick. fetchErr
// is true when the records fetch failed, and empty is true when the
// store enumerates no job path for the tenant.
type tenantSeedOutcome struct {
	fetchErr    bool
	empty       bool
	seeds       []rehydrate.JobSeed
	failedPaths []string
}

// gatherTenantSeeds fetches st's build records and renders one seed per
// job. A job whose render fails goes to failedPaths, so the fence still
// counts it as unseeded.
func (r *Reconciler) gatherTenantSeeds(ctx context.Context, cellKey string, st rehydrationTenantState, opts rehydrate.Options) tenantSeedOutcome {
	tenant := st.team.Name
	jobRecords, err := fetchTenantJobRecords(ctx, r.recordsFunc, r.clusterName, tenant, int(st.builds))
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("rehydration records fetch failed", "tenant", tenant, "controller", cellKey, "error", err)
		}
		return tenantSeedOutcome{fetchErr: true}
	}
	if len(jobRecords) == 0 {
		return tenantSeedOutcome{empty: true}
	}
	var out tenantSeedOutcome
	for _, jr := range jobRecords {
		seed, err := r.buildJobSeed(adaptJobRecords(jr), int(st.builds), cellJenkinsHome, opts)
		if err != nil {
			if r.Logger != nil {
				r.Logger.Warn("rehydration seed build failed", "tenant", tenant, "job", jr.JobPath, "error", err)
			}
			out.failedPaths = append(out.failedPaths, jr.JobPath)
			continue
		}
		out.seeds = append(out.seeds, seed)
	}
	return out
}

// appendUnseededEntry appends the entry of a pending tenant with no seed
// to send this tick. A failed fetch keeps the previous entry. An empty
// enumeration counts as applied at once (task 6.0a). A tenant whose every
// job failed to render keeps its previous entry, or gets a new one, so
// the fence still sees it as unseeded rather than absent from status.
func (r *Reconciler) appendUnseededEntry(entries []v1alpha1.CellRehydrationEntry, cellKey string, st rehydrationTenantState, o tenantSeedOutcome, now time.Time) []v1alpha1.CellRehydrationEntry {
	tenant := st.team.Name
	switch {
	case o.empty:
		r.clearRehydrationPendingSince(cellKey, tenant)
		return append(entries, emptySelectionEntry(tenant, st.placement.Generation, st.placement.CellInstance, now))
	case st.existing != nil:
		return append(entries, *st.existing)
	case !o.fetchErr && len(o.failedPaths) > 0:
		return append(entries, preSendEntry(tenant, "", st.placement.Generation, st.placement.CellInstance, 0, false, int32(len(o.failedPaths)), now))
	}
	return entries
}

// expiredEntry answers the give-up entry of a tenant past its deadline,
// carrying the job paths its previous entry still deferred.
func expiredEntry(st rehydrationTenantState, cellInstance string, now time.Time) v1alpha1.CellRehydrationEntry {
	var deferred []string
	if st.existing != nil {
		deferred = st.existing.Deferred
	}
	return deadlineEntry(st.team.Name, st.placement.Generation, cellInstance, deferred, now)
}

// keepTenants answers the tenants of a kept in-flight push, or nil.
func keepTenants(keep *rehydrationPush) map[string]rehydrationPlacement {
	if keep == nil {
		return nil
	}
	return keep.Tenants
}

// publishRehydrationEntries writes entries to cr's status. A tenant of a
// kept in-flight push keeps its status entry whole until a delivered push
// replaces it: a result of the kept push must update the entry that
// describes the kept bundle, never the new selection. It answers the new
// entry of each such tenant, which markRehydrationSent writes after a
// delivered push. Every return after the per-tenant pass calls it, so an
// error return also keeps the stamp.
func publishRehydrationEntries(cr *v1alpha1.Controller, entries []v1alpha1.CellRehydrationEntry, keep *rehydrationPush, existingByTenant map[string]*v1alpha1.CellRehydrationEntry) map[string]v1alpha1.CellRehydrationEntry {
	var deferred map[string]v1alpha1.CellRehydrationEntry
	for i := range entries {
		tenant := entries[i].Tenant
		if _, covered := keepTenants(keep)[tenant]; !covered {
			continue
		}
		existing := existingByTenant[tenant]
		if existing == nil {
			continue
		}
		if deferred == nil {
			deferred = make(map[string]v1alpha1.CellRehydrationEntry)
		}
		deferred[tenant] = entries[i]
		entries[i] = *existing
	}
	cr.Status.Cell.Rehydration = entries
	return deferred
}
