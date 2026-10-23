package cells

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func rehydratedTestCondition(status *v1alpha1.TenantStatus) *v1alpha1.TenantCondition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TenantConditionRehydrated {
			return &status.Conditions[i]
		}
	}
	return nil
}

func TestRehydrationPass_UnplacedIsNoOp(t *testing.T) {
	status := &v1alpha1.TenantStatus{}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1"})
	if out.MarkedUnrehydrated {
		t.Fatal("expected no mark for an unplaced tenant")
	}
	if rehydratedTestCondition(status) != nil {
		t.Fatal("expected no Rehydrated condition for an unplaced tenant")
	}
}

func TestRehydrationPass_GenerationChangeMarksPending(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1"})
	if !out.MarkedUnrehydrated {
		t.Fatal("expected the first-placement generation to mark unrehydrated")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationPending {
		t.Fatalf("expected Rehydrated=False/RehydrationPending, got %+v", cond)
	}
}

func TestRehydrationPass_CellInstanceChangeMarksPending(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement:   &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
		Rehydration: &v1alpha1.TenantRehydrationStatus{Generation: 1, CellInstance: "uid-1"},
		Conditions:  []v1alpha1.TenantCondition{{Type: v1alpha1.TenantConditionRehydrated, Status: metav1.ConditionTrue, Reason: v1alpha1.TenantReasonTenantRehydrated}},
	}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-2"})
	if !out.MarkedUnrehydrated {
		t.Fatal("expected a cell instance change to mark unrehydrated")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationPending {
		t.Fatalf("expected Rehydrated=False/RehydrationPending, got %+v", cond)
	}
}

// TestRehydrationPass_UnchangedIsNoOp updated for task 6.5: the pass now
// derives True/TenantRehydrated from the bound cell's own entry rather
// than assuming it whenever neither trigger fired, so a matching, applied
// entry must be supplied for the condition to stay True.
func TestRehydrationPass_UnchangedIsNoOp(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement:   &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
		Rehydration: &v1alpha1.TenantRehydrationStatus{Generation: 1, CellInstance: "uid-1"},
		Conditions:  []v1alpha1.TenantCondition{{Type: v1alpha1.TenantConditionRehydrated, Status: metav1.ConditionTrue, Reason: v1alpha1.TenantReasonTenantRehydrated}},
	}
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: entry})
	if out.MarkedUnrehydrated {
		t.Fatal("expected no mark when neither the generation nor the cell instance changed")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.TenantReasonTenantRehydrated {
		t.Fatalf("expected the condition to stay True/TenantRehydrated, got %+v", cond)
	}
}

// TestRehydrationPass_RealTransitionStampsLastTransitionTime proves a
// first-time Rehydrated condition, and a later real transition, both get
// a non-zero LastTransitionTime — never the zero time a caller that
// builds a TenantCondition literal with no explicit field defaults to.
// TestRehydrationPass_UnchangedIsNoOp above already covers the sibling
// case: an unchanged condition keeps its prior timestamp.
func TestRehydrationPass_RealTransitionStampsLastTransitionTime(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: nil})
	first := rehydratedTestCondition(status)
	if first == nil || first.LastTransitionTime.IsZero() {
		t.Fatalf("expected a non-zero LastTransitionTime on the first Rehydrated condition, got %+v", first)
	}
	// A real transition (Pending -> TenantRehydrated) must also stamp a
	// non-zero LastTransitionTime, not carry the zero time forward.
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true}
	RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: entry})
	second := rehydratedTestCondition(status)
	if second == nil || second.LastTransitionTime.IsZero() {
		t.Fatalf("expected a non-zero LastTransitionTime after a real transition, got %+v", second)
	}
}

// --- task 6.5: the four Rehydrated reasons, from the copied cell entry ---

func TestRehydrationPass_AppliedEntryIsTenantRehydrated(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true, Records: 3}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: entry})
	if out.MarkedUnrehydrated {
		t.Fatal("expected no mark for an applied entry")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.TenantReasonTenantRehydrated {
		t.Fatalf("expected Rehydrated=True/TenantRehydrated, got %+v", cond)
	}
	if status.Rehydration == nil || status.Rehydration.Records != 3 {
		t.Fatalf("expected the entry's Records copied into status.Rehydration, got %+v", status.Rehydration)
	}
}

func TestRehydrationPass_NoMatchingEntryIsPending(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: nil})
	if !out.MarkedUnrehydrated {
		t.Fatal("expected a mark when no entry has landed yet")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationPending {
		t.Fatalf("expected Rehydrated=False/RehydrationPending, got %+v", cond)
	}
}

func TestRehydrationPass_DeferredJobIsPending(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true, Deferred: []string{"team/job-a"}}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: entry})
	if !out.MarkedUnrehydrated {
		t.Fatal("expected a mark for a deferred job")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationPending {
		t.Fatalf("expected Rehydrated=False/RehydrationPending, got %+v", cond)
	}
	if len(status.Rehydration.Deferred) != 1 || status.Rehydration.Deferred[0] != "team/job-a" {
		t.Fatalf("expected the deferred job path copied, got %+v", status.Rehydration)
	}
}

