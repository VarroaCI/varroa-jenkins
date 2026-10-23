package controller

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestSetTenantWarningsCondition_ItemsHeldOutranksItemsApplyFailed asserts
// that a held tenant's reason wins the Tenant's TenantWarnings condition.
func TestSetTenantWarningsCondition_ItemsHeldOutranksItemsApplyFailed(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TenantReasonItemsApplyFailed, Message: "create payments/repos: boom"},
		{Tenant: "payments", Reason: v1alpha1.TenantReasonItemsHeld, Message: "held"},
	})
	if cond.Reason != v1alpha1.TenantReasonItemsHeld {
		t.Fatalf("reason = %q, want ItemsHeld", cond.Reason)
	}
}

// TestSetTenantWarningsCondition_ItemsApplyFailedOutranksBundleKeysIgnored
// asserts that a refused item outranks a bundle key the cell ignores.
func TestSetTenantWarningsCondition_ItemsApplyFailedOutranksBundleKeysIgnored(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TenantReasonBundleKeysIgnored, Message: "removeStrategy ignored"},
		{Tenant: "payments", Reason: v1alpha1.TenantReasonItemsApplyFailed, Message: "create payments/repos: boom"},
	})
	if cond.Reason != v1alpha1.TenantReasonItemsApplyFailed {
		t.Fatalf("reason = %q, want ItemsApplyFailed", cond.Reason)
	}
}
