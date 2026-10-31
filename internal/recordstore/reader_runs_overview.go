package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RecentResultsSize is the number of recent runs that one scope reports.
const RecentResultsSize = 10

// RunsOverviewQuery names the scopes of the overview and its reference time.
// A zero Now means the current time, in UTC.
type RunsOverviewQuery struct {
	Scopes []RunScope
	Now    time.Time
}

// RunsOverviewTotals holds the aggregate counts of the overview window.
// The window is the 24 hours before Now. The live counts have no time bound.
// Finished counts every run with a result. Counted counts the finished runs
// that count toward a pass rate. INTERRUPTED and NOT_BUILT runs do not count.
type RunsOverviewTotals struct {
	Runs24h, RunsPrior24h, Finished, Counted, Passed, Failed, Interrupted int64
	Running, Queued                                                       int64
	MedianQueueWaitMs, MedianDurationMs, P90DurationMs                    *int64
}

// RecentResult is one recent run of a scope. Result is empty while the run
// has no result.
type RecentResult struct {
	RunID     string
	CreatedAt time.Time
	Status    string
	Result    string
}

// ScopeOverview holds the counts of one scope. Recent is newest first and
// holds at most RecentResultsSize entries.
type ScopeOverview struct {
	Cluster, Tenant                           string
	Runs24h, Failed24h, Running, WaitingInput int64
	Recent                                    []RecentResult
}

// RunsOverview is the answer of the overview read. Scopes keeps the input
// order of the query, and a scope with no run still has a row.
type RunsOverview struct {
	WindowStart time.Time
	Totals      RunsOverviewTotals
	Scopes      []ScopeOverview
}

// overviewScopedCTE gives the runs that the overview counts as the relation
// scoped. It holds every run of the scopes created since $2, the start of the
// prior window. It also holds every older live run, so the live counts need no
// time bound. The two halves are disjoint by created_at, so no run appears
// twice. The statement that uses it must bind $1 to scopesJSON and $2 to the
// prior-window start.
const overviewScopedCTE = `scoped AS (
	SELECT r.* FROM scopes s
	JOIN varroa.runs r ON r.cluster = s.cluster AND r.tenant = s.tenant
		AND (s.patterns IS NULL OR r.job_path ~ ANY(s.patterns))
	WHERE r.created_at >= $2 AND ` + recordsBuildRunSQL + `
	UNION ALL
	SELECT r.* FROM scopes s
	JOIN varroa.runs r ON r.cluster = s.cluster AND r.tenant = s.tenant
		AND (s.patterns IS NULL OR r.job_path ~ ANY(s.patterns))
	WHERE r.created_at < $2 AND r.status IN ('RUNNING', 'QUEUED') AND ` + recordsBuildRunSQL + `
)`

