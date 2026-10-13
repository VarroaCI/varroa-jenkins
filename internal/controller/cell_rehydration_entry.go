package controller

import (
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// emptySelectionEntry implements task 6.0a: a tenant whose enumeration
// returns no job path is applied at once, with zero records, no
// deferred job, and no bundleId, so its Rehydrated condition reaches
// True/TenantRehydrated on this same tick instead of waiting forever —
// the applied predicate never reopens this case, so a tenant that never
// builds a bundle must never be left with Applied false.
func emptySelectionEntry(tenant string, generation int64, cellInstance string, now time.Time) v1alpha1.CellRehydrationEntry {
	at := metav1.NewTime(now)
	return v1alpha1.CellRehydrationEntry{
		Tenant: tenant, Generation: generation, CellInstance: cellInstance,
		Applied: true, At: &at,
	}
}

// preSendEntry implements task 6.1: the per-tenant entry the controller
// reconciler writes before it sends the command, with Applied false and
// Deferred empty. Records, Truncated, and UnseededJobs come from the
// tenant's own selection; bundleID from the bundle SelectForBundle and
// BuildBundle built for the whole cell. The entry carries no command ID:
// markRehydrationSent stamps it after a delivered push, so a result of a
// push that carried no bundle never matches it.
func preSendEntry(tenant, bundleID string, generation int64, cellInstance string, records int32, truncated bool, unseededJobs int32, now time.Time) v1alpha1.CellRehydrationEntry {
	at := metav1.NewTime(now)
	return v1alpha1.CellRehydrationEntry{
		Tenant: tenant, BundleID: bundleID,
		Generation: generation, CellInstance: cellInstance,
		Records: records, Truncated: truncated, UnseededJobs: unseededJobs,
		Applied: false, At: &at,
	}
}

// deadlineExceededMessage names the still-deferred jobs in the
// RehydrationFailed message a give-up entry carries (design D13's
// scenario "the message names the job").
func deadlineExceededMessage(deferred []string) string {
	if len(deferred) == 0 {
		return "rehydration deadline exceeded"
	}
	return fmt.Sprintf("rehydration deadline exceeded with %d job(s) still deferred: %s",
		len(deferred), strings.Join(deferred, ", "))
}

// deadlineEntry implements task 6.3's give-up path: the controller
// reconciler stops retrying once rehydrationExpired reports the
// deadline passed, and writes that as the entry's own Error — the same
// field a mite-reported failure uses — so cells.RehydrationPass (task
// 6.5) surfaces both the same way, under Rehydrated=False/
// RehydrationFailed.
func deadlineEntry(tenant string, generation int64, cellInstance string, deferred []string, now time.Time) v1alpha1.CellRehydrationEntry {
	at := metav1.NewTime(now)
	return v1alpha1.CellRehydrationEntry{
		Tenant: tenant, Generation: generation, CellInstance: cellInstance,
		Deferred: deferred, Error: deadlineExceededMessage(deferred), At: &at,
	}
}

// seedAnswer is the seed state of one tenant on one member cell: the cell
// instance the tenant may build on, and the job paths still deferred.
type seedAnswer struct {
	instance string
	deferred []string
}

// seedAnswerOf returns the seed answer that the Team reconciler reads from
// one rehydration entry. An entry that is not applied answers no instance,
// which holds every build on the member until its seed lands.
func seedAnswerOf(e *v1alpha1.CellRehydrationEntry) seedAnswer {
	if !e.Applied {
		return seedAnswer{deferred: e.Deferred}
	}
	return seedAnswer{instance: e.CellInstance, deferred: e.Deferred}
}

// seedAnswersOf maps each tenant of a cell to its seed answer. A cell with
// no status, or a tenant with no entry, answers the zero value.
func seedAnswersOf(cr *v1alpha1.Controller) map[string]seedAnswer {
	out := map[string]seedAnswer{}
	if cr == nil || cr.Status.Cell == nil {
		return out
	}
	for i := range cr.Status.Cell.Rehydration {
		e := &cr.Status.Cell.Rehydration[i]
		out[e.Tenant] = seedAnswerOf(e)
	}
	return out
}

// CellSeedStateChanged reports whether a Controller update changed the seed
// answer of any tenant on a cell. The Controller informer handler calls it
// on every replica. A true answer wakes the Team ticker, which reads the same
// answers through memberSeedReader. Only the instance and the deferred job
// paths count. A change of the entry time alone is false.
func CellSeedStateChanged(oldCR, newCR *v1alpha1.Controller) bool {
	if newCR == nil || !v1alpha1.IsCell(newCR) || newCR.Status.Cell == nil {
		return false
	}
	oldAnswers := seedAnswersOf(oldCR)
	newAnswers := seedAnswersOf(newCR)
	for tenant, now := range newAnswers {
		prev := oldAnswers[tenant]
		if prev.instance != now.instance || !slices.Equal(prev.deferred, now.deferred) {
			return true
		}
	}
	for tenant, prev := range oldAnswers {
		if _, ok := newAnswers[tenant]; ok {
			continue
		}
		if prev.instance != "" || len(prev.deferred) > 0 {
			return true
		}
	}
	return false
}

// applyRehydrationResult implements task 6.2: it updates only the
// entries whose CommandID matches commandID. A whole-bundle
// rehydrationError applies to every one of them, because the mite
// applies the rehydration section as one unit; each tenant otherwise
// gets its own deferred job paths (already partitioned by
// partitionDeferredJobs) and an Applied flag derived from success, the
// error, and its own deferred list.
func applyRehydrationResult(entries []v1alpha1.CellRehydrationEntry, commandID string, success bool, rehydrationError string, deferredByTenant map[string][]string, now time.Time) []v1alpha1.CellRehydrationEntry {
	out := make([]v1alpha1.CellRehydrationEntry, len(entries))
	at := metav1.NewTime(now)
	for i, e := range entries {
		if e.CommandID != commandID {
			out[i] = e
			continue
		}
		tenantDeferred := deferredByTenant[e.Tenant]
		e.Deferred = tenantDeferred
		e.Error = rehydrationError
		e.Applied = success && rehydrationError == "" && len(tenantDeferred) == 0
		e.At = &at
		out[i] = e
	}
	return out
}

// partitionDeferredJobs implements task 6.2's per-tenant attribution of
// DesiredStateResult.rehydration_deferred_jobs — a flat job-path list
// across every tenant of one cell, because one command covers every
// tenant on it — back to each path's owning tenant, by that tenant's
// own job-path prefix (a pipelines tenant's jobs all live under its own
// top-level folder, its Team name).
func partitionDeferredJobs(deferredJobs []string, tenants []string) map[string][]string {
	out := make(map[string][]string, len(tenants))
	for _, path := range deferredJobs {
		for _, tenant := range tenants {
			if path == tenant || strings.HasPrefix(path, tenant+"/") {
				out[tenant] = append(out[tenant], path)
				break
			}
		}
	}
	return out
}
