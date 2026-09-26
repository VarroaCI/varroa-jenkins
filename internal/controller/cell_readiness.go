package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// isCellController reports whether cr runs as a cell: a disposable Jenkins
// JVM with no durable state that hosts many tenants as top-level folders
// (design D4, D6).
func isCellController(cr *v1alpha1.Controller) bool {
	return v1alpha1.IsCell(cr)
}

// cellConfigGateReason returns the CellReady refusal reason for a cell's
// configuration gates, checked in the fixed order the record store, the
// blob tier, and artifact storage (design D6, task 5.1). It returns "" when
// every gate passes.
//
// claimGate and blobstoreURL are the operator's own VARROA_CLAIM_GATE and
// VARROA_BLOBSTORE_URL boot-time environment (the same values C1 stamps on
// the record store and blob tier). artifactStorage is C3's ArtifactStorage
// condition, computed inside resolveBundleForController. The caller MUST
// call this only after that resolution has run on the same tick, or it
// reads a stale or absent condition and reports ArtifactStorageLocal
// forever.
func cellConfigGateReason(claimGate, blobstoreURL string, artifactStorage *v1alpha1.ControllerCondition) string {
	if claimGate != "1" {
		return v1alpha1.ReasonRecordStoreUnsupported
	}
	params, parsed := parseBlobstoreURL(blobstoreURL)
	if !parsed || !params.IsS3 {
		return v1alpha1.ReasonBlobTierUnsupported
	}
	if artifactStorage == nil || artifactStorage.Status != metav1.ConditionTrue || artifactStorage.Reason != "External" {
		return v1alpha1.ReasonArtifactStorageLocal
	}
	return ""
}

// cellConfigGateMessage renders the CellReady condition message for a
// configuration-gate refusal.
func cellConfigGateMessage(reason string) string {
	switch reason {
	case v1alpha1.ReasonRecordStoreUnsupported:
		return "the cell needs the record store enabled (recordStore.enabled)"
	case v1alpha1.ReasonBlobTierUnsupported:
		return "the cell needs the s3 blob tier (recordStore.blobs.mode)"
	case v1alpha1.ReasonArtifactStorageLocal:
		return "the cell needs external artifact storage; the ArtifactStorage condition is not External"
	default:
		return ""
	}
}

// cellRollResult is what evaluateCellRoll computes for a cell whose
// CellReady gate is False for baseReason, a reason that is never one of the
// three configuration gates (design D6).
type cellRollResult struct {
	// Reason is the CellReady condition's Reason to set: baseReason, or
	// ReasonCellRolled once the roll has happened during this unready
	// period.
	Reason string
	// Message carries the original fault reason once Reason is
	// ReasonCellRolled; otherwise empty.
	Message string
	// DeletePod is true when the caller must delete the cell pod this
	// tick to attempt recovery.
	DeletePod bool
	// UnreadySince is cell.UnreadySince: set at the first observation and
	// held unchanged after.
	UnreadySince metav1.Time
	// RolledAt is the roll marker to keep or set once DeletePod succeeds.
	// A tick that neither rolls nor was already rolled leaves this nil.
	RolledAt *metav1.Time
	// Evacuate is true once the unready period has run twice the grace
	// period.
	Evacuate bool
}

