package api

import (
	"context"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// Placement states of a readable pipelines tenant.
const (
	// placementPlaced is a tenant with one live cell.
	placementPlaced = "placed"
	// placementAwaitingCell is a local pipelines tenant with no cell yet.
	placementAwaitingCell = "awaitingCell"
	// placementControllers is a tenant without spec.pipelines whose namespaces
	// hold controllers. Its runs come from each readable controller.
	placementControllers = "controllers"
)

// readableTenant is one pipelines tenant the caller may read. Row is its
// GET /pipelines row. Refs lists every record-store tenant of the row, so a
// run read covers each of them. An awaiting-cell tenant has no refs.
type readableTenant struct {
	Row       pipelineTenantJSON
	Placement string
	Refs      []TenantRef
}

// readablePipelineTenants answers the pipelines tenants the caller may read,
// in name order. It is the one walk that GET /pipelines, the fleet run read,
// and the overview all use. The bool reports whether the record store
// answered the placement rows.
func (s *Server) readablePipelineTenants(ctx context.Context, claims *auth.Claims) ([]readableTenant, bool, error) {
	local := ""
	if s.deps.Brood != nil {
		local = s.deps.Brood.LocalCluster()
	}

	var candidates []readableTenant
	var fanOut []*v1alpha1.Tenant
	localNames := make(map[string]bool)
	if s.deps.Store != nil {
		tenants, err := crdstore.List[v1alpha1.Tenant](ctx, s.deps.Store, "", "")
		if err != nil {
			return nil, false, err
		}
		for _, tenant := range tenants {
			localNames[tenant.Name] = true
			if tenant.Spec.Pipelines == nil {
				if len(tenant.Spec.Namespaces) > 0 {
					fanOut = append(fanOut, tenant)
				}
				continue
			}
			candidates = append(candidates, pipelineCandidate(localPipelineTenantRow(tenant, local)))
		}
	}

	placements, recordStore, err := s.allTenantPlacements(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, row := range placements {
		if localNames[row.Tenant] || strings.Contains(row.Tenant, "/") || !livePlacementRow(row, local) {
			continue
		}
		candidates = append(candidates, pipelineCandidate(pipelineTenantJSON{
			Name:          row.Tenant,
			Cluster:       row.Cluster,
			Cell:          row.Cell,
			CellNamespace: row.Namespace,
			Placed:        true,
		}))
	}

	readable := make(map[string]bool)
	items := make([]readableTenant, 0, len(candidates))
	for _, c := range candidates {
		ok, seen := readable[c.Row.Name]
		if !seen {
			ref := TenantRef{Tenant: c.Row.Name, TenantName: c.Row.Name, Folder: c.Row.Name}
			ok = s.deps.Authorizer.CanReadPipeline(claims, ref, "")
			readable[c.Row.Name] = ok
		}
		if ok {
			items = append(items, c)
		}
	}
	items = append(items, s.readableNamespaceTenants(ctx, claims, fanOut, local)...)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Row.Name != items[j].Row.Name {
			return items[i].Row.Name < items[j].Row.Name
		}
		return items[i].Row.Cluster < items[j].Row.Cluster
	})
	return items, recordStore, nil
}

// pipelineCandidate wraps a row of a local or member pipelines tenant. A placed
// row has one ref. A row with no cell has none.
func pipelineCandidate(row pipelineTenantJSON) readableTenant {
	if !row.Placed {
		return readableTenant{Row: row, Placement: placementAwaitingCell}
	}
	ref := TenantRef{
		Cluster:    row.Cluster,
		Namespace:  row.CellNamespace,
		Controller: row.Cell,
		Tenant:     row.Name,
		TenantName: row.Name,
		Folder:     row.Name,
	}
	return readableTenant{Row: row, Placement: placementPlaced, Refs: []TenantRef{ref}}
}

// readableNamespaceTenants answers the local Tenants without spec.pipelines
// that the tenant resolver would fan out over controllers. A Tenant with no
// readable controller tenant, or with more tenants than the resolver admits,
// is left out, because the resolver answers 404 or 422 for it. Every readable
// controller tenant of a listed Tenant stays in Refs. A failed controller
// list leaves these rows out; it does not fail the read.
func (s *Server) readableNamespaceTenants(ctx context.Context, claims *auth.Claims, tenants []*v1alpha1.Tenant, local string) []readableTenant {
	if len(tenants) == 0 || s.deps.Brood == nil {
		return nil
	}
	all, _, err := s.deps.Brood.ListAll(ctx, "", "")
	if err != nil {
		s.deps.Logger.Warn("list pipelines tenants: list controllers", "error", err)
		return nil
	}
	var out []readableTenant
	for _, tenant := range tenants {
		refs := tenantNamespaceTenants(tenant.Spec.Namespaces, all)
		if len(refs) == 0 || len(refs) > maxTenantTenants {
			continue
		}
		var readableRefs []TenantRef
		for _, ref := range refs {
			if s.deps.Authorizer.CanReadPipeline(claims, ref, "") {
				readableRefs = append(readableRefs, ref)
			}
		}
		if len(readableRefs) == 0 {
			continue
		}
		out = append(out, readableTenant{
			Row:       pipelineTenantJSON{Name: tenant.Name, Cluster: local, Placed: false},
			Placement: placementControllers,
			Refs:      readableRefs,
		})
	}
	return out
}

// runScopes answers one fleet-read scope for each readable ref whose caller
// holds a job-pattern read. The returned map names each "<cluster>/<tenant>"
// key with its rail name, the tenant name of its row.
func (s *Server) runScopes(claims *auth.Claims, tenants []readableTenant) ([]recordstore.RunScope, map[string][]string) {
	var scopes []recordstore.RunScope
	names := make(map[string][]string)
	seen := make(map[string]bool)
	for _, t := range tenants {
		for _, ref := range t.Refs {
			key := ref.Cluster + "/" + ref.Tenant
			patterns, ok := s.deps.Authorizer.PipelineReadJobPatterns(claims, ref)
			if !ok {
				continue
			}
			// Two rows can name one controller tenant. Every such row keeps its
			// rail name, and the scope is read once.
			if !containsName(names[key], t.Row.Name) {
				names[key] = append(names[key], t.Row.Name)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			scopes = append(scopes, recordstore.RunScope{Cluster: ref.Cluster, Tenant: ref.Tenant, JobPatterns: patterns})
		}
	}
	return scopes, names
}

// containsName reports whether name is in names.
func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
