package recordstore

import (
	"context"
	"fmt"
	"time"
)

// orphanGrantedAge is the age a GRANTED intent must reach, with no
// matching runs row, before step 8 deletes it. The age test protects a
// claim that has not started yet; the existence test protects a run that
// reached the store through an out-of-order event.
const orphanGrantedAge = 7 * 24 * time.Hour

// The orphan rule asks whether a real build was recorded. A
// TRIGGER_FAILED row records that no build exists, so it never answers
// that question: an orphaned retry intent would otherwise outlive the
// window forever.

// sweepIntents runs steps 7 and 8 of the maintenance round. Step 7
// deletes run_intents rows older than the lower bound of the oldest
// surviving runs partition, so an intent always outlives its run. Step 8
// deletes GRANTED intents older than seven days with no matching runs
// row. Neither step reads or writes job_numbers.
func sweepIntents(ctx context.Context, db maintDB, now time.Time) (expiredByPartition, orphanGranted int, err error) { //nolint:unparam // expiredByPartition documents step 7's count for a future caller and a test that asserts on it
	lowerBound, err := oldestRunsPartitionLowerBound(ctx, db)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve oldest runs partition: %w", err)
	}

	tag, err := db.Exec(ctx, `DELETE FROM varroa.run_intents WHERE created_at < $1`, lowerBound)
	if err != nil {
		return 0, 0, fmt.Errorf("delete intents older than the oldest runs partition: %w", err)
	}
	expiredByPartition = int(tag.RowsAffected())

	granted, err := db.Exec(ctx, `
		DELETE FROM varroa.run_intents ri
		WHERE ri.state = 'GRANTED'
		  AND ri.created_at < $1
		  AND NOT EXISTS (
			SELECT 1 FROM varroa.runs r
			WHERE r.run_id = ri.run_id AND `+notTriggerFailedRunSQL+`
		  )
	`, now.Add(-orphanGrantedAge))
	if err != nil {
		return expiredByPartition, 0, fmt.Errorf("delete orphan granted intents: %w", err)
	}
	orphanGranted = int(granted.RowsAffected())

	return expiredByPartition, orphanGranted, nil
}