func TestRehydrationPass_UnseededJobsIsTruncated(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true, UnseededJobs: 5}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: entry})
	if !out.MarkedUnrehydrated {
		t.Fatal("expected a mark for unseeded jobs")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationTruncated {
		t.Fatalf("expected Rehydrated=False/RehydrationTruncated, got %+v", cond)
	}
}

func TestRehydrationPass_EntryErrorIsFailed(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Error: "mite rejected a path"}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: entry})
	if !out.MarkedUnrehydrated {
		t.Fatal("expected a mark for a whole-bundle error")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationFailed ||
		cond.Message != "mite rejected a path" {
		t.Fatalf("expected Rehydrated=False/RehydrationFailed naming the error, got %+v", cond)
	}
}

// TestRehydrationPass_DeadlineExpiredEntryErrorIsFailed covers the
// deadline path of Failed: task 6.3's controller-reconciler predicate
// writes the deadline message into the entry's own Error field once
// rehydrateDeadline passes, and this pass surfaces it the same way it
// surfaces a mite-reported error — it computes no deadline of its own.
func TestRehydrationPass_DeadlineExpiredEntryErrorIsFailed(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	entry := &v1alpha1.CellRehydrationEntry{
		Generation: 1, CellInstance: "uid-1",
		Deferred: []string{"team/job-a"},
		Error:    "rehydration deadline exceeded with 1 job(s) still deferred: team/job-a",
	}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: entry})
	if !out.MarkedUnrehydrated {
		t.Fatal("expected a mark once the deadline entry error lands")
	}
	cond := rehydratedTestCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationFailed {
		t.Fatalf("expected Rehydrated=False/RehydrationFailed, got %+v", cond)
	}
	if cond.Message == "" {
		t.Fatal("expected the deadline message to name the still-deferred job")
	}
}

// TestRehydrationPass_TeardownClearsRehydration covers task 6.5's
// teardown scenario: cells.Place's own finishTeardown clears
// status.Placement before this pass runs on the same tick, and this pass
// must clear any stale status.Rehydration and Rehydrated condition it
// left from a prior placement — otherwise the field is present (not
// absent) in the status patch and crdstore's clearStatusFields registry
// never fires for it.
func TestRehydrationPass_TeardownClearsRehydration(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		Placement:   nil, // finishTeardown already ran this tick
		Rehydration: &v1alpha1.TenantRehydrationStatus{Generation: 1, CellInstance: "uid-1"},
		Conditions:  []v1alpha1.TenantCondition{{Type: v1alpha1.TenantConditionRehydrated, Status: metav1.ConditionTrue, Reason: v1alpha1.TenantReasonTenantRehydrated}},
	}
	out := RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1"})
	if out.MarkedUnrehydrated {
		t.Fatal("expected no mark for a torn-down tenant")
	}
	if status.Rehydration != nil {
		t.Fatalf("expected status.Rehydration cleared on teardown, got %+v", status.Rehydration)
	}
	if rehydratedTestCondition(status) != nil {
		t.Fatal("expected the Rehydrated condition removed on teardown")
	}
}

// TestRehydrationPass_NeverTouchesOtherStatusFields asserts that this
// pass changes only status.Rehydration and the Rehydrated condition —
// no other Tenant status field, so it cannot regress an unrelated field a
// sibling pass (placement, pipelines) already wrote this tick.
func TestRehydrationPass_NeverTouchesOtherStatusFields(t *testing.T) {
	status := &v1alpha1.TenantStatus{
		ObservedGeneration: 7,
		GroupRef:           "group-a",
		BindingRef:         "binding-a",
		Placement:          &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
		Pipelines:          &v1alpha1.TenantPipelinesStatus{AgentNamespace: "payments-ns"},
		Conditions: []v1alpha1.TenantCondition{
			{Type: v1alpha1.TenantConditionPlaced, Status: metav1.ConditionTrue, Reason: v1alpha1.TenantReasonTenantPlaced},
			{Type: v1alpha1.TenantConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}
	entry := &v1alpha1.CellRehydrationEntry{Generation: 1, CellInstance: "uid-1", Applied: true}
	RehydrationPass(status, RehydrationInputs{CellInstance: "uid-1", Entry: entry})

	if status.ObservedGeneration != 7 || status.GroupRef != "group-a" || status.BindingRef != "binding-a" {
		t.Fatalf("expected the unrelated top-level fields untouched, got %+v", status)
	}
	if status.Placement == nil || status.Placement.Cell != "cell-a" {
		t.Fatalf("expected Placement untouched, got %+v", status.Placement)
	}
	if status.Pipelines == nil || status.Pipelines.AgentNamespace != "payments-ns" {
		t.Fatalf("expected Pipelines untouched, got %+v", status.Pipelines)
	}
	placed := false
	ready := false
	for _, c := range status.Conditions {
		switch c.Type {
		case v1alpha1.TenantConditionPlaced:
			placed = true
		case v1alpha1.TenantConditionReady:
			ready = true
		}
	}
	if !placed || !ready {
		t.Fatalf("expected the Placed and Ready conditions untouched, got %+v", status.Conditions)
	}
}
