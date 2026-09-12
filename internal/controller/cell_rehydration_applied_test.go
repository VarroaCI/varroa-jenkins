package controller

import (
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// --- task 6.3: the applied predicate, one test per failing condition
// plus one matching, and one for the deadline ---

func TestRehydrationApplied_GenerationMismatch(t *testing.T) {
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true}
	if rehydrationApplied(entry, 2, "uid-1") {
		t.Fatal("expected a generation mismatch to answer false")
	}
}

func TestRehydrationApplied_CellInstanceMismatch(t *testing.T) {
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true}
	if rehydrationApplied(entry, 1, "uid-2") {
		t.Fatal("expected a cell instance mismatch to answer false")
	}
}

func TestRehydrationApplied_NotApplied(t *testing.T) {
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: false}
	if rehydrationApplied(entry, 1, "uid-1") {
		t.Fatal("expected Applied=false to answer false")
	}
}

func TestRehydrationApplied_ErrorSet(t *testing.T) {
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true, Error: "boom"}
	if rehydrationApplied(entry, 1, "uid-1") {
		t.Fatal("expected a non-empty error to answer false")
	}
}

func TestRehydrationApplied_DeferredNonEmpty(t *testing.T) {
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true, Deferred: []string{"team/job-a"}}
	if rehydrationApplied(entry, 1, "uid-1") {
		t.Fatal("expected a non-empty deferred list to answer false")
	}
}

func TestRehydrationApplied_UnseededJobsAboveZero(t *testing.T) {
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true, UnseededJobs: 1}
	if rehydrationApplied(entry, 1, "uid-1") {
		t.Fatal("expected UnseededJobs above zero to answer false, so the next tick sends the next number-only block")
	}
}

func TestRehydrationApplied_Matches(t *testing.T) {
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true}
	if !rehydrationApplied(entry, 1, "uid-1") {
		t.Fatal("expected a matching, applied, error-free, non-deferred entry to answer true")
	}
}

func TestRehydrationExpired_Deadline(t *testing.T) {
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	pendingSince := now.Add(-31 * time.Minute)
	if !rehydrationExpired(pendingSince, 30*time.Minute, now) {
		t.Fatal("expected a pending streak older than the deadline to answer true")
	}
	if rehydrationExpired(now.Add(-29*time.Minute), 30*time.Minute, now) {
		t.Fatal("expected a pending streak younger than the deadline to answer false")
	}
	if rehydrationExpired(time.Time{}, 30*time.Minute, now) {
		t.Fatal("expected a zero pendingSince (no streak recorded yet) to answer false")
	}
}
