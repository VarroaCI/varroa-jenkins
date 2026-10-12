package recordstore

import (
	"context"
	"fmt"
	"time"
)

// runWaitStaleAge is how long a wait row may go without a claim refresh
// before the sweep deletes it. A capped run retries every 10 seconds, so
// an hour without a refresh means the run is gone.
const runWaitStaleAge = time.Hour

// sweepRunWaits deletes the wait rows that no claim has refreshed for
// runWaitStaleAge. It answers the number of rows it deleted.
func sweepRunWaits(ctx context.Context, db maintDB, now time.Time) (int, error) {
	tag, err := db.Exec(ctx, `DELETE FROM varroa.run_waits WHERE last_seen_at < $1`,
		now.Add(-runWaitStaleAge))
	if err != nil {
		return 0, fmt.Errorf("delete stale run waits: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
