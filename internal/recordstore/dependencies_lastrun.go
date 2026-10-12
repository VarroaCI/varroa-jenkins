package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lastRunWindow bounds how far back a dependency edge looks for its last
// linked run. An older run does not describe the edge's current state.
const lastRunWindow = 30 * 24 * time.Hour

// lastUpstreamRunSQL reads the newest run of a subscriber job that a
// given producer job started. It answers the upstream columns of that
// run, which name the upstream run itself.
const lastUpstreamRunSQL = `
	SELECT upstream_run_id::text, upstream_tenant, upstream_job, upstream_number
	FROM varroa.runs
	WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	  AND upstream_tenant = $4 AND upstream_job = $5 AND created_at >= $6
	ORDER BY created_at DESC, run_id DESC
	LIMIT 1
`

// lastDownstreamRunSQL reads the newest run of a subscriber job that a
// given producer job started, with its own status and result.
const lastDownstreamRunSQL = `
	SELECT run_id::text, tenant, job_path, build_number, status, COALESCE(result, '')
	FROM varroa.runs
	WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	  AND upstream_tenant = $4 AND upstream_job = $5 AND created_at >= $6
	ORDER BY created_at DESC, run_id DESC
	LIMIT 1
`

// lastUpstreamRun answers the upstream run that last started the job
// (cluster, tenant, jobPath) from the producer job upJob of upTenant. It
// answers nil when no such run began within the window. A run whose
// upstream run matched no row keeps its tenant and job, with an empty
// run id.
func lastUpstreamRun(ctx context.Context, pool *pgxpool.Pool, cluster, tenant, jobPath, upTenant, upJob string, since time.Time) (*RunLinkRow, error) {
	var (
		runID    *string
		tenantOf *string
		jobOf    *string
		number   *int64
	)
	err := pool.QueryRow(ctx, lastUpstreamRunSQL, cluster, tenant, jobPath, upTenant, upJob, since).
		Scan(&runID, &tenantOf, &jobOf, &number)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read last upstream run for %s/%s: %w", tenant, jobPath, err)
	}
	link := &RunLinkRow{BuildNumber: number}
	if runID != nil {
		link.RunID = *runID
	}
	if tenantOf != nil {
		link.Tenant = *tenantOf
	}
	if jobOf != nil {
		link.JobPath = *jobOf
	}
	return link, nil
}

// lastDownstreamRun answers the newest run of the subscriber job
// (cluster, tenant, jobPath) that the producer job upJob of upTenant
// started. It answers nil when no such run began within the window.
func lastDownstreamRun(ctx context.Context, pool *pgxpool.Pool, cluster, tenant, jobPath, upTenant, upJob string, since time.Time) (*RunLinkRow, error) {
	var link RunLinkRow
	err := pool.QueryRow(ctx, lastDownstreamRunSQL, cluster, tenant, jobPath, upTenant, upJob, since).
		Scan(&link.RunID, &link.Tenant, &link.JobPath, &link.BuildNumber, &link.Status, &link.Result)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read last downstream run for %s/%s: %w", tenant, jobPath, err)
	}
	return &link, nil
}
