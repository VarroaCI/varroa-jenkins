// Package recordstore: the cross-tenant dependency edge, design D13.
// This file reads varroa.job_schedules' three upstream columns
// (migration 00007) and fills each edge's grant verdict from
// GrantEvaluator, on the fanout path GrantEvaluator already defines.
package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// varroaUpstreamTriggerType is the job_schedules trigger_type value a
// varroaUpstream declaration reports.
const varroaUpstreamTriggerType = "varroa-upstream"

// DependencyEdge is one cross-tenant dependency edge on the pipelines
// job detail page. Job is the job's name, relative to the edge tenant's
// own folder. It is a display name, never a route. JobPath is the job's
// path with its tenant folder, the path every pipelines read and route
// uses. Granted is false, never omitted, when the producer's exports do
// not cover the edge.
type DependencyEdge struct {
	Tenant  string
	Job     string
	JobPath string
	Result  string
	Granted bool
}

// JobDependencies is a job's upstream and downstream cross-tenant
// dependency edges. Upstream holds the job's own varroa-upstream
// trigger row, at most one per design D9's one-trigger-per-job rule.
// Downstream holds every other job's varroa-upstream row that names
// this job as its producer. Both are empty, never nil, for a job with
// no edge.
type JobDependencies struct {
	Upstream   []DependencyEdge
	Downstream []DependencyEdge
}

// jobDependenciesUpstreamQuery reads this job's own varroa-upstream
// trigger row, when it declares one. The primary key
// (cluster, tenant, job_path, trigger_type) answers at most one row.
const jobDependenciesUpstreamQuery = `
	SELECT upstream_tenant, upstream_job, upstream_result
	FROM varroa.job_schedules
	WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND trigger_type = $4
`

// jobDependenciesDownstreamQuery reads every job_schedules row, across
// every cluster, whose upstream_tenant and upstream_job name this job
// as their producer, so a subscriber in another cluster still appears.
// It selects the subscriber's own cluster: a downstream grant check
// resolves the subscriber's placement, never the producer's.
const jobDependenciesDownstreamQuery = `
	SELECT cluster, tenant, job_path, upstream_result
	FROM varroa.job_schedules
	WHERE upstream_tenant = $1 AND upstream_job = $2 AND trigger_type = $3
	ORDER BY tenant, job_path
`

// JobDependencies implements Reader. cluster and tenant identify
// jobPath's own placement; jobPath carries tenant's folder prefix, the
// same convention every other pipelines read uses.
func (r *pgReader) JobDependencies(ctx context.Context, cluster, tenant, jobPath string) (JobDependencies, error) {
	if err := r.checkSchema(ctx); err != nil {
		return JobDependencies{}, err
	}
	evaluator := NewGrantEvaluator(r.pool)

	upstream, err := r.jobDependenciesUpstream(ctx, evaluator, cluster, tenant, jobPath)
	if err != nil {
		return JobDependencies{}, err
	}
	downstream, err := r.jobDependenciesDownstream(ctx, evaluator, tenant, jobPath)
	if err != nil {
		return JobDependencies{}, err
	}
	return JobDependencies{Upstream: upstream, Downstream: downstream}, nil
}

// jobDependenciesUpstream reads jobPath's own varroa-upstream row and
// checks the fanout-path grant against the producer it names. The
// resource a job-kind grant check matches is the producer's own
// tenant-qualified job path, so this builds it from the row's two
// columns.
func (r *pgReader) jobDependenciesUpstream(ctx context.Context, evaluator *GrantEvaluator, cluster, tenant, jobPath string) ([]DependencyEdge, error) {
	edges := make([]DependencyEdge, 0, 1)

	var upstreamTenant, upstreamJob, upstreamResult string
	err := r.pool.QueryRow(ctx, jobDependenciesUpstreamQuery, cluster, tenant, jobPath, varroaUpstreamTriggerType).
		Scan(&upstreamTenant, &upstreamJob, &upstreamResult)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return edges, nil
	case err != nil:
		return nil, fmt.Errorf("read job_schedules upstream row for %s/%s: %w", tenant, jobPath, err)
	}

	producerPath := upstreamTenant + "/" + upstreamJob
	result, err := evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			Path:    GrantPathFanout,
			Cluster: cluster,
			Tenant:  tenant,
		},
		CallerTenant:    tenant,
		TargetTenant:    upstreamTenant,
		ResourceKind:    GrantResourceJob,
		Resource:        producerPath,
		IntentKeyScheme: IntentSchemeUpstream,
	})
	if err != nil {
		return nil, fmt.Errorf("check upstream grant for %s/%s: %w", tenant, jobPath, err)
	}
	return append(edges, DependencyEdge{Tenant: upstreamTenant, Job: upstreamJob, JobPath: producerPath, Result: upstreamResult, Granted: result.Allow}), nil
}

// jobDependenciesDownstream reads every varroa-upstream row naming
// (tenant, jobPath) as its producer, and checks the fanout-path grant
// for each subscriber found. The resource a job-kind grant check
// matches is the producer's own tenant-qualified job path, which
// jobPath already is. Each grant check resolves the subscriber's own
// placement, never the producer's cluster: a subscriber commonly lives
// in another cluster than the producer.
func (r *pgReader) jobDependenciesDownstream(ctx context.Context, evaluator *GrantEvaluator, tenant, jobPath string) ([]DependencyEdge, error) {
	relativeJob, ok := stripTenantPrefix(tenant, jobPath)
	if !ok {
		relativeJob = jobPath
	}

	rows, err := r.pool.Query(ctx, jobDependenciesDownstreamQuery, tenant, relativeJob, varroaUpstreamTriggerType)
	if err != nil {
		return nil, fmt.Errorf("list job_schedules downstream rows for %s/%s: %w", tenant, jobPath, err)
	}
	defer rows.Close()

	type subscriberRow struct {
		cluster, tenant, jobPath, result string
	}
	var subscribers []subscriberRow
	for rows.Next() {
		var sub subscriberRow
		if err := rows.Scan(&sub.cluster, &sub.tenant, &sub.jobPath, &sub.result); err != nil {
			return nil, fmt.Errorf("scan job_schedules downstream row for %s/%s: %w", tenant, jobPath, err)
		}
		subscribers = append(subscribers, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list job_schedules downstream rows for %s/%s: %w", tenant, jobPath, err)
	}

	edges := make([]DependencyEdge, 0, len(subscribers))
	for _, sub := range subscribers {
		result, err := evaluator.Check(ctx, GrantInput{
			PlacementInput: PlacementInput{
				Path:    GrantPathFanout,
				Cluster: sub.cluster,
				Tenant:  sub.tenant,
			},
			CallerTenant:    sub.tenant,
			TargetTenant:    tenant,
			ResourceKind:    GrantResourceJob,
			Resource:        jobPath,
			IntentKeyScheme: IntentSchemeUpstream,
		})
		if err != nil {
			return nil, fmt.Errorf("check downstream grant for %s/%s: %w", sub.tenant, sub.jobPath, err)
		}
		subJob, ok := stripTenantPrefix(sub.tenant, sub.jobPath)
		if !ok {
			subJob = sub.jobPath
		}
		edges = append(edges, DependencyEdge{Tenant: sub.tenant, Job: subJob, JobPath: sub.jobPath, Result: sub.result, Granted: result.Allow})
	}
	return edges, nil
}
