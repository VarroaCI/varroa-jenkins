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

// setNumberOnlyPending records cellKey's in-flight number-only block,
// creating the tracking map on first use — a Reconciler built directly
// in a test, bypassing NewReconciler, would otherwise panic on this
// write.
func (r *Reconciler) setNumberOnlyPending(cellKey string, block numberOnlyPendingBlock) {
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	if r.numberOnlyPending == nil {
		r.numberOnlyPending = make(map[string]numberOnlyPendingBlock)
	}
	r.numberOnlyPending[cellKey] = block
}

// commitNumberOnlyProgress implements task 5.7's per-block commit rule:
// a number-only block's job paths join the seeded cache only once a
// DesiredStateResult whose CommandId matches the block reports
// rehydration success with no error. A failed or unmatched result
// leaves the cache untouched, so the next tick's AdvanceNumberOnlyRound
// call recomputes the same remaining set and resends the same block —
// design.md D5's write rule 3 makes a repeat number-only write of an
// already-numbered job a harmless no-op, so resending costs nothing.
// A job the mite deferred (write rule 1: its folder did not exist yet)
// never counts as seeded even on a success result, because its
// nextBuildNumber file was not written.
func (r *Reconciler) commitNumberOnlyProgress(cellKey, commandID string, success bool, rehydrationErr string, deferredJobs []string) {
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	pending, ok := r.numberOnlyPending[cellKey]
	if !ok || pending.CommandID != commandID {
		return
	}
	delete(r.numberOnlyPending, cellKey)
	if !success || rehydrationErr != "" {
		return
	}
	deferred := make(map[string]bool, len(deferredJobs))
	for _, p := range deferredJobs {
		deferred[p] = true
	}
	seeded := r.numberSeededJobsLocked(cellKey)
	for _, p := range pending.JobPaths {
		if !deferred[p] {
			seeded[p] = true
		}
	}
}

