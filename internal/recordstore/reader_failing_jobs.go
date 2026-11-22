package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// FailingJobsWindow bounds the failed run of a failing job. A job whose
// newest counted run failed longer ago than this does not count as failing.
const FailingJobsWindow = 7 * 24 * time.Hour

// failingJobsDefaultLimit and failingJobsMaxLimit bound FailingJobsQuery.Limit.
const (
	failingJobsDefaultLimit = 20
	failingJobsMaxLimit     = 50
)

// FailingJobsQuery names the scopes of the failing-jobs read. Since bounds the
// created_at of the failed run. Limit <= 0 means failingJobsDefaultLimit, and
// a larger value is clamped to failingJobsMaxLimit.
type FailingJobsQuery struct {
	Scopes []RunScope
	Since  time.Time
	Limit  int
}

// PassedRun is the last passed build run of a job before its failure.
type PassedRun struct {
	RunID       string
	BuildNumber *int64
	CreatedAt   time.Time
	EndedAt     *time.Time
}

// FailingJob is one job whose newest counted build run failed. Failed is that
// run. FailedStages holds its FAILURE stages in stage order. LastPassed is the
// newest passed run before Failed, or nil. FailuresSincePass counts the
// FAILURE runs created after LastPassed, Failed included.
type FailingJob struct {
	Failed            Run
	FailedStages      []Stage
	LastPassed        *PassedRun
	FailuresSincePass int64
}

// FailingJobsPage is one page of FailingJobs. More is true when the read found
// more failing jobs than the limit.
type FailingJobsPage struct {
	Jobs []FailingJob
	More bool
}

