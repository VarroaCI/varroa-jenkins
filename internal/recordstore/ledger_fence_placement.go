package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// crossTenantActorPrefix is the tenant service identity prefix the
// cross-tenant-veto capability defines. A build running as svc-<team>
// carries that team's own identity as its actor.
const crossTenantActorPrefix = "svc-"

// crossTenantMessage is the exact message the cross-tenant-veto
// capability requires.
const crossTenantMessage = "cross-tenant triggers go through Varroa."

// PlacementFence is the placement-fencing capability's Fence
// implementation. It reads varroa.tenant_placements and applies the
// twelve-order decision table.
type PlacementFence struct {
	pool      *pgxpool.Pool
	metrics   *FenceMetrics
	evaluator *GrantEvaluator
}

// NewPlacementFence returns a PlacementFence over pool. metrics may be
// nil, which silences recorder_fence_veto_total.
func NewPlacementFence(pool *pgxpool.Pool, metrics *FenceMetrics) *PlacementFence {
	return &PlacementFence{pool: pool, metrics: metrics, evaluator: NewGrantEvaluator(pool)}
}

// tenantPlacementFenceRow is the subset of one tenant_placements row the
// decision table reads.
type tenantPlacementFenceRow struct {
	namespace          string
	cell               string
	generation         int64
	cellInstance       string
	rehydratedInstance string
	pendingJobs        []string
	// members resolves every member's identity for this row. A row with
	// an empty stored list answers one synthetic slot-0 member, so a
	// single-member tenant reads exactly as it did before slots existed.
	members placementMembers
}

// tenantPlacementFenceQuery is the column list both readRow and
// readRowTx select, in the order scanTenantPlacementFenceRow expects.
const tenantPlacementFenceQuery = `
	SELECT namespace, cell, generation, cell_instance, rehydrated_instance, pending_jobs,
	       members, job_homes
	FROM varroa.tenant_placements
	WHERE cluster = $1 AND tenant = $2
`

// Check implements Fence. It reads the stored placement row for the
// claim's cluster and tenant with no lock, then applies decidePlacement.
// A veto is counted in recorder_fence_veto_total, labelled by reason.
// Claim never calls this method on a PlacementFence: it calls CheckTx,
// so the read locks the row for the whole claim transaction. Check
// stays exported for a caller with no open claim transaction, and for
// TestPlacementFenceCheckFailsClosedOnReadError.
func (f *PlacementFence) Check(ctx context.Context, req ClaimRequest) (FenceResult, error) {
	row, exists, err := scanTenantPlacementFenceRow(f.pool.QueryRow(ctx, tenantPlacementFenceQuery, req.Cluster, req.Tenant))
	if err != nil {
		return FenceResult{}, err
	}
	return f.decide(ctx, row, exists, req)
}

// CheckTx implements TxFence. It reads the claim's placement row with
// FOR SHARE inside tx, so the row stays locked for the rest of the
// claim transaction. A concurrent placement announcement takes the same
// row under FOR UPDATE (tenant_sync.go's readTenantPlacementForUpdate),
// so its generation bump either commits before this read runs, and this
// check then sees the new generation, or it blocks until tx commits or
// rolls back, and this check's decision is made and committed before
// the bump lands. Either order is consistent: no run is ever inserted
// GRANTED against a generation the placement row had already left
// behind by the time the insert's commit is visible.
func (f *PlacementFence) CheckTx(ctx context.Context, tx pgx.Tx, req ClaimRequest) (FenceResult, error) {
	row, exists, err := scanTenantPlacementFenceRow(tx.QueryRow(ctx, tenantPlacementFenceQuery+" FOR SHARE", req.Cluster, req.Tenant))
	if err != nil {
		return FenceResult{}, err
	}
	return f.decide(ctx, row, exists, req)
}

// decide applies decidePlacement, then replaces a cross-tenant veto with
// a call to the tenant-export-grant capability's evaluator, so a granted
// upstream or retry claim passes instead of a permanent veto. It counts
// whatever veto stands after that replacement.
func (f *PlacementFence) decide(ctx context.Context, row tenantPlacementFenceRow, exists bool, req ClaimRequest) (FenceResult, error) {
	result := decidePlacement(row, exists, req)
	if !result.Allow && result.ReasonCode == ReasonCrossTenant {
		granted, err := f.checkCrossTenantGrant(ctx, req)
		if err != nil {
			return FenceResult{}, err
		}
		result = granted
		if result.Allow {
			// The grant answers the cross-tenant order only. The seed
			// orders after it still apply, so the claim runs through them
			// with the actor treated as the tenant's own.
			own := req
			own.Actor = ""
			result = decidePlacement(row, exists, own)
		}
	}
	if !result.Allow {
		f.metrics.addVeto(ctx, result.ReasonCode)
	}
	return result, nil
}