// evaluateCellRoll implements design D6's roll and evacuate rule. cell is
// read only, never mutated — the caller commits the result, after actually
// deleting the pod when DeletePod is true. now and grace let a test inject
// a fake clock instead of the wall clock.
func evaluateCellRoll(cell *v1alpha1.CellStatus, baseReason string, now time.Time, grace time.Duration) cellRollResult {
	unreadySince := now
	var rolledAt *metav1.Time
	if cell != nil {
		if cell.UnreadySince != nil {
			unreadySince = cell.UnreadySince.Time
		}
		rolledAt = cell.RolledAt
	}
	result := cellRollResult{
		Reason:       baseReason,
		UnreadySince: metav1.NewTime(unreadySince),
		RolledAt:     rolledAt,
	}
	age := now.Sub(unreadySince)
	// unreadySince alone cannot record that a roll already happened, so a
	// later tick would delete the pod again on every pass. rolledAt not
	// earlier than unreadySince is the durable marker that stops it.
	alreadyRolled := rolledAt != nil && !rolledAt.Time.Before(unreadySince)
	if !alreadyRolled && age >= grace {
		result.DeletePod = true
		t := metav1.NewTime(now)
		result.RolledAt = &t
		alreadyRolled = true
	}
	if alreadyRolled {
		result.Reason = v1alpha1.ReasonCellRolled
		result.Message = baseReason
	}
	if age >= 2*grace {
		result.Evacuate = true
	}
	return result
}

// cellUnreadyGrace returns the unready grace period for one cell: the
// unreadyGracePeriod of the cell's effective owner, when the cell carries a
// pool label and that pool sets the field, then the chart value. It falls
// back to the package default when a narrow test fixture leaves cellsConfig
// at its zero value. The doubled value that raises status.cell.evacuate
// follows the same resolved number.
func (r *Reconciler) cellUnreadyGrace(ctx context.Context, cr *v1alpha1.Controller) time.Duration {
	if pool := r.cellPoolFor(ctx, cr); pool != nil && pool.Spec.UnreadyGracePeriod != nil &&
		pool.Spec.UnreadyGracePeriod.Duration > 0 {
		return pool.Spec.UnreadyGracePeriod.Duration
	}
	if r.cellsConfig.UnreadyGracePeriod > 0 {
		return r.cellsConfig.UnreadyGracePeriod
	}
	return defaultCellsUnreadyGracePeriod
}

// cellPoolFor reads the CellPool a cell's varroa.dev/cell-pool label names.
// It answers nil for an unlabelled cell and for a label naming a pool that
// no longer exists, which is the effective-owner rule.
func (r *Reconciler) cellPoolFor(ctx context.Context, cr *v1alpha1.Controller) *v1alpha1.CellPool {
	name := cr.Labels[cells.LabelCellPool]
	if name == "" || r.store == nil {
		return nil
	}
	pool, err := crdstore.Get[v1alpha1.CellPool](ctx, r.store, name, "")
	if err != nil {
		return nil
	}
	return pool
}

