// Package recordstore: the cell-usage aggregates of design D4. The
// cell-wide aggregate answers every tenant of a cell set in one
// statement. The tenant-scoped read answers one row for each of one
// tenant's jobs, so a caller narrowed to part of a tenant sums only the
// jobs it may read.
package recordstore

import (
	"context"
	"fmt"
	"time"
)

// cellUsageWindow is the fixed window of runsLast24h and
// failedLast24h. No caller chooses another window.
const cellUsageWindow = 24 * time.Hour

// cellUsageQuery aggregates one cell set, tenant by tenant. The
// parameters are $1 cluster, $2 namespace, $3 the cell names, and $4
// the window start.
//
// The two runs aggregates stay separate. One scan carries one
// predicate: the windowed scan keeps the created_at term that prunes
// the partitions, and the running scan must find a build older than the
// window.
//
// Neither runs aggregate carries a namespace term. The ledger moves an
// intent's cell on an ownership transfer, and it does not move the
// intent's namespace. A namespace term would drop a run the cell did
// execute. The term is also unnecessary, because a tenant holds one
// placement row.
//
// The jobs aggregate counts distinct job paths. The job_schedules
// primary key is (cluster, tenant, job_path, trigger_type), so one job
// holds one row for each trigger type.
const cellUsageQuery = `
	WITH placed AS (
		SELECT m.cell, p.tenant, m.role, m.slot, p.generation, p.agent_namespace
		FROM varroa.tenant_placements p
		CROSS JOIN LATERAL jsonb_to_recordset(
			CASE WHEN jsonb_array_length(p.members) > 0 THEN p.members
			     ELSE jsonb_build_array(jsonb_build_object(
			         'cell', p.cell, 'namespace', p.namespace,
			         'role', 'active', 'slot', 0))
			END
		) AS m(cell text, namespace text, role text, slot int)
		WHERE p.cluster = $1 AND m.namespace = $2 AND m.cell = ANY($3)
	),
	windowed AS (
		SELECT r.cell, r.tenant,
		       count(*) AS runs,
		       count(*) FILTER (WHERE r.result IS NOT NULL AND r.result <> 'SUCCESS') AS failed
		FROM varroa.runs r
		JOIN placed p ON p.tenant = r.tenant AND p.cell = r.cell
		WHERE r.cluster = $1 AND r.created_at >= $4 AND ` + recordsBuildRunSQL + `
		GROUP BY r.cell, r.tenant
	),
	running AS (
		SELECT r.cell, r.tenant, count(*) AS running_now
		FROM varroa.runs r
		JOIN placed p ON p.tenant = r.tenant AND p.cell = r.cell
		WHERE r.cluster = $1 AND r.status = 'RUNNING' AND ` + notTriggerFailedRunSQL + `
		GROUP BY r.cell, r.tenant
	),
	jobs AS (
		SELECT s.cell, s.tenant, count(DISTINCT s.job_path) AS jobs
		FROM varroa.job_schedules s
		JOIN placed p ON p.tenant = s.tenant AND p.cell = s.cell
		WHERE s.cluster = $1 AND s.namespace = $2 AND s.cell = ANY($3)
		GROUP BY s.cell, s.tenant
	)
	SELECT p.cell, p.tenant, p.role, p.slot, p.generation, p.agent_namespace,
	       COALESCE(n.running_now, 0),
	       COALESCE(w.runs, 0),
	       COALESCE(w.failed, 0),
	       COALESCE(j.jobs, 0)
	FROM placed p
	LEFT JOIN windowed w ON w.cell = p.cell AND w.tenant = p.tenant
	LEFT JOIN running n ON n.cell = p.cell AND n.tenant = p.tenant
	LEFT JOIN jobs j ON j.cell = p.cell AND j.tenant = p.tenant
	ORDER BY p.cell, p.tenant
`

