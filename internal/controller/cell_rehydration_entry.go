package controller

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/rehydrate"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// maxUnseededPaths bounds CellRehydrationEntry.Unseeded. It must equal the MaxItems marker on that field.
const maxUnseededPaths = 256

// maxUnseededBytesPerCell bounds the encoded size of the Unseeded lists of
// every entry of one cell, so the status stays far below the object size
// limit. Each path is measured as its length plus three JSON bytes (two
// quotes and a comma).
const maxUnseededBytesPerCell = 256 << 10

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

// deadlineExceededPrefix opens the Error of a give-up entry. A reload
// failure never opens its Error with this prefix.
const deadlineExceededPrefix = "rehydration deadline exceeded"

// deadlineExceededMessage names the still-deferred jobs in the
// RehydrationFailed message a give-up entry carries (design D13's
// scenario "the message names the job").
func deadlineExceededMessage(deferred []string) string {
	if len(deferred) == 0 {
		return deadlineExceededPrefix
	}
	return fmt.Sprintf(deadlineExceededPrefix+" with %d job(s) still deferred: %s",
		len(deferred), strings.Join(deferred, ", "))
}

// deadlineEntry implements task 6.3's give-up path: the controller
// reconciler stops retrying once rehydrationExpired reports the
// deadline passed, and writes that as the entry's own Error — the same
// field a mite-reported failure uses — so cells.RehydrationPass (task
// 6.5) surfaces both the same way, under Rehydrated=False/
// RehydrationFailed.
//
// released reports whether the tenant's bundle applied earlier on this
// generation and cell instance. A released give-up keeps Applied true,
// so the deferred jobs build with no seeded history. A give-up of a
// bundle that never applied keeps Applied false, so the whole tenant
// stays held.
func deadlineEntry(tenant string, generation int64, cellInstance string, deferred []string, released bool, now time.Time) v1alpha1.CellRehydrationEntry {
	at := metav1.NewTime(now)
	return v1alpha1.CellRehydrationEntry{
		Tenant: tenant, Generation: generation, CellInstance: cellInstance,
		Deferred: deferred, Error: deadlineExceededMessage(deferred),
		Applied: released, At: &at,
	}
}

// carryAppliedSeed keeps the applied flag, the deferred list, and the
// unseeded list of an existing entry for the same generation and cell
// instance. The seed of an earlier apply stays on the cell while a resend
// is outstanding. The Error of the existing entry is not carried here;
// carryRehydrationError carries it.
func carryAppliedSeed(next v1alpha1.CellRehydrationEntry, existing *v1alpha1.CellRehydrationEntry) v1alpha1.CellRehydrationEntry {
	if existing == nil || !existing.Applied ||
		existing.Generation != next.Generation || existing.CellInstance != next.CellInstance {
		return next
	}
	next.Applied = true
	next.Deferred = append([]string(nil), existing.Deferred...)
	next.Unseeded = append([]string(nil), existing.Unseeded...)
	return next
}

// carryRehydrationError keeps the Error of an existing entry for the same
// generation and cell instance. A resend keeps the failure of the last
// result, so the status shows it until a later result clears it.
func carryRehydrationError(next v1alpha1.CellRehydrationEntry, existing *v1alpha1.CellRehydrationEntry) v1alpha1.CellRehydrationEntry {
	if existing == nil || existing.Generation != next.Generation || existing.CellInstance != next.CellInstance {
		return next
	}
	next.Error = existing.Error
	return next
}

// seedAnswer is the seed state of one tenant on one member cell: the cell
// instance the tenant may build on, and the job paths still pending a
// seed on that instance.
type seedAnswer struct {
	instance string
	pending  []string
}

// seedAnswerOf returns the seed answer that the Team reconciler reads from
// one rehydration entry. An entry that is not applied answers no instance,
// which holds every build on the member until its seed lands. An applied
// entry answers its instance and its pending jobs, the deferred and the
// unseeded paths, so only those jobs wait. A released give-up is applied
// with an error and answers no pending jobs, so they build with no seeded
// history. An applied entry whose unseeded list is shorter than its
// UnseededJobs count dropped the list, so it holds the whole tenant.
func seedAnswerOf(e *v1alpha1.CellRehydrationEntry) seedAnswer {
	pending := rehydrate.PendingJobPaths(e.Deferred, e.Unseeded)
	switch {
	case !e.Applied:
		return seedAnswer{pending: pending}
	case e.Error != "" && strings.HasPrefix(e.Error, deadlineExceededPrefix):
		return seedAnswer{instance: e.CellInstance}
	case int(e.UnseededJobs) > len(e.Unseeded):
		return seedAnswer{pending: pending}
	default:
		return seedAnswer{instance: e.CellInstance, pending: pending}
	}
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
// answers through memberSeedReader. Only the instance and the pending job
// paths count. A change of the entry time alone is false.
func CellSeedStateChanged(oldCR, newCR *v1alpha1.Controller) bool {
	if newCR == nil || !v1alpha1.IsCell(newCR) || newCR.Status.Cell == nil {
		return false
	}
	oldAnswers := seedAnswersOf(oldCR)
	newAnswers := seedAnswersOf(newCR)
	for tenant, now := range newAnswers {
		prev := oldAnswers[tenant]
		if prev.instance != now.instance || !slices.Equal(prev.pending, now.pending) {
			return true
		}
	}
	for tenant, prev := range oldAnswers {
		if _, ok := newAnswers[tenant]; ok {
			continue
		}
		if prev.instance != "" || len(prev.pending) > 0 {
			return true
		}
	}
	return false
}

// boundUnseeded sets e.Unseeded to the sorted, unique paths. A list above
// limit, or a carried list that was already incomplete, is dropped, and
// Applied is cleared so the whole tenant waits for the result.
func boundUnseeded(e v1alpha1.CellRehydrationEntry, paths []string, complete bool, limit int) v1alpha1.CellRehydrationEntry {
	p := rehydrate.PendingJobPaths(paths, nil)
	if !complete || len(p) > limit {
		e.Unseeded = nil
		e.Applied = false
		return e
	}
	e.Unseeded = p
	return e
}

// unseededBytes measures one entry's Unseeded list in encoded bytes: each
// path counts its length plus three JSON bytes.
func unseededBytes(e *v1alpha1.CellRehydrationEntry) int {
	n := 0
	for _, p := range e.Unseeded {
		n += len(p) + 3
	}
	return n
}

// capCellUnseeded keeps the sum of unseededBytes over entries within budget.
// While the total exceeds budget, it drops the entry with the largest size,
// ties by tenant name. Each dropped entry waits whole: boundUnseeded clears
// its list and Applied.
func capCellUnseeded(entries []v1alpha1.CellRehydrationEntry, budget int) {
	total := 0
	for i := range entries {
		total += unseededBytes(&entries[i])
	}
	if total <= budget {
		return
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ba, bb := unseededBytes(&entries[order[a]]), unseededBytes(&entries[order[b]])
		if ba != bb {
			return ba > bb
		}
		return entries[order[a]].Tenant < entries[order[b]].Tenant
	})
	for _, i := range order {
		if total <= budget {
			break
		}
		total -= unseededBytes(&entries[i])
		entries[i] = boundUnseeded(entries[i], nil, false, maxUnseededPaths)
	}
}

