package api

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// tenantCellWarningObject builds a placed tenant whose TenantWarnings
// condition has the given status.
func tenantCellWarningObject(status string) *unstructured.Unstructured {
	obj := tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced)
	conds := []any{
		map[string]any{
			"type":   v1alpha1.TenantConditionPlaced,
			"status": "True",
			"reason": v1alpha1.TenantReasonTenantPlaced,
		},
		map[string]any{
			"type":    v1alpha1.TenantConditionTenantWarnings,
			"status":  status,
			"reason":  v1alpha1.TenantReasonItemsApplyFailed,
			"message": "create " + tenantCellTenant + "/repos: HTTP 500: Jenkins internal error",
		},
	}
	_ = unstructured.SetNestedSlice(obj.Object, conds, "status", "conditions")
	return obj
}

// TestGetTenantCellCarriesTheTenantWarning asserts the tenant cell read
// carries the Tenant's TenantWarnings condition when that condition is True.
func TestGetTenantCellCarriesTheTenantWarning(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, tenantCellWarningObject("True"))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTenantCell(t, rec)
	if body.Warning == nil {
		t.Fatalf("warning absent, want the ItemsApplyFailed warning (body %s)", rec.Body.String())
	}
	if body.Warning.Reason != v1alpha1.TenantReasonItemsApplyFailed {
		t.Errorf("warning reason = %q, want ItemsApplyFailed", body.Warning.Reason)
	}
	if body.Warning.Message == "" {
		t.Error("warning message is empty")
	}
}

// TestGetTenantCellOmitsWarningWhenConditionFalse asserts the tenant cell read
// omits the warning when the TenantWarnings condition is not True.
func TestGetTenantCellOmitsWarningWhenConditionFalse(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, tenantCellWarningObject("False"))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body := decodeTenantCell(t, rec); body.Warning != nil {
		t.Errorf("warning = %+v, want none when the condition is False", body.Warning)
	}
}