// cellTenantJobQuery reports one tenant's load on one cell, one row for
// each job. The parameters are $1 cluster, $2 namespace, $3 cell, $4
// tenant, and $5 the window start.
//
// The two runs aggregates keep the shape of cellUsageQuery: each leads
// with cluster and joins the placement row on tenant, so Postgres still
// uses runs_tenant_idx, and each excludes a row that records no build.
// Neither carries a namespace term, for the reason cellUsageQuery
// states.
//
// The row set is the union of three job-path sources, so a job with a
// schedule row and no run appears, and a job with a run and no schedule
// row appears as well. jobs is one or zero for each row, so a caller that
// sums a subset of the rows reads the same four counts the tenant-wide
// aggregate reports for those jobs.
const cellTenantJobQuery = `
	WITH placed AS (
		SELECT m.cell, p.tenant
		FROM varroa.tenant_placements p
		CROSS JOIN LATERAL jsonb_to_recordset(
			CASE WHEN jsonb_array_length(p.members) > 0 THEN p.members
			     ELSE jsonb_build_array(jsonb_build_object(
			         'cell', p.cell, 'namespace', p.namespace,
			         'role', 'active', 'slot', 0))
			END
		) AS m(cell text, namespace text, role text, slot int)
		WHERE p.cluster = $1 AND m.namespace = $2 AND m.cell = $3 AND p.tenant = $4
	),
	windowed AS (
		SELECT r.job_path,
		       count(*) AS runs,
		       count(*) FILTER (WHERE r.result IS NOT NULL AND r.result <> 'SUCCESS') AS failed
		FROM varroa.runs r
		JOIN placed p ON p.tenant = r.tenant AND p.cell = r.cell
		WHERE r.cluster = $1 AND r.created_at >= $5 AND ` + recordsBuildRunSQL + `
		GROUP BY r.job_path
	),
	running AS (
		SELECT r.job_path, count(*) AS running_now
		FROM varroa.runs r
		JOIN placed p ON p.tenant = r.tenant AND p.cell = r.cell
		WHERE r.cluster = $1 AND r.status = 'RUNNING' AND ` + notTriggerFailedRunSQL + `
		GROUP BY r.job_path
	),
	scheduled AS (
		SELECT DISTINCT s.job_path
		FROM varroa.job_schedules s
		JOIN placed p ON p.tenant = s.tenant AND p.cell = s.cell
		WHERE s.cluster = $1 AND s.namespace = $2 AND s.cell = $3
	),
	paths AS (
		SELECT job_path FROM windowed
		UNION
		SELECT job_path FROM running
		UNION
		SELECT job_path FROM scheduled
	)
	SELECT paths.job_path,
	       COALESCE(n.running_now, 0),
	       COALESCE(w.runs, 0),
	       COALESCE(w.failed, 0),
	       CASE WHEN s.job_path IS NULL THEN 0 ELSE 1 END
	FROM paths
	LEFT JOIN windowed w ON w.job_path = paths.job_path
	LEFT JOIN running n ON n.job_path = paths.job_path
	LEFT JOIN scheduled s ON s.job_path = paths.job_path
	ORDER BY paths.job_path
`

// cellUsageRow is one row of cellUsageQuery: one tenant on one cell.
type cellUsageRow struct {
	cell                string
	tenant              string
	role                string
	slot                *int32
	placementGeneration int64
	agentNamespace      string
	counts              CellUsageCounts
}

// queryCellUsage runs one cell-usage statement and returns its rows.
func (r *pgReader) queryCellUsage(ctx context.Context, cluster, namespace string, cells []string, windowStart time.Time) ([]cellUsageRow, error) {
	pgRows, err := r.pool.Query(ctx, cellUsageQuery, cluster, namespace, cells, windowStart)
	if err != nil {
		return nil, fmt.Errorf("read cell usage: %w", err)
	}
	defer pgRows.Close()

	var rows []cellUsageRow
	for pgRows.Next() {
		var row cellUsageRow
		if err := pgRows.Scan(&row.cell, &row.tenant, &row.role, &row.slot,
			&row.placementGeneration, &row.agentNamespace, &row.counts.RunningNow,
			&row.counts.RunsLast24h, &row.counts.FailedLast24h, &row.counts.Jobs); err != nil {
			return nil, fmt.Errorf("scan cell usage: %w", err)
		}
		rows = append(rows, row)
	}
	if err := pgRows.Err(); err != nil {
		return nil, fmt.Errorf("read cell usage: %w", err)
	}
	return rows, nil
}

