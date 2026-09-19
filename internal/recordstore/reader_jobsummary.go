package recordstore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// rowQuerier is the subset of *pgxpool.Pool and pgx.Tx that a grouped
// job-summary query needs. jobSummaries passes a read-only transaction
// through this interface, so all three queries see one snapshot.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ListJobSummaries lists every job under cluster and tenant that has at
// least one run. A job the catalog knows with no run does not appear;
// the pipelines API supplies that job from the catalog instead.
// MedianQueueWaitMs counts runs created at or after since only.
func (r *pgReader) ListJobSummaries(ctx context.Context, cluster, tenant string, since time.Time) ([]JobSummary, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	byJobPath, err := r.jobSummaries(ctx, cluster, tenant, "")
	if err != nil {
		return nil, err
	}
	if err := addJobQueueWaitMedians(ctx, r.pool, cluster, tenant, since, byJobPath); err != nil {
		return nil, err
	}
	summaries := make([]JobSummary, 0, len(byJobPath))
	for _, s := range byJobPath {
		summaries = append(summaries, s)
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].JobPath < summaries[j].JobPath })
	return summaries, nil
}

// GetJobSummary summarizes one job's run history. It returns a
// JobSummary with no result counts and a nil LastRun when jobPath has
// never run.
func (r *pgReader) GetJobSummary(ctx context.Context, cluster, tenant, jobPath string) (JobSummary, error) {
	if err := r.checkSchema(ctx); err != nil {
		return JobSummary{}, err
	}
	byJobPath, err := r.jobSummaries(ctx, cluster, tenant, jobPath)
	if err != nil {
		return JobSummary{}, err
	}
	if s, ok := byJobPath[jobPath]; ok {
		return s, nil
	}
	return JobSummary{JobPath: jobPath}, nil
}

// jobSummaries computes RecentResults, LastRun, and DurationTrendMs for
// every job under cluster and tenant in three grouped queries, so the
// query count stays fixed no matter how many jobs a tenant has. A
// non-empty jobPath narrows every query to that one job. All three
// queries run inside one read-only transaction, so a run written
// between them cannot appear in one query's result and not another's.
func (r *pgReader) jobSummaries(ctx context.Context, cluster, tenant, jobPath string) (map[string]JobSummary, error) {
	args := []any{cluster, tenant}
	jobFilter := ""
	if jobPath != "" {
		args = append(args, jobPath)
		jobFilter = fmt.Sprintf("AND job_path = $%d", len(args))
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin job summary transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	summaries := map[string]JobSummary{}
	getOrCreate := func(path string) JobSummary {
		if s, ok := summaries[path]; ok {
			return s
		}
		return JobSummary{JobPath: path}
	}

	if err := addJobRecentResults(ctx, tx, args, jobFilter, summaries, getOrCreate); err != nil {
		return nil, err
	}
	if err := addJobLastRuns(ctx, tx, args, jobFilter, summaries, getOrCreate); err != nil {
		return nil, err
	}
	if err := addJobDurationTrends(ctx, tx, args, jobFilter, summaries, getOrCreate); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit job summary transaction: %w", err)
	}
	return summaries, nil
}

// addJobRecentResults fills RecentResults with each job's newest
// jobHealthRecentResultsSize finished runs' results, newest first. The
// lateral subquery runs once per distinct job_path, so the query count
// stays fixed no matter how many runs a job has, and a run with no
// result yet (QUEUED or RUNNING) never enters it.
//
// A row that records no build can still pass the result-is-not-null
// filter — a TRIGGER_FAILED row always does — so the lateral subquery
// needs its own exclusion. Without it a mixed job shows a false fail
// dot.
func addJobRecentResults(
	ctx context.Context, db rowQuerier, args []any, jobFilter string, summaries map[string]JobSummary, getOrCreate func(string) JobSummary,
) error {
	recentArgs := append(append([]any{}, args...), jobHealthRecentResultsSize)
	sizeParam := len(recentArgs)
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT jobs.job_path, recent.result
		FROM (
			SELECT DISTINCT job_path
			FROM varroa.runs
			WHERE cluster = $1 AND tenant = $2 %[1]s
		) jobs
		CROSS JOIN LATERAL (
			SELECT result, created_at, run_id
			FROM varroa.runs r
			WHERE r.cluster = $1 AND r.tenant = $2 AND r.job_path = jobs.job_path AND r.result IS NOT NULL
			  AND `+recordsBuildRunSQL+`
			ORDER BY r.created_at DESC, r.run_id DESC
			LIMIT $%[2]d
		) recent
		ORDER BY jobs.job_path, recent.created_at DESC, recent.run_id DESC
	`, jobFilter, sizeParam), recentArgs...)
	if err != nil {
		return fmt.Errorf("list job recent results: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var path, result string
		if err := rows.Scan(&path, &result); err != nil {
			return fmt.Errorf("scan job recent result: %w", err)
		}
		s := getOrCreate(path)
		s.RecentResults = append(s.RecentResults, result)
		summaries[path] = s
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list job recent results: %w", err)
	}
	return nil
}

// addJobLastRuns fills LastRun with the newest run of every job_path, in
// one query keyed by DISTINCT ON (job_path).
func addJobLastRuns(
	ctx context.Context, db rowQuerier, args []any, jobFilter string, summaries map[string]JobSummary, getOrCreate func(string) JobSummary,
) error {
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT DISTINCT ON (job_path) `+runColumns+`
		FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND `+recordsBuildSQL+` %s
		ORDER BY job_path, created_at DESC, run_id DESC
	`, jobFilter), args...)
	if err != nil {
		return fmt.Errorf("list last job runs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return fmt.Errorf("scan last job run: %w", err)
		}
		s := getOrCreate(run.JobPath)
		runCopy := run
		s.LastRun = &runCopy
		summaries[run.JobPath] = s
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list last job runs: %w", err)
	}
	return nil
}

