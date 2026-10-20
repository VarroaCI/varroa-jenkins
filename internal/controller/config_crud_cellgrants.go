package controller

import (
	"encoding/json"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// TenantGrantReader answers the grant walk of one pipelines tenant. The
// operator's RBAC resolver implements it over this cluster's own state.
type TenantGrantReader interface {
	TenantGrantSourcesTruncated(rbac.ParsedTenant) ([]rbac.TenantGrantSource, bool)
	TenantMemberRoles(rbac.ParsedTenant) []rbac.TenantMemberRole
}

// HandleCellGrants handles operator.<cluster>.cells.grants. It answers each
// requested tenant from this cluster's own resolver, so a member cell reports
// the grants its own operator renders.
func (c *ConfigCRUD) HandleCellGrants(data []byte) []byte {
	var req bus.CellGrantsRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.CellGrantsResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	if c.Grants == nil {
		return c.configReply(bus.CellGrantsResponse{Error: "grants reader not configured", Code: bus.CodeInternal})
	}
	out := make([]bus.CellTenantGrants, 0, len(req.Tenants))
	for _, name := range req.Tenants {
		parsed, err := rbac.ParseTenant(name)
		if err != nil {
			out = append(out, CellTenantGrantsReport(name, nil, false, nil))
			continue
		}
		sources, truncated := c.Grants.TenantGrantSourcesTruncated(parsed)
		out = append(out, CellTenantGrantsReport(name, sources, truncated, c.Grants.TenantMemberRoles(parsed)))
	}
	raw, err := json.Marshal(bus.CellGrantsResponse{Tenants: out})
	if err != nil {
		return c.configReply(bus.CellGrantsResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	if len(raw) > bus.ListPageBudget {
		return c.configReply(bus.CellGrantsResponse{Error: "grants reply too large", Code: bus.CodeInternal})
	}
	return raw
}

// CellTenantGrantsReport shapes one tenant's grants for the wire. Verbs,
// Grants, MemberRoles, and Subjects never marshal as null. Jobs stays absent
// for an unnarrowed grant.
func CellTenantGrantsReport(tenant string, sources []rbac.TenantGrantSource, truncated bool, roles []rbac.TenantMemberRole) bus.CellTenantGrants {
	entry := bus.CellTenantGrants{
		Tenant:      tenant,
		ItemRole:    rbac.TenantItemRoleName(tenant),
		ItemPattern: rbac.TenantItemRolePattern(tenant),
		Truncated:   truncated,
		Grants:      make([]bus.CellGrant, 0, len(sources)),
		MemberRoles: cellMemberRoleReports(roles),
	}
	for _, src := range sources {
		grant := bus.CellGrant{
			Source:        src.Source,
			Binding:       src.Binding,
			RoleRef:       src.RoleRef,
			Subjects:      subjectReports(src.Subjects),
			Verbs:         src.Verbs,
			FederatedFrom: src.FederatedFrom,
		}
		if grant.Verbs == nil {
			grant.Verbs = []string{}
		}
		if len(src.Jobs) > 0 {
			grant.Jobs = src.Jobs
		}
		entry.Grants = append(entry.Grants, grant)
	}
	return entry
}

// cellMemberRoleReports converts the member roles of a tenant. It never
// returns nil, so the JSON carries an empty array and not null.
func cellMemberRoleReports(roles []rbac.TenantMemberRole) []bus.CellMemberRole {
	out := make([]bus.CellMemberRole, 0, len(roles))
	for _, r := range roles {
		out = append(out, bus.CellMemberRole{
			Name:        r.Name,
			Pattern:     r.Pattern,
			Binding:     r.Binding,
			Permissions: append(make([]string, 0, len(r.Permissions)), r.Permissions...),
			Subjects:    subjectReports(r.Subjects),
		})
	}
	return out
}

// subjectReports converts subject refs. It never returns nil.
func subjectReports(subjects []v1alpha1.SubjectRef) []bus.CellGrantSubject {
	out := make([]bus.CellGrantSubject, 0, len(subjects))
	for _, s := range subjects {
		out = append(out, bus.CellGrantSubject{Kind: s.Kind, Name: s.Name})
	}
	return out
}
