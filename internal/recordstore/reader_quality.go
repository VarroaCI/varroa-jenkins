package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// querier is the read surface a pgx transaction and a pool share.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// RunQuality answers the coverage and warnings rows of one run. Each row
// carries its base, the newest earlier row of the same cluster, tenant,
// job path, and report id. The base join runs per row over the
// *_job_idx index, so it reads one index entry per row. The runs join
// names runs' own primary key, so the planner prunes the partitions. A
// base row with no runs row keeps a nil BuildNumber. Both statements run
// in one repeatable-read transaction, so the coverage and warnings rows
// come from one snapshot.
func (r *pgReader) RunQuality(ctx context.Context, runID string) (RunQuality, error) {
	if err := r.checkSchema(ctx); err != nil {
		return RunQuality{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RunQuality{}, fmt.Errorf("run quality: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	coverage, err := runCoverageRows(ctx, tx, runID)
	if err != nil {
		return RunQuality{}, err
	}
	warnings, err := runWarningsRows(ctx, tx, runID)
	if err != nil {
		return RunQuality{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RunQuality{}, fmt.Errorf("run quality: commit: %w", err)
	}
	return RunQuality{Coverage: coverage, Warnings: warnings}, nil
}

// JobQualityTrend answers the coverage and warnings rows of the listed
// runs of one job. It reads by run id under the primary key and keeps the
// scope as filters. An empty runIDs answers an empty RunQuality and issues
// no statement. A row carries no base.
func (r *pgReader) JobQualityTrend(ctx context.Context, cluster, tenant, jobPath string, runIDs []string) (RunQuality, error) {
	if len(runIDs) == 0 {
		return RunQuality{}, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return RunQuality{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RunQuality{}, fmt.Errorf("job quality trend: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	coverage, err := trendCoverageRows(ctx, tx, cluster, tenant, jobPath, runIDs)
	if err != nil {
		return RunQuality{}, err
	}
	warnings, err := trendWarningsRows(ctx, tx, cluster, tenant, jobPath, runIDs)
	if err != nil {
		return RunQuality{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RunQuality{}, fmt.Errorf("job quality trend: commit: %w", err)
	}
	return RunQuality{Coverage: coverage, Warnings: warnings}, nil
}

func runCoverageRows(ctx context.Context, q querier, runID string) ([]RunCoverage, error) {
	rows, err := q.Query(ctx, `
		SELECT c.run_id::text, c.report_id, c.name,
		       c.line_covered, c.line_total, c.branch_covered, c.branch_total,
		       c.modified_line_covered, c.modified_line_total, c.metrics, c.gate_status,
		       b.run_id::text, b.line_covered, b.line_total, b.branch_covered, b.branch_total,
		       br.build_number
		FROM varroa.run_coverage c
		LEFT JOIN LATERAL (
			SELECT p.run_id, p.created_at, p.line_covered, p.line_total, p.branch_covered, p.branch_total
			FROM varroa.run_coverage p
			WHERE p.cluster = c.cluster AND p.tenant = c.tenant AND p.job_path = c.job_path
			  AND p.report_id = c.report_id AND (p.created_at, p.run_id) < (c.created_at, c.run_id)
			ORDER BY p.created_at DESC, p.run_id DESC
			LIMIT 1
		) b ON true
		LEFT JOIN varroa.runs br ON br.run_id = b.run_id AND br.created_at = b.created_at
		WHERE c.run_id = $1
		ORDER BY c.report_id
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("run quality coverage: %w", err)
	}
	defer rows.Close()

	var out []RunCoverage
	for rows.Next() {
		var (
			row                                    RunCoverage
			lineCov, lineTot, branchCov, branchTot *int32
			modCov, modTot                         *int32
			metrics                                []byte
			baseRunID                              *string
			baseLineCov, baseLineTot               *int32
			baseBranchCov, baseBranchTot           *int32
			baseBuild                              *int64
		)
		if err := rows.Scan(&row.RunID, &row.ReportID, &row.Name,
			&lineCov, &lineTot, &branchCov, &branchTot, &modCov, &modTot, &metrics, &row.GateStatus,
			&baseRunID, &baseLineCov, &baseLineTot, &baseBranchCov, &baseBranchTot, &baseBuild,
		); err != nil {
			return nil, fmt.Errorf("scan run quality coverage: %w", err)
		}
		if row.Metrics, err = decodeMetrics(metrics); err != nil {
			return nil, err
		}
		row.Line = counterOf(lineCov, lineTot)
		row.Branch = counterOf(branchCov, branchTot)
		row.ModifiedLines = counterOf(modCov, modTot)
		if baseRunID != nil {
			row.Base = &CoverageBase{
				RunID:       *baseRunID,
				BuildNumber: baseBuild,
				Line:        counterOf(baseLineCov, baseLineTot),
				Branch:      counterOf(baseBranchCov, baseBranchTot),
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run quality coverage: %w", err)
	}
	return out, nil
}

func runWarningsRows(ctx context.Context, q querier, runID string) ([]RunWarnings, error) {
	rows, err := q.Query(ctx, `
		SELECT w.run_id::text, w.report_id, w.name, w.total, w.error, w.high, w.normal, w.low,
		       w.new_total, w.fixed_total, w.gate_status,
		       b.run_id::text, b.total, br.build_number
		FROM varroa.run_warnings w
		LEFT JOIN LATERAL (
			SELECT p.run_id, p.created_at, p.total
			FROM varroa.run_warnings p
			WHERE p.cluster = w.cluster AND p.tenant = w.tenant AND p.job_path = w.job_path
			  AND p.report_id = w.report_id AND (p.created_at, p.run_id) < (w.created_at, w.run_id)
			ORDER BY p.created_at DESC, p.run_id DESC
			LIMIT 1
		) b ON true
		LEFT JOIN varroa.runs br ON br.run_id = b.run_id AND br.created_at = b.created_at
		WHERE w.run_id = $1
		ORDER BY w.report_id
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("run quality warnings: %w", err)
	}
	defer rows.Close()

	var out []RunWarnings
	for rows.Next() {
		var (
			row       RunWarnings
			baseRunID *string
			baseTotal *int32
			baseBuild *int64
		)
		if err := rows.Scan(&row.RunID, &row.ReportID, &row.Name,
			&row.Total, &row.Error, &row.High, &row.Normal, &row.Low,
			&row.NewTotal, &row.FixedTotal, &row.GateStatus,
			&baseRunID, &baseTotal, &baseBuild,
		); err != nil {
			return nil, fmt.Errorf("scan run quality warnings: %w", err)
		}
		if baseRunID != nil {
			row.Base = &WarningsBase{RunID: *baseRunID, BuildNumber: baseBuild, Total: derefInt32(baseTotal)}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run quality warnings: %w", err)
	}
	return out, nil
}

func trendCoverageRows(ctx context.Context, q querier, cluster, tenant, jobPath string, runIDs []string) ([]RunCoverage, error) {
	rows, err := q.Query(ctx, `
		SELECT run_id::text, report_id, name, line_covered, line_total, branch_covered, branch_total,
		       modified_line_covered, modified_line_total, gate_status
		FROM varroa.run_coverage
		WHERE run_id = ANY($4::uuid[]) AND cluster = $1 AND tenant = $2 AND job_path = $3
		ORDER BY report_id, created_at DESC, run_id DESC
	`, cluster, tenant, jobPath, runIDs)
	if err != nil {
		return nil, fmt.Errorf("job quality trend coverage: %w", err)
	}
	defer rows.Close()

	var out []RunCoverage
	for rows.Next() {
		var (
			row                                    RunCoverage
			lineCov, lineTot, branchCov, branchTot *int32
			modCov, modTot                         *int32
		)
		if err := rows.Scan(&row.RunID, &row.ReportID, &row.Name,
			&lineCov, &lineTot, &branchCov, &branchTot, &modCov, &modTot, &row.GateStatus,
		); err != nil {
			return nil, fmt.Errorf("scan job quality trend coverage: %w", err)
		}
		row.Line = counterOf(lineCov, lineTot)
		row.Branch = counterOf(branchCov, branchTot)
		row.ModifiedLines = counterOf(modCov, modTot)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("job quality trend coverage: %w", err)
	}
	return out, nil
}

func trendWarningsRows(ctx context.Context, q querier, cluster, tenant, jobPath string, runIDs []string) ([]RunWarnings, error) {
	rows, err := q.Query(ctx, `
		SELECT run_id::text, report_id, name, total, error, high, normal, low,
		       new_total, fixed_total, gate_status
		FROM varroa.run_warnings
		WHERE run_id = ANY($4::uuid[]) AND cluster = $1 AND tenant = $2 AND job_path = $3
		ORDER BY report_id, created_at DESC, run_id DESC
	`, cluster, tenant, jobPath, runIDs)
	if err != nil {
		return nil, fmt.Errorf("job quality trend warnings: %w", err)
	}
	defer rows.Close()

	var out []RunWarnings
	for rows.Next() {
		var row RunWarnings
		if err := rows.Scan(&row.RunID, &row.ReportID, &row.Name,
			&row.Total, &row.Error, &row.High, &row.Normal, &row.Low,
			&row.NewTotal, &row.FixedTotal, &row.GateStatus,
		); err != nil {
			return nil, fmt.Errorf("scan job quality trend warnings: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("job quality trend warnings: %w", err)
	}
	return out, nil
}

// counterOf returns a counter only when both of its columns hold a value.
func counterOf(covered, total *int32) *CoverageCounter {
	if covered == nil || total == nil {
		return nil
	}
	return &CoverageCounter{Covered: *covered, Total: *total}
}

// decodeMetrics reads the metrics jsonb of one coverage row. An empty
// object answers a nil map.
func decodeMetrics(raw []byte) (map[string]CoverageCounter, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]CoverageCounter
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode run_coverage metrics: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

func derefInt32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}