// FailingJobs answers the jobs of the scopes whose newest counted build run
// is a FAILURE created at or after q.Since, newest failure first. It runs three
// statements in one read-only transaction: the candidate jobs, the failed runs,
// and the FAILURE stages of those runs. It issues none for an empty scope list.
func (r *pgReader) FailingJobs(ctx context.Context, q FailingJobsQuery) (FailingJobsPage, error) {
	if len(q.Scopes) == 0 {
		return FailingJobsPage{}, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return FailingJobsPage{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = failingJobsDefaultLimit
	}
	if limit > failingJobsMaxLimit {
		limit = failingJobsMaxLimit
	}
	scopes, err := scopesJSON(q.Scopes)
	if err != nil {
		return FailingJobsPage{}, fmt.Errorf("encode failing-jobs scopes: %w", err)
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return FailingJobsPage{}, fmt.Errorf("begin failing-jobs transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Statement 1: the newest counted run of each candidate job, newest failure first.
	type candidate struct {
		runID       string
		passRunID   *string
		passNumber  *int64
		passCreated *time.Time
		passEnded   *time.Time
		since       int64
	}
	rows, err := tx.Query(ctx, scopesCTE+`,
	jobs AS (
		SELECT DISTINCT r.cluster, r.tenant, r.job_path FROM scopes s
		JOIN varroa.runs r ON r.cluster = s.cluster AND r.tenant = s.tenant
			AND (s.patterns IS NULL OR r.job_path ~ ANY(s.patterns))
		WHERE r.created_at >= $2 AND r.result = 'FAILURE' AND `+historyRunRunSQL+`
	),
	newest AS (
		SELECT j.cluster, j.tenant, j.job_path, n.run_id, n.created_at, n.result
		FROM jobs j CROSS JOIN LATERAL (`+newestCountedRunSQL+`) n
	)
	SELECT n.run_id::text, p.run_id::text, p.build_number, p.created_at, p.ended_at,
		(SELECT count(*) FROM varroa.runs r
			WHERE r.cluster = n.cluster AND r.tenant = n.tenant AND r.job_path = n.job_path
				AND r.result = 'FAILURE' AND `+historyRunRunSQL+`
				AND (p.created_at IS NULL OR r.created_at > p.created_at))
	FROM newest n
	LEFT JOIN LATERAL (
		SELECT r.run_id, r.build_number, r.created_at, r.ended_at FROM varroa.runs r
		WHERE r.cluster = n.cluster AND r.tenant = n.tenant AND r.job_path = n.job_path
			AND r.result = 'SUCCESS' AND r.created_at < n.created_at AND `+historyRunRunSQL+`
		ORDER BY r.created_at DESC, r.run_id DESC LIMIT 1
	) p ON true
	WHERE n.result = 'FAILURE' AND n.created_at >= $2
	ORDER BY n.created_at DESC, n.run_id DESC LIMIT $3`,
		string(scopes), q.Since, limit+1)
	if err != nil {
		return FailingJobsPage{}, fmt.Errorf("list failing jobs: %w", err)
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.runID, &c.passRunID, &c.passNumber, &c.passCreated, &c.passEnded, &c.since); err != nil {
			rows.Close()
			return FailingJobsPage{}, fmt.Errorf("scan failing job: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return FailingJobsPage{}, fmt.Errorf("list failing jobs: %w", err)
	}

	page := FailingJobsPage{}
	if len(cands) > limit {
		page.More = true
		cands = cands[:limit]
	}
	if len(cands) == 0 {
		return page, nil
	}

	failedIDs := make([]string, 0, len(cands))
	for _, c := range cands {
		failedIDs = append(failedIDs, c.runID)
	}

	// Statement 2: the failed runs.
	runRows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM varroa.runs WHERE run_id = ANY($1::uuid[])`, failedIDs)
	if err != nil {
		return FailingJobsPage{}, fmt.Errorf("read failing runs: %w", err)
	}
	runs := map[string]Run{}
	for runRows.Next() {
		run, err := scanRun(runRows)
		if err != nil {
			runRows.Close()
			return FailingJobsPage{}, fmt.Errorf("scan failing run: %w", err)
		}
		runs[run.RunID] = run
	}
	runRows.Close()
	if err := runRows.Err(); err != nil {
		return FailingJobsPage{}, fmt.Errorf("read failing runs: %w", err)
	}

	// Statement 3: the FAILURE stages of the failed runs, in stage order.
	stageRows, err := tx.Query(ctx, `
		SELECT run_id, stage_id, created_at, parent_id, name, node_type, status,
		       started_at, ended_at, duration_ms, error, payload
		FROM varroa.run_stages
		WHERE run_id = ANY($1::uuid[]) AND status = 'FAILURE'
		ORDER BY run_id, started_at ASC NULLS LAST, created_at, length(stage_id), stage_id
	`, failedIDs)
	if err != nil {
		return FailingJobsPage{}, fmt.Errorf("read failing stages: %w", err)
	}
	stages := map[string][]Stage{}
	for stageRows.Next() {
		var s Stage
		var stageRunID string
		if err := stageRows.Scan(&stageRunID, &s.StageID, &s.CreatedAt, &s.ParentID, &s.Name, &s.NodeType,
			&s.Status, &s.StartedAt, &s.EndedAt, &s.DurationMs, &s.Error, &s.Payload); err != nil {
			stageRows.Close()
			return FailingJobsPage{}, fmt.Errorf("scan failing stage: %w", err)
		}
		s.RunID = stageRunID
		stages[stageRunID] = append(stages[stageRunID], s)
	}
	stageRows.Close()
	if err := stageRows.Err(); err != nil {
		return FailingJobsPage{}, fmt.Errorf("read failing stages: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return FailingJobsPage{}, fmt.Errorf("commit failing-jobs transaction: %w", err)
	}

	for _, c := range cands {
		job := FailingJob{
			Failed:            runs[c.runID],
			FailedStages:      stages[c.runID],
			FailuresSincePass: c.since,
		}
		if c.passRunID != nil {
			job.LastPassed = &PassedRun{
				RunID:       *c.passRunID,
				BuildNumber: c.passNumber,
				CreatedAt:   *c.passCreated,
				EndedAt:     c.passEnded,
			}
		}
		page.Jobs = append(page.Jobs, job)
	}
	return page, nil
}
