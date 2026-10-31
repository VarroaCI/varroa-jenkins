package controller

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// fakeGrantReader answers the grant walk from a map keyed by tenant name.
type fakeGrantReader struct {
	sources map[string][]rbac.TenantGrantSource
	roles   map[string][]rbac.TenantMemberRole
}

func (f *fakeGrantReader) TenantGrantSourcesTruncated(t rbac.ParsedTenant) ([]rbac.TenantGrantSource, bool) {
	return f.sources[t.Tenant], false
}

func (f *fakeGrantReader) TenantMemberRoles(t rbac.ParsedTenant) []rbac.TenantMemberRole {
	return f.roles[t.Tenant]
}

func grantsCRUD(reader TenantGrantReader) *ConfigCRUD {
	c := NewConfigCRUD(nil, nil, nil, "varroa-system", slog.Default())
	c.Grants = reader
	return c
}

func decodeGrantsReply(t *testing.T, raw []byte) bus.CellGrantsResponse {
	t.Helper()
	var resp bus.CellGrantsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode reply: %v (body %s)", err, raw)
	}
	return resp
}

func TestHandleCellGrants_AnswersEachTenantInOrder(t *testing.T) {
	reader := &fakeGrantReader{
		sources: map[string][]rbac.TenantGrantSource{
			"alpha": {{
				Source:        rbac.GrantSourceFederatedBinding,
				Binding:       "varroa-fed-tenant-alpha",
				RoleRef:       "pipelines-writer",
				Subjects:      []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
				Verbs:         []string{"write"},
				FederatedFrom: []string{"alpha-writers"},
			}},
		},
		roles: map[string][]rbac.TenantMemberRole{
			"alpha": {{Name: rbac.TenantMemberRoleName("alpha"), Pattern: rbac.TenantMemberRolePattern("alpha")}},
		},
	}
	c := grantsCRUD(reader)

	raw := c.HandleCellGrants([]byte(`{"tenants":["beta","alpha"]}`))
	resp := decodeGrantsReply(t, raw)
	if resp.Code != "" {
		t.Fatalf("code = %q, want none", resp.Code)
	}
	if len(resp.Tenants) != 2 || resp.Tenants[0].Tenant != "beta" || resp.Tenants[1].Tenant != "alpha" {
		t.Fatalf("tenants = %+v, want beta then alpha", resp.Tenants)
	}
	alpha := resp.Tenants[1]
	if len(alpha.Grants) != 1 || alpha.Grants[0].Source != rbac.GrantSourceFederatedBinding {
		t.Fatalf("alpha grants = %+v, want one federatedBinding grant", alpha.Grants)
	}
	if len(alpha.Grants[0].FederatedFrom) != 1 || alpha.Grants[0].FederatedFrom[0] != "alpha-writers" {
		t.Errorf("federatedFrom = %v, want alpha-writers", alpha.Grants[0].FederatedFrom)
	}
	if len(alpha.MemberRoles) != 1 {
		t.Errorf("member roles = %+v, want one", alpha.MemberRoles)
	}
}

func TestHandleCellGrants_InvalidRequest(t *testing.T) {
	c := grantsCRUD(&fakeGrantReader{})
	resp := decodeGrantsReply(t, c.HandleCellGrants([]byte(`not json`)))
	if resp.Code != bus.CodeInvalid {
		t.Errorf("code = %q, want %q", resp.Code, bus.CodeInvalid)
	}
}

func TestHandleCellGrants_NoReaderAnswersInternal(t *testing.T) {
	c := grantsCRUD(nil)
	resp := decodeGrantsReply(t, c.HandleCellGrants([]byte(`{"tenants":["alpha"]}`)))
	if resp.Code != bus.CodeInternal || resp.Error != "grants reader not configured" {
		t.Errorf("reply = %+v, want an internal error", resp)
	}
}

func TestCellTenantGrantsReport_NeverNullAndOmitsEmptyJobs(t *testing.T) {
	report := CellTenantGrantsReport("alpha", []rbac.TenantGrantSource{{
		Source:   rbac.GrantSourceClusterWideBinding,
		Binding:  "fleet",
		RoleRef:  "developer",
		Subjects: nil,
		Verbs:    nil,
		Jobs:     nil,
	}}, false, nil)

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, want := range []string{`"memberRoles":[]`, `"subjects":[]`, `"verbs":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s lacks %s", body, want)
		}
	}
	if strings.Contains(body, `"jobs"`) {
		t.Errorf("body %s carries a jobs key for an unnarrowed grant", body)
	}
	if strings.Contains(body, `"federatedFrom"`) {
		t.Errorf("body %s carries federatedFrom for a cluster-wide grant", body)
	}
	if strings.Contains(body, `"allTenants"`) {
		t.Errorf("body %s carries allTenants for a cluster-wide grant", body)
	}
}

func TestCellTenantGrantsReport_CarriesAllTenants(t *testing.T) {
	report := CellTenantGrantsReport("alpha", []rbac.TenantGrantSource{{
		Source:        rbac.GrantSourceFederatedBinding,
		Binding:       "varroa-fed-tenant-all-abc",
		RoleRef:       "pipelines-writer",
		Subjects:      []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
		Verbs:         []string{"write"},
		FederatedFrom: []string{"fleet-writers"},
		AllTenants:    true,
	}}, false, nil)

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"allTenants":true`) {
		t.Errorf("body %s lacks allTenants:true on the all-tenants copy", raw)
	}
}
