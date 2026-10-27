package recordstore

import (
	"context"
	"fmt"
	"time"
)

// childRowCounts holds how many expired child rows one retention round
// deleted from each table.
type childRowCounts struct {
	Stages, Tests, Cases, Coverage, Warnings int
}

// deleteExpiredChildRows deletes run_stages, run_tests, run_test_cases,
// run_coverage, and run_warnings rows older than cutoff, in bounded
// batches. It returns how many rows it deleted from each table. On an
// error it returns the counts it reached before the error.
func deleteExpiredChildRows(ctx context.Context, db maintDB, cutoff time.Time) (childRowCounts, error) {
	var counts childRowCounts
	var err error

	counts.Stages, err = deleteBatched(ctx, db, `
		DELETE FROM varroa.run_stages WHERE (run_id, stage_id) IN (
			SELECT run_id, stage_id FROM varroa.run_stages WHERE created_at < $1 LIMIT $2
		)
	`, cutoff)
	if err != nil {
		return counts, fmt.Errorf("delete expired run_stages: %w", err)
	}

	counts.Tests, err = deleteBatched(ctx, db, `
		DELETE FROM varroa.run_tests WHERE (run_id, suite) IN (
			SELECT run_id, suite FROM varroa.run_tests WHERE created_at < $1 LIMIT $2
		)
	`, cutoff)
	if err != nil {
		return counts, fmt.Errorf("delete expired run_tests: %w", err)
	}

	counts.Cases, err = deleteBatched(ctx, db, `
		DELETE FROM varroa.run_test_cases WHERE (run_id, suite, class_name, name) IN (
			SELECT run_id, suite, class_name, name
			FROM varroa.run_test_cases WHERE created_at < $1 LIMIT $2
		)
	`, cutoff)
	if err != nil {
		return counts, fmt.Errorf("delete expired run_test_cases: %w", err)
	}

	counts.Coverage, err = deleteBatched(ctx, db, `
		DELETE FROM varroa.run_coverage WHERE (run_id, report_id) IN (
			SELECT run_id, report_id FROM varroa.run_coverage WHERE created_at < $1 LIMIT $2
		)
	`, cutoff)
	if err != nil {
		return counts, fmt.Errorf("delete expired run_coverage: %w", err)
	}

	counts.Warnings, err = deleteBatched(ctx, db, `
		DELETE FROM varroa.run_warnings WHERE (run_id, report_id) IN (
			SELECT run_id, report_id FROM varroa.run_warnings WHERE created_at < $1 LIMIT $2
		)
	`, cutoff)
	if err != nil {
		return counts, fmt.Errorf("delete expired run_warnings: %w", err)
	}
	return counts, nil
}

// deleteBatched runs stmt, a "DELETE ... WHERE key IN (SELECT key FROM
// table WHERE created_at < $1 LIMIT $2)" statement, repeatedly, until one
// call affects fewer than retentionBatchSize rows. It returns the total
// rows deleted.
func deleteBatched(ctx context.Context, db maintDB, stmt string, cutoff time.Time) (int, error) {
	total := 0
	for {
		tag, err := db.Exec(ctx, stmt, cutoff, retentionBatchSize)
		if err != nil {
			return total, err
		}
		n := int(tag.RowsAffected())
		total += n
		if n < retentionBatchSize {
			return total, nil
		}
	}
}
