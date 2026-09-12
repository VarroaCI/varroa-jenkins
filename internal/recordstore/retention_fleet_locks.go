package recordstore

import (
	"context"
	"fmt"
	"time"
)

// sweepFleetLocks runs the fleet-locks maintenance step design.md D11
// and task 4.4 define: it moves every held row whose run is INTERRUPTED
// to releasing first, the same derived move the acquire transaction's
// own step 1 runs, then deletes every expired row that is not
// releasing, in bounded batches. A releasing row survives both the move
// (it is already releasing) and the expiry delete (design.md D11: lease
// expiry never applies to a releasing row). It answers how many rows it
// moved and how many it deleted for expiry.
func sweepFleetLocks(ctx context.Context, db maintDB, now time.Time) (moved, expired int, err error) {
	moved, err = moveInterruptedHoldersToReleasing(ctx, db, "", now)
	if err != nil {
		return 0, 0, fmt.Errorf("move interrupted fleet lock holders: %w", err)
	}

	expired, err = deleteBatched(ctx, db, `
		DELETE FROM varroa.fleet_locks WHERE (name, run_id) IN (
			SELECT name, run_id FROM varroa.fleet_locks
			WHERE expires_at < $1 AND state <> 'releasing'
			LIMIT $2
		)
	`, now)
	if err != nil {
		return moved, 0, fmt.Errorf("delete expired fleet locks: %w", err)
	}
	return moved, expired, nil
}
