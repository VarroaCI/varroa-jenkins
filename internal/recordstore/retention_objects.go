package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// retentionBatchSize bounds every batched retention delete, so one round
// never holds a lock across an unbounded number of rows.
const retentionBatchSize = 1000

// ObjectDeleter is the subset of blob.Store retention needs: deleting a
// whole object by key. It stays a narrow local interface, not the full
// blob.Store, because retention never reads, writes, or lists an object;
// it only deletes one by key. A *blob.Store value satisfies this
// interface without an import, because Go interface satisfaction needs
// no declared relation.
type ObjectDeleter interface {
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every object under one prefix. Run state is
	// the one class Varroa stores under a prefix it owns whole, so a
	// run's whole set is removed by one call.
	DeletePrefix(ctx context.Context, prefix string) error
}

// deleteExpiredLogObjects deletes both fixed log keys, logs/<run_id> and
// logindex/<run_id>, for every run whose created_at is older than
// cutoff. Delete is idempotent, so a run whose objects a previous round
// already removed costs one no-op call per key. It never calls
// DeletePrefix and never derives an artifact key. A nil store is a no-op,
// which a test exercising only the row-level steps relies on.
func deleteExpiredLogObjects(ctx context.Context, db maintDB, store ObjectDeleter, cutoff time.Time) (int, error) { //nolint:unparam // the count documents step 2's work for a future caller and a test that asserts on it
	if store == nil {
		return 0, nil
	}
	total := 0
	offset := 0
	for {
		rows, err := db.Query(ctx, `
			SELECT run_id FROM varroa.runs
			WHERE created_at < $1
			ORDER BY created_at, run_id
			LIMIT $2 OFFSET $3
		`, cutoff, retentionBatchSize, offset)
		if err != nil {
			return total, fmt.Errorf("select runs for log expiry: %w", err)
		}
		var runIDs []string
		for rows.Next() {
			var runID string
			if err := rows.Scan(&runID); err != nil {
				rows.Close()
				return total, fmt.Errorf("scan run id: %w", err)
			}
			runIDs = append(runIDs, runID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, fmt.Errorf("select runs for log expiry: %w", err)
		}
		rows.Close()

		for _, runID := range runIDs {
			if err := store.Delete(ctx, blob.LogKey(runID)); err != nil {
				return total, fmt.Errorf("delete log object for %s: %w", runID, err)
			}
			if err := store.Delete(ctx, blob.LogIndexKey(runID)); err != nil {
				return total, fmt.Errorf("delete log index object for %s: %w", runID, err)
			}
			total++
		}

		if len(runIDs) < retentionBatchSize {
			return total, nil
		}
		offset += retentionBatchSize
	}
}

// runStateBackstopAge is how long a run stays terminal before the
// backstop removes its run-state objects. The prompt delete runs on the
// terminal commit, so this value only has to cover a recorder that
// stopped between that commit and the delete.
const runStateBackstopAge = time.Hour

// deleteExpiredRunState deletes the run-state prefix of every run that
// has been terminal for longer than runStateBackstopAge. It runs its
// own bounded query over varroa.runs, because the round's log
// enumeration uses a different cutoff. A nil store is a no-op.
//
// The backstop exists because a recorder can stop between the terminal
// commit and the prompt delete.
func deleteExpiredRunState(ctx context.Context, db maintDB, store ObjectDeleter, now time.Time) (int, error) { //nolint:unparam // the count documents the step's work for a test that asserts on it
	if store == nil {
		return 0, nil
	}
	cutoff := now.Add(-runStateBackstopAge)
	total := 0
	offset := 0
	for {
		rows, err := db.Query(ctx, `
			SELECT run_id FROM varroa.runs
			WHERE status IN ('COMPLETED', 'DUPLICATE') AND ended_at IS NOT NULL AND ended_at < $1
			ORDER BY created_at, run_id
			LIMIT $2 OFFSET $3
		`, cutoff, retentionBatchSize, offset)
		if err != nil {
			return total, fmt.Errorf("select runs for run-state expiry: %w", err)
		}
		var runIDs []string
		for rows.Next() {
			var runID string
			if err := rows.Scan(&runID); err != nil {
				rows.Close()
				return total, fmt.Errorf("scan run id: %w", err)
			}
			runIDs = append(runIDs, runID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, fmt.Errorf("select runs for run-state expiry: %w", err)
		}
		rows.Close()

		for _, runID := range runIDs {
			if err := store.DeletePrefix(ctx, blob.RunStatePrefix(runID)); err != nil {
				return total, fmt.Errorf("delete run state for %s: %w", runID, err)
			}
			total++
		}

		if len(runIDs) < retentionBatchSize {
			return total, nil
		}
		offset += retentionBatchSize
	}
}

// deleteExpiredArtifacts deletes the blob object of every run_artifacts
// row older than cutoff, then the row itself, in bounded batches. An
// artifact is one whole object, so this deletes it through Delete on
// run_artifacts.blob_key and never calls DeletePrefix. A nil store is a
// no-op.
func deleteExpiredArtifacts(ctx context.Context, db maintDB, store ObjectDeleter, cutoff time.Time) (int, error) {
	if store == nil {
		return 0, nil
	}
	total := 0
	for {
		rows, err := db.Query(ctx, `
			SELECT run_id, path, blob_key FROM varroa.run_artifacts
			WHERE created_at < $1
			LIMIT $2
		`, cutoff, retentionBatchSize)
		if err != nil {
			return total, fmt.Errorf("select expired artifacts: %w", err)
		}
		type artifactRow struct {
			runID   string
			path    string
			blobKey string
		}
		var batch []artifactRow
		for rows.Next() {
			var r artifactRow
			if err := rows.Scan(&r.runID, &r.path, &r.blobKey); err != nil {
				rows.Close()
				return total, fmt.Errorf("scan expired artifact: %w", err)
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, fmt.Errorf("select expired artifacts: %w", err)
		}
		rows.Close()

		if len(batch) == 0 {
			return total, nil
		}
		for _, r := range batch {
			if err := store.Delete(ctx, r.blobKey); err != nil {
				return total, fmt.Errorf("delete artifact object %s: %w", r.blobKey, err)
			}
			if _, err := db.Exec(ctx, `
				DELETE FROM varroa.run_artifacts WHERE run_id = $1 AND path = $2
			`, r.runID, r.path); err != nil {
				return total, fmt.Errorf("delete artifact row: %w", err)
			}
			total++
		}
		if len(batch) < retentionBatchSize {
			return total, nil
		}
	}
}
