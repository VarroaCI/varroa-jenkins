package api

import (
	"context"
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// JobHealthJSON is one job's health over its recent finished build runs.
// Bucket is one of sunny, partlyCloudy, cloudy, rainy, stormy, or unknown.
// Score is the percentage of counted runs that passed. It is absent when
// no run counts. Window is the number of recent finished runs read, and
// Counted is how many of them count. Passed is how many counted runs
// passed.
type JobHealthJSON struct {
	Bucket  string `json:"bucket"`
	Score   *int   `json:"score,omitempty"`
	Window  int    `json:"window"`
	Counted int    `json:"counted"`
	Passed  int    `json:"passed"`
}

// JobTrendTestsJSON holds one run's suite counters. Passed is the total
// minus the failed and skipped counters, never below zero.
type JobTrendTestsJSON struct {
	Passed  int32 `json:"passed"`
	Failed  int32 `json:"failed"`
	Skipped int32 `json:"skipped"`
}

// JobTrendPointJSON is one finished build run of the job trend. Tests is
// absent when the run recorded no suite counters.
type JobTrendPointJSON struct {
	RunID       string             `json:"runId"`
	BuildNumber *int64             `json:"buildNumber,omitempty"`
	Result      string             `json:"result"`
	DurationMs  *int64             `json:"durationMs,omitempty"`
	Tests       *JobTrendTestsJSON `json:"tests,omitempty"`
}

// JobHealthResult is the job health read that the get_job_health MCP tool
// returns. Its health, trend, and qualityTrend match the job detail's fields.
// QualityTrend is absent when no run of the health window holds a quality row.
type JobHealthResult struct {
	JobPath      string              `json:"jobPath"`
	Health       JobHealthJSON       `json:"health"`
	Trend        []JobTrendPointJSON `json:"trend"`
	QualityTrend *QualityTrendJSON   `json:"qualityTrend,omitempty"`
}

// healthBucketOf maps a pass score to its weather bucket: above 80 is
// sunny, above 60 partlyCloudy, above 40 cloudy, above 20 rainy, and
// anything lower stormy.
func healthBucketOf(score int) string {
	switch {
	case score > 80:
		return "sunny"
	case score > 60:
		return "partlyCloudy"
	case score > 40:
		return "cloudy"
	case score > 20:
		return "rainy"
	default:
		return "stormy"
	}
}

// countsForHealth reports whether a finished run counts toward a job's
// health. An interrupted run and a run that was not built say nothing
// about the job's code or its tests, so neither counts.
func countsForHealth(result string) bool {
	return result != "INTERRUPTED" && result != "NOT_BUILT"
}

// computeJobHealth scores the runs of the health window. SUCCESS passes.
// FAILURE, UNSTABLE, and ABORTED fail. A job with no counted run is
// unknown and has no score.
func computeJobHealth(runs []recordstore.HealthRun) JobHealthJSON {
	out := JobHealthJSON{Bucket: "unknown", Window: recordstore.JobHealthWindow}
	for _, run := range runs {
		if !countsForHealth(run.Result) {
			continue
		}
		out.Counted++
		if run.Result == "SUCCESS" {
			out.Passed++
		}
	}
	if out.Counted == 0 {
		return out
	}
	score := out.Passed * 100 / out.Counted
	out.Score = &score
	out.Bucket = healthBucketOf(score)
	return out
}

// buildJobTrend converts the health window into trend points, newest
// first. It never returns nil, so the JSON field is always an array.
func buildJobTrend(runs []recordstore.HealthRun) []JobTrendPointJSON {
	out := make([]JobTrendPointJSON, 0, len(runs))
	for _, run := range runs {
		point := JobTrendPointJSON{
			RunID:       run.RunID,
			BuildNumber: run.BuildNumber,
			Result:      run.Result,
			DurationMs:  run.DurationMs,
		}
		if run.TestsTotal != nil {
			point.Tests = &JobTrendTestsJSON{
				Passed:  max(*run.TestsTotal-deref32(run.TestsFailed)-deref32(run.TestsSkipped), 0),
				Failed:  deref32(run.TestsFailed),
				Skipped: deref32(run.TestsSkipped),
			}
		}
		out = append(out, point)
	}
	return out
}

func deref32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

// JobHealth answers the health and the trend of one job of a tenant. It
// is the shared read for the HTTP route and the get_job_health MCP tool.
// Its health and trend match the job detail's health and trend fields.
func (s *Server) JobHealth(ctx context.Context, claims *auth.Claims, tenant, jobPath string) (JobHealthResult, *ServiceError) {
	if s.deps.RecordStore == nil {
		return JobHealthResult{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	refs, serr := s.resolveTenantTenants(ctx, claims, tenant)
	if serr != nil {
		return JobHealthResult{}, serr
	}
	ref, ok := s.tenantForJob(claims, refs, jobPath)
	if !ok {
		return JobHealthResult{}, &ServiceError{Status: http.StatusNotFound, Message: "job not found"}
	}
	summary, err := s.deps.RecordStore.GetJobSummary(ctx, ref.Cluster, ref.Tenant, jobPath)
	if err != nil {
		return JobHealthResult{}, s.readerServiceError(err, "get job summary")
	}
	return JobHealthResult{
		JobPath:      jobPath,
		Health:       computeJobHealth(summary.RecentRuns),
		Trend:        buildJobTrend(summary.RecentRuns),
		QualityTrend: s.jobQualityTrend(ctx, ref.Cluster, ref.Tenant, jobPath, summary.RecentRuns),
	}, nil
}
