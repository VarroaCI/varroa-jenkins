package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// crossTenantBuildSeqPattern matches design.md D6's build-kind seq
// segment: a lowercase "s" and a 1-based ordinal with no leading zero.
var crossTenantBuildSeqPattern = regexp.MustCompile(`^s[1-9][0-9]*$`)

// handleCrossTenantBuild implements the build kind: design.md D6's
// identity reservation. req.JobPath arrives folder relative to
// req.TargetTenant, the documented wire form; this handler rejects a
// malformed seq, then qualifies the job path once, before the grant
// check and before the published action carries it. It runs the
// request-path grant, then reserves the run identifier against the
// ledger. It calls no identifier-deriving function; C4 withdrew it. It
// writes no run_intents row of its own: the target cell's claim gate
// creates the one row, so this responder only reads run_intents and
// publishes a trigger_run action.
//
// The kind also serves an intra-tenant forward, whose caller tenant
// equals its target tenant. Such a request may carry its own intent key,
// and the responder then derives none.
func handleCrossTenantBuild(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	if !crossTenantBuildSeqPattern.MatchString(req.Seq) {
		return CrossTenantResponse{Code: CodeInternal,
			Error: fmt.Sprintf("seq %q must match s<n>, a 1-based ordinal", req.Seq)}, nil
	}

	suppliedKey, resp, ok := crossTenantSuppliedKey(req)
	if !ok {
		return resp, nil
	}

	qualifiedJobPath := qualifyCrossTenantJobPath(req.TargetTenant, req.JobPath)

	grant, err := deps.Evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			Path:                GrantPathRequest,
			Cluster:             req.Cluster,
			Namespace:           req.Namespace,
			Cell:                req.Cell,
			Tenant:              req.CallerTenant,
			PlacementGeneration: req.PlacementGeneration,
		},
		CallerTenant:    req.CallerTenant,
		TargetTenant:    req.TargetTenant,
		ResourceKind:    GrantResourceJob,
		Resource:        qualifiedJobPath,
		IntentKeyScheme: IntentSchemeUpstream,
	})
	if err != nil {
		return CrossTenantResponse{}, err
	}
	if !grant.Allow {
		return CrossTenantResponse{ReasonCode: grant.ReasonCode, Message: grant.Message}, nil
	}

	intentKey := suppliedKey
	if intentKey == "" {
		intentKey = fmt.Sprintf("%s:%s:%s", IntentSchemeUpstream, req.CallerRunID, req.Seq)
	}
	intent, found, err := deps.Reader.GetIntentByKey(ctx, intentKey)
	if err != nil {
		return CrossTenantResponse{}, err
	}
	if found {
		rec, recFound, err := readCrossTenantRunRecord(ctx, deps.Pool, intent.RunID)
		if err != nil {
			return CrossTenantResponse{}, err
		}
		resp := CrossTenantResponse{RunID: intent.RunID}
		if recFound {
			resp.State = crossTenantRunState(rec)
			resp.BuildNumber = rec.buildNumber
		} else {
			resp.State = CrossTenantStateQueued
		}
		return resp, nil
	}

	runID, err := uuid.NewV7()
	if err != nil {
		return CrossTenantResponse{}, fmt.Errorf("mint reserved run id: %w", err)
	}

	target, err := resolveCrossTenantTargetPlacement(ctx, deps.Pool, req.Cluster, req.TargetTenant, qualifiedJobPath)
	if err != nil {
		return CrossTenantResponse{}, err
	}
	if target.cell == "" {
		// The target job's home slot holds no cell yet. The caller
		// retries and the recorder drops nothing.
		return CrossTenantResponse{State: CrossTenantStatePending}, nil
	}

	action := LedgerAction{
		Kind:       LedgerActionTriggerRun,
		Cluster:    target.cluster,
		Namespace:  target.namespace,
		Controller: target.cell,
		Actor:      crossTenantActorPrefix + req.CallerTenant,
		RunID:      runID.String(),
		IntentKey:  intentKey,
		JobPath:    qualifiedJobPath,
		Parameters: req.Params,
	}
	if err := PublishLedgerAction(deps.Conn, action); err != nil {
		return CrossTenantResponse{}, err
	}

	return CrossTenantResponse{State: CrossTenantStatePending}, nil
}

