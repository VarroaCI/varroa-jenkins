package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GrantPath names one of GrantEvaluator's four call-site shapes: a
// claim, a cross-tenant request, an upstream fan-out, or a fleet-lock
// acquire or renew.
type GrantPath string

// The four allocated GrantPath values.
const (
	GrantPathClaim   GrantPath = "claim"
	GrantPathRequest GrantPath = "request"
	GrantPathFanout  GrantPath = "fanout"
	GrantPathLock    GrantPath = "lock"
)

// GrantResourceKind names the kind of resource a GrantInput asks about.
type GrantResourceKind string

// The two allocated GrantResourceKind values.
const (
	GrantResourceJob  GrantResourceKind = "job"
	GrantResourceLock GrantResourceKind = "lock"
)

// PlacementInput is CheckPlacement's argument. Tenant is the tenant
// whose placement the fence checks against the calling cell; it is not
// necessarily the caller of a cross-tenant call.
type PlacementInput struct {
	Path                GrantPath
	Cluster             string
	Namespace           string
	Cell                string
	Tenant              string
	PlacementGeneration int64
}

// GrantInput is Check's argument. It embeds PlacementInput and adds the
// tenant-export-grant capability's own fields. Resource is the job path
// for a job, and the bare lock name for a lock.
type GrantInput struct {
	PlacementInput
	CallerTenant    string
	TargetTenant    string
	ResourceKind    GrantResourceKind
	Resource        string
	IntentKeyScheme IntentScheme
}

// GrantResult is CheckPlacement's and Check's answer. Its shape matches
// FenceResult, so a caller that also implements Fence can build one from
// the other with a plain field copy.
type GrantResult struct {
	Allow      bool
	ReasonCode string
	Message    string
}

// GrantEvaluator is the one implementation of the placement-fencing
// capability's fence and the tenant-export-grant capability's decision
// table, for every call site beyond C6's own claim-path Fence.
// CheckPlacement runs the placement orders; Check runs CheckPlacement
// first, then the grant orders.
type GrantEvaluator struct {
	pool *pgxpool.Pool
}

// NewGrantEvaluator returns a GrantEvaluator over pool.
func NewGrantEvaluator(pool *pgxpool.Pool) *GrantEvaluator {
	return &GrantEvaluator{pool: pool}
}

// placementRow is the subset of one tenant_placements row CheckPlacement
// reads for a (cluster, tenant) lookup.
type placementRow struct {
	namespace  string
	cell       string
	generation int64
	// members resolves the caller's own member. PlacementInput carries a
	// namespace and a cell and carries no cell instance and no job path,
	// so memberForCell is the exact lookup for that question.
	members placementMembers
}

// placementRowQuery reads the columns placementRow needs.
const placementRowQuery = `
	SELECT namespace, cell, generation, cell_instance, rehydrated_instance, pending_jobs,
	       members, job_homes
	FROM varroa.tenant_placements
	WHERE cluster = $1 AND tenant = $2
`

