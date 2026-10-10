package controller

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

const (
	// condTypeDependenciesKnown reports whether every stored plugin version has a
	// known dependency list. It is informational and does not gate Ready.
	condTypeDependenciesKnown = "DependenciesKnown"

	reasonAllDependenciesKnown = "AllDependenciesKnown"
	reasonUnknownDependencies  = "UnknownDependencies"

	// maxUnknownDependencies bounds the status list. The count is not truncated.
	maxUnknownDependencies = 50
)

// applyUnknownDependencies records the stored plugin versions whose dependency
// list the update center cannot read. It writes the count, the first
// maxUnknownDependencies entries in name and version order, and the
// DependenciesKnown condition.
func (r *UpdateCenterReconciler) applyUnknownDependencies(status *v1alpha1.UpdateCenterStatus, inventory []inventoryEntry) {
	seen := make(map[string]struct{})
	var unknown []v1alpha1.UpdateCenterPluginRef
	for _, e := range inventory {
		if e.DependenciesKnown {
			continue
		}
		key := e.Name + "@" + e.Version
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		unknown = append(unknown, v1alpha1.UpdateCenterPluginRef{Plugin: e.Name, Version: e.Version})
	}
	sort.Slice(unknown, func(i, j int) bool {
		if unknown[i].Plugin != unknown[j].Plugin {
			return unknown[i].Plugin < unknown[j].Plugin
		}
		return unknown[i].Version < unknown[j].Version
	})

	status.UnknownDependencyCount = len(unknown)
	status.UnknownDependencies = nil
	for i := 0; i < len(unknown) && i < maxUnknownDependencies; i++ {
		status.UnknownDependencies = append(status.UnknownDependencies, unknown[i])
	}

	cond := v1alpha1.UpdateCenterCondition{
		Type:   condTypeDependenciesKnown,
		Status: metav1.ConditionTrue,
		Reason: reasonAllDependenciesKnown,
	}
	if len(unknown) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonUnknownDependencies
		cond.Message = fmt.Sprintf("%d stored plugin versions have an unknown dependency list; "+
			"the update center could not resolve their dependency lists", len(unknown))
	}
	r.setCondition(status, cond)
}
