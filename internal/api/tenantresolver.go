package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// maxTenantTenants bounds the fan-out one tenant resolves to. Design D1 sets
// twenty parallel record-store queries as the accepted ceiling.
const maxTenantTenants = 20

// TenantResolver maps the {tenant} URL segment onto record-store tenants.
// This change owns the implementation for the life of the epic; no later
// change replaces it.
type TenantResolver interface {
	// Resolve returns every tenant the caller may read for tenant, applying
	// design D1's five-step order. It drops a candidate the tenant read
	// check refuses before that candidate reaches any response, and it
	// answers a *ServiceError for every other refusal: 409 for an
	// ambiguous controller name, 422 for a tenant that fans out past the
	// tenant ceiling, and 404 when nothing matches or nothing survives
	// the check.
	Resolve(ctx context.Context, claims *auth.Claims, tenant string) ([]TenantRef, error)

	// ResolveRun builds the TenantRef a stored run belongs to, from the
	// run's own cluster, namespace, cell, and tenant columns. It performs
	// no lookup and applies no authorization check; the caller decides
	// readability with Authorizer.CanReadRun.
	ResolveRun(ctx context.Context, claims *auth.Claims, r recordstore.Run) (TenantRef, error)
}

// tenantTenantResolver is TenantResolver's only implementation.
type tenantTenantResolver struct {
	store      crdstore.Backend
	brood      Brood
	authorizer *Authorizer
	placements TenantPlacementReader
}

// TenantPlacementReader answers a tenant's placement rows from the record
// store. recordstore.Reader satisfies it.
type TenantPlacementReader interface {
	ListTenantPlacements(ctx context.Context, tenant string) ([]recordstore.TenantPlacement, error)
}

// ResolverOption configures NewTenantTenantResolver.
type ResolverOption func(*tenantTenantResolver)

// WithMemberPlacements lets the resolver find a placed Tenant that another
// cluster's API server holds. The member operator places the Tenant and the
// core recorder stores the placement row, so the record store is the only
// view the core has of it. A nil reader leaves the option off.
func WithMemberPlacements(placements TenantPlacementReader) ResolverOption {
	return func(t *tenantTenantResolver) { t.placements = placements }
}

// NewTenantTenantResolver builds the resolver design D1 owns. store reads
// Tenant CRDs, brood lists controllers across every cluster, and authorizer
// decides which candidate tenant the caller may read.
func NewTenantTenantResolver(store crdstore.Backend, brood Brood, authorizer *Authorizer, opts ...ResolverOption) TenantResolver {
	t := &tenantTenantResolver{store: store, brood: brood, authorizer: authorizer}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Resolve implements TenantResolver.
func (t *tenantTenantResolver) Resolve(ctx context.Context, claims *auth.Claims, tenant string) ([]TenantRef, error) {
	candidates, err := t.candidates(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if len(candidates) > maxTenantTenants {
		return nil, &ServiceError{
			Status:  http.StatusUnprocessableEntity,
			Message: fmt.Sprintf("tenant %q resolves to %d tenants, past the %d-tenant limit", tenant, len(candidates), maxTenantTenants),
		}
	}

	admitted := make([]TenantRef, 0, len(candidates))
	for _, ref := range candidates {
		if t.authorizer.CanReadPipeline(claims, ref, "") {
			admitted = append(admitted, ref)
		}
	}
	if len(admitted) == 0 {
		// Step 5: an absent tenant, an unreadable tenant, and a tenant whose
		// candidates the caller may not read all answer 404. A 403 would
		// tell the caller a tenant it may not read exists.
		return nil, &ServiceError{Status: http.StatusNotFound, Message: "tenant not found"}
	}
	return admitted, nil
}

// candidates applies steps 1 through 4 of design D1's resolution order and
// returns the raw candidate set, before any authorization check.
func (t *tenantTenantResolver) candidates(ctx context.Context, tenant string) ([]TenantRef, error) {
	gvr, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		return nil, err
	}
	obj, err := t.store.GetObject(ctx, gvr, "", tenant)
	switch {
	case err == nil:
		ref, placed := placedTenant(tenant, obj)
		if placed {
			// Step 1: a placed Tenant resolves to exactly one tenant.
			return []TenantRef{ref}, nil
		}
		// Step 2: an unplaced Tenant fans out over its namespaces.
		return t.namespaceTenants(ctx, obj)
	case k8serrors.IsNotFound(err):
		// Step 1 for a Tenant a member cluster holds: the local API server
		// holds no such Tenant, but a member operator placed one of that
		// name, and the record store holds its placement row.
		refs, err := t.memberPlacedTenant(ctx, tenant)
		if err != nil || len(refs) > 0 {
			return refs, err
		}
		// Steps 3 and 4: no such Tenant. A controller of that name is a
		// synthetic single-controller tenant, unless the name is
		// ambiguous across clusters.
		return t.syntheticTenant(ctx, tenant)
	default:
		// An unreadable Tenant is indistinguishable from an absent one.
		return nil, nil
	}
}

// placedTenant reports design D1 step 1: a Tenant carrying spec.pipelines
// and a set status.placement.cell resolves to one tenant, built entirely
// from the placement the cell-controllers change writes. It reads both
// fields by path because Tenant's typed Go struct does not carry them yet.
func placedTenant(tenant string, obj *unstructured.Unstructured) (TenantRef, bool) {
	if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "pipelines"); !found {
		return TenantRef{}, false
	}
	cell, found, _ := unstructured.NestedString(obj.Object, "status", "placement", "cell")
	if !found || cell == "" {
		return TenantRef{}, false
	}
	cellNamespace, _, _ := unstructured.NestedString(obj.Object, "status", "placement", "cellNamespace")
	cluster, _, _ := unstructured.NestedString(obj.Object, "status", "placement", "cluster")
	return TenantRef{
		Cluster:    cluster,
		Namespace:  cellNamespace,
		Controller: cell,
		Tenant:     tenant,
		TenantName: tenant,
		Folder:     tenant,
	}, true
}

