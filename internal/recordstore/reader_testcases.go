// Package recordstore: the test case reads of the run-test-cases
// capability. The run read answers one run's stored case set. The history
// read answers one test case's outcomes across one job's runs. Neither
// read writes, and neither read computes a rate or a trend.
package recordstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ListTestCases reads every run_test_cases row for runID. A boolean sorts
// false before true, so every row that is not SKIPPED comes first. The
// remaining order is stable, so two reads of one run answer one order.
func (r *pgReader) ListTestCases(ctx context.Context, runID string) ([]TestCase, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT suite, class_name, name, status, duration_ms, error_details
		FROM varroa.run_test_cases
		WHERE run_id = $1
		ORDER BY (status = 'SKIPPED'), suite, class_name, name
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list test cases: %w", err)
	}
	defer rows.Close()

	var cases []TestCase
	for rows.Next() {
		var c TestCase
		if err := rows.Scan(&c.Suite, &c.ClassName, &c.Name, &c.Status, &c.DurationMs, &c.ErrorDetails); err != nil {
			return nil, fmt.Errorf("scan test case: %w", err)
		}
		cases = append(cases, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list test cases: %w", err)
	}
	return cases, nil
}

// ListTestCaseHistory reads one test case's rows for one job, newest
// first, through run_test_cases_history_idx. The join names runs' own
// primary key (created_at, run_id). run_test_cases.created_at equals
// runs.created_at, so the planner prunes the runs partitions at run time.
// The caller bounds q.Limit; a value below one answers no row.
func (r *pgReader) ListTestCaseHistory(ctx context.Context, q TestCaseHistoryQuery) ([]TestCaseOutcome, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	if q.Limit < 1 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT c.run_id, c.suite, c.status, c.duration_ms, c.error_details, c.created_at,
		       r.build_number, r.result
		FROM varroa.run_test_cases c
		JOIN varroa.runs r ON r.run_id = c.run_id AND r.created_at = c.created_at
		WHERE c.cluster = $1 AND c.tenant = $2 AND c.job_path = $3
		  AND c.class_name = $4 AND c.name = $5
		ORDER BY c.created_at DESC, c.run_id DESC, c.suite
		LIMIT $6
	`, q.Cluster, q.Tenant, q.JobPath, q.ClassName, q.Name, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("list test case history: %w", err)
	}
	defer rows.Close()

	var outcomes []TestCaseOutcome
	for rows.Next() {
		var o TestCaseOutcome
		var runID uuid.UUID
		if err := rows.Scan(&runID, &o.Suite, &o.Status, &o.DurationMs, &o.ErrorDetails, &o.CreatedAt,
			&o.BuildNumber, &o.Result); err != nil {
			return nil, fmt.Errorf("scan test case outcome: %w", err)
		}
		o.RunID = runID.String()
		outcomes = append(outcomes, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list test case history: %w", err)
	}
	return outcomes, nil
}
