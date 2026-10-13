package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

var rehydrationTestNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestMemberSeedReader_AnswersAppliedInstance pins the Team-side read: an
// applied entry answers its cell instance, and a pending or missing entry
// answers none.
func TestMemberSeedReader_AnswersAppliedInstance(t *testing.T) {
	at := metav1.NewTime(rehydrationTestNow)
	entry := v1alpha1.CellRehydrationEntry{Tenant: "team-a", CellInstance: "uid-1", At: &at}
	cell := cellWithSeeds(entry)
	cells := map[string]*v1alpha1.Controller{teamCellKey("ns", "cell"): cell}
	read := memberSeedReader(cells, "team-a")

	if inst, _ := read("ns", "cell"); inst != "" {
		t.Fatalf("pending entry answered instance %q, want none", inst)
	}
	entry.Applied = true
	cell.Status.Cell.Rehydration[0] = entry
	if inst, _ := read("ns", "cell"); inst != "uid-1" {
		t.Fatalf("applied entry answered instance %q, want uid-1", inst)
	}
	if inst, _ := memberSeedReader(cells, "team-b")("ns", "cell"); inst != "" {
		t.Fatalf("missing entry answered instance %q, want none", inst)
	}
}

// cellWithSeeds builds a cell Controller whose status holds the given
// rehydration entries.
func cellWithSeeds(entries ...v1alpha1.CellRehydrationEntry) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status: v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{
			Rehydration: entries,
		}},
	}
}

// TestCellSeedStateChanged pins the informer wake: only a change of the
// per-tenant seed answer (instance or deferred list) is true.
func TestCellSeedStateChanged(t *testing.T) {
	at := metav1.NewTime(rehydrationTestNow)
	later := metav1.NewTime(rehydrationTestNow.Add(time.Minute))
	pending := func(tenant string) v1alpha1.CellRehydrationEntry {
		return v1alpha1.CellRehydrationEntry{Tenant: tenant, CellInstance: "uid-1", At: &at}
	}
	applied := func(tenant string) v1alpha1.CellRehydrationEntry {
		e := pending(tenant)
		e.Applied = true
		return e
	}
	deferred := func(tenant string, jobs ...string) v1alpha1.CellRehydrationEntry {
		e := pending(tenant)
		e.Deferred = jobs
		return e
	}
	withLaterAt := func(e v1alpha1.CellRehydrationEntry) v1alpha1.CellRehydrationEntry {
		e.At = &later
		return e
	}
	newInstance := applied("a")
	newInstance.CellInstance = "uid-2"
	notCell := cellWithSeeds(pending("a"))
	notCell.Spec.Cell = nil
	notCellApplied := cellWithSeeds(applied("a"))
	notCellApplied.Spec.Cell = nil
	cases := []struct {
		name     string
		old, new *v1alpha1.Controller
		want     bool
	}{
		{"pending to applied", cellWithSeeds(pending("a")), cellWithSeeds(applied("a")), true},
		{"new cell instance", cellWithSeeds(applied("a")), cellWithSeeds(newInstance), true},
		{"deferred list shrinks", cellWithSeeds(deferred("a", "a/x", "a/y")), cellWithSeeds(deferred("a", "a/x")), true},
		{"identical", cellWithSeeds(applied("a")), cellWithSeeds(applied("a")), false},
		{"only at changes", cellWithSeeds(applied("a")), cellWithSeeds(withLaterAt(applied("a"))), false},
		{"not a cell", notCell, notCellApplied, false},
		{"no old status", &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}}},
			cellWithSeeds(applied("a")), true},
		{"new pending, old missing", cellWithSeeds(), cellWithSeeds(pending("a")), false},
		{"second tenant applied", cellWithSeeds(pending("a"), pending("b")),
			cellWithSeeds(pending("a"), applied("b")), true},
		{"tenant removed while applied", cellWithSeeds(applied("a")), cellWithSeeds(), true},
	}
	for _, tc := range cases {
		if got := CellSeedStateChanged(tc.old, tc.new); got != tc.want {
			t.Errorf("%s: CellSeedStateChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

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
	entry := preSendEntry("team-a", "bundle-1", 1, "uid-1", 5, true, 2, rehydrationTestNow)
	if entry.Applied {
		t.Fatal("expected Applied false before any result has arrived")
	}
	if len(entry.Deferred) != 0 {
		t.Fatal("expected Deferred empty before any result has arrived")
	}
	if entry.CommandID != "" || entry.Records != 5 || !entry.Truncated || entry.UnseededJobs != 2 {
		t.Fatalf("expected the selection's own fields and no command ID, got %+v", entry)
	}
}

func TestPreSendEntry_BundleIDMatches(t *testing.T) {
	entry := preSendEntry("team-a", "bundle-xyz", 1, "uid-1", 0, false, 0, rehydrationTestNow)
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
