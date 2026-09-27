package recordstore

import (
	"context"
	"fmt"
)

// handleCrossTenantStatus implements the status kind. The request names
// exactly one of run_id or intent_key. It resolves the identifier to a
// run_intents row, reads that row's tenant and job path, then runs the
// request-path grant against them: the grant needs the job path to
// evaluate the export glob. A run that failed to trigger holds no
// run_intents row, so the identifier then resolves to its TRIGGER_FAILED
// runs row, and the same grant runs against that row's tenant and job
// path.
func handleCrossTenantStatus(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	if (req.RunID == "") == (req.IntentKey == "") {
		return CrossTenantResponse{Code: CodeInternal,
			Error: "a status request must name exactly one of run_id or intent_key"}, nil
	}

	var (
		runID string
		rec   crossTenantRunRecord
		found bool
		err   error
	)
	if req.IntentKey != "" {
		runID, rec, found, err = readCrossTenantRun(ctx, deps.Pool, "intent_key", req.IntentKey)
		if err != nil {
			return CrossTenantResponse{}, err
		}
		if !found {
			return CrossTenantResponse{State: CrossTenantStatePending}, nil
		}
	} else {
		runID = req.RunID
		rec, found, err = readCrossTenantRunRecord(ctx, deps.Pool, runID)
		if err != nil {
			return CrossTenantResponse{}, err
		}
		if !found {
			return CrossTenantResponse{Code: CodeNotFound, Error: fmt.Sprintf("unknown run_id %q", runID)}, nil
		}
	}

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
		TargetTenant:    rec.tenant,
		ResourceKind:    GrantResourceJob,
		Resource:        rec.jobPath,
		IntentKeyScheme: IntentSchemeUpstream,
	})
	if err != nil {
		return CrossTenantResponse{}, err
	}
	if !grant.Allow {
		return CrossTenantResponse{ReasonCode: grant.ReasonCode, Message: grant.Message}, nil
	}

	return CrossTenantResponse{
		RunID:             runID,
		State:             crossTenantRunState(rec),
		Result:            rec.result,
		BuildNumber:       rec.buildNumber,
		JobPath:           rec.jobPath,
		URL:               crossTenantRunURL(rec.tenant, rec.jobPath, runID),
		TriggerReasonCode: rec.triggerReasonCode,
		TriggerError:      rec.triggerError,
	}, nil
}

// handleCrossTenantLastSuccessful implements the last_successful kind.
// It names a target tenant and a job path directly, with no run
// identifier. req.JobPath arrives folder relative to targetTenant, the
// documented wire form; this handler qualifies it once, before the
// grant check and before the run lookup, to match the qualified form
// runs.job_path stores.
func handleCrossTenantLastSuccessful(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
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

	target, err := resolveCrossTenantTargetPlacement(ctx, deps.Pool, req.Cluster, req.TargetTenant, qualifiedJobPath)
	if err != nil {
		return CrossTenantResponse{}, err
	}

	run, found, err := readCrossTenantLastSuccessfulRun(ctx, deps.Pool, target.cluster, req.TargetTenant, qualifiedJobPath)
	if err != nil {
		return CrossTenantResponse{}, err
	}
	if !found {
		return CrossTenantResponse{}, nil
	}

	return CrossTenantResponse{
		RunID:       run.runID,
		BuildNumber: run.buildNumber,
		Result:      run.result,
		StartedAt:   run.startedAt,
		EndedAt:     run.endedAt,
		URL:         crossTenantRunURL(req.TargetTenant, qualifiedJobPath, run.runID),
	}, nil
}