// crossTenantSuppliedKey answers the intent key the caller supplied on a
// build request, and the refusal to send when the request may not carry
// one. An empty key with ok true means the responder derives its own.
//
// The responder reads a supplied key only from the caller's own tenant.
// An intra-tenant forward must preserve the claimer's key, so a cron fire
// on two members reaches one ledger row. A cross-tenant caller may not
// supply one: the grant's intent-key-scheme order tests a key the
// recorder derived, and a caller-supplied key would meet that order under
// the caller's control.
func crossTenantSuppliedKey(req CrossTenantRequest) (string, CrossTenantResponse, bool) {
	if req.IntentKey == "" {
		return "", CrossTenantResponse{}, true
	}
	if req.CallerTenant != req.TargetTenant {
		return "", CrossTenantResponse{Code: CodeInternal,
			Error: "intent_key is allowed only when the caller tenant equals the target tenant"}, false
	}
	if _, err := ParseIntentKey(req.IntentKey); err != nil {
		return "", CrossTenantResponse{ReasonCode: ReasonInvalidKey, Message: err.Error()}, false
	}
	return req.IntentKey, CrossTenantResponse{}, true
}

// crossTenantTargetPlacement is the subset of a target tenant's
// tenant_placements row the trigger_run publisher needs to route the
// action.
type crossTenantTargetPlacement struct {
	cluster   string
	namespace string
	cell      string
}

// resolveCrossTenantTargetPlacement answers target's tenant_placements
// row, preferring callerCluster's own row when one exists there, and
// the lone fleet-wide row otherwise. GrantEvaluator.Check has already
// vetoed the ambiguous case, where two or more rows exist and neither
// is the caller's own, before this function ever runs, so exactly one
// candidate is always resolvable here. This mirrors
// GrantEvaluator.resolveTargetPolicy's own resolution, duplicated
// because that method is unexported in a file this batch does not
// touch.
//
// The namespace and the cell name the member that holds targetJobPath's
// home slot, so a trigger for a job homed on slot 1 reaches slot 1's
// holder. An empty targetJobPath resolves slot 0, which is what a lock
// call carries: a fleet lock belongs to the tenant and not to one job.
func resolveCrossTenantTargetPlacement(ctx context.Context, pool *pgxpool.Pool, callerCluster, target,
	targetJobPath string) (crossTenantTargetPlacement, error) {
	rows, err := pool.Query(ctx, `
		SELECT cluster, namespace, cell, cell_instance, rehydrated_instance, pending_jobs,
		       members, job_homes
		FROM varroa.tenant_placements WHERE tenant = $1
	`, target)
	if err != nil {
		return crossTenantTargetPlacement{}, fmt.Errorf("select tenant placements for %s: %w", target, err)
	}
	defer rows.Close()

	var (
		all     []crossTenantTargetPlacement
		matched crossTenantTargetPlacement
		found   bool
	)
	for rows.Next() {
		p, err := scanCrossTenantTargetPlacement(rows, targetJobPath)
		if err != nil {
			return crossTenantTargetPlacement{}, fmt.Errorf("scan tenant placement for %s: %w", target, err)
		}
		all = append(all, p)
		if p.cluster == callerCluster {
			matched, found = p, true
		}
	}
	if err := rows.Err(); err != nil {
		return crossTenantTargetPlacement{}, fmt.Errorf("select tenant placements for %s: %w", target, err)
	}

	if found {
		return matched, nil
	}
	if len(all) == 0 {
		return crossTenantTargetPlacement{}, fmt.Errorf("no tenant placement for %s", target)
	}
	return all[0], nil
}

// scanCrossTenantTargetPlacement reads one placement row and resolves the
// member that holds targetJobPath's home slot.
func scanCrossTenantTargetPlacement(row pgx.Row, targetJobPath string) (crossTenantTargetPlacement, error) {
	var (
		cluster     string
		namespace   string
		cell        string
		instance    string
		rehydrated  string
		pendingRaw  json.RawMessage
		membersRaw  json.RawMessage
		jobHomesRaw json.RawMessage
	)
	if err := row.Scan(&cluster, &namespace, &cell, &instance, &rehydrated, &pendingRaw,
		&membersRaw, &jobHomesRaw); err != nil {
		return crossTenantTargetPlacement{}, err
	}
	var pending []string
	if len(pendingRaw) > 0 {
		if err := json.Unmarshal(pendingRaw, &pending); err != nil {
			return crossTenantTargetPlacement{}, fmt.Errorf("decode pending jobs: %w", err)
		}
	}
	decoded, err := decodePlacementMembers(membersRaw, jobHomesRaw, placementScalars{
		cell: cell, namespace: namespace, cellInstance: instance,
		rehydratedInstance: rehydrated, pendingJobs: pending,
	})
	if err != nil {
		return crossTenantTargetPlacement{}, err
	}
	home := memberForJob(decoded, targetJobPath)
	return crossTenantTargetPlacement{cluster: cluster, namespace: home.Namespace, cell: home.Cell}, nil
}