// addJobDurationTrends fills DurationTrendMs with each job's most recent
// jobSummaryTrendSize runs, newest first, skipping a run with no
// duration yet. It ranks every run of a job before it drops the
// null-duration ones, so a run of the job that has not finished yet
// still occupies its rank and the trend never reaches past
// jobSummaryTrendSize runs into older history to fill in a duration.
//
// The no-build-recorded exclusion belongs to the inner ranking subquery.
// A failed trigger or an accepted-only row must consume no rank before
// the outer clause drops it.
func addJobDurationTrends(
	ctx context.Context, db rowQuerier, args []any, jobFilter string, summaries map[string]JobSummary, getOrCreate func(string) JobSummary,
) error {
	trendArgs := append(append([]any{}, args...), jobSummaryTrendSize)
	rows, err := db.Query(ctx, fmt.Sprintf(`
		SELECT job_path, duration_ms
		FROM (
			SELECT job_path, duration_ms,
			       row_number() OVER (PARTITION BY job_path ORDER BY created_at DESC, run_id DESC) AS rn
			FROM varroa.runs
			WHERE cluster = $1 AND tenant = $2 AND `+recordsBuildSQL+` %s
		) ranked
		WHERE rn <= $%d AND duration_ms IS NOT NULL
		ORDER BY job_path, rn
	`, jobFilter, len(trendArgs)), trendArgs...)
	if err != nil {
		return fmt.Errorf("list job duration trend: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var path string
		var durationMs int64
		if err := rows.Scan(&path, &durationMs); err != nil {
			return fmt.Errorf("scan job duration trend: %w", err)
		}
		s := getOrCreate(path)
		s.DurationTrendMs = append(s.DurationTrendMs, durationMs)
		summaries[path] = s
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list job duration trend: %w", err)
	}
	return nil
}

// addJobQueueWaitMedians fills MedianQueueWaitMs for every job of cluster
// and tenant with a run created at or after since that carries both
// queued_at and started_at. It runs one query outside jobSummaries'
// transaction: ListJobSummaries is the only caller, because the job
// detail page's own queue wait comes from JobWindowStats.
func addJobQueueWaitMedians(ctx context.Context, db rowQuerier, cluster, tenant string, since time.Time, summaries map[string]JobSummary) error {
	rows, err := db.Query(ctx, `
		SELECT job_path,
		       ROUND(percentile_cont(0.5) WITHIN GROUP (
		           ORDER BY EXTRACT(EPOCH FROM (started_at - queued_at)) * 1000
		       )::numeric)::bigint AS median_queue_wait_ms
		FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND created_at >= $3 AND `+recordsBuildSQL+`
		  AND queued_at IS NOT NULL AND started_at IS NOT NULL
		GROUP BY job_path
	`, cluster, tenant, since)
	if err != nil {
		return fmt.Errorf("list job queue wait medians: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var medianMs int64
		if err := rows.Scan(&path, &medianMs); err != nil {
			return fmt.Errorf("scan job queue wait median: %w", err)
		}
		if s, ok := summaries[path]; ok {
			v := medianMs
			s.MedianQueueWaitMs = &v
			summaries[path] = s
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list job queue wait medians: %w", err)
	}
	return nil
}
