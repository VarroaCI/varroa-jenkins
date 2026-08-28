package controller

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// The five verdicts, frozen by the proposal. There is deliberately no sixth:
// pluginVersionConflict fires on ANY divergence between a requested pin and a
// lock pin, in either direction, which is a provisioning conflict with no
// compatibility judgement attached. That divergence is surfaced as data in the
// detail view's lock-pin column instead.
const (
	verdictCompatible      = "compatible"
	verdictCoreTooOld      = "core-too-old"
	verdictDepBelowMinimum = "dep-below-minimum"
	verdictLockTooOld      = "lock-too-old"
	verdictUnknown         = "unknown"
)

// compatWarningCondition is the condition type summarizing an item's verdicts.
const compatWarningCondition = "CompatWarning"

// verdictRank orders verdicts by precedence, worst first:
// core-too-old > dep-below-minimum > lock-too-old > unknown > compatible.
func verdictRank(v string) int {
	switch v {
	case verdictCoreTooOld:
		return 0
	case verdictDepBelowMinimum:
		return 1
	case verdictLockTooOld:
		return 2
	case verdictUnknown:
		return 3
	default:
		return 4
	}
}

// verdictBlocking reports whether a verdict warrants a CompatWarning.
func verdictBlocking(v string) bool {
	switch v {
	case verdictCoreTooOld, verdictDepBelowMinimum, verdictLockTooOld:
		return true
	}
	return false
}

