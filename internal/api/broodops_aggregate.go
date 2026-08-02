package api

import (
	"sort"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// broodRunGroup is a group of children sharing the same (namespace, name).
type broodRunGroup struct {
	Namespace string
	Name      string
	Children  []ClusterBroodOp
}

// aggregatePhase computes the run phase from child phases per §2 rules.
// Returns (phase, ok). ok=false on an empty slice (empty-set guard).
// An empty-string phase counts as Pending.
func aggregatePhase(children []v1alpha1.BroodOperationPhase) (v1alpha1.BroodOperationPhase, bool) {
	if len(children) == 0 {
		return "", false
	}

	// Classify phases.
	hasSucceeded := false
	hasFailed := false
	hasCanceled := false
	hasSuspended := false
	hasPending := false
	hasRunning := false

	for _, p := range children {
		switch p {
		case v1alpha1.BroodOperationPhaseSucceeded:
			hasSucceeded = true
		case v1alpha1.BroodOperationPhaseFailed:
			hasFailed = true
		case v1alpha1.BroodOperationPhaseCanceled:
			hasCanceled = true
		case v1alpha1.BroodOperationPhaseSuspended:
			hasSuspended = true
		case v1alpha1.BroodOperationPhasePending, "":
			hasPending = true
		case v1alpha1.BroodOperationPhaseRunning:
			hasRunning = true
		default:
			hasRunning = true
		}
	}

	allTerminal := !hasPending && !hasRunning && !hasSuspended

	// Rule 1: all Succeeded.
	if allTerminal && hasSucceeded && !hasFailed && !hasCanceled {
		return v1alpha1.BroodOperationPhaseSucceeded, true
	}

	// Rule 2: all terminal and ≥1 Failed.
	if allTerminal && hasFailed {
		return v1alpha1.BroodOperationPhaseFailed, true
	}

	// Rule 3: all terminal (Succeeded/Canceled mix).
	if allTerminal {
		return v1alpha1.BroodOperationPhaseCanceled, true
	}

	// Rule 4: every non-terminal child Suspended (≥1 Suspended).
	if hasSuspended && !hasPending && !hasRunning {
		return v1alpha1.BroodOperationPhaseSuspended, true
	}

	// Rule 5: all Pending.
	if hasPending && !hasSucceeded && !hasFailed && !hasCanceled && !hasSuspended && !hasRunning {
		return v1alpha1.BroodOperationPhasePending, true
	}

	// Rule 6: otherwise Running.
	return v1alpha1.BroodOperationPhaseRunning, true
}

// sumSummaries field-wise sums the child summary values.
func sumSummaries(summaries []v1alpha1.BroodSummary) v1alpha1.BroodSummary {
	var total v1alpha1.BroodSummary
	for _, s := range summaries {
		total.Total += s.Total
		total.Succeeded += s.Succeeded
		total.Failed += s.Failed
		total.Skipped += s.Skipped
	}
	return total
}

// groupRuns groups ClusterBroodOp entries by (namespace, name).
// Returns groups sorted by namespace then name, with member clusters sorted.
func groupRuns(ops []ClusterBroodOp) []broodRunGroup {
	type key struct {
		Namespace string
		Name      string
	}
	groups := make(map[key][]ClusterBroodOp)
	for _, op := range ops {
		if op.Op == nil {
			continue
		}
		k := key{Namespace: op.Op.Namespace, Name: op.Op.Name}
		groups[k] = append(groups[k], op)
	}

	result := make([]broodRunGroup, 0, len(groups))
	for k, children := range groups {
		// Sort member clusters by cluster name.
		sort.Slice(children, func(i, j int) bool {
			return children[i].Cluster < children[j].Cluster
		})
		result = append(result, broodRunGroup{
			Namespace: k.Namespace,
			Name:      k.Name,
			Children:  children,
		})
	}

	// Sort groups by namespace then name.
	sort.Slice(result, func(i, j int) bool {
		if result[i].Namespace != result[j].Namespace {
			return result[i].Namespace < result[j].Namespace
		}
		return result[i].Name < result[j].Name
	})

	return result
}