// RunsOverview answers the aggregate read of the pipelines overview. It
// issues three statements over the scopes: the totals, the counts of each
// scope, and the recent runs of each scope. It issues none for an empty scope
// list.
//
// runs_tenant_idx serves the window and the recent reads. runs_running_tenant_idx
// serves the live counts.
func (r *pgReader) RunsOverview(ctx context.Context, q RunsOverviewQuery) (RunsOverview, error) {
	now := q.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	windowStart := now.Add(-24 * time.Hour)
	priorStart := now.Add(-48 * time.Hour)
	out := RunsOverview{WindowStart: windowStart}
	if len(q.Scopes) == 0 {
		return out, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return RunsOverview{}, err
	}
	scopes, err := scopesJSON(q.Scopes)
	if err != nil {
		return RunsOverview{}, fmt.Errorf("encode overview scopes: %w", err)
	}
	// $1 scopes, $2 prior-window start, $3 window start, $4 now.
	bounds := []any{string(scopes), priorStart, windowStart, now}

	totalsSQL := `WITH ` + scopesRelation + `, ` + overviewScopedCTE + `,
	win AS (SELECT * FROM scoped runs WHERE runs.created_at >= $3 AND runs.created_at < $4)
	SELECT
		(SELECT count(*) FROM win),
		(SELECT count(*) FROM scoped runs WHERE runs.created_at >= $2 AND runs.created_at < $3),
		(SELECT count(*) FROM win WHERE win.result IS NOT NULL),
		(SELECT count(*) FROM win WHERE ` + countedResultSQL + `),
		(SELECT count(*) FROM win WHERE win.result = 'SUCCESS'),
		(SELECT count(*) FROM win WHERE win.result = 'FAILURE'),
		(SELECT count(*) FROM win WHERE win.result = 'INTERRUPTED'),
		(SELECT count(*) FROM scoped runs WHERE runs.status = 'RUNNING'),
		(SELECT count(*) FROM scoped runs WHERE runs.status = 'QUEUED'),
		(SELECT ROUND((percentile_cont(0.5) WITHIN GROUP (ORDER BY win.duration_ms))::numeric)::bigint FROM win WHERE win.duration_ms IS NOT NULL),
		(SELECT ROUND((percentile_cont(0.9) WITHIN GROUP (ORDER BY win.duration_ms))::numeric)::bigint FROM win WHERE win.duration_ms IS NOT NULL),
		(SELECT ROUND((percentile_cont(0.5) WITHIN GROUP (
			ORDER BY EXTRACT(EPOCH FROM (win.started_at - win.queued_at)) * 1000))::numeric)::bigint
			FROM win WHERE win.queued_at IS NOT NULL AND win.started_at IS NOT NULL)`
	t := out.Totals
	if err := r.pool.QueryRow(ctx, totalsSQL, bounds...).Scan(
		&t.Runs24h, &t.RunsPrior24h, &t.Finished, &t.Counted, &t.Passed, &t.Failed, &t.Interrupted,
		&t.Running, &t.Queued, &t.MedianDurationMs, &t.P90DurationMs, &t.MedianQueueWaitMs,
	); err != nil {
		return RunsOverview{}, fmt.Errorf("read runs overview totals: %w", err)
	}
	out.Totals = t

	countsSQL := `WITH ` + scopesRelation + `, ` + overviewScopedCTE + `
	SELECT runs.cluster, runs.tenant,
		count(*) FILTER (WHERE runs.created_at >= $3 AND runs.created_at < $4),
		count(*) FILTER (WHERE runs.created_at >= $3 AND runs.created_at < $4 AND runs.result = 'FAILURE'),
		count(*) FILTER (WHERE runs.status = 'RUNNING'),
		count(*) FILTER (WHERE runs.status = 'RUNNING' AND ` + waitingInputSQL + `)
	FROM scoped runs
	GROUP BY runs.cluster, runs.tenant`
	type scopeKey struct{ cluster, tenant string }
	counts := map[scopeKey]ScopeOverview{}
	countRows, err := r.pool.Query(ctx, countsSQL, bounds...)
	if err != nil {
		return RunsOverview{}, fmt.Errorf("read runs overview scope counts: %w", err)
	}
	for countRows.Next() {
		var cluster, tenant string
		var row ScopeOverview
		if err := countRows.Scan(&cluster, &tenant, &row.Runs24h, &row.Failed24h, &row.Running, &row.WaitingInput); err != nil {
			countRows.Close()
			return RunsOverview{}, fmt.Errorf("scan runs overview scope counts: %w", err)
		}
		counts[scopeKey{cluster, tenant}] = row
	}
	countRows.Close()
	if err := countRows.Err(); err != nil {
		return RunsOverview{}, fmt.Errorf("read runs overview scope counts: %w", err)
	}

	recentSQL := scopesCTE + `
	SELECT s.cluster, s.tenant, rec.run_id, rec.created_at, rec.status, COALESCE(rec.result, '')
	FROM scopes s
	CROSS JOIN LATERAL (
		SELECT r.run_id, r.created_at, r.status, r.result
		FROM varroa.runs r
		WHERE r.cluster = s.cluster AND r.tenant = s.tenant
			AND (s.patterns IS NULL OR r.job_path ~ ANY(s.patterns))
			AND ` + recordsBuildRunSQL + `
		ORDER BY r.created_at DESC, r.run_id DESC
		LIMIT ` + fmt.Sprint(RecentResultsSize) + `
	) rec
	ORDER BY s.cluster, s.tenant, rec.created_at DESC, rec.run_id DESC`
	recent := map[scopeKey][]RecentResult{}
	recentRows, err := r.pool.Query(ctx, recentSQL, bounds[0])
	if err != nil {
		return RunsOverview{}, fmt.Errorf("read runs overview recent runs: %w", err)
	}
	for recentRows.Next() {
		var cluster, tenant string
		var res RecentResult
		var runID uuid.UUID
		if err := recentRows.Scan(&cluster, &tenant, &runID, &res.CreatedAt, &res.Status, &res.Result); err != nil {
			recentRows.Close()
			return RunsOverview{}, fmt.Errorf("scan runs overview recent runs: %w", err)
		}
		res.RunID = runID.String()
		key := scopeKey{cluster, tenant}
		recent[key] = append(recent[key], res)
	}
	recentRows.Close()
	if err := recentRows.Err(); err != nil {
		return RunsOverview{}, fmt.Errorf("read runs overview recent runs: %w", err)
	}

	out.Scopes = make([]ScopeOverview, 0, len(q.Scopes))
	for _, s := range q.Scopes {
		key := scopeKey{s.Cluster, s.Tenant}
		row := counts[key]
		row.Cluster, row.Tenant = s.Cluster, s.Tenant
		row.Recent = recent[key]
		out.Scopes = append(out.Scopes, row)
	}
	return out, nil
}