// evaluateCellReadiness sets the CellReady and WarmPool conditions for cr
// when it is a cell, applies the roll rule, and clears the eviction signal
// on recovery (design D6). now is the caller's clock; production passes
// time.Now(), and a test passes a fixed value to exercise the grace-period
// arithmetic deterministically.
//
// It MUST run after resolveBundleForController's tick: the ArtifactStorage
// condition it reads off cr.Status.Conditions is only fresh once that step
// has run this tick, and a gate evaluated before it would read a stale or
// absent condition and wedge the cell forever.
func (r *Reconciler) evaluateCellReadiness(ctx context.Context, cr *v1alpha1.Controller, now time.Time) {
	if !isCellController(cr) {
		return
	}
	if cr.Status.Cell == nil {
		cr.Status.Cell = &v1alpha1.CellStatus{}
	}
	cell := cr.Status.Cell

	artifactStorage := findCondition(cr.Status.Conditions, ConditionArtifactStorage)
	gateReason := cellConfigGateReason(r.claimGate, r.blobstoreURL, artifactStorage)

	var cond v1alpha1.ControllerCondition
	switch {
	case gateReason != "":
		// A configuration gate never rolls and never replaces its own
		// reason: unreadySince, rolledAt, and evacuate are left untouched.
		cond = v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionCellReady,
			Status:  metav1.ConditionFalse,
			Reason:  gateReason,
			Message: cellConfigGateMessage(gateReason),
		}
	case cr.Status.Phase != v1alpha1.ControllerPhaseConnected:
		cond = v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionCellReady,
			Status: metav1.ConditionFalse,
		}
		baseReason := v1alpha1.ReasonNotProvisioned
		if cr.Status.Phase == v1alpha1.ControllerPhaseRunning && isStreamLost(cr) {
			baseReason = v1alpha1.ReasonMiteUnreachable
		}
		grace := r.cellUnreadyGrace(ctx, cr)
		roll := evaluateCellRoll(cell, baseReason, now, grace)
		cell.UnreadySince = &roll.UnreadySince
		cell.Evacuate = roll.Evacuate
		previousRolledAt := cell.RolledAt
		cell.RolledAt = roll.RolledAt
		cond.Reason, cond.Message = roll.Reason, roll.Message
		if roll.DeletePod {
			if err := r.client.DeleteControllerPod(ctx, cr.Namespace, cr.Name); err != nil {
				if r.Logger != nil {
					r.Logger.Warn("roll cell pod failed", "controller", cr.Namespace+"/"+cr.Name, "error", err)
				}
				// The delete did not happen: revert the tentative roll
				// marker so the next tick retries instead of skipping it.
				cell.RolledAt = previousRolledAt
				cond.Reason, cond.Message = baseReason, ""
			} else {
				r.emitOperatorEvent("cell.rolled", cr,
					fmt.Sprintf("rolled the cell pod to recover from %s", roll.Message), roll.Message)
			}
		}
	default:
		cell.UnreadySince = nil
		cell.RolledAt = nil
		cell.Evacuate = false
		cond = v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonConnected,
		}
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, cond)

	warm := len(cell.Tenants) == 0 && cond.Status == metav1.ConditionTrue
	cell.Warm = warm
	warmCond := v1alpha1.ControllerCondition{Type: v1alpha1.ConditionWarmPool, Status: metav1.ConditionFalse, Reason: "NotFree"}
	if warm {
		warmCond.Status = metav1.ConditionTrue
		warmCond.Reason = "Free"
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, warmCond)
}

// sortedTenantCopy returns a sorted copy of in, or nil for an empty in.
func sortedTenantCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// recordCellHibernationIgnored appends a HibernationIgnored TenantWarning
// for every tenant this cell currently hosts (design D16): a cell accepts
// spec.hibernation.enabled and ignores it, because it keeps nothing on disk
// for a hibernate-then-wake cycle to preserve. It replaces only this
// reason's prior entries, leaving every other reason's entries untouched —
// each writer of status.cell.tenantWarnings rebuilds only its own reason
// class each tick (design D4).
func (r *Reconciler) recordCellHibernationIgnored(cr *v1alpha1.Controller) {
	if cr.Status.Cell == nil {
		cr.Status.Cell = &v1alpha1.CellStatus{}
	}
	fresh := make([]v1alpha1.TenantWarning, 0, len(cr.Status.Cell.Tenants))
	for _, tenant := range cr.Status.Cell.Tenants {
		fresh = append(fresh, v1alpha1.TenantWarning{
			Tenant:  tenant,
			Reason:  v1alpha1.TeamReasonHibernationIgnored,
			Message: "spec.hibernation.enabled is ignored on a cell; a cell holds no durable state to hibernate",
		})
	}
	cr.Status.Cell.TenantWarnings = replaceTenantWarnings(cr.Status.Cell.TenantWarnings, v1alpha1.TeamReasonHibernationIgnored, fresh)
}

// replaceTenantWarnings returns existing with every entry of the given
// reason replaced by fresh, leaving every other reason's entries
// untouched.
func replaceTenantWarnings(existing []v1alpha1.TenantWarning, reason string, fresh []v1alpha1.TenantWarning) []v1alpha1.TenantWarning {
	kept := make([]v1alpha1.TenantWarning, 0, len(existing)+len(fresh))
	for _, w := range existing {
		if w.Reason != reason {
			kept = append(kept, w)
		}
	}
	return append(kept, fresh...)
}
