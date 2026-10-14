package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// defaultMiteCommandDeadline is the mite's own bound for one
// desired-state command when the command sets no deadline. It must equal
// defaultCommandDeadline in cmd/mite/agent.go.
const defaultMiteCommandDeadline = 20 * time.Minute

// rehydrationResultSlack is the time a result can take to travel back
// after the mite's command deadline ends.
const rehydrationResultSlack = time.Minute

// rehydrationPlacement is the placement generation and cell instance a
// tenant's seeds were built for.
type rehydrationPlacement struct {
	Generation   int64
	CellInstance string
}

// rehydrationSendPlan is what attachCellRehydration answers for one
// tick. Tenants names the tenants whose entries describe Bundle. Held is
// true when Bundle is the in-flight bundle that the tick holds: the tick
// attaches nothing, and the caller attaches Bundle only to a push that
// goes out for another reason. Entries holds the new entry of each
// tenant whose status entry still describes a kept in-flight push; a
// delivered push writes it.
type rehydrationSendPlan struct {
	Bundle             *mitev1.RehydrationBundle
	Tenants            map[string]rehydrationPlacement
	NumberOnlyJobPaths []string
	// CoveredJobPaths lists every job path Bundle carries a seed for.
	CoveredJobPaths []string
	// CellInstance is the cell instance Bundle was built for.
	CellInstance string
	Held         bool
	Entries      map[string]v1alpha1.CellRehydrationEntry
}

// rehydrationPush is one cell's delivered rehydration push that has no
// result yet. CommandIDs lists every delivered command that carried
// Bundle. The entries carry CommandIDs[0], so a result for any of the
// IDs updates them. The mite keeps only the newest desired state, so a
// later push can replace an earlier one that did not start.
//
// The record lives in memory only. An operator restart or a shard move
// loses it, and the next tick then sends the bundle once more.
type rehydrationPush struct {
	CommandIDs         []string
	Bundle             *mitev1.RehydrationBundle
	Tenants            map[string]rehydrationPlacement
	NumberOnlyJobPaths []string
	CoveredJobPaths    []string
	CellInstance       string
	Until              time.Time
}

// rehydrationResultMatch is the in-flight push that one DesiredStateResult
// answers. StampedID is the command ID the entries carry. The job path
// lists and CellInstance come from the push, so the result commits the
// seeds that the push actually sent.
type rehydrationResultMatch struct {
	StampedID          string
	NumberOnlyJobPaths []string
	CoveredJobPaths    []string
	CellInstance       string
}

// rehydrationPushWindow answers how long a delivered push holds: the
// command's own deadline on the mite plus the result's travel time. A
// shorter window lets a slow apply never count, because each resend
// replaces the stamped command ID before the result arrives.
func rehydrationPushWindow(desired *mitev1.DesiredStateCommand) time.Duration {
	deadline := defaultMiteCommandDeadline
	if desired.CommandDeadlineSec > 0 {
		deadline = time.Duration(desired.CommandDeadlineSec) * time.Second
	}
	return deadline + rehydrationResultSlack
}

// rehydrationInFlightFor answers a copy of cellKey's in-flight push, or
// nil.
func (r *Reconciler) rehydrationInFlightFor(cellKey string) *rehydrationPush {
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	push, ok := r.rehydrationInFlight[cellKey]
	if !ok {
		return nil
	}
	out := *push
	out.CommandIDs = append([]string(nil), push.CommandIDs...)
	return &out
}

// dropRehydrationInFlight forgets cellKey's in-flight push.
func (r *Reconciler) dropRehydrationInFlight(cellKey string) {
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	delete(r.rehydrationInFlight, cellKey)
}