// withoutPaths returns paths minus drop, or nil when nothing is left.
func withoutPaths(paths, drop []string) []string {
	if len(drop) == 0 {
		return paths
	}
	gone := make(map[string]bool, len(drop))
	for _, p := range drop {
		gone[p] = true
	}
	var out []string
	for _, p := range paths {
		if !gone[p] {
			out = append(out, p)
		}
	}
	return out
}

// applyRehydrationResult implements task 6.2: it updates only the
// entries whose CommandID matches commandID. A whole-bundle
// rehydrationError applies to every one of them, because the mite
// applies the rehydration section as one unit; each tenant otherwise
// gets its own deferred job paths (already partitioned by
// partitionJobPathsByTenant). A deferred job does not clear Applied: the
// flag is derived from success and the error only. A successful apply
// removes the tenant's covered job paths from its unseeded list, because
// the mite now holds their number seeds. A failed apply keeps the list.
//
// A failed reload of one of a tenant's jobs sets that tenant's Error and
// keeps Applied: the seed is on the cell, so the tenant builds, and a resend
// reloads the job until a result clears the Error. The reload failures of
// other tenants never reach this tenant's Error.
// A reload failure does not keep the covered paths: the seed writes ran
// before the reloads, so the mite holds every number seed.
func applyRehydrationResult(entries []v1alpha1.CellRehydrationEntry, commandID string, success bool, rehydrationError string, deferredByTenant, coveredByTenant map[string][]string, reloadFailedByTenant map[string][]*mitev1.RehydrationReloadFailure, now time.Time) []v1alpha1.CellRehydrationEntry {
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
		// A job whose reload failed keeps its seed unconfirmed: it stays in
		// the unseeded list, so the next push resends it and reloads it. A
		// later result that does not name the job cannot clear the failure.
		reloadFailed := map[string]bool{}
		if failures := reloadFailedByTenant[e.Tenant]; len(failures) > 0 {
			messages := make([]string, 0, len(failures))
			for _, f := range failures {
				reloadFailed[f.Item] = true
				messages = append(messages, "reload "+f.Item+": "+f.Error)
			}
			msg := strings.Join(messages, "; ")
			if e.Error != "" {
				msg = e.Error + "; " + msg
			}
			e.Error = msg
		}
		e.Applied = success && rehydrationError == ""
		if e.Applied {
			var covered []string
			for _, p := range coveredByTenant[e.Tenant] {
				if !reloadFailed[p] {
					covered = append(covered, p)
				}
			}
			e.Unseeded = withoutPaths(e.Unseeded, covered)
		}
		e.At = &at
		out[i] = e
	}
	return out
}

// partitionReloadFailuresByTenant attributes each reload failure to the
// tenant that owns its item, by the same prefix rule as
// partitionJobPathsByTenant. Each message names the item and its error. An
// item that matches no tenant is dropped.
func partitionReloadFailuresByTenant(failures []*mitev1.RehydrationReloadFailure, tenants []string) map[string][]*mitev1.RehydrationReloadFailure {
	out := make(map[string][]*mitev1.RehydrationReloadFailure, len(tenants))
	for _, f := range failures {
		if f == nil {
			continue
		}
		for _, tenant := range tenants {
			if f.Item == tenant || strings.HasPrefix(f.Item, tenant+"/") {
				out[tenant] = append(out[tenant], f)
				break
			}
		}
	}
	return out
}

// partitionJobPathsByTenant attributes a flat job-path list across every
// tenant of one cell, because one command covers every tenant on it. It
// uses each path's owning tenant, by that tenant's own job-path prefix (a
// pipelines tenant's jobs all live under its own top-level folder, its
// Team name). It serves the deferred list of DesiredStateResult
// (task 6.2) and the covered job paths of a number-only push.
func partitionJobPathsByTenant(jobPaths []string, tenants []string) map[string][]string {
	out := make(map[string][]string, len(tenants))
	for _, path := range jobPaths {
		for _, tenant := range tenants {
			if path == tenant || strings.HasPrefix(path, tenant+"/") {
				out[tenant] = append(out[tenant], path)
				break
			}
		}
	}
	return out
}
