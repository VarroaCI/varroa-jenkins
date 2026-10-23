// Package api: the non-HTTP entry point the get_cell_usage MCP tool
// calls. The tool and the HTTP path share one implementation of every
// rule, so the two surfaces authorize and read identically.
package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// CellUsageCounts holds the four load counts of one tenant on a cell,
// or the same four summed over the cell.
type CellUsageCounts struct {
	RunningNow    int64 `json:"runningNow"`
	RunsLast24h   int64 `json:"runsLast24h"`
	FailedLast24h int64 `json:"failedLast24h"`
	Jobs          int64 `json:"jobs"`
}

// CellTenantUsage names one tenant on a cell and carries that tenant's
// four counts: the same shape GET /cells/{namespace}/{name}/usage
// serializes.
type CellTenantUsage struct {
	Tenant string `json:"tenant"`
	// Role is that tenant's role on this cell: "slot <n>" for an active
	// member and "standby" for a standby. A standby holds the tenant
	// folder and runs no build.
	Role string `json:"role,omitempty"`
	// AgentNamespace is the namespace of the tenant's build agents, as
	// the tenant's own cluster last announced it. It is absent until an
	// announcement names one.
	AgentNamespace string `json:"agentNamespace,omitempty"`
	// PlacementGeneration is the generation of the tenant's placement
	// row in the record store.
	PlacementGeneration int64 `json:"placementGeneration"`
	CellUsageCounts
}

// CellUsage is one cell's load: the same shape
// GET /cells/{namespace}/{name}/usage serializes.
type CellUsage struct {
	Cluster        string            `json:"cluster"`
	Namespace      string            `json:"namespace"`
	Name           string            `json:"name"`
	WindowStart    time.Time         `json:"windowStart"`
	CellPool       string            `json:"cellPool,omitempty"`
	Profile        string            `json:"profile,omitempty"`
	JenkinsVersion string            `json:"jenkinsVersion,omitempty"`
	JenkinsHealth  string            `json:"jenkinsHealth,omitempty"`
	QueueLength    *int64            `json:"queueLength,omitempty"`
	QueueLengthAt  *time.Time        `json:"queueLengthAt,omitempty"`
	Tenants        []CellTenantUsage `json:"tenants"`
	Totals         CellUsageCounts   `json:"totals"`
	MaxTenants     int32             `json:"maxTenants"`
	FreeSlots      int32             `json:"freeSlots"`
}

// cellUsageRowJSON maps a reader value onto the wire shape. The cell
// coordinates and the presentation fields are filled by the caller.
func cellUsageRowJSON(usage recordstore.CellUsage) cellUsageJSON {
	row := cellUsageJSON{
		WindowStart: usage.WindowStart.UTC(),
		Tenants:     make([]cellTenantUsageJSON, 0, len(usage.Tenants)),
		Totals: cellUsageCountsJSON{
			RunningNow:    usage.Totals.RunningNow,
			RunsLast24h:   usage.Totals.RunsLast24h,
			FailedLast24h: usage.Totals.FailedLast24h,
			Jobs:          usage.Totals.Jobs,
		},
	}
	for _, tenant := range usage.Tenants {
		row.Tenants = append(row.Tenants, cellTenantUsageJSON{
			Tenant:              tenant.Tenant,
			Role:                tenant.Role,
			AgentNamespace:      tenant.AgentNamespace,
			PlacementGeneration: tenant.PlacementGeneration,
			cellUsageCountsJSON: cellUsageCountsJSON{
				RunningNow:    tenant.RunningNow,
				RunsLast24h:   tenant.RunsLast24h,
				FailedLast24h: tenant.FailedLast24h,
				Jobs:          tenant.Jobs,
			},
		})
	}
	sort.Slice(row.Tenants, func(i, j int) bool { return row.Tenants[i].Tenant < row.Tenants[j].Tenant })
	return row
}

// fillCellUsagePresentation adds the cell Controller's own fields and
// the computed tenant cap to row.
func (s *Server) fillCellUsagePresentation(row *cellUsageJSON, cr *v1alpha1.Controller, cluster string, caps *cellCapResolver) {
	presentation := s.cellPresentationFor(cr, cluster)
	row.CellPool = presentation.CellPool
	row.Profile = presentation.Profile
	row.JenkinsVersion = presentation.JenkinsVersion
	row.JenkinsHealth = presentation.JenkinsHealth
	row.QueueLength = presentation.QueueLength
	row.QueueLengthAt = presentation.QueueLengthAt
	row.MaxTenants = caps.resolve(cluster, cr)
	row.FreeSlots = cellFreeSlots(row.MaxTenants, cellPlacedTenants(cr))
	if row.WindowStart.IsZero() {
		row.WindowStart = time.Now().UTC().Add(-24 * time.Hour)
	}
}