// memberPlacedTenant resolves a Tenant placed on a member cluster from its
// record-store placement row. It skips a row of the local cluster, whose
// Tenant the local API server no longer holds, a row that names no cell,
// and a row at generation 0, whose tenant a teardown ended. Rows on two
// or more member clusters are ambiguous.
func (t *tenantTenantResolver) memberPlacedTenant(ctx context.Context, tenant string) ([]TenantRef, error) {
	if t.placements == nil {
		return nil, nil
	}
	rows, err := t.placements.ListTenantPlacements(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("read placement of tenant %q: %w", tenant, err)
	}
	local := ""
	if t.brood != nil {
		local = t.brood.LocalCluster()
	}
	var refs []TenantRef
	var clusters []string
	for _, row := range rows {
		if !livePlacementRow(row, local) {
			continue
		}
		refs = append(refs, TenantRef{
			Cluster:    row.Cluster,
			Namespace:  row.Namespace,
			Controller: row.Cell,
			Tenant:     tenant,
			TenantName: tenant,
			Folder:     tenant,
		})
		clusters = append(clusters, row.Cluster)
	}
	if len(refs) > 1 {
		return nil, &ServiceError{
			Status:  http.StatusConflict,
			Message: fmt.Sprintf("tenant %q is placed on clusters %s", tenant, strings.Join(clusters, ", ")),
		}
	}
	return refs, nil
}

// namespaceTenants applies design D1 step 2: one TenantRef per controller
// in each namespace obj's spec.namespaces names.
func (t *tenantTenantResolver) namespaceTenants(ctx context.Context, obj *unstructured.Unstructured) ([]TenantRef, error) {
	namespaces, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "namespaces")
	if len(namespaces) == 0 {
		return nil, nil
	}

	all, _, err := t.brood.ListAll(ctx, "", "")
	if err != nil {
		return nil, err
	}
	return tenantNamespaceTenants(namespaces, all), nil
}

// tenantNamespaceTenants returns one TenantRef for each controller in all
// whose namespace namespaces names. It is the pure core of design D1 step
// 2, shared with the landing-page list.
func tenantNamespaceTenants(namespaces []string, all []ClusterController) []TenantRef {
	inScope := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		inScope[ns] = true
	}
	var refs []TenantRef
	for _, row := range all {
		if row.CR == nil || !inScope[row.CR.Namespace] {
			continue
		}
		refs = append(refs, controllerTenant(row))
	}
	return refs
}

// syntheticTenant applies design D1 steps 3 and 4: no Tenant named tenant
// exists, so a controller literally named tenant is its own tenant. Two or
// more such controllers across clusters are ambiguous.
func (t *tenantTenantResolver) syntheticTenant(ctx context.Context, tenant string) ([]TenantRef, error) {
	all, _, err := t.brood.ListAll(ctx, "", "")
	if err != nil {
		return nil, err
	}

	var refs []TenantRef
	clusterSet := make(map[string]bool)
	for _, row := range all {
		if row.CR == nil || row.CR.Name != tenant {
			continue
		}
		refs = append(refs, controllerTenant(row))
		clusterSet[row.Cluster] = true
	}
	if len(refs) > 1 {
		clusters := make([]string, 0, len(clusterSet))
		for c := range clusterSet {
			clusters = append(clusters, c)
		}
		sort.Strings(clusters)
		return nil, &ServiceError{
			Status:  http.StatusConflict,
			Message: fmt.Sprintf("controller %q is ambiguous across clusters %s", tenant, strings.Join(clusters, ", ")),
		}
	}
	return refs, nil
}

// controllerTenant builds the synthetic TenantRef for one controller: the
// tenant is "<namespace>/<controller>", the tenant segment is the
// controller name, and the folder is empty because an unmanaged
// controller hosts its jobs at the Jenkins root.
func controllerTenant(row ClusterController) TenantRef {
	return TenantRef{
		Cluster:    row.Cluster,
		Namespace:  row.CR.Namespace,
		Controller: row.CR.Name,
		Tenant:     row.CR.Namespace + "/" + row.CR.Name,
		TenantName: row.CR.Name,
		Folder:     "",
	}
}

// ResolveRun implements TenantResolver. It reuses TenantRefFromRow, which
// computes Tenant and Folder from the tenant column alone, matching design
// D1's one rule: a tenant with no "/" is a placed Tenant, and a tenant of
// the form "<namespace>/<cell>" is a controller tenant.
func (t *tenantTenantResolver) ResolveRun(_ context.Context, _ *auth.Claims, r recordstore.Run) (TenantRef, error) {
	return TenantRefFromRow(r.Cluster, r.Namespace, r.Cell, r.Tenant)
}

// livePlacementRow reports whether a placement row names a live tenant on
// a cluster other than local. A torn-down tenant keeps its row at
// generation 0, and a row with no cell names no tenant home.
func livePlacementRow(row recordstore.TenantPlacement, local string) bool {
	return row.Cluster != local && row.Cell != "" && row.Generation > 0
}
