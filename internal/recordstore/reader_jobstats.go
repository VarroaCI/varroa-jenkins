package recordstore

import (
	"context"
	"fmt"
	"time"
)

// StageStat names one stage and its duration, in milliseconds.
type StageStat struct {
	Name       string
	DurationMs int64
}

// JobWindowStats summarizes one job's runs since a point in time, for
// the pipeline job page's "Last 30 days" card. PassRate is nil when the
// window holds no finished run, because a rate over zero runs has no
// value to report. MedianDurationMs is nil for the same reason.
// SlowestStage is nil when no run in the window carries a finished
// stage. MedianQueueWaitMs is nil when no run in the window has both
// queued_at and started_at set.
type JobWindowStats struct {
	Builds            int64
	PassRate          *float64
	MedianDurationMs  *int64
	MedianQueueWaitMs *int64
	SlowestStage      *StageStat
}

// JobWindowStats summarizes jobPath's runs created at or after since.
// Builds counts every run in the window, whatever its status. PassRate
// and MedianDurationMs draw from finished runs only, a run whose
// "result" column is set. A run reaches "result" exactly when Jenkins
// has resolved its outcome, which excludes a queued, running, or
// duplicate-superseded run. PassRate counts a "SUCCESS" result as a
// pass; "UNSTABLE" and every other result count against the rate,
// because the card answers "did the pipeline work", not "did it merely
// finish". SlowestStage compares each stage name's median duration
// across the window's runs, not its single slowest run: a median
// resists one outlier run inflating a stage that usually runs fast.
//
// A run that records no build counts in no value here: a run whose
// result is TRIGGER_FAILED, or a run whose payload still holds
// accepted_at. Neither started a build, so neither is a build outcome.
func (r *pgReader) JobWindowStats(ctx context.Context, cluster, tenant, jobPath string, since time.Time) (JobWindowStats, error) {
	if err := r.checkSchema(ctx); err != nil {
		return JobWindowStats{}, err
	}

	var builds, finished, passed int64
	var medianDurationMs *int64
	var medianQueueWaitMs *int64
	var slowestStageName *string
	var slowestStageDurationMs *int64

	err := r.pool.QueryRow(ctx, `
		WITH window_runs AS (
			SELECT run_id, result, duration_ms, queued_at, started_at
			FROM varroa.runs
			WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND created_at >= $4
			  AND `+recordsBuildSQL+`
		),
		stage_medians AS (
			SELECT rs.name AS stage_name,
			       ROUND(percentile_cont(0.5) WITHIN GROUP (ORDER BY rs.duration_ms)::numeric)::bigint AS median_duration_ms
			FROM varroa.run_stages rs
			JOIN window_runs wr ON wr.run_id = rs.run_id
			WHERE rs.duration_ms IS NOT NULL
			GROUP BY rs.name
		)
		SELECT
			(SELECT count(*) FROM window_runs) AS builds,
			(SELECT count(*) FROM window_runs WHERE result IS NOT NULL) AS finished,
			(SELECT count(*) FROM window_runs WHERE result = 'SUCCESS') AS passed,
			(SELECT ROUND(percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)::numeric)::bigint
			   FROM window_runs WHERE duration_ms IS NOT NULL) AS median_duration_ms,
			(SELECT ROUND(percentile_cont(0.5) WITHIN GROUP (
			    ORDER BY EXTRACT(EPOCH FROM (started_at - queued_at)) * 1000
			 )::numeric)::bigint
			   FROM window_runs WHERE queued_at IS NOT NULL AND started_at IS NOT NULL) AS median_queue_wait_ms,
			(SELECT stage_name FROM stage_medians ORDER BY median_duration_ms DESC, stage_name LIMIT 1) AS slowest_stage_name,
			(SELECT median_duration_ms FROM stage_medians ORDER BY median_duration_ms DESC, stage_name LIMIT 1) AS slowest_stage_duration_ms
	`, cluster, tenant, jobPath, since).Scan(
		&builds, &finished, &passed, &medianDurationMs, &medianQueueWaitMs, &slowestStageName, &slowestStageDurationMs,
	)
	if err != nil {
		return JobWindowStats{}, fmt.Errorf("read job window stats: %w", err)
	}

	stats := JobWindowStats{Builds: builds, MedianDurationMs: medianDurationMs, MedianQueueWaitMs: medianQueueWaitMs}
	if finished > 0 {
		rate := float64(passed) / float64(finished)
		stats.PassRate = &rate
	}
	if slowestStageName != nil && slowestStageDurationMs != nil {
		stats.SlowestStage = &StageStat{Name: *slowestStageName, DurationMs: *slowestStageDurationMs}
	}
	return stats, nil
}