// checkCrossTenantGrant calls GrantEvaluator.Check on the claim path, in
// place of decidePlacement's own permanent cross-tenant veto. It derives
// caller_tenant from the claim's actor, and leaves it empty for an actor
// that does not match svc-<name>.
func (f *PlacementFence) checkCrossTenantGrant(ctx context.Context, req ClaimRequest) (FenceResult, error) {
	scheme, _ := ParseIntentKey(req.IntentKey)
	result, err := f.evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			Path:                GrantPathClaim,
			Cluster:             req.Cluster,
			Namespace:           req.Namespace,
			Cell:                req.Cell,
			Tenant:              req.Tenant,
			PlacementGeneration: req.PlacementGeneration,
		},
		CallerTenant:    deriveCallerTenant(req.Actor),
		TargetTenant:    req.Tenant,
		ResourceKind:    GrantResourceJob,
		Resource:        req.JobPath,
		IntentKeyScheme: scheme,
	})
	if err != nil {
		return FenceResult{}, err
	}
	return FenceResult{Allow: result.Allow, ReasonCode: result.ReasonCode, Message: result.Message}, nil
}

// scanTenantPlacementFenceRow scans one tenantPlacementFenceQuery row
// from either a pool or a transaction. It answers exists=false, with no
// error, when no row is stored yet.
func scanTenantPlacementFenceRow(row pgx.Row) (tenantPlacementFenceRow, bool, error) {
	var (
		out         tenantPlacementFenceRow
		pendingRaw  json.RawMessage
		membersRaw  json.RawMessage
		jobHomesRaw json.RawMessage
	)
	err := row.Scan(&out.namespace, &out.cell, &out.generation, &out.cellInstance,
		&out.rehydratedInstance, &pendingRaw, &membersRaw, &jobHomesRaw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return tenantPlacementFenceRow{}, false, nil
	case err != nil:
		return tenantPlacementFenceRow{}, false, fmt.Errorf("read tenant placement: %w", err)
	}
	if len(pendingRaw) > 0 {
		if err := json.Unmarshal(pendingRaw, &out.pendingJobs); err != nil {
			return tenantPlacementFenceRow{}, false, fmt.Errorf("decode pending jobs: %w", err)
		}
	}
	out.members, err = decodePlacementMembers(membersRaw, jobHomesRaw, placementScalars{
		cell: out.cell, namespace: out.namespace, cellInstance: out.cellInstance,
		rehydratedInstance: out.rehydratedInstance, pendingJobs: out.pendingJobs,
	})
	if err != nil {
		return tenantPlacementFenceRow{}, false, err
	}
	return out, true, nil
}

