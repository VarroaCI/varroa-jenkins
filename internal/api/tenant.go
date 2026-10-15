package api

import (
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// TenantRef is a resolved tenant identity. The BFF tenant resolver produces
// it. TenantRef is a value type, passed by value.
type TenantRef struct {
	Cluster    string
	Namespace  string
	Controller string
	Tenant     string
	Team       string
	Folder     string
}

// RunTenantRow is the subset of a stored run record that decides its
// authorization. It is not the record-store row type: internal/api must not
// import internal/recordstore for an authorization decision.
type RunTenantRow struct {
	Tenant  string
	JobPath string
}

// TenantRefFromTenant builds a reference from a stored or resolved tenant
// value. Cluster is empty. Namespace and Controller are filled for a
// synthetic (single-controller) tenant, and stay empty for a Team tenant.
func TenantRefFromTenant(tenant string) (TenantRef, error) {
	parsed, err := rbac.ParseTenant(tenant)
	if err != nil {
		return TenantRef{}, err
	}
	ref := TenantRef{
		Tenant: tenant,
		Team:   tenantTeamName(parsed),
		Folder: parsed.Folder,
	}
	if parsed.Kind == rbac.TenantController {
		ref.Namespace = parsed.Namespace
		ref.Controller = parsed.Controller
	}
	return ref, nil
}

// TenantRefFromRow builds a reference from a record row's four identity
// columns: cluster, namespace, cell, and tenant. Team and Folder come from
// parsing tenant, not from the row.
func TenantRefFromRow(cluster, namespace, cell, tenant string) (TenantRef, error) {
	parsed, err := rbac.ParseTenant(tenant)
	if err != nil {
		return TenantRef{}, err
	}
	return TenantRef{
		Cluster:    cluster,
		Namespace:  namespace,
		Controller: cell,
		Tenant:     tenant,
		Team:       tenantTeamName(parsed),
		Folder:     parsed.Folder,
	}, nil
}

// tenantTeamName returns the Team name for a pipelines tenant and the
// controller name for a synthetic tenant, matching C4's resolver table.
func tenantTeamName(parsed rbac.ParsedTenant) string {
	if parsed.Kind == rbac.TenantTeam {
		return parsed.Team
	}
	return parsed.Controller
}
