package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestReconcileHeadlessRoll asserts design D3: reconcileHeadlessRoll compares
// the desired v1alpha1.IsHeadless value against the live VARROA_HEADLESS
// entry and transitions a Connected controller to Provisioning only on a
// difference.
func TestReconcileHeadlessRoll(t *testing.T) {
	ctx := context.Background()

	t.Run("flip transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsJenkinsEnvFound = true
		client.stsJenkinsEnv = map[string]string{} // no VARROA_HEADLESS entry: live is false
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Headless = true

		if !rec.reconcileHeadlessRoll(ctx, cr) {
			t.Fatal("expected reconcileHeadlessRoll to return true (transition)")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
		if cr.Status.ProvisioningStartedAt == nil {
			t.Error("ProvisioningStartedAt must be set")
		}
		pc := findCondition(cr.Status.Conditions, v1alpha1.ConditionProvisioning)
		if pc == nil || pc.Status != metav1.ConditionTrue || pc.Reason != "ProvisioningStarted" {
			t.Errorf("Provisioning condition = %+v, want True/ProvisioningStarted", pc)
		}
	})

	t.Run("converged is a no-op", func(t *testing.T) {
		client := newTestClient()
		client.stsJenkinsEnvFound = true
		client.stsJenkinsEnv = map[string]string{"VARROA_HEADLESS": "true"}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Headless = true

		if rec.reconcileHeadlessRoll(ctx, cr) {
			t.Fatal("expected no transition when converged")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase = %q, want Connected (unchanged)", cr.Status.Phase)
		}
	})

	t.Run("read error is a no-op", func(t *testing.T) {
		client := newTestClient()
		client.stsJenkinsEnvErr = errors.New("boom")
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Headless = true

		if rec.reconcileHeadlessRoll(ctx, cr) {
			t.Fatal("expected no transition on read error")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase changed on read error: %q", cr.Status.Phase)
		}
	})

	t.Run("found false is a no-op", func(t *testing.T) {
		client := newTestClient()
		client.stsJenkinsEnvFound = false // no StatefulSet yet
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Headless = true

		if rec.reconcileHeadlessRoll(ctx, cr) {
			t.Fatal("expected no transition when found is false")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase changed when found is false: %q", cr.Status.Phase)
		}
	})
}
