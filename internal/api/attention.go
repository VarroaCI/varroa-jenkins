package api

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// AttentionJSON is the one-line answer to "why is this controller not
// healthy". It is present on list summaries as well as the detail DTO so the
// fleet views can flag a wedged controller without fetching every detail.
type AttentionJSON struct {
	Kind    string  `json:"kind"`
	Reason  string  `json:"reason,omitempty"`
	Message string  `json:"message,omitempty"`
	Since   *string `json:"since,omitempty"`
}

// Attention kinds, in precedence order: the earliest present is reported,
// because it is the root cause of any later ones (a blocked reconcile
// explains a boot failure that follows it).
const (
	AttentionFailed           = "failed"
	AttentionReconcileBlocked = "reconcileBlocked"
	AttentionBootFailed       = "bootFailed"
	AttentionPluginRollFailed = "pluginRollFailed"
	AttentionApplyFailed      = "applyFailed"
)

// buildAttentionJSON reports at most one reason a controller needs an
// operator's attention. An asleep controller reports nothing: its pod is gone,
// the runtime conditions it carries are stale until it wakes, and
// LastApplyResult is never cleared by hibernation.
func buildAttentionJSON(cr *v1alpha1.Controller) *AttentionJSON {
	trueCond := func(typ v1alpha1.ControllerConditionType) *v1alpha1.ControllerCondition {
		for i := range cr.Status.Conditions {
			c := &cr.Status.Conditions[i]
			if c.Type == typ && c.Status == metav1.ConditionTrue {
				return c
			}
		}
		return nil
	}
	fromCond := func(kind string, c *v1alpha1.ControllerCondition, since *metav1.Time) *AttentionJSON {
		a := &AttentionJSON{Kind: kind, Reason: c.Reason, Message: c.Message}
		if since == nil && !c.LastTransitionTime.IsZero() {
			since = &c.LastTransitionTime
		}
		if since != nil {
			s := since.UTC().Format(time.RFC3339)
			a.Since = &s
		}
		return a
	}

	switch cr.Status.Phase {
	case v1alpha1.ControllerPhaseHibernated, v1alpha1.ControllerPhaseStopped:
		return nil
	}
	if cr.Status.Phase == v1alpha1.ControllerPhaseFailed {
		if c := trueCond(v1alpha1.ConditionFailed); c != nil {
			return fromCond(AttentionFailed, c, nil)
		}
		return &AttentionJSON{Kind: AttentionFailed}
	}
	if c := trueCond(v1alpha1.ConditionReconcileBlocked); c != nil {
		return fromCond(AttentionReconcileBlocked, c, cr.Status.LastReconcileErrorAt)
	}
	if c := trueCond(v1alpha1.ConditionJenkinsBootFailed); c != nil {
		return fromCond(AttentionBootFailed, c, nil)
	}
	if c := trueCond(v1alpha1.ConditionPluginRollFailed); c != nil {
		return fromCond(AttentionPluginRollFailed, c, nil)
	}
	if r := cr.Status.LastApplyResult; r != nil && !r.Succeeded {
		a := &AttentionJSON{Kind: AttentionApplyFailed, Reason: "ApplyFailed", Message: applyFailureMessage(r)}
		if !r.Timestamp.IsZero() {
			s := r.Timestamp.UTC().Format(time.RFC3339)
			a.Since = &s
		}
		return a
	}
	return nil
}

// applyFailureMessage joins the failed sections as "name: error" so the
// one-line summary names what broke without the caller walking Sections.
func applyFailureMessage(r *v1alpha1.ApplyResult) string {
	var parts []string
	for _, s := range r.Sections {
		if s.OK {
			continue
		}
		if s.Error != "" {
			parts = append(parts, s.Name+": "+s.Error)
		} else {
			parts = append(parts, s.Name)
		}
	}
	if len(parts) == 0 {
		return "apply failed"
	}
	return strings.Join(parts, "; ")
}