// takeRehydrationResult consumes cellKey's in-flight push when commandID
// is one of its commands. It answers the push's match, and ok is false
// when no in-flight push holds commandID.
func (r *Reconciler) takeRehydrationResult(cellKey, commandID string) (match rehydrationResultMatch, ok bool) {
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	push, found := r.rehydrationInFlight[cellKey]
	if !found {
		return rehydrationResultMatch{}, false
	}
	for _, id := range push.CommandIDs {
		if id == commandID {
			delete(r.rehydrationInFlight, cellKey)
			return rehydrationResultMatch{
				StampedID:          push.CommandIDs[0],
				NumberOnlyJobPaths: push.NumberOnlyJobPaths,
				CoveredJobPaths:    push.CoveredJobPaths,
				CellInstance:       push.CellInstance,
			}, true
		}
	}
	return rehydrationResultMatch{}, false
}

// markRehydrationSent records a delivered push that carried a
// rehydration bundle. A new bundle stamps the command ID on each entry it
// describes and starts a new in-flight record. A held bundle adds the
// command ID to the record and changes no entry. The caller calls it only
// after a delivered push: a stamp for a push that did not go out would
// match no result, and a push with no bundle must never mark a tenant
// applied.
func (r *Reconciler) markRehydrationSent(cr *v1alpha1.Controller, plan *rehydrationSendPlan, desired *mitev1.DesiredStateCommand, now time.Time) {
	if plan == nil || desired.Rehydration == nil || desired.Rehydration != plan.Bundle {
		return
	}
	cellKey := cr.Namespace + "/" + cr.Name
	r.numberCacheMu.Lock()
	defer r.numberCacheMu.Unlock()
	if r.rehydrationInFlight == nil {
		r.rehydrationInFlight = make(map[string]*rehydrationPush)
	}
	if plan.Held {
		if push, ok := r.rehydrationInFlight[cellKey]; ok && push.Bundle == plan.Bundle {
			push.CommandIDs = append(push.CommandIDs, desired.CommandId)
		}
		return
	}
	if cr.Status.Cell != nil {
		at := metav1.NewTime(now)
		for i := range cr.Status.Cell.Rehydration {
			if entry, ok := plan.Entries[cr.Status.Cell.Rehydration[i].Tenant]; ok {
				cr.Status.Cell.Rehydration[i] = entry
			}
			if _, ok := plan.Tenants[cr.Status.Cell.Rehydration[i].Tenant]; ok {
				cr.Status.Cell.Rehydration[i].CommandID = desired.CommandId
				cr.Status.Cell.Rehydration[i].At = &at
			}
		}
	}
	r.rehydrationInFlight[cellKey] = &rehydrationPush{
		CommandIDs:         []string{desired.CommandId},
		Bundle:             plan.Bundle,
		Tenants:            plan.Tenants,
		NumberOnlyJobPaths: append([]string(nil), plan.NumberOnlyJobPaths...),
		CoveredJobPaths:    append([]string(nil), plan.CoveredJobPaths...),
		CellInstance:       plan.CellInstance,
		Until:              now.Add(rehydrationPushWindow(desired)),
	}
}

// heldRehydrationPlan answers a held plan when push can still hold for
// this tick: the window is open, and each tenant the push covered is
// still pending with the same placement and an entry that carries the
// push's stamped command ID. Any other state answers nil, and the caller
// drops the record and builds a new bundle. The caller also ends the hold
// when a tenant outside the push has a seed to send.
func heldRehydrationPlan(push *rehydrationPush, pending map[string]rehydrationPlacement, entries map[string]*v1alpha1.CellRehydrationEntry, now time.Time) *rehydrationSendPlan {
	if push == nil || len(push.CommandIDs) == 0 || !now.Before(push.Until) || len(push.Tenants) == 0 {
		return nil
	}
	for tenant, sent := range push.Tenants {
		placement, ok := pending[tenant]
		if !ok || sent != placement {
			return nil
		}
		entry := entries[tenant]
		if entry == nil || entry.CommandID != push.CommandIDs[0] {
			return nil
		}
	}
	return &rehydrationSendPlan{
		Bundle: push.Bundle, Tenants: push.Tenants, NumberOnlyJobPaths: push.NumberOnlyJobPaths,
		CoveredJobPaths: push.CoveredJobPaths, CellInstance: push.CellInstance, Held: true,
	}
}