// decidePlacement applies the placement-fencing capability's
// twelve-order decision table to one claim. It is pure, so every order is
// testable with no database. row and exists describe the stored
// placement; exists is false when no row is stored for the claim's
// cluster and tenant.
//
// The fence resolves the claiming member and the job's home member, then
// compares. Orders 2 and 3 are the permanent identity test: a cell the
// tenant has left is no member of the row. Every request that reaches
// order 7 therefore comes from a current member whose gateway resolver
// cache is stale, and the retryable hold is correct for it.
func decidePlacement(row tenantPlacementFenceRow, exists bool, req ClaimRequest) FenceResult {
	if !exists {
		// Order 1: a missing row is recoverable, and holding no
		// exemption is the epic finding the fence change fixes.
		return FenceResult{Allow: false, ReasonCode: ReasonNotReady,
			Message: fmt.Sprintf("no placement row for tenant %s", req.Tenant)}
	}

	claimer, isMember := memberForCell(row.members, req.Namespace, req.Cell)
	home := memberForJob(row.members, req.JobPath)
	homeSlot := homeSlotForJob(row.members, req.JobPath)

	switch {
	case !hasMemberInNamespace(row.members, req.Namespace):
		// Order 2: no member of the tenant sits in this namespace.
		return FenceResult{Allow: false, ReasonCode: ReasonFenced,
			Message: fmt.Sprintf("cell %s is not the placement of tenant %s", req.Cell, req.Tenant)}

	case !isMember:
		// Order 3: the namespace holds a member and this cell is not
		// one. The gateway resolver caches a tenant for 30 seconds, so
		// a claim can carry a stale cell after a move or a scale.
		return FenceResult{Allow: false, ReasonCode: ReasonFenced,
			Message: fmt.Sprintf("cell %s is not the placement of tenant %s", req.Cell, req.Tenant)}

	case !instanceIsAnnounced(row.members, req.CellInstance):
		// Order 4: Kubernetes replaces a pod on an eviction or a node
		// failure with no generation change, so an instance the row
		// does not name is usually the new pod. The answer is the
		// retryable hold either way.
		return FenceResult{Allow: false, ReasonCode: ReasonNotReady,
			Message: fmt.Sprintf("cell instance %s is not announced for tenant %s", req.CellInstance, req.Tenant)}

	case claimer.Role == MemberRoleStandby:
		// Order 5: a standby holds the folder and runs no build.
		return notHomeResult(req, homeSlot)

	case !sameSlot(claimer, home):
		// Order 6: an active slot that is not this job's home slot.
		return notHomeResult(req, homeSlot)

	case req.PlacementGeneration < row.generation, req.PlacementGeneration > row.generation:
		// Orders 7 and 8: the request's generation does not match the
		// stored one, in either direction. The claimer is a current
		// member, so the hold is retryable: its next claim carries the
		// current generation.
		return FenceResult{Allow: false, ReasonCode: ReasonNotReady,
			Message: fmt.Sprintf("placement generation %d is stale for tenant %s", req.PlacementGeneration, req.Tenant)}

	case isCrossTenantActor(req.Actor, req.Tenant):
		// Order 9: a cross-tenant claim is a permanent answer, so it
		// runs before the two retryable seed orders below.
		return FenceResult{Allow: false, ReasonCode: ReasonCrossTenant, Message: crossTenantMessage}

	case home.RehydratedInstance != req.CellInstance:
		// Order 10: the home member's rehydration seed is not applied
		// on the claiming cell instance yet.
		return FenceResult{Allow: false, ReasonCode: ReasonNotReady,
			Message: fmt.Sprintf("rehydration seed is not applied for job %s", req.JobPath)}

	case containsJobPath(home.PendingJobs, req.JobPath):
		// Order 11: this one job is still deferred on its home member,
		// so one member's deferred job never holds another's build.
		return FenceResult{Allow: false, ReasonCode: ReasonNotReady,
			Message: fmt.Sprintf("rehydration seed is not applied for job %s", req.JobPath)}

	default:
		// Order 12: every comparison passed.
		return FenceResult{Allow: true}
	}
}

// notHomeResult answers the NOT_HOME veto of orders 5 and 6. It names
// the job's home slot, because the claimer's own home map can lack a new
// job, and the claimer then forwards to the slot the veto names.
func notHomeResult(req ClaimRequest, homeSlot int32) FenceResult {
	return FenceResult{Allow: false, ReasonCode: ReasonNotHome,
		Message:  fmt.Sprintf("cell %s is not the home of job %s; home slot is %d", req.Cell, req.JobPath, homeSlot),
		HomeSlot: &homeSlot}
}

// instanceIsAnnounced reports whether any member of the row names
// instance. An empty instance is announced only by a member that names no
// instance either, which is the shape a controller with no running pod
// carries.
func instanceIsAnnounced(row placementMembers, instance string) bool {
	for _, m := range row.members {
		if m.Instance == instance {
			return true
		}
	}
	return false
}

// sameSlot reports whether the claiming member holds the job's home slot.
// A home member with no slot is the zero member of an unbound slot, so
// the answer is false and the claim fails closed.
func sameSlot(claimer, home member) bool {
	if claimer.Slot == nil || home.Slot == nil {
		return false
	}
	return *claimer.Slot == *home.Slot
}

// deriveCallerTenant derives the tenant-export-grant capability's
// caller_tenant from a claim's actor: svc-<name> gives <name>, and any
// other actor gives the empty string.
func deriveCallerTenant(actor string) string {
	name, ok := strings.CutPrefix(actor, crossTenantActorPrefix)
	if !ok {
		return ""
	}
	return name
}

// isCrossTenantActor implements the cross-tenant-veto capability's test:
// actor matches svc-<name> and <name> differs from tenant. An actor that
// does not carry the prefix passes.
func isCrossTenantActor(actor, tenant string) bool {
	name, ok := strings.CutPrefix(actor, crossTenantActorPrefix)
	if !ok {
		return false
	}
	return name != tenant
}

// containsJobPath reports whether jobPath is one of paths.
func containsJobPath(paths []string, jobPath string) bool {
	for _, p := range paths {
		if p == jobPath {
			return true
		}
	}
	return false
}
