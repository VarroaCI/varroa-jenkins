package controller

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestSetTenantWarningsCondition_ItemsHeldOutranksItemsApplyFailed asserts
// that a held tenant's reason wins the Team's TenantWarnings condition.
func TestSetTenantWarningsCondition_ItemsHeldOutranksItemsApplyFailed(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TeamReasonItemsApplyFailed, Message: "create payments/repos: boom"},
		{Tenant: "payments", Reason: v1alpha1.TeamReasonItemsHeld, Message: "held"},
	})
	if cond.Reason != v1alpha1.TeamReasonItemsHeld {
		t.Fatalf("reason = %q, want ItemsHeld", cond.Reason)
	}
}

// TestSetTenantWarningsCondition_ItemsApplyFailedOutranksBundleKeysIgnored
// asserts that a refused item outranks a bundle key the cell ignores.
func TestSetTenantWarningsCondition_ItemsApplyFailedOutranksBundleKeysIgnored(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TeamReasonBundleKeysIgnored, Message: "removeStrategy ignored"},
		{Tenant: "payments", Reason: v1alpha1.TeamReasonItemsApplyFailed, Message: "create payments/repos: boom"},
	})
	if cond.Reason != v1alpha1.TeamReasonItemsApplyFailed {
		t.Fatalf("reason = %q, want ItemsApplyFailed", cond.Reason)
	}
}
