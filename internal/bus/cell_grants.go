package bus

// OperatorCellsSubject returns operator.<cluster>.cells.<verb>.
// verb is grants. The grants verb takes CellGrantsRequest and answers
// CellGrantsResponse.
func OperatorCellsSubject(cluster, verb string) string {
	return "operator." + cluster + ".cells." + verb
}

// CellGrantSubject is one subject of a grant.
type CellGrantSubject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// CellGrant is one grant that reaches a tenant of a cell. Jobs carries
// omitempty because an absent list and an empty list mean different
// things: an absent list narrows nothing. FederatedFrom names the core
// VarroaRoleBindings of a federatedBinding copy.
type CellGrant struct {
	Source        string             `json:"source"`
	Binding       string             `json:"binding,omitempty"`
	RoleRef       string             `json:"roleRef"`
	Subjects      []CellGrantSubject `json:"subjects"`
	Verbs         []string           `json:"verbs"`
	Jobs          []string           `json:"jobs,omitempty"`
	FederatedFrom []string           `json:"federatedFrom,omitempty"`
}

// CellMemberRole is one generated Jenkins member role of a tenant. Binding
// is absent for the role on the whole folder subtree.
type CellMemberRole struct {
	Name        string             `json:"name"`
	Pattern     string             `json:"pattern"`
	Binding     string             `json:"binding,omitempty"`
	Permissions []string           `json:"permissions"`
	Subjects    []CellGrantSubject `json:"subjects"`
}

// CellTenantGrants is every grant that reaches one tenant of a cell, with
// the tenant's generated Jenkins item role and member roles.
type CellTenantGrants struct {
	Tenant      string           `json:"tenant"`
	ItemRole    string           `json:"itemRole"`
	ItemPattern string           `json:"itemPattern"`
	Truncated   bool             `json:"truncated"`
	Grants      []CellGrant      `json:"grants"`
	MemberRoles []CellMemberRole `json:"memberRoles"`
}

// CellGrantsRequest is the payload for cells.grants. Tenants lists the
// tenants of one cell, in the order the reply must answer them.
type CellGrantsRequest struct {
	Tenants []string `json:"tenants"`
}

// CellGrantsResponse is the reply for cells.grants. Tenants holds one entry
// for each requested tenant, in request order. Code and Error are set on a
// failed read.
type CellGrantsResponse struct {
	Tenants []CellTenantGrants `json:"tenants,omitempty"`
	Code    string             `json:"code,omitempty"`
	Error   string             `json:"error,omitempty"`
}
