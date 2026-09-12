package cells

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// RehydrationInputs carries the values the rehydration pass needs beyond
// the Team status itself.
type RehydrationInputs struct {
	// CellInstance is Controller.status.cell.instance for the tenant's
	// currently bound cell (empty when the cell is not found, or the
	// tenant is unplaced). The tenant-placement spec makes this the
	// second unrehydration trigger, alongside a generation change.
	CellInstance string

	// Entry is the tenant's own Controller.status.cell.rehydration entry
	// on the bound cell (design D13, task 6.5), read from the same
	// cellsByKey snapshot the caller already built this tick. Nil when
	// the cell carries no entry for this tenant yet — a fresh placement
	// is always one tick behind the controller reconciler's own write.
	Entry *v1alpha1.CellRehydrationEntry
}

// RehydrationOutcome names what RehydrationPass decided, for the caller to
// log or assert.
type RehydrationOutcome struct {
	// MarkedUnrehydrated is true when this call left Rehydrated False,
	// under any of its three False reasons.
	MarkedUnrehydrated bool
}

// RehydrationPass runs after the placement pass, inside the same tick and
// the same status patch (design D13, tenant-placement spec, tasks 4.5 and
// 6.5). TeamReconciler.Reconcile calls it with the already-built
// *TeamStatus, which this pass mutates in place — C5 makes TeamReconciler
// the only Team status writer, so no second crdstore.PatchStatus[Team]
// call is ever needed for this pass's own write.
//
// It copies in.Entry into status.Rehydration every tick, deriving Attempts
// and LastAttempt itself (task 6.5): Attempts resets to zero once the
// entry counts as applied, and otherwise counts consecutive ticks since
// the placement generation and cell instance last changed. It then writes
// the Rehydrated condition from four inputs: whether in.Entry matches the
// current generation and cell instance, and, when it does, that entry's
// own Applied, Error, Deferred, and UnseededJobs fields.
//
// The entry's own Error is the RehydrationFailed surface for both a
// mite-reported failure and the controller reconciler's own deadline give-
// up (task 6.3 writes the same field for both, per design D13's condition
// table naming a single reason for either cause) — this pass reads
// in.Entry.Error and never computes a deadline of its own.
func RehydrationPass(status *v1alpha1.TeamStatus, in RehydrationInputs) RehydrationOutcome {
	if status.Placement == nil {
		// Never placed, or torn down this same tick by cells.Place's own
		// finishTeardown (which clears status.Placement before this pass
		// runs): build-record rehydration does not apply, and any state
		// from a prior placement must not survive — otherwise a stale
		// non-nil status.Rehydration is present in the status patch and
		// the crdstore clearStatusFields registry's absent-key rule never
		// fires for it.
		status.Rehydration = nil
		status.Conditions = removeCondition(status.Conditions, v1alpha1.TeamConditionRehydrated)
		return RehydrationOutcome{}
	}

	generation := status.Placement.Generation
	cellInstance := in.CellInstance
	matches := in.Entry != nil && in.Entry.Generation == generation && in.Entry.CellInstance == cellInstance
	// appliedNow is the FULL condition for True/TenantRehydrated: the
	// narrower task 6.3 applied predicate (matching generation/
	// cellInstance, Applied true, no error, no deferred job) SHALL
	// additionally hold no unseeded job — RehydrationTruncated SHALL
	// still report while any job has no seed, even once the mite
	// reports the number-only bundle it did receive as applied.
	appliedNow := matches && in.Entry.Applied && in.Entry.Error == "" &&
		len(in.Entry.Deferred) == 0 && in.Entry.UnseededJobs == 0

	status.Rehydration = copyRehydrationEntry(status.Rehydration, in.Entry, generation, cellInstance, appliedNow)

	condStatus, reason, message := rehydrationCondition(in.Entry, matches, appliedNow)
	setRehydrated(status, condStatus, reason, message)
	return RehydrationOutcome{MarkedUnrehydrated: condStatus == metav1.ConditionFalse}
}

// copyRehydrationEntry builds the tenant's Team.status.rehydration from
// the bound cell's own entry (design D13, task 6.5). Records, Truncated,
// UnseededJobs, and Deferred come straight from entry when it matches the
// current generation and cell instance; a non-matching or absent entry
// answers the zero values for all four, since the fresh cycle has not
// written anything yet. Attempts counts consecutive not-applied ticks
// since generation and cellInstance last changed, resetting to zero once
// applied; LastAttempt is this call's own time while not applied, and is
// left unchanged once applied (the last real attempt stays on record).
func copyRehydrationEntry(prev *v1alpha1.TeamRehydrationStatus, entry *v1alpha1.CellRehydrationEntry, generation int64, cellInstance string, appliedNow bool) *v1alpha1.TeamRehydrationStatus {
	out := &v1alpha1.TeamRehydrationStatus{Generation: generation, CellInstance: cellInstance}
	if entry != nil && entry.Generation == generation && entry.CellInstance == cellInstance {
		out.Records = entry.Records
		out.Truncated = entry.Truncated
		out.UnseededJobs = entry.UnseededJobs
		out.Deferred = append([]string(nil), entry.Deferred...)
	}

	samecycle := prev != nil && prev.Generation == generation && prev.CellInstance == cellInstance
	if appliedNow {
		out.Attempts = 0
		if samecycle {
			out.LastAttempt = prev.LastAttempt
		}
		return out
	}
	now := metav1.Now()
	out.LastAttempt = &now
	if samecycle {
		out.Attempts = prev.Attempts + 1
	} else {
		out.Attempts = 1
	}
	return out
}

// rehydrationCondition decides the Rehydrated condition's status, reason,
// and message from the entry's own state (design D13, task 6.5). A
// mismatched or absent entry always reports RehydrationPending: the
// bundle for the current generation and cell instance has not landed yet.
func rehydrationCondition(entry *v1alpha1.CellRehydrationEntry, matches, appliedNow bool) (metav1.ConditionStatus, string, string) {
	switch {
	case appliedNow:
		return metav1.ConditionTrue, v1alpha1.TeamReasonTenantRehydrated, "every selected seed applied"
	case matches && entry.Error != "":
		return metav1.ConditionFalse, v1alpha1.TeamReasonRehydrationFailed, entry.Error
	case matches && entry.UnseededJobs > 0:
		return metav1.ConditionFalse, v1alpha1.TeamReasonRehydrationTruncated,
			fmt.Sprintf("%d job(s) have no rehydration seed yet", entry.UnseededJobs)
	default:
		return metav1.ConditionFalse, v1alpha1.TeamReasonRehydrationPending,
			"a rehydration bundle is outstanding, or a job is deferred"
	}
}

// setRehydrated upserts the Rehydrated condition, following the same
// idempotent-preserve-LastTransitionTime rule as TeamReconciler.setCondition
// and this package's own setPlaced: kept local here because this package
// (not the controller package) owns every write of this condition.
func setRehydrated(status *v1alpha1.TeamStatus, s metav1.ConditionStatus, reason, message string) {
	cond := v1alpha1.TeamCondition{
		Type:               v1alpha1.TeamConditionRehydrated,
		Status:             s,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	for i, existing := range status.Conditions {
		if existing.Type != cond.Type {
			continue
		}
		if existing.Status == cond.Status && existing.Reason == cond.Reason && existing.Message == cond.Message {
			cond.LastTransitionTime = existing.LastTransitionTime
		}
		status.Conditions[i] = cond
		return
	}
	status.Conditions = append(status.Conditions, cond)
}
