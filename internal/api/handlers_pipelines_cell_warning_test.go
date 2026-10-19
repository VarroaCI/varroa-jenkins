package api

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// teamCellWarningObject builds a placed tenant whose TenantWarnings
// condition has the given status.
func teamCellWarningObject(status string) *unstructured.Unstructured {
	obj := teamCellObject("core", v1alpha1.TeamReasonTenantPlaced)
	conds := []any{
		map[string]any{
			"type":   v1alpha1.TeamConditionPlaced,
			"status": "True",
			"reason": v1alpha1.TeamReasonTenantPlaced,
		},
		map[string]any{
			"type":    v1alpha1.TeamConditionTenantWarnings,
			"status":  status,
			"reason":  v1alpha1.TeamReasonItemsApplyFailed,
			"message": "create " + teamCellTenant + "/repos: HTTP 500: Jenkins internal error",
		},
	}
	_ = unstructured.SetNestedSlice(obj.Object, conds, "status", "conditions")
	return obj
}

// TestGetTeamCellCarriesTheTenantWarning asserts the team cell read
// carries the Team's TenantWarnings condition when that condition is True.
func TestGetTeamCellCarriesTheTenantWarning(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, teamCellWarningObject("True"))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, nil), reader)

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTeamCell(t, rec)
	if body.Warning == nil {
		t.Fatalf("warning absent, want the ItemsApplyFailed warning (body %s)", rec.Body.String())
	}
	if body.Warning.Reason != v1alpha1.TeamReasonItemsApplyFailed {
		t.Errorf("warning reason = %q, want ItemsApplyFailed", body.Warning.Reason)
	}
	if body.Warning.Message == "" {
		t.Error("warning message is empty")
	}
}

// TestGetTeamCellOmitsWarningWhenConditionFalse asserts the team cell read
// omits the warning when the TenantWarnings condition is not True.
func TestGetTeamCellOmitsWarningWhenConditionFalse(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, teamCellWarningObject("False"))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, nil), reader)

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body := decodeTeamCell(t, rec); body.Warning != nil {
		t.Errorf("warning = %+v, want none when the condition is False", body.Warning)
	}
}
