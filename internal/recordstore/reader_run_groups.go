package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// runGroupsDefaultLimit and runGroupsMaxLimit bound RunGroupsQuery.Limit.
const (
	runGroupsDefaultLimit = 100
	runGroupsMaxLimit     = 200
)

// RunGroupsQuery names the scopes of the run-groups read and its window. The
// window is Since <= created_at < Now. Limit <= 0 means runGroupsDefaultLimit,
// and a larger value is clamped to runGroupsMaxLimit.
type RunGroupsQuery struct {
	Scopes     []RunScope
	Since, Now time.Time
	Limit      int
}

// RunGroup holds the finished build runs of one job in the window, except the
// held run. Runs counts them, Passed and Failed count the SUCCESS and FAILURE
// results, and FirstAt is the oldest created_at. Newest is the newest of them.
type RunGroup struct {
	Cluster, Tenant, JobPath string
	Runs, Passed, Failed     int64
	FirstAt                  time.Time
	Newest                   Run
}

// RunGroupsPage is one page of RunGroups. Held counts the runs that the read
// left out because each one is the held run of its job: the newest counted run
// of the job, when that run failed. More is true when the read found more
// groups than the limit.
type RunGroupsPage struct {
	Groups []RunGroup
	Held   int64
	More   bool
}

// runGroupsWindowCTE is the shared relation of the run-groups statements. It
// needs $1 for the scopes JSON, $2 for Since, and $3 for Now. The held CTE
// names the newest counted run of each job created before Now, when that run
// failed. The kept CTE drops the held runs.
const runGroupsWindowCTE = scopesRelation + `,
	win AS (
		SELECT r.run_id, r.cluster, r.tenant, r.job_path, r.created_at, r.result
		FROM scopes s
		JOIN varroa.runs r ON r.cluster = s.cluster AND r.tenant = s.tenant
			AND (s.patterns IS NULL OR r.job_path ~ ANY(s.patterns))
		WHERE r.created_at >= $2 AND r.created_at < $3 AND r.result IS NOT NULL AND ` + historyRunRunSQL + `
	),
	jobs AS (SELECT DISTINCT cluster, tenant, job_path FROM win),
	held AS (
		SELECT n.run_id FROM jobs j CROSS JOIN LATERAL (` + newestCountedRunBeforeSQL + `) n
		WHERE n.result = 'FAILURE'
	),
	kept AS (SELECT * FROM win WHERE run_id NOT IN (SELECT run_id FROM held))`

// RunGroups answers the finished build runs of the scopes created in the
// window, grouped by cluster, tenant, and job path. Each job's held run is left
// out, so a failing job's newest failure stays out of its group. Groups are in
// newest-run order, and a group with no kept run is absent. It runs three
// statements in one read-only transaction: the held count, the groups, and the
// newest run of each group. It issues none for an empty scope list.
func (r *pgReader) RunGroups(ctx context.Context, q RunGroupsQuery) (RunGroupsPage, error) {
	if len(q.Scopes) == 0 {
		return RunGroupsPage{}, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return RunGroupsPage{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = runGroupsDefaultLimit
	}
	if limit > runGroupsMaxLimit {
		limit = runGroupsMaxLimit
	}
	scopes, err := scopesJSON(q.Scopes)
	if err != nil {
		return RunGroupsPage{}, fmt.Errorf("encode run-group scopes: %w", err)
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RunGroupsPage{}, fmt.Errorf("begin run-groups transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var page RunGroupsPage
	if err := tx.QueryRow(ctx, `WITH `+runGroupsWindowCTE+`
		SELECT count(*) FROM win WHERE run_id IN (SELECT run_id FROM held)`,
		string(scopes), q.Since, q.Now).Scan(&page.Held); err != nil {
		return RunGroupsPage{}, fmt.Errorf("count held runs: %w", err)
	}

	type groupRow struct {
		group  RunGroup
		newest uuid.UUID
	}
	rows, err := tx.Query(ctx, `WITH `+runGroupsWindowCTE+`
		SELECT cluster, tenant, job_path, count(*),
			count(*) FILTER (WHERE result = 'SUCCESS'),
			count(*) FILTER (WHERE result = 'FAILURE'),
			min(created_at),
			(array_agg(run_id ORDER BY created_at DESC, run_id DESC))[1]
		FROM kept
		GROUP BY cluster, tenant, job_path
		ORDER BY max(created_at) DESC, cluster, tenant, job_path
		LIMIT $4`,
		string(scopes), q.Since, q.Now, limit+1)
	if err != nil {
		return RunGroupsPage{}, fmt.Errorf("list run groups: %w", err)
	}
	var groups []groupRow
	for rows.Next() {
		var g groupRow
		if err := rows.Scan(&g.group.Cluster, &g.group.Tenant, &g.group.JobPath, &g.group.Runs,
			&g.group.Passed, &g.group.Failed, &g.group.FirstAt, &g.newest); err != nil {
			rows.Close()
			return RunGroupsPage{}, fmt.Errorf("scan run group: %w", err)
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RunGroupsPage{}, fmt.Errorf("list run groups: %w", err)
	}
	if len(groups) > limit {
		page.More = true
		groups = groups[:limit]
	}

	newestIDs := make([]string, 0, len(groups))
	for _, g := range groups {
		newestIDs = append(newestIDs, g.newest.String())
	}
	newest := map[string]Run{}
	if len(newestIDs) > 0 {
		runRows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM varroa.runs WHERE run_id = ANY($1::uuid[])`, newestIDs)
		if err != nil {
			return RunGroupsPage{}, fmt.Errorf("read newest group runs: %w", err)
		}
		for runRows.Next() {
			run, err := scanRun(runRows)
			if err != nil {
				runRows.Close()
				return RunGroupsPage{}, fmt.Errorf("scan newest group run: %w", err)
			}
			newest[run.RunID] = run
		}
		runRows.Close()
		if err := runRows.Err(); err != nil {
			return RunGroupsPage{}, fmt.Errorf("read newest group runs: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RunGroupsPage{}, fmt.Errorf("commit run-groups transaction: %w", err)
	}

	for _, g := range groups {
		out := g.group
		out.Newest = newest[g.newest.String()]
		page.Groups = append(page.Groups, out)
	}
	return page, nil
}