// CheckPlacement runs the placement orders for in.Path. A missing row
// continues on the claim path, because the claim's own Fence already
// vetoes a missing row with NOT_READY before this call ever runs; it
// vetoes CROSS_TENANT on the request and lock paths, because a
// cross-tenant call from an unplaced cell carries nothing the recorder
// can trust; and it answers a retryable NOT_READY on the fanout path,
// which the caller reads as a skip. A found row checks the input
// generation and cell against the stored ones for every path except
// fanout, which asks only whether the subscriber is placed at all.
//
// The member order runs before the generation order. A tenant holds
// several members, so the stored scalar cell names slot 0 alone, and a
// caller on any slot passes. A caller on a cell the tenant has left gets
// the permanent FENCED. Every input that reaches the generation order
// therefore comes from a current member whose gateway resolver cache is
// stale, so that order answers the retryable NOT_READY: a lock acquire
// and every cross-tenant request enter this method directly, not through
// the claim fence, and a permanent answer would refuse a live member
// after any generation raise.
func (e *GrantEvaluator) CheckPlacement(ctx context.Context, in PlacementInput) (GrantResult, error) {
	var (
		row         placementRow
		instance    string
		rehydrated  string
		pendingRaw  json.RawMessage
		membersRaw  json.RawMessage
		jobHomesRaw json.RawMessage
	)
	err := e.pool.QueryRow(ctx, placementRowQuery, in.Cluster, in.Tenant).
		Scan(&row.namespace, &row.cell, &row.generation, &instance, &rehydrated,
			&pendingRaw, &membersRaw, &jobHomesRaw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return checkPlacementNoRow(in), nil
	case err != nil:
		return GrantResult{}, fmt.Errorf("read tenant placement for %s: %w", in.Tenant, err)
	}

	if in.Path == GrantPathFanout {
		return GrantResult{Allow: true}, nil
	}
	var pending []string
	if len(pendingRaw) > 0 {
		if err := json.Unmarshal(pendingRaw, &pending); err != nil {
			return GrantResult{}, fmt.Errorf("decode pending jobs for %s: %w", in.Tenant, err)
		}
	}
	row.members, err = decodePlacementMembers(membersRaw, jobHomesRaw, placementScalars{
		cell: row.cell, namespace: row.namespace, cellInstance: instance,
		rehydratedInstance: rehydrated, pendingJobs: pending,
	})
	if err != nil {
		return GrantResult{}, err
	}

	if _, isMember := memberForCell(row.members, in.Namespace, in.Cell); !isMember {
		return GrantResult{Allow: false, ReasonCode: ReasonFenced,
			Message: fmt.Sprintf("cell %s is not the placement of tenant %s", in.Cell, in.Tenant)}, nil
	}
	if in.PlacementGeneration != row.generation {
		return GrantResult{Allow: false, ReasonCode: ReasonNotReady,
			Message: fmt.Sprintf("placement generation %d is stale for tenant %s", in.PlacementGeneration, in.Tenant)}, nil
	}
	return GrantResult{Allow: true}, nil
}

// checkPlacementNoRow answers CheckPlacement's first order for a tenant
// with no stored placement, one answer per path.
func checkPlacementNoRow(in PlacementInput) GrantResult {
	switch in.Path {
	case GrantPathClaim:
		return GrantResult{Allow: true}
	case GrantPathFanout:
		return GrantResult{Allow: false, ReasonCode: ReasonNotReady,
			Message: fmt.Sprintf("no placement row for tenant %s", in.Tenant)}
	default: // GrantPathRequest, GrantPathLock
		return GrantResult{Allow: false, ReasonCode: ReasonCrossTenant,
			Message: crossTenantMessage + fmt.Sprintf(" Tenant %s is not placed. A cell role is required.", in.Tenant)}
	}
}

// Check runs CheckPlacement, then the tenant-export-grant capability's
// grant orders. It reads the target tenant's exports from
// tenant_placements.policy, never from a Kubernetes object.
func (e *GrantEvaluator) Check(ctx context.Context, in GrantInput) (GrantResult, error) {
	placement, err := e.CheckPlacement(ctx, in.PlacementInput)
	if err != nil {
		return GrantResult{}, err
	}
	if !placement.Allow {
		return placement, nil
	}

	if in.CallerTenant == "" {
		// Order 6: an ordinary build carries no caller tenant and costs
		// no grant lookup.
		return GrantResult{Allow: true}, nil
	}
	if in.CallerTenant == in.TargetTenant {
		// Order 7: a same-tenant call needs no grant.
		return GrantResult{Allow: true}, nil
	}
	if in.ResourceKind == GrantResourceJob &&
		in.IntentKeyScheme != IntentSchemeUpstream && in.IntentKeyScheme != IntentSchemeRetry {
		// Order 8: only the recorder and the BFF mint an upstream or a
		// retry intent key, so any other scheme on a job is a build the
		// cell invented. A lock carries no intent key, so this order
		// does not apply to a lock.
		return GrantResult{Allow: false, ReasonCode: ReasonCrossTenant,
			Message: crossTenantMessage + " Only the Varroa path may cross a tenant boundary."}, nil
	}

	policy, veto, err := e.resolveTargetPolicy(ctx, in.Cluster, in.TargetTenant)
	if err != nil {
		return GrantResult{}, err
	}
	if veto != nil {
		// Orders 9 and 10.
		return *veto, nil
	}

	entries, matched := matchExportEntries(decodeExports(policy), in.ResourceKind, in.Resource, in.TargetTenant)
	if !matched {
		// Order 11.
		return GrantResult{Allow: false, ReasonCode: ReasonCrossTenant,
			Message: crossTenantMessage + fmt.Sprintf(" Tenant %s exports no %s matching %s.",
				in.TargetTenant, in.ResourceKind, in.Resource)}, nil
	}
	// Order 12 grants when the caller is in ANY matching entry's callers
	// list, the union over every entry the resource matches, rather than
	// only the first one.
	granted := false
	for _, entry := range entries {
		if containsString(entry.Callers, in.CallerTenant) {
			granted = true
			break
		}
	}
	if !granted {
		return GrantResult{Allow: false, ReasonCode: ReasonCrossTenant,
			Message: crossTenantMessage + fmt.Sprintf(" Tenant %s does not grant %s %s to %s.",
				in.TargetTenant, in.ResourceKind, in.Resource, in.CallerTenant)}, nil
	}
	// Order 13.
	return GrantResult{Allow: true}, nil
}

