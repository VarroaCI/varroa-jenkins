package controller

import (
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// rehydrationApplied implements task 6.3's applied predicate (design
// D13): a tenant's rehydration counts as applied only when its entry's
// Generation and CellInstance match the cell's current placement,
// Applied is true, Error is empty, Deferred is empty, UnseededJobs is
// zero, and Unseeded is empty. Any other state means the controller
// reconciler must keep building a new bundle for this tenant, until
// rehydrationExpired says otherwise. UnseededJobs above zero means a
// number-only round applied but did not yet cover every job (design D4):
// treating that state as applied would stop the per-tick number-only
// progression before every job of the tenant holds its number. A listed
// unseeded path keeps the tenant pending too, so a lost in-flight record
// heals on the next round.
func rehydrationApplied(entry *v1alpha1.CellRehydrationEntry, generation int64, cellInstance string) bool {
	return entry != nil &&
		entry.Generation == generation &&
		entry.CellInstance == cellInstance &&
		entry.Applied &&
		entry.Error == "" &&
		len(entry.Deferred) == 0 &&
		entry.UnseededJobs == 0 &&
		len(entry.Unseeded) == 0
}

// rehydrationExpired reports whether a still-pending tenant has retried
// past Team.spec.pipelines.rehydrateDeadline (design D13, task 6.3).
// pendingSince is when this generation/cellInstance combination first
// became pending; the caller resets it whenever the placement changes or
// the tenant becomes applied, and a zero pendingSince (no pending streak
// recorded yet) never expires.
func rehydrationExpired(pendingSince time.Time, deadline time.Duration, now time.Time) bool {
	return !pendingSince.IsZero() && deadline > 0 && now.Sub(pendingSince) > deadline
}
