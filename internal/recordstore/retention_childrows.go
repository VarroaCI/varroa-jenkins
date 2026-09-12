package recordstore

import (
	"context"
	"fmt"
	"time"
)

// deleteExpiredChildRows deletes run_stages and run_tests rows older than
// cutoff, in bounded batches. It returns how many rows it deleted from
// each table.
func deleteExpiredChildRows(ctx context.Context, db maintDB, cutoff time.Time) (stagesDeleted, testsDeleted int, err error) {
	stagesDeleted, err = deleteBatched(ctx, db, `
		DELETE FROM varroa.run_stages WHERE (run_id, stage_id) IN (
			SELECT run_id, stage_id FROM varroa.run_stages WHERE created_at < $1 LIMIT $2
		)
	`, cutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("delete expired run_stages: %w", err)
	}

	testsDeleted, err = deleteBatched(ctx, db, `
		DELETE FROM varroa.run_tests WHERE (run_id, suite) IN (
			SELECT run_id, suite FROM varroa.run_tests WHERE created_at < $1 LIMIT $2
		)
	`, cutoff)
	if err != nil {
		return stagesDeleted, 0, fmt.Errorf("delete expired run_tests: %w", err)
	}
	return stagesDeleted, testsDeleted, nil
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
