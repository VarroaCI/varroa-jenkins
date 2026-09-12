package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

var rehydrationTestNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// --- task 6.0a: the empty-selection entry ---

func TestEmptySelectionEntry_AppliedNoRehydrationField(t *testing.T) {
	entry := emptySelectionEntry("team-a", 1, "uid-1", rehydrationTestNow)
	if !entry.Applied || entry.Records != 0 || len(entry.Deferred) != 0 || entry.BundleID != "" {
		t.Fatalf("expected an applied, empty entry with no bundleId, got %+v", entry)
	}
	// The command carries no rehydration field for this tenant: this
	// entry names no CommandID, which the orchestrator never sets on a
	// desired-state command's own bundle for it.
	if entry.CommandID != "" {
		t.Fatalf("expected no CommandID on an empty-selection entry, got %q", entry.CommandID)
	}
}

// TestEmptySelectionEntry_RehydratedBecomesTrue covers 6.0a's second
// scenario end to end through cells.RehydrationPass (task 6.5): the
// condition reaches True/TenantRehydrated on the same tick the empty
// entry is written.
func TestEmptySelectionEntry_RehydratedBecomesTrue(t *testing.T) {
	entry := emptySelectionEntry("team-a", 1, "uid-1", rehydrationTestNow)
	status := &v1alpha1.TeamStatus{
		Placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	cells.RehydrationPass(status, cells.RehydrationInputs{CellInstance: "uid-1", Entry: &entry})

	var cond *v1alpha1.TeamCondition
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TeamConditionRehydrated {
			cond = &status.Conditions[i]
		}
	}
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.TeamReasonTenantRehydrated {
		t.Fatalf("expected Rehydrated=True/TenantRehydrated, got %+v", cond)
	}
}

// --- task 6.1: the pre-send entry ---

func TestPreSendEntry_WrittenBeforeSend(t *testing.T) {
	entry := preSendEntry("team-a", "cmd-1", "bundle-1", 1, "uid-1", 5, true, 2, rehydrationTestNow)
	if entry.Applied {
		t.Fatal("expected Applied false before any result has arrived")
	}
	if len(entry.Deferred) != 0 {
		t.Fatal("expected Deferred empty before any result has arrived")
	}
	if entry.CommandID != "cmd-1" || entry.Records != 5 || !entry.Truncated || entry.UnseededJobs != 2 {
		t.Fatalf("expected the selection's own fields carried through, got %+v", entry)
	}
}

func TestPreSendEntry_BundleIDMatches(t *testing.T) {
	entry := preSendEntry("team-a", "cmd-1", "bundle-xyz", 1, "uid-1", 0, false, 0, rehydrationTestNow)
	if entry.BundleID != "bundle-xyz" {
		t.Fatalf("expected bundleId to match the bundle, got %q", entry.BundleID)
	}
}

// --- task 6.2: applying the arriving result ---

func TestApplyRehydrationResult_OnlyMatchingCommandUpdated(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1"},
		{Tenant: "team-b", CommandID: "cmd-old"},
	}
	out := applyRehydrationResult(entries, "cmd-1", true, "", nil, rehydrationTestNow)
	if !out[0].Applied {
		t.Fatalf("expected the matching entry updated, got %+v", out[0])
	}
	if out[1].CommandID != "cmd-old" || out[1].Applied {
		t.Fatalf("expected the non-matching entry untouched, got %+v", out[1])
	}
}

// TestApplyRehydrationResult_ThreeTenantsOneCommand covers 6.2's named
// scenario: a whole-bundle error applies to every entry of that
// command, across three tenants.
func TestApplyRehydrationResult_ThreeTenantsOneCommand(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1"},
		{Tenant: "team-b", CommandID: "cmd-1"},
		{Tenant: "team-c", CommandID: "cmd-1"},
	}
	out := applyRehydrationResult(entries, "cmd-1", false, "mite rejected a path", nil, rehydrationTestNow)
	for _, e := range out {
		if e.Applied || e.Error != "mite rejected a path" {
			t.Fatalf("expected every entry of the command to carry the whole-bundle error, got %+v", e)
		}
	}
}

func TestApplyRehydrationResult_PerTenantDeferred(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1"},
		{Tenant: "team-b", CommandID: "cmd-1"},
	}
	deferred := map[string][]string{"team-a": {"team-a/job-x"}}
	out := applyRehydrationResult(entries, "cmd-1", true, "", deferred, rehydrationTestNow)
	if len(out[0].Deferred) != 1 || out[0].Applied {
		t.Fatalf("expected team-a deferred and not applied, got %+v", out[0])
	}
	if len(out[1].Deferred) != 0 || !out[1].Applied {
		t.Fatalf("expected team-b applied with no deferred job, got %+v", out[1])
	}
}

// --- partitionDeferredJobs ---

func TestPartitionDeferredJobs(t *testing.T) {
	out := partitionDeferredJobs([]string{"team-a/job-x", "team-b/job-y", "team-a/job-z"}, []string{"team-a", "team-b"})
	if len(out["team-a"]) != 2 || len(out["team-b"]) != 1 {
		t.Fatalf("expected 2 team-a and 1 team-b deferred jobs, got %+v", out)
	}
}
