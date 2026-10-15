package recordstore

import (
	"context"
	"time"
)

// activityRetentionBatchSize bounds the activity retention round's
// batched delete. It is 5000, not retentionBatchSize's 1000: an
// activity row carries no blob call and no second statement, so a
// larger batch costs one short lock and fewer round trips.
const activityRetentionBatchSize = 5000

// deleteExpiredActivity deletes "activity" rows older than cutoff, in
// batches of activityRetentionBatchSize, until one batch deletes fewer
// rows than the batch size. Deleting by exact range before the
// partition drop step makes effective activity retention exact, unlike
// run retention, which rounds up to a month boundary.
func deleteExpiredActivity(ctx context.Context, db maintDB, cutoff time.Time) (int, error) {
	total := 0
	for {
		tag, err := db.Exec(ctx, `
			DELETE FROM varroa.activity WHERE (ts, event_key) IN (
				SELECT ts, event_key FROM varroa.activity WHERE ts < $1 LIMIT $2
			)
		`, cutoff, activityRetentionBatchSize)
		if err != nil {
			return total, err
		}
		n := int(tag.RowsAffected())
		total += n
		if n < activityRetentionBatchSize {
			return total, nil
		}
	}
}