// attachCellRehydration builds and attaches this cell's rehydration
// bundle onto desired (design D3/D4/D13, tasks 5.7/5.8/6.0a/6.1/6.3/6.4).
// It mutates cr.Status.Cell.Rehydration in place; the caller's own
// end-of-tick status patch persists it, matching every other status
// field the controller reconciler writes on cr directly.
//
// It answers true when any tenant is not yet applied, so the caller can
// bypass the idle-converged short-circuit: field 17 stays out of the
// composite desired-state hash (design D3), so a converged cell would
// otherwise never retry a still-pending tenant.
func (r *Reconciler) attachCellRehydration(ctx context.Context, cr *v1alpha1.Controller, desired *mitev1.DesiredStateCommand, hasArtifactManagerS3 bool, now time.Time) bool {
	if !isCellController(cr) {
		return false
	}
	teams := r.listPlacedTeams(ctx, cr)
	if len(teams) == 0 {
		return false
	}

	cellKey := cr.Namespace + "/" + cr.Name
	cellInstance := ""
	if cr.Status.Cell != nil {
		cellInstance = cr.Status.Cell.Instance
	}
	opts := rehydrate.Options{HasArtifactManagerS3: hasArtifactManagerS3}

	entries := make([]v1alpha1.CellRehydrationEntry, 0, len(teams))
	var allSeeds []rehydrate.JobSeed
	tenantJobPaths := make(map[string][]string)
	tenantSeedRecordCounts := make(map[string]int)
	// tenantFailedJobPaths holds, per tenant, every enumerated job path
	// whose BuildJobSeed call errored this tick (design D4: every
	// enumerated job gets a seed or a pending/unseeded representation
	// — a render failure is never silently dropped from the fence
	// accounting). The selection pass never sees these paths, since
	// they never reach allSeeds, so they are folded into the unseeded
	// set by hand once the cross-tenant selection is known.
	tenantFailedJobPaths := make(map[string][]string)
	pending := false

	for _, team := range teams {
		tenant := team.Name
		generation := int64(0)
		if team.Status.Placement != nil {
			generation = team.Status.Placement.Generation
		}
		existing := findCellRehydrationEntry(cr, tenant)

		if rehydrationApplied(existing, generation, cellInstance) {
			r.clearRehydrationPendingSince(cellKey, tenant)
			entries = append(entries, *existing)
			continue
		}

		rehydrateBuilds := v1alpha1.DefaultRehydrateBuilds
		rehydrateDeadline := v1alpha1.DefaultRehydrateDeadline().Duration
		if team.Spec.Pipelines != nil {
			rehydrateBuilds = team.Spec.Pipelines.EffectiveRehydrateBuilds()
			rehydrateDeadline = team.Spec.Pipelines.EffectiveRehydrateDeadline()
		}

		pendingSince := r.rehydrationPendingSinceFor(cellKey, tenant, generation, cellInstance, now)
		if rehydrationExpired(pendingSince, rehydrateDeadline, now) {
			var deferred []string
			if existing != nil {
				deferred = existing.Deferred
			}
			entries = append(entries, deadlineEntry(tenant, generation, cellInstance, deferred, now))
			pending = true
			continue
		}

		jobRecords, err := fetchTenantJobRecords(ctx, r.recordsFunc, r.clusterName, tenant, int(rehydrateBuilds))
		if err != nil {
			if r.Logger != nil {
				r.Logger.Warn("rehydration records fetch failed", "tenant", tenant, "controller", cellKey, "error", err)
			}
			pending = true
			if existing != nil {
				entries = append(entries, *existing)
			}
			continue
		}
		if len(jobRecords) == 0 {
			r.clearRehydrationPendingSince(cellKey, tenant)
			entries = append(entries, emptySelectionEntry(tenant, generation, cellInstance, now))
			continue
		}

		var tenantSeeds []rehydrate.JobSeed
		var failedPaths []string
		for _, jr := range jobRecords {
			seed, err := r.buildJobSeed(adaptJobRecords(jr), int(rehydrateBuilds), cellJenkinsHome, opts)
			if err != nil {
				if r.Logger != nil {
					r.Logger.Warn("rehydration seed build failed", "tenant", tenant, "job", jr.JobPath, "error", err)
				}
				failedPaths = append(failedPaths, jr.JobPath)
				continue
			}
			tenantSeeds = append(tenantSeeds, seed)
		}
		if len(tenantSeeds) == 0 {
			pending = true
			switch {
			case existing != nil:
				entries = append(entries, *existing)
			case len(failedPaths) > 0:
				// No prior entry and every job of the tenant failed to
				// render this tick: write one anyway, so the fence
				// still sees this tenant as unseeded rather than
				// absent from status entirely.
				entries = append(entries, preSendEntry(tenant, "", "", generation, cellInstance, 0, false, int32(len(failedPaths)), now))
			}
			continue
		}

		paths := make([]string, len(tenantSeeds), len(tenantSeeds)+len(failedPaths))
		count := 0
		for i, s := range tenantSeeds {
			paths[i] = s.JobPath
			count += s.RecordCount()
		}
		paths = append(paths, failedPaths...)
		tenantJobPaths[tenant] = paths
		tenantFailedJobPaths[tenant] = failedPaths
		tenantSeedRecordCounts[tenant] = count
		allSeeds = append(allSeeds, tenantSeeds...)
		pending = true
		// Task 6.1: written before the send, Applied false, Deferred
		// empty. Truncated and UnseededJobs are filled below, once the
		// whole cell's cross-tenant selection is known — the failed
		// paths already count toward it here, in case a later error
		// return leaves this initial value standing.
		entries = append(entries, preSendEntry(tenant, desired.CommandId, "", generation, cellInstance, int32(count), false, int32(len(failedPaths)), now))
	}

	if cr.Status.Cell == nil {
		cr.Status.Cell = &v1alpha1.CellStatus{}
	}

	if len(allSeeds) == 0 {
		cr.Status.Cell.Rehydration = entries
		return pending
	}

	result, err := rehydrate.SelectForBundle(allSeeds)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("rehydration selection failed", "controller", cellKey, "error", err)
		}
		cr.Status.Cell.Rehydration = entries
		return pending
	}

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
		// seeded only once commitNumberOnlyProgress sees a matching,
		// successful DesiredStateResult for it (task 5.7) — otherwise a
		// block whose delivery failed, or that the mite rejected, would
		// never be resent, and the fence would treat its jobs as
		// numbered when no number was ever written.
		candidate := r.numberSeededJobsCopy(cellKey)
		blockResult, _, blockErr := rehydrate.AdvanceNumberOnlyRound(allSeeds, candidate, rehydrate.BundleBudgetBytes)
		if blockErr != nil {
			if r.Logger != nil {
				r.Logger.Warn("rehydration number-only round failed", "controller", cellKey, "error", blockErr)
			}
		} else {
			result = blockResult
			r.setNumberOnlyPending(cellKey, numberOnlyPendingBlock{
				CommandID: desired.CommandId,
				JobPaths:  append([]string(nil), blockResult.JobPaths...),
			})
		}
	}

	bundle, err := rehydrate.BuildBundle(result)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("rehydration bundle build failed", "controller", cellKey, "error", err)
		}
		cr.Status.Cell.Rehydration = entries
		return pending
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
		if entries[i].CommandID != desired.CommandId {
			continue
		}
		tenant := entries[i].Tenant
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

	cr.Status.Cell.Rehydration = entries
	return pending
}
