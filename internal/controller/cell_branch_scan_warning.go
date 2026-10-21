package controller

import (
	"fmt"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// branchScanFailedWarnings answers one BranchScanFailed entry for each
// tenant that owns a failed folder. The mite lists each folder once, so a
// tenant gets one entry that names every folder it owns. A folder that no
// tenant of the cell owns answers no entry.
func branchScanFailedWarnings(cell *v1alpha1.CellStatus, scans []*mitev1.RehydrationFailedScan) []v1alpha1.TenantWarning {
	if cell == nil || len(scans) == 0 {
		return nil
	}
	messagesByTenant := make(map[string][]string)
	for _, s := range scans {
		if s == nil {
			continue
		}
		msg := fmt.Sprintf("the last branch scan of %s ended with %s; %d branch job(s) were deferred until a scan succeeds or rehydrateDeadline passes",
			s.Folder, s.IndexResult, s.DeferredJobs)
		for tenant := range partitionJobPathsByTenant([]string{s.Folder}, cell.Tenants) {
			messagesByTenant[tenant] = append(messagesByTenant[tenant], msg)
		}
	}
	tenants := make([]string, 0, len(messagesByTenant))
	for tenant := range messagesByTenant {
		tenants = append(tenants, tenant)
	}
	sort.Strings(tenants)
	var out []v1alpha1.TenantWarning
	for _, tenant := range tenants {
		out = append(out, v1alpha1.TenantWarning{
			Tenant:  tenant,
			Reason:  v1alpha1.TeamReasonBranchScanFailed,
			Message: joinScanMessages(messagesByTenant[tenant]),
		})
	}
	return out
}

// joinScanMessages joins one tenant's folder lines within the 1024 byte
// warning limit. A line that does not fit is replaced by a count, so no
// folder vanishes without a trace.
func joinScanMessages(msgs []string) string {
	const limit = 1024
	if joined := strings.Join(msgs, "; "); len(joined) <= limit {
		return joined
	}
	var kept []string
	for _, m := range msgs {
		candidate := strings.Join(append(append([]string(nil), kept...), m), "; ")
		if len(candidate)+len("; and 999 more folder(s) with a failed scan") > limit {
			break
		}
		kept = append(kept, m)
	}
	out := strings.Join(kept, "; ")
	if rest := len(msgs) - len(kept); rest > 0 {
		out += fmt.Sprintf("; and %d more folder(s) with a failed scan", rest)
	}
	return truncate(out, limit)
}

// branchScanFailedAfter returns the TenantWarnings list after one
// rehydration result. Only the tenants whose entry carries the result's
// command ID change. A result that did not succeed, or whose branch
// inventory was unknown, keeps the list unchanged. A matched tenant with no
// failed folder loses its BranchScanFailed entry.
func branchScanFailedAfter(existing []v1alpha1.TenantWarning, cell *v1alpha1.CellStatus, d *mitev1.DesiredStateResult, tenants map[string]bool) []v1alpha1.TenantWarning {
	if d == nil || !d.RehydrationSuccess || d.RehydrationBranchScansUnknown || len(tenants) == 0 {
		return existing
	}
	kept := make([]v1alpha1.TenantWarning, 0, len(existing))
	for _, w := range existing {
		if w.Reason == v1alpha1.TeamReasonBranchScanFailed && tenants[w.Tenant] {
			continue
		}
		kept = append(kept, w)
	}
	for _, w := range branchScanFailedWarnings(cell, d.RehydrationFailedScans) {
		if tenants[w.Tenant] {
			kept = append(kept, w)
		}
	}
	return kept
}

// pruneBranchScanFailedWarnings drops each BranchScanFailed entry whose
// tenant has no rehydration entry, or whose entry defers no job. A deadline
// give-up keeps its deferred jobs, so its warning stays.
func pruneBranchScanFailedWarnings(cell *v1alpha1.CellStatus) {
	if cell == nil || len(cell.TenantWarnings) == 0 {
		return
	}
	deferring := make(map[string]bool, len(cell.Rehydration))
	for _, e := range cell.Rehydration {
		if len(e.Deferred) > 0 {
			deferring[e.Tenant] = true
		}
	}
	var kept []v1alpha1.TenantWarning
	for _, w := range cell.TenantWarnings {
		if w.Reason == v1alpha1.TeamReasonBranchScanFailed && !deferring[w.Tenant] {
			continue
		}
		kept = append(kept, w)
	}
	cell.TenantWarnings = kept
}
