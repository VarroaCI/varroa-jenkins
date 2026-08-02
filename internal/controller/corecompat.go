package controller

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

// ProfilePluginSetReady returns true when p is non-nil and its PluginSetReady
// condition is true.
func ProfilePluginSetReady(p *v1alpha1.JenkinsVersionProfile) bool {
	return p != nil && profileIsPluginSetReady(p)
}

// CompatVerdict is the result of a core-compatibility evaluation.
type CompatVerdict int

const (
	// CompatOK means the requested core is safe to use.
	CompatOK CompatVerdict = iota
	// CompatUnsafe means the requested core is older than the plugin baseline.
	CompatUnsafe
	// CompatUnknown means the version string could not be parsed.
	CompatUnknown
)

// CompatResult carries the full outcome of EvaluateCoreCompat.
type CompatResult struct {
	Verdict         CompatVerdict
	EffectiveSource string
	UsingBaseline   bool
	Reason          string
	Message         string
}

// EvaluateCoreCompat evaluates whether a requested Jenkins core version is
// compatible with the available plugin set. The decision follows the
// guard-version-upgrade-path design (§3).
//
// Parameters:
//   - version: the raw spec.version from the Controller CR.
//   - profile: the resolved JenkinsVersionProfile (nil when none matched).
//   - kind:    how the profile was matched (MatchBaseline when no profile).
//   - pluginSetReady: whether the profile's plugin set is materialized.
//   - baseline: the embedded plugin lock baseline version string.
func EvaluateCoreCompat(version string, profile *v1alpha1.JenkinsVersionProfile, kind MatchKind, pluginSetReady bool, baseline string) CompatResult {
	v := strings.TrimSpace(version)

	// Unpinned version — always compatible; core and plugin set both come from
	// the embedded baseline, so they cannot skew.
	if v == "" || v == "lts" {
		return CompatResult{
			Verdict:         CompatOK,
			Reason:          v1alpha1.ReasonCoreCompatible,
			EffectiveSource: baseline,
			UsingBaseline:   true,
			Message:         "version unpinned; deploys the embedded plugin-lock baseline core, compatible",
		}
	}

	// A profile matched (exact or line) and its plugin set is ready.
	if profile != nil && kind != MatchBaseline && pluginSetReady {
		return CompatResult{
			Verdict:         CompatOK,
			Reason:          v1alpha1.ReasonCoreCompatible,
			EffectiveSource: profile.Spec.Version,
			UsingBaseline:   false,
			Message:         fmt.Sprintf("core %s matches version profile %s; compatible", v, profile.Spec.Version),
		}
	}

	// Effective set is the embedded baseline (no profile, or matched-not-ready / metadata-only).
	cv, okC := jenkinsver.Core(v)
	bv, okB := jenkinsver.Core(baseline)

	if !okC || !okB {
		return CompatResult{
			Verdict:         CompatUnknown,
			Reason:          v1alpha1.ReasonUnparseableVersion,
			EffectiveSource: baseline,
			UsingBaseline:   true,
			Message:         fmt.Sprintf("version %q is unparseable and no version profile vouches for it; install a JenkinsVersionProfile for %s", v, v),
		}
	}

	if jenkinsver.Compare(cv, bv) < 0 {
		return CompatResult{
			Verdict:         CompatUnsafe,
			Reason:          v1alpha1.ReasonCoreOlderThanPluginBaseline,
			EffectiveSource: baseline,
			UsingBaseline:   true,
			Message:         fmt.Sprintf("core %s is older than the plugin baseline %s; install a JenkinsVersionProfile for %s", v, baseline, v),
		}
	}

	return CompatResult{
		Verdict:         CompatOK,
		Reason:          v1alpha1.ReasonCoreCompatible,
		EffectiveSource: baseline,
		UsingBaseline:   true,
		Message:         fmt.Sprintf("core %s is at or above the plugin baseline %s; compatible", v, baseline),
	}
}

// evaluateVersionRollGate is the version-roll gate hook. Its signature matches
// the gate function field so it can be plugged directly into the version-roll
// reconciliation path.
func (r *Reconciler) evaluateVersionRollGate(ctx context.Context, cr *v1alpha1.Controller, currentImage, targetImage string) (bool, string, string) {
	profile, kind := r.resolveProfileForCr(cr)
	res := EvaluateCoreCompat(cr.Spec.Version, profile, kind, ProfilePluginSetReady(profile), pluginlock.Baseline())
	r.setVersionUpgradeBlocked(cr, res)
	if res.Verdict == CompatOK {
		return true, "", ""
	}
	return false, res.Reason, res.Message
}

// setVersionUpgradeBlocked stamps or clears the ConditionVersionUpgradeBlocked
// condition on the Controller CR status using the supplied CompatResult.
func (r *Reconciler) setVersionUpgradeBlocked(cr *v1alpha1.Controller, res CompatResult) {
	status := metav1.ConditionFalse
	if res.Verdict != CompatOK {
		status = metav1.ConditionTrue
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionVersionUpgradeBlocked,
		Status:  status,
		Reason:  res.Reason,
		Message: res.Message,
	})
}
