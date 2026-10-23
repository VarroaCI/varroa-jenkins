package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// rehydratedCondition finds the Rehydrated condition, or nil.
func rehydratedCondition(status *v1alpha1.TenantStatus) *v1alpha1.TenantCondition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TenantConditionRehydrated {
			return &status.Conditions[i]
		}
	}
	return nil
}

// TestTenantReconciler_Rehydration_MarksUnrehydratedOnFirstPlacement asserts
// task 4.5's first trigger: a first placement raises the generation from 0,
// so the same status patch writes Rehydrated=False/RehydrationPending. It
// also pins the task's other acceptance: exactly one
// crdstore.PatchStatus[Tenant] call for the tick.
func TestTenantReconciler_Rehydration_MarksUnrehydratedOnFirstPlacement(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)

	uid := types.UID("20000000-0000-0000-0000-000000000001")
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	rec := newPipelinesTenantReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTenantStatus(client)
	if status == nil || status.Placement == nil || status.Placement.Generation != 1 {
		t.Fatalf("expected a first placement at generation 1, got %+v", status)
	}
	cond := rehydratedCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationPending {
		t.Fatalf("expected Rehydrated=False/RehydrationPending, got %+v", cond)
	}

	gvr, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		t.Fatalf("GVRFor failed: %v", err)
	}
	if n := len(client.store.StatusPatches(gvr)); n != 1 {
		t.Errorf("expected exactly one Tenant status patch, got %d", n)
	}
}

// TestTenantReconciler_Rehydration_MarksUnrehydratedOnCellInstanceChange
// asserts task 4.5's second trigger: a placed tenant that already reads
// Rehydrated=True is marked unrehydrated again the tick its owning cell's
// pod instance changes, even though the placement generation itself does
// not change.
func TestTenantReconciler_Rehydration_MarksUnrehydratedOnCellInstanceChange(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)

	uid := types.UID("20000000-0000-0000-0000-000000000002")
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	rec := newPipelinesTenantReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("tick 1: unexpected error: %v", err)
	}
	status := lastTenantStatus(client)
	generation := status.Placement.Generation

	// Simulate task 6.5 having already completed this tenant's rehydration
	// for cell instance "uid-1" at this generation, and roll the cell to
	// a new pod. A fresh TenantReconciler simulates the in-memory sync-cache
	// restart cost being irrelevant here: this test only exercises the
	// unrehydrate trigger, not the announce dedup.
	current, err := crdstore.Get[v1alpha1.Tenant](context.Background(), client.store, "payments", "")
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	current.Status.Rehydration = &v1alpha1.TenantRehydrationStatus{Generation: generation, CellInstance: "uid-1"}
	current.Status.Conditions = append(current.Status.Conditions, v1alpha1.TenantCondition{
		Type: v1alpha1.TenantConditionRehydrated, Status: metav1.ConditionTrue, Reason: v1alpha1.TenantReasonTenantRehydrated,
	})
	if err := crdstore.PatchStatus[v1alpha1.Tenant](context.Background(), client.store, "payments", "", &current.Status); err != nil {
		t.Fatalf("seed rehydrated status: %v", err)
	}

	cell, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "cell-a", "cells-ns")
	if err != nil {
		t.Fatalf("get cell: %v", err)
	}
	cell.Status.Cell.Instance = "uid-2"
	crdstore.MustSeed(client.store, cell)

	rec2 := newPipelinesTenantReconciler(client, nil)
	if _, err := rec2.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("tick 2: unexpected error: %v", err)
	}

	status = lastTenantStatus(client)
	if status.Placement.Generation != generation {
		t.Fatalf("expected the placement generation to stay %d, got %d", generation, status.Placement.Generation)
	}
	cond := rehydratedCondition(status)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonRehydrationPending {
		t.Fatalf("expected the pod roll to mark Rehydrated=False/RehydrationPending, got %+v", cond)
	}
}