// CellUsageMany reports each cell's per-tenant counts and totals in one
// statement. Every named cell carries an entry, so a cell with no
// tenant reads as an empty tenant list and four zero totals.
func (r *pgReader) CellUsageMany(ctx context.Context, cluster, namespace string, cells []string) (map[string]CellUsage, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}

	windowStart := time.Now().UTC().Add(-cellUsageWindow)
	usage := make(map[string]CellUsage, len(cells))
	for _, cell := range cells {
		usage[cell] = CellUsage{Cluster: cluster, Namespace: namespace, Cell: cell, WindowStart: windowStart}
	}
	if len(cells) == 0 {
		return usage, nil
	}

	rows, err := r.queryCellUsage(ctx, cluster, namespace, cells, windowStart)
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		cell, ok := usage[row.cell]
		if !ok {
			continue
		}
		cell.Tenants = append(cell.Tenants, CellTenantUsage{
			Tenant:              row.tenant,
			Role:                renderMemberRole(row.role, row.slot),
			AgentNamespace:      row.agentNamespace,
			PlacementGeneration: row.placementGeneration,
			CellUsageCounts:     row.counts,
		})
		cell.Totals.RunningNow += row.counts.RunningNow
		cell.Totals.RunsLast24h += row.counts.RunsLast24h
		cell.Totals.FailedLast24h += row.counts.FailedLast24h
		cell.Totals.Jobs += row.counts.Jobs
		usage[row.cell] = cell
	}
	return usage, nil
}

// CellUsage reports one cell's per-tenant counts and totals.
func (r *pgReader) CellUsage(ctx context.Context, cluster, namespace, cell string) (CellUsage, error) {
	usage, err := r.CellUsageMany(ctx, cluster, namespace, []string{cell})
	if err != nil {
		return CellUsage{}, err
	}
	return usage[cell], nil
}

// CellTenantUsage reports one row for each of a tenant's jobs on one
// cell, ordered by job path. A tenant the placement table does not bind
// to cell reads as no row, never an error. The caller sums the rows it
// may read, so a caller narrowed to part of the tenant's jobs never
// learns the tenant's whole load.
func (r *pgReader) CellTenantUsage(ctx context.Context, cluster, namespace, cell, tenant string) ([]CellJobUsage, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}

	windowStart := time.Now().UTC().Add(-cellUsageWindow)
	pgRows, err := r.pool.Query(ctx, cellTenantJobQuery, cluster, namespace, cell, tenant, windowStart)
	if err != nil {
		return nil, fmt.Errorf("read cell tenant usage: %w", err)
	}
	defer pgRows.Close()

	var rows []CellJobUsage
	for pgRows.Next() {
		var row CellJobUsage
		if err := pgRows.Scan(&row.JobPath, &row.RunningNow,
			&row.RunsLast24h, &row.FailedLast24h, &row.Jobs); err != nil {
			return nil, fmt.Errorf("scan cell tenant usage: %w", err)
		}
		rows = append(rows, row)
	}
	if err := pgRows.Err(); err != nil {
		return nil, fmt.Errorf("read cell tenant usage: %w", err)
	}
	return rows, nil
}

// JobHome answers the member that holds jobPath's home slot. A job with
// no stored home resolves to its computed hash home, the same home the
// claim fence reads. A tenant with no placement row answers Placed false. A tenant whose home slot
// holds no cell answers Placed true and an empty Cell.
func (r *pgReader) JobHome(ctx context.Context, cluster, tenant, jobPath string) (JobHome, error) {
	if err := r.checkSchema(ctx); err != nil {
		return JobHome{}, err
	}
	return resolveJobHome(ctx, r.pool, cluster, tenant, jobPath)
}
