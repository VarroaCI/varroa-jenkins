package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
)

// annotationUpgradeRelease is the Controller annotation a brood upgrade
// dispatch stamps with the exact image it approved for release; this gate
// consumes and clears it once the version-roll it names has been applied.
const annotationUpgradeRelease = "varroa.dev/upgrade-release"

// evaluateVersionRollGateFn holds (*Reconciler).evaluateVersionRollGate as a
// package-level function value, since controller_controller.go wires
// `rec.versionRollGate` to upgradePolicyVersionRollGate instead. Without a
// value-usage somewhere, evaluateVersionRollGate would only ever be called
// directly and unparam would flag its unused ctx parameter — corecompat.go
// itself must not change to work around that.
var evaluateVersionRollGateFn = (*Reconciler).evaluateVersionRollGate

// upgradePolicyVersionRollGate wraps rec.evaluateVersionRollGate with the
// upgradePolicy dial, scoped to profile-driven line advancement only.
// Assigned once, at construction, as rec.versionRollGate.
func (r *Reconciler) upgradePolicyVersionRollGate(ctx context.Context, cr *v1alpha1.Controller, currentImage, targetImage string) (bool, string, string) {
	allow, reason, message := evaluateVersionRollGateFn(r, ctx, cr, currentImage, targetImage)
	if !allow {
		// core-compat already blocked it; upgradePolicy is irrelevant here.
		clearUpgradePending(cr)
		return allow, reason, message
	}
	// Scope: upgradePolicy holds a profile-driven image delta only. MatchKind
	// cannot be used to scope this: a bare-line-string cr.Spec.Version that
	// equals a profile's own Spec.Version resolves as MatchExact per
	// ResolveProfile, not MatchLine, so scoping on MatchLine alone would miss
	// the normal profile-advancement case. The real signal is the same
	// condition jenkinsImageForVersion branches on to source targetImage from
	// the profile at all (profile != nil && profile.Spec.ResolveVersion !=
	// ""), minus the resourceOverlay-governs case reconcileVersionRoll
	// already computes for its own converged-message branch — an explicit
	// resourceOverlay image always proceeds regardless of what any profile
	// says, matching effectiveDesiredJenkinsImage's own overlay-wins
	// precedence.
	profile, _ := r.resolveProfileForCr(cr)
	overlayGoverns := false
	if cr.Spec.ResourceOverlay != nil && cr.Spec.ResourceOverlay.StatefulSet != "" {
		if _, ok, ovErr := overlay.ImageOverride([]byte(cr.Spec.ResourceOverlay.StatefulSet), "jenkins"); ovErr == nil && ok {
			overlayGoverns = true
		}
	}
	if overlayGoverns || profile == nil || profile.Spec.ResolveVersion == "" {
		clearUpgradePending(cr)
		return allow, reason, message
	}
	// A brood upgrade dispatch stamps this annotation with the exact image it
	// approved for release, whether that came from admin's manual approval or
	// from a targetVersion bulk-write; a match here means the roll this dial
	// would otherwise hold has already been explicitly authorized, so it takes
	// precedence over upgradePolicy=manual without ever consulting
	// ProvisioningDefaults.
	if released, ok := cr.Annotations[annotationUpgradeRelease]; ok && released == targetImage {
		if err := crdstore.PatchAnnotations[v1alpha1.Controller](ctx, r.store, cr.Name, cr.Namespace, map[string]*string{
			annotationUpgradeRelease: nil,
		}); err != nil {
			msg := "upgrade held: release annotation clear failed, retrying"
			setUpgradePending(cr, msg)
			return false, "UpgradePending", msg
		}
		clearUpgradePending(cr)
		return allow, reason, message
	}
	// Uses the same inline
	// crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, "")
	// pattern as other ProvisioningDefaults call sites in this file — there is
	// no `rec.provisioningDefaultsFn` seam.
	//
	// Unlike those other call sites, which treat `err == nil && d != nil` as
	// the only "defaults present" case and collapse not-found and any other
	// read error into "no defaults", this gate cannot reuse that convention
	// verbatim: its whole purpose is to enforce a hold, so a transient read
	// error must not silently fail open into allowing a roll a stored
	// `manual` policy would have blocked. Not-found is still treated as
	// "policy unset, default auto" (matching UpgradeIsManual's own documented
	// empty-means-auto default); any other error holds, conservatively, same
	// as a confirmed `manual` policy.
	defaults, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, "")
	if err != nil && !apierrors.IsNotFound(err) {
		setUpgradePending(cr, "upgrade held: ProvisioningDefaults unreadable, failing safe")
		return false, "UpgradePending", "upgrade held: ProvisioningDefaults unreadable, failing safe"
	}
	if defaults == nil || !defaults.Spec.UpgradeIsManual() {
		clearUpgradePending(cr)
		return allow, reason, message
	}
	msg := fmt.Sprintf("upgrade to %s held by upgradePolicy=manual", targetImage)
	setUpgradePending(cr, msg)
	return false, "UpgradePending", msg
}

// setUpgradePending stamps ConditionUpgradePending true, mirroring
// setVersionUpgradeBlocked's shape (corecompat.go).
func setUpgradePending(cr *v1alpha1.Controller, message string) {
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionUpgradePending,
		Status:  metav1.ConditionTrue,
		Reason:  "UpgradePending",
		Message: message,
	})
}

// clearUpgradePending clears ConditionUpgradePending, mirroring
// setVersionUpgradeBlocked's shape (corecompat.go).
func clearUpgradePending(cr *v1alpha1.Controller) {
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionUpgradePending,
		Status:  metav1.ConditionFalse,
		Reason:  "UpgradeNotPending",
		Message: "no profile-driven version roll is held by upgradePolicy",
	})
}