// resolveTargetPolicy resolves target's tenant_placements policy for the
// grant orders. It answers the row of the caller's own cluster when one
// exists there. Otherwise it answers the lone row fleet-wide, a veto
// naming the missing tenant when no row exists anywhere, or a veto
// naming every candidate cluster when two or more exist and none is the
// caller's own.
func (e *GrantEvaluator) resolveTargetPolicy(ctx context.Context, callerCluster, target string) (json.RawMessage, *GrantResult, error) {
	rows, err := e.pool.Query(ctx, `SELECT cluster, policy FROM varroa.tenant_placements WHERE tenant = $1`, target)
	if err != nil {
		return nil, nil, fmt.Errorf("select tenant placements for %s: %w", target, err)
	}
	defer rows.Close()

	type candidate struct {
		cluster string
		policy  json.RawMessage
	}
	var (
		all     []candidate
		matched candidate
		found   bool
	)
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.cluster, &c.policy); err != nil {
			return nil, nil, fmt.Errorf("scan tenant placement for %s: %w", target, err)
		}
		all = append(all, c)
		if c.cluster == callerCluster {
			matched, found = c, true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("select tenant placements for %s: %w", target, err)
	}

	if found {
		return matched.policy, nil, nil
	}
	switch len(all) {
	case 0:
		veto := GrantResult{Allow: false, ReasonCode: ReasonCrossTenant,
			Message: crossTenantMessage + fmt.Sprintf(" Tenant %s does not exist.", target)}
		return nil, &veto, nil
	case 1:
		return all[0].policy, nil, nil
	default:
		clusters := make([]string, len(all))
		for i, c := range all {
			clusters[i] = c.cluster
		}
		sort.Strings(clusters)
		veto := GrantResult{Allow: false, ReasonCode: ReasonCrossTenant,
			Message: crossTenantMessage + fmt.Sprintf(" Tenant %s exists in clusters %s. Name one cluster.",
				target, strings.Join(clusters, ", "))}
		return nil, &veto, nil
	}
}

// exportEntry is one entry of a tenant's exports list: a job export or a
// lock export, plus the callers it grants.
type exportEntry struct {
	Job     string   `json:"job,omitempty"`
	Locks   []string `json:"locks,omitempty"`
	Callers []string `json:"callers,omitempty"`
}

// policyExports decodes the "exports" field of a tenant_placements
// policy column.
type policyExports struct {
	Exports []exportEntry `json:"exports,omitempty"`
}

// decodeExports decodes policy's exports list. An empty or malformed
// policy answers no exports, following decodeConcurrency's convention: a
// tenant with no readable policy grants nothing, and the grant orders
// veto on their own.
func decodeExports(policy json.RawMessage) []exportEntry {
	if len(policy) == 0 {
		return nil
	}
	var p policyExports
	if err := json.Unmarshal(policy, &p); err != nil {
		return nil
	}
	return p.Exports
}

// containsString reports whether s is one of list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