// CellUsage reads one cell's load. It is the single-cell HTTP path's
// implementation and the get_cell_usage tool's implementation. It
// answers 404 for an unknown controller, a controller that is not a
// cell, and a caller the controller read check refuses: one code for
// all three keeps the surface from confirming that a cell exists.
func (s *Server) CellUsage(ctx context.Context, claims *auth.Claims, cluster, namespace, name string) (CellUsage, *ServiceError) {
	if s.deps.RecordStore == nil {
		return CellUsage{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	if s.deps.Authorizer == nil || claims == nil || !s.deps.Authorizer.CanReadController(claims, namespace, name) {
		return CellUsage{}, &ServiceError{Status: http.StatusNotFound, Message: "cell not found"}
	}

	cr, err := s.cellController(ctx, cluster, namespace, name)
	if err != nil || !v1alpha1.IsCell(cr) {
		return CellUsage{}, &ServiceError{Status: http.StatusNotFound, Message: "cell not found"}
	}

	usage, err := s.deps.RecordStore.CellUsage(ctx, cluster, namespace, name)
	if serr := s.readerServiceError(err, "read cell usage"); serr != nil {
		return CellUsage{}, serr
	}

	row := cellUsageRowJSON(usage)
	row.Cluster = cluster
	row.Namespace = namespace
	row.Name = name
	s.fillCellUsagePresentation(&row, cr, cluster, s.cellCaps(ctx))

	out, cerr := convertJSON[CellUsage](row)
	if cerr != nil {
		return CellUsage{}, &ServiceError{Status: http.StatusInternalServerError, Message: "encode cell usage failed"}
	}
	if out.Tenants == nil {
		out.Tenants = []CellTenantUsage{}
	}
	return out, nil
}

// cellController reads one cell Controller from the named cluster.
func (s *Server) cellController(ctx context.Context, cluster, namespace, name string) (*v1alpha1.Controller, error) {
	if s.deps.Brood == nil {
		return nil, nil
	}
	return s.deps.Brood.Get(ctx, cluster, namespace, name)
}

// CellGrantSubject is one subject of a grant. It is the bus wire type.
type CellGrantSubject = bus.CellGrantSubject

// CellGrant is one grant that reaches a tenant of a cell. It is the bus
// wire type.
type CellGrant = bus.CellGrant

// CellMemberRole is one generated Jenkins member role of a tenant. It is
// the bus wire type.
type CellMemberRole = bus.CellMemberRole

// CellTenantGrants is every grant that reaches one tenant of a cell. It is
// the bus wire type.
type CellTenantGrants = bus.CellTenantGrants

// CellGrants names a cell and carries one entry for each of its tenants.
type CellGrants struct {
	Cluster   string             `json:"cluster"`
	Namespace string             `json:"namespace"`
	Cell      string             `json:"cell"`
	Tenants   []CellTenantGrants `json:"tenants"`
}

// CellGrants answers which subjects may build on each tenant of a cell.
//
// It applies two gates. The controller read check on the cell comes first,
// exactly as the usage reads apply it, and a refusal answers 404 together
// with an unknown controller and a controller that is not a cell. The
// cluster-wide rolebindings:read check comes second, and a refusal answers
// 403: that caller already knows the cell exists.
//
// A local cell is read on the BFF's own resolver. A member cell is read on
// its own operator, which holds the Tenant and binding state the member
// renders. That path answers 502 when the member does not answer. The read
// touches no record store, so it answers no 503.
func (s *Server) CellGrants(ctx context.Context, claims *auth.Claims, cluster, namespace, name string) (CellGrants, *ServiceError) {
	notFound := &ServiceError{Status: http.StatusNotFound, Message: "cell not found"}
	if s.deps.Authorizer == nil || claims == nil || !s.deps.Authorizer.CanReadController(claims, namespace, name) {
		return CellGrants{}, notFound
	}
	cr, err := s.cellController(ctx, cluster, namespace, name)
	if err != nil || !v1alpha1.IsCell(cr) {
		return CellGrants{}, notFound
	}
	if !s.deps.Authorizer.CanReadRoleBindings(claims) {
		return CellGrants{}, &ServiceError{Status: http.StatusForbidden, Message: "forbidden"}
	}

	out := CellGrants{
		Cluster:   cluster,
		Namespace: namespace,
		Cell:      name,
		Tenants:   []CellTenantGrants{},
	}
	if cr.Status.Cell == nil {
		return out, nil
	}
	if s.isLocalCluster(cluster) || cluster == s.localCluster() {
		for _, tenant := range cr.Status.Cell.Tenants {
			sources, truncated := s.deps.Authorizer.TenantGrantSources(tenant)
			out.Tenants = append(out.Tenants, controller.CellTenantGrantsReport(
				tenant, sources, truncated, s.deps.Authorizer.TenantMemberRoles(tenant)))
		}
		return out, nil
	}
	if s.deps.ConfigBrood == nil {
		return CellGrants{}, &ServiceError{Status: http.StatusBadGateway, Message: "cluster " + cluster + " unreachable"}
	}
	tenants, err := s.deps.ConfigBrood.CellGrants(ctx, cluster, cr.Status.Cell.Tenants)
	if err != nil {
		return CellGrants{}, cellGrantsServiceError(err, cluster)
	}
	if tenants != nil {
		out.Tenants = tenants
	}
	return out, nil
}

// cellGrantsServiceError maps a member read error onto the status table of
// the grants read.
func cellGrantsServiceError(err error, cluster string) *ServiceError {
	var unreachable *ErrClusterUnreachable
	var brood *BroodError
	switch {
	case errors.As(err, &unreachable):
		return &ServiceError{Status: http.StatusBadGateway, Message: "cluster " + cluster + " unreachable"}
	case errors.As(err, &brood) && brood.Code == bus.CodeNotFound:
		return &ServiceError{Status: http.StatusNotFound, Message: "cell not found"}
	}
	return &ServiceError{Status: http.StatusInternalServerError, Message: "failed to read cell grants"}
}
