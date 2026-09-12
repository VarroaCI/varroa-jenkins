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

// maxTeamTenants bounds the fan-out one team resolves to. Design D1 sets
// twenty parallel record-store queries as the accepted ceiling.
const maxTeamTenants = 20

// TenantResolver maps the {team} URL segment onto record-store tenants.
// This change owns the implementation for the life of the epic; no later
// change replaces it.
type TenantResolver interface {
	// Resolve returns every tenant the caller may read for team, applying
	// design D1's five-step order. It drops a candidate the tenant read
	// check refuses before that candidate reaches any response, and it
	// answers a *ServiceError for every other refusal: 409 for an
	// ambiguous controller name, 422 for a team that fans out past the
	// tenant ceiling, and 404 when nothing matches or nothing survives
	// the check.
	Resolve(ctx context.Context, claims *auth.Claims, team string) ([]TenantRef, error)

	// ResolveRun builds the TenantRef a stored run belongs to, from the
	// run's own cluster, namespace, cell, and tenant columns. It performs
	// no lookup and applies no authorization check; the caller decides
	// readability with Authorizer.CanReadRun.
	ResolveRun(ctx context.Context, claims *auth.Claims, r recordstore.Run) (TenantRef, error)
}

// teamTenantResolver is TenantResolver's only implementation.
type teamTenantResolver struct {
	store      crdstore.Backend
	brood      Brood
	authorizer *Authorizer
}

// NewTeamTenantResolver builds the resolver design D1 owns. store reads
// Team CRDs, brood lists controllers across every cluster, and authorizer
// decides which candidate tenant the caller may read.
func NewTeamTenantResolver(store crdstore.Backend, brood Brood, authorizer *Authorizer) TenantResolver {
	return &teamTenantResolver{store: store, brood: brood, authorizer: authorizer}
}

// Resolve implements TenantResolver.
func (t *teamTenantResolver) Resolve(ctx context.Context, claims *auth.Claims, team string) ([]TenantRef, error) {
	candidates, err := t.candidates(ctx, team)
	if err != nil {
		return nil, err
	}
	if len(candidates) > maxTeamTenants {
		return nil, &ServiceError{
			Status:  http.StatusUnprocessableEntity,
			Message: fmt.Sprintf("team %q resolves to %d tenants, past the %d-tenant limit", team, len(candidates), maxTeamTenants),
		}
	}

	admitted := make([]TenantRef, 0, len(candidates))
	for _, ref := range candidates {
		if t.authorizer.CanReadPipeline(claims, ref, "") {
			admitted = append(admitted, ref)
		}
	}
	if len(admitted) == 0 {
		// Step 5: an absent team, an unreadable team, and a team whose
		// candidates the caller may not read all answer 404. A 403 would
		// tell the caller a team it may not read exists.
		return nil, &ServiceError{Status: http.StatusNotFound, Message: "team not found"}
	}
	return admitted, nil
}

// candidates applies steps 1 through 4 of design D1's resolution order and
// returns the raw candidate set, before any authorization check.
func (t *teamTenantResolver) candidates(ctx context.Context, team string) ([]TenantRef, error) {
	gvr, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		return nil, err
	}
	obj, err := t.store.GetObject(ctx, gvr, "", team)
	switch {
	case err == nil:
		ref, placed := placedTenant(team, obj)
		if placed {
			// Step 1: a placed Team resolves to exactly one tenant.
			return []TenantRef{ref}, nil
		}
		// Step 2: an unplaced Team fans out over its namespaces.
		return t.namespaceTenants(ctx, obj)
	case k8serrors.IsNotFound(err):
		// Steps 3 and 4: no such Team. A controller of that name is a
		// synthetic single-controller tenant, unless the name is
		// ambiguous across clusters.
		return t.syntheticTenant(ctx, team)
	default:
		// An unreadable Team is indistinguishable from an absent one.
		return nil, nil
	}
}

// placedTenant reports design D1 step 1: a Team carrying spec.pipelines
// and a set status.placement.cell resolves to one tenant, built entirely
// from the placement the cell-controllers change writes. It reads both
// fields by path because Team's typed Go struct does not carry them yet.
func placedTenant(team string, obj *unstructured.Unstructured) (TenantRef, bool) {
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
		Tenant:     team,
		Team:       team,
		Folder:     team,
	}, true
}

// namespaceTenants applies design D1 step 2: one TenantRef per controller
// in each namespace obj's spec.namespaces names.
func (t *teamTenantResolver) namespaceTenants(ctx context.Context, obj *unstructured.Unstructured) ([]TenantRef, error) {
	namespaces, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "namespaces")
	if len(namespaces) == 0 {
		return nil, nil
	}
	inScope := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		inScope[ns] = true
	}

	all, _, err := t.brood.ListAll(ctx, "", "")
	if err != nil {
		return nil, err
	}

	var refs []TenantRef
	for _, row := range all {
		if row.CR == nil || !inScope[row.CR.Namespace] {
			continue
		}
		refs = append(refs, controllerTenant(row))
	}
	return refs, nil
}

// syntheticTenant applies design D1 steps 3 and 4: no Team named team
// exists, so a controller literally named team is its own tenant. Two or
// more such controllers across clusters are ambiguous.
func (t *teamTenantResolver) syntheticTenant(ctx context.Context, team string) ([]TenantRef, error) {
	all, _, err := t.brood.ListAll(ctx, "", "")
	if err != nil {
		return nil, err
	}

	var refs []TenantRef
	clusterSet := make(map[string]bool)
	for _, row := range all {
		if row.CR == nil || row.CR.Name != team {
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
			Message: fmt.Sprintf("controller %q is ambiguous across clusters %s", team, strings.Join(clusters, ", ")),
		}
	}
	return refs, nil
}

// controllerTenant builds the synthetic TenantRef for one controller: the
// tenant is "<namespace>/<controller>", the team segment is the
// controller name, and the folder is empty because an unmanaged
// controller hosts its jobs at the Jenkins root.
func controllerTenant(row ClusterController) TenantRef {
	return TenantRef{
		Cluster:    row.Cluster,
		Namespace:  row.CR.Namespace,
		Controller: row.CR.Name,
		Tenant:     row.CR.Namespace + "/" + row.CR.Name,
		Team:       row.CR.Name,
		Folder:     "",
	}
}

// ResolveRun implements TenantResolver. It reuses TenantRefFromRow, which
// computes Team and Folder from the tenant column alone, matching design
// D1's one rule: a tenant with no "/" is a placed Team, and a tenant of
// the form "<namespace>/<cell>" is a controller tenant.
func (t *teamTenantResolver) ResolveRun(_ context.Context, _ *auth.Claims, r recordstore.Run) (TenantRef, error) {
	return TenantRefFromRow(r.Cluster, r.Namespace, r.Cell, r.Tenant)
}
