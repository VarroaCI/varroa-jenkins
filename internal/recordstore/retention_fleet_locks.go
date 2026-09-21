package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// sweepFleetLocks runs the fleet-locks maintenance step design.md D11
// and task 4.4 define: it moves every held row whose run is INTERRUPTED
// to releasing first, the same derived move the acquire transaction's
// own step 1 runs, then deletes every expired row that is not
// releasing, in bounded batches. A releasing row survives both the move
// (it is already releasing) and the expiry delete (design.md D11: lease
// expiry never applies to a releasing row). It answers how many rows it
// moved and how many it deleted for expiry.
//
// It publishes one lock notification per row it moved and per row it
// deleted, through notifier, on the same best-effort rule every other
// lock publish follows: a nil notifier publishes nothing, and a failed
// publish never fails the round. The two steps run on one held
// connection, so each step drains its own rows before the next
// statement runs.
func sweepFleetLocks(ctx context.Context, db maintDB, now time.Time, notifier *RunNotifier) (moved, expired int, err error) {
	movedRows, moved, err := moveInterruptedHoldersToReleasing(ctx, db, "", now)
	if err != nil {
		return 0, 0, fmt.Errorf("move interrupted fleet lock holders: %w", err)
	}
	for _, row := range movedRows {
		notifier.Notify(lockNotification(row.cluster, row.tenant, row.jobPath, row.name, row.runID, FleetLockStateReleasing))
	}

	expired, err = deleteExpiredFleetLocksNotifying(ctx, db, now, notifier)
	if err != nil {
		return moved, 0, err
	}
	return moved, expired, nil
}

// deleteExpiredFleetLocksNotifying deletes expired, non-releasing
// fleet_locks rows in bounded batches, and publishes one lock
// notification per deleted row through notifier. It does not use the
// shared deleteBatched: that helper reads only the affected-row count a
// notification cannot name, while a lock notification carries each row's
// own identity, which only RETURNING answers.
//
// A nil notifier publishes nothing, and a publish failure never fails
// the round. Each batch drains and closes its own rows, so the round's
// single held connection is free for the next statement.
func deleteExpiredFleetLocksNotifying(ctx context.Context, db maintDB, now time.Time, notifier *RunNotifier) (int, error) {
	total := 0
	for {
		rows, err := db.Query(ctx, `
			DELETE FROM varroa.fleet_locks WHERE (name, run_id) IN (
				SELECT name, run_id FROM varroa.fleet_locks
				WHERE expires_at < $1 AND state <> 'releasing'
				LIMIT $2
			)
			RETURNING cluster, tenant, job_path, name, run_id
		`, now, retentionBatchSize)
		if err != nil {
			return total, fmt.Errorf("delete expired fleet locks: %w", err)
		}
		n, err := publishDeletedFleetLocks(rows, notifier)
		if err != nil {
			return total, err
		}
		total += n
		if n < retentionBatchSize {
			return total, nil
		}
	}
}

// publishDeletedFleetLocks reads one batch's deleted rows, publishes one
// lock notification per row, and answers how many rows it read. It
// drains and closes rows before it returns.
func publishDeletedFleetLocks(rows pgx.Rows, notifier *RunNotifier) (int, error) {
	defer rows.Close()

	count := 0
	for rows.Next() {
		var row movedFleetLockRow
		if err := rows.Scan(&row.cluster, &row.tenant, &row.jobPath, &row.name, &row.runID); err != nil {
			return count, fmt.Errorf("delete expired fleet locks: %w", err)
		}
		notifier.Notify(lockNotification(row.cluster, row.tenant, row.jobPath, row.name, row.runID, FleetLockStateReleased))
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("delete expired fleet locks: %w", err)
	}
	return count, nil
}
