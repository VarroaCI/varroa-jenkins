package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestReconcileBlocked_ClearOnSuccess is a table-driven test for the
// clear-on-success defer in reconcileController.
func TestReconcileBlocked_ClearOnSuccess(t *testing.T) {
	tests := []struct {
		name string
		// crOverrides customises the controller before reconcile.
		crOverrides func(cr *v1alpha1.Controller)
		// wantBlockedTrue asserts that ConditionReconcileBlocked=True after reconcile.
		wantBlockedTrue bool
		// wantConditionAbsent asserts that ConditionReconcileBlocked is absent after reconcile.
		wantConditionAbsent bool
	}{
		{
			name: "blocked before, oldPhase NOT Failed → clears",
			crOverrides: func(cr *v1alpha1.Controller) {
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:    v1alpha1.ConditionReconcileBlocked,
					Status:  metav1.ConditionTrue,
					Reason:  v1alpha1.ReasonReconcileBlockedBundleUnreadable,
					Message: "spec.composedBundleRef is required",
				})
				cr.Status.LastReconcileError = "spec.composedBundleRef is required"
				now := metav1Now()
				cr.Status.LastReconcileErrorAt = &now
			},
			wantBlockedTrue:     false,
			wantConditionAbsent: false, // condition should be False, not absent
		},
		{
			name: "never blocked → stays absent",
			crOverrides: func(cr *v1alpha1.Controller) {
				// No pre-existing condition.
			},
			wantBlockedTrue:     false,
			wantConditionAbsent: true,
		},
		{
			name: "oldPhase Failed, success → condition stays True",
			crOverrides: func(cr *v1alpha1.Controller) {
				cr.Status.Phase = v1alpha1.ControllerPhaseFailed
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:    v1alpha1.ConditionReconcileBlocked,
					Status:  metav1.ConditionTrue,
					Reason:  "SomeReason",
					Message: "blocked before Failed recovery",
				})
				cr.Status.LastReconcileError = "blocked before Failed recovery"
				now := metav1Now()
				cr.Status.LastReconcileErrorAt = &now
			},
			wantBlockedTrue: true, // skip honored: oldPhase was Failed
		},
		{
			name: "Stopped controller, blocked before → clears in defer",
			crOverrides: func(cr *v1alpha1.Controller) {
				cr.Status.Phase = v1alpha1.ControllerPhaseStopped
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:    v1alpha1.ConditionReconcileBlocked,
					Status:  metav1.ConditionTrue,
					Reason:  "SomeReason",
					Message: "was blocked",
				})
				cr.Status.LastReconcileError = "was blocked"
				now := metav1Now()
				cr.Status.LastReconcileErrorAt = &now
			},
			wantBlockedTrue: false, // cleared because oldPhase != Failed
		},
		{
			name: "Hibernated controller, blocked before → clears in defer",
			crOverrides: func(cr *v1alpha1.Controller) {
				cr.Status.Phase = v1alpha1.ControllerPhaseHibernated
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:    v1alpha1.ConditionReconcileBlocked,
					Status:  metav1.ConditionTrue,
					Reason:  "SomeReason",
					Message: "was blocked",
				})
				cr.Status.LastReconcileError = "was blocked"
				now := metav1Now()
				cr.Status.LastReconcileErrorAt = &now
			},
			wantBlockedTrue: false, // cleared because oldPhase != Failed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClientWithBundle()
			rec := newTestReconciler(client)

			cr := testController("test", "ns1", v1alpha1.ControllerPhasePending)
			tt.crOverrides(cr)

			err := rec.reconcileController(context.Background(), cr)
			if err != nil {
				t.Fatalf("reconcileController: %v", err)
			}

			cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)

			if tt.wantConditionAbsent {
				if cond != nil {
					t.Errorf("expected ConditionReconcileBlocked absent, got Status=%q Reason=%q", cond.Status, cond.Reason)
				}
				return
			}

			if cond == nil {
				t.Fatal("ConditionReconcileBlocked absent, expected present")
			}
			if tt.wantBlockedTrue {
				if cond.Status != metav1.ConditionTrue {
					t.Errorf("expected Status=True, got %q", cond.Status)
				}
			} else {
				if cond.Status != metav1.ConditionFalse {
					t.Errorf("expected Status=False, got %q", cond.Status)
				}
				if cr.Status.LastReconcileError != "" {
					t.Errorf("LastReconcileError = %q, want empty", cr.Status.LastReconcileError)
				}
				if cr.Status.LastReconcileErrorAt != nil {
					t.Error("LastReconcileErrorAt should be nil")
				}
			}
		})
	}
}