// evaluateCompat computes one advisory verdict per profile from the FINAL
// closure, not from which branch the resolver happened to take.
//
// lock-too-old in particular is evaluated against every profile lock
// independently: D6 defines it as "a dependency minimum exceeding the profile
// lock's pin", which is a property of the profile, not of how the version was
// chosen. Tying it to the resolver's lock-fallback row would silently skip it
// whenever the store supplied the version — the common case.
//
// Verdicts never set status.valid = false, never prevent an item being selected
// into a ComposedBundle, and never block provisioning. EvaluateCoreCompat and
// pluginVersionConflict remain the only enforcement.
func evaluateCompat(cl *ucClosure, inv *ucInventory, profiles []ucProfile) []v1alpha1.CatalogItemCompat {
	if len(profiles) == 0 {
		return nil
	}
	out := make([]v1alpha1.CatalogItemCompat, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, evaluateProfileCompat(cl, inv, p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out
}

// closureMember is one (artifactId, version, provenance, minimum, shortfall)
// tuple, the root included. Verdicts are computed over the closure PLUS the
// root, because a transitive dependency needing a newer core matters exactly as
// much as the root doing so.
type closureMember struct {
	name        string
	version     string
	minimum     string
	provenance  string
	shortfall   bool
	lockSources []string
}

func closureMembers(cl *ucClosure) []closureMember {
	out := make([]closureMember, 0, len(cl.Selected)+1)
	out = append(out, closureMember{
		name:       cl.RootName,
		version:    cl.RootVersion,
		minimum:    cl.RootMinimum,
		provenance: provenanceStore,
		shortfall:  cl.RootShortfall,
	})
	for _, name := range sortedKeys(cl.Selected) {
		sel := cl.Selected[name]
		out = append(out, closureMember{
			name:        name,
			version:     sel.Version,
			minimum:     cl.Minimums[name],
			provenance:  sel.Provenance,
			shortfall:   sel.Shortfall,
			lockSources: sel.LockSources,
		})
	}
	return out
}

func evaluateProfileCompat(cl *ucClosure, inv *ucInventory, p ucProfile) v1alpha1.CatalogItemCompat {
	res := v1alpha1.CatalogItemCompat{Profile: p.Name, JenkinsVersion: p.EffectiveCore}

	// An ineligible profile short-circuits BEFORE any other row. It is not one
	// alternative among five — it is a statement that this profile cannot be
	// judged at all. Its lock may be stale, so letting the higher-precedence
	// core-too-old or lock-too-old rows run against it would emit a concrete
	// verdict derived from data already declared untrustworthy.
	if !p.Eligible {
		res.Verdict = verdictUnknown
		res.Message = "profile plugin set is not ready or has no materialized lock; compatibility cannot be judged"
		return res
	}

	members := closureMembers(cl)

	// core-too-old — the ONLY use of internal/jenkinsver in this change, and
	// both operands really are Jenkins core versions. jenkinsver.Core cuts at
	// the first '-' and requires numeric segments, so it must never touch a
	// plugin version.
	coreSeen := false
	var coreOffenders []string
	coreParses := p.EffectiveCore != ""
	for _, m := range members {
		e, ok := inv.entry(m.name, m.version)
		if !ok || e.RequiredCore == "" {
			continue
		}
		coreSeen = true
		if !coreParses {
			continue
		}
		atLeast, ok := jenkinsver.AtLeast(p.EffectiveCore, e.RequiredCore)
		if !ok {
			// The profile's core (or the plugin's requiredCore) is unparseable.
			// An unjudgeable comparison must not produce a warning.
			coreParses = false
			continue
		}
		if !atLeast {
			coreOffenders = append(coreOffenders, fmt.Sprintf("%s@%s requires core %s", m.name, m.version, e.RequiredCore))
		}
	}
	if len(coreOffenders) > 0 {
		res.Verdict = verdictCoreTooOld
		res.Message = fmt.Sprintf("profile deploys Jenkins %s: %s", p.EffectiveCore, strings.Join(coreOffenders, "; "))
		return res
	}

	// dep-below-minimum — a STORE-side shortfall. It is the same for every
	// profile, so it is recorded against all of them. Classifying by a bare
	// "selected < minimum" test would emit this for a lock-sourced shortfall
	// too, and its higher precedence would then hide the lock-too-old that D6
	// requires.
	var storeShort []string
	for _, m := range members {
		if m.shortfall && m.provenance == provenanceStore {
			storeShort = append(storeShort, fmt.Sprintf("%s pinned at %s, below the declared minimum %s", m.name, m.version, m.minimum))
		}
	}
	if len(storeShort) > 0 {
		res.Verdict = verdictDepBelowMinimum
		res.Message = "the store's best available version is below a declared minimum: " + strings.Join(storeShort, "; ")
		return res
	}

	// lock-too-old — two independent clauses. The first is provenance-blind: a
	// profile whose lock pins a closure entry below its minimum warrants the
	// warning even when the store supplied a satisfying version.
	var lockShort []string
	for _, m := range members {
		if m.minimum != "" {
			if pinned, ok := p.Lock[m.name]; ok && pinned != "" && !pluginver.AtLeast(pinned, m.minimum) {
				lockShort = append(lockShort, fmt.Sprintf("lock pins %s at %s, below the effective minimum %s", m.name, pinned, m.minimum))
				continue
			}
		}
		// Second clause: this profile is one of the locks that supplied a pin
		// that itself fell short.
		if m.shortfall && m.provenance == provenanceLock && containsString(m.lockSources, p.Name) {
			lockShort = append(lockShort, fmt.Sprintf("lock supplied %s at %s, below the effective minimum %s", m.name, m.version, m.minimum))
		}
	}
	if len(lockShort) > 0 {
		res.Verdict = verdictLockTooOld
		res.Message = strings.Join(lockShort, "; ")
		return res
	}

	if !coreSeen {
		res.Verdict = verdictUnknown
		res.Message = "no plugin in the closure declares a required core version"
		return res
	}
	if !coreParses {
		res.Verdict = verdictUnknown
		res.Message = fmt.Sprintf("profile core version %q cannot be compared", p.EffectiveCore)
		return res
	}

	res.Verdict = verdictCompatible
	return res
}

// compatWarning summarizes an item's verdicts as a condition. With zero
// profiles the verdict list is empty and the condition is Unknown: there is
// nothing to judge against, and evaluating against the embedded pluginlock
// baseline is deliberately out of scope.
func compatWarning(compat []v1alpha1.CatalogItemCompat) v1alpha1.TemplateCatalogCondition {
	now := metav1.Now()
	if len(compat) == 0 {
		return v1alpha1.TemplateCatalogCondition{
			Type:               compatWarningCondition,
			Status:             metav1.ConditionUnknown,
			Reason:             "NoProfiles",
			Message:            "no JenkinsVersionProfile exists to evaluate compatibility against",
			LastTransitionTime: now,
		}
	}

	worst := verdictCompatible
	var offenders []string
	for _, c := range compat {
		if verdictRank(c.Verdict) < verdictRank(worst) {
			worst = c.Verdict
		}
		if verdictBlocking(c.Verdict) {
			offenders = append(offenders, c.Profile)
		}
	}
	if len(offenders) == 0 {
		return v1alpha1.TemplateCatalogCondition{
			Type:               compatWarningCondition,
			Status:             metav1.ConditionFalse,
			Reason:             worst,
			Message:            "no profile reports a compatibility warning",
			LastTransitionTime: now,
		}
	}

	shown := offenders
	if len(shown) > 3 {
		shown = shown[:3]
	}
	msg := fmt.Sprintf("%d of %d profiles report a compatibility warning: %s",
		len(offenders), len(compat), strings.Join(shown, ", "))
	if len(offenders) > len(shown) {
		msg += fmt.Sprintf(" (+%d more)", len(offenders)-len(shown))
	}
	return v1alpha1.TemplateCatalogCondition{
		Type:               compatWarningCondition,
		Status:             metav1.ConditionTrue,
		Reason:             worst,
		Message:            msg,
		LastTransitionTime: now,
	}
}
