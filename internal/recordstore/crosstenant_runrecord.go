package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// crossTenantRunRecord is what the cross-tenant responder reads about
// one run to run the grant check and answer a build or a status reply.
// Tenant and JobPath come from run_intents, which always holds them from
// the claim onward. HasRuns, Status, Result, BuildNumber, StartedAt, and
// EndedAt come from runs, which holds nothing until the target cell's
// first run.started or run.heartbeat envelope lands.
type crossTenantRunRecord struct {
	tenant      string
	jobPath     string
	hasRuns     bool
	status      string
	result      *string
	buildNumber *int64
	startedAt   *time.Time
	endedAt     *time.Time
}

// readCrossTenantRunRecord reads runID's tenant and job path from
// run_intents, left-joined onto its runs row so a run the target cell
// has not started yet still answers found=true with hasRuns false. It
// answers found=false, with no error, when no run_intents row names
// runID at all.
//
// The join excludes a TRIGGER_FAILED row, so a run whose only row is
// synthetic reads as queued rather than completed. The operator can
// redeliver one trigger action, so an intent can arrive after a refused
// trigger under the same run identifier.
func readCrossTenantRunRecord(ctx context.Context, pool *pgxpool.Pool, runID string) (crossTenantRunRecord, bool, error) {
	var (
		rec    crossTenantRunRecord
		status *string
	)
	err := pool.QueryRow(ctx, `
		SELECT ri.tenant, ri.job_path, r.status, r.result, r.build_number, r.started_at, r.ended_at
		FROM varroa.run_intents ri
		LEFT JOIN varroa.runs r ON r.run_id = ri.run_id AND `+notTriggerFailedRunSQL+`
		WHERE ri.run_id = $1
	`, runID).Scan(&rec.tenant, &rec.jobPath, &status, &rec.result, &rec.buildNumber, &rec.startedAt, &rec.endedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return crossTenantRunRecord{}, false, nil
	case err != nil:
		return crossTenantRunRecord{}, false, fmt.Errorf("read cross-tenant run record: %w", err)
	}
	if status != nil {
		rec.hasRuns = true
		rec.status = *status
	}
	return rec, true, nil
}

// crossTenantLastSuccessfulRun is what handleCrossTenantLastSuccessful
// reads for one job's newest successful run.
type crossTenantLastSuccessfulRun struct {
	runID       string
	buildNumber *int64
	result      *string
	startedAt   *time.Time
	endedAt     *time.Time
}

// readCrossTenantLastSuccessfulRun answers the newest COMPLETED,
// SUCCESS run of cluster/tenant/jobPath. It answers found=false, with
// no error, when the job holds no successful run.
func readCrossTenantLastSuccessfulRun(ctx context.Context, pool *pgxpool.Pool, cluster, tenant, jobPath string) (crossTenantLastSuccessfulRun, bool, error) {
	var (
		run   crossTenantLastSuccessfulRun
		runID uuid.UUID
	)
	err := pool.QueryRow(ctx, `
		SELECT run_id, build_number, result, started_at, ended_at
		FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
		  AND status = 'COMPLETED' AND result = 'SUCCESS'
		ORDER BY created_at DESC
		LIMIT 1
	`, cluster, tenant, jobPath).Scan(&runID, &run.buildNumber, &run.result, &run.startedAt, &run.endedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return crossTenantLastSuccessfulRun{}, false, nil
	case err != nil:
		return crossTenantLastSuccessfulRun{}, false, fmt.Errorf("read last successful run: %w", err)
	}
	run.runID = runID.String()
	return run, true, nil
}

// crossTenantRunState maps rec onto one of the five cross-tenant run
// states (design.md D1, D8). It never answers "pending": a caller only
// reaches this function once run_intents holds a row, and "pending"
// names the row's absence.
func crossTenantRunState(rec crossTenantRunRecord) string {
	if !rec.hasRuns {
		return CrossTenantStateQueued
	}
	switch rec.status {
	case StatusRunning:
		return CrossTenantStateRunning
	case StatusCompleted:
		if rec.result != nil && *rec.result == "INTERRUPTED" {
			return CrossTenantStateInterrupted
		}
		return CrossTenantStateCompleted
	default:
		return CrossTenantStateQueued
	}
}
