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
	// the one class Varroa stores under a prefix it owns whole. A
	// terminal cleanup keeps the run's build.xml and deletes every other
	// object by key. The retention round, the run-state cap, and a run
	// whose record names no build record delete the whole prefix.
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

// deleteRetainedRunScripts deletes the run-state prefix of every run whose
// created_at passes the record retention cutoff. The terminal cleanup keeps
// one build.xml for a replay, and this round removes it. The prefix delete
// also removes a superseded object whose own per-key delete a dispatched
// resume held.
//
// The query tests created_at alone. The same round drops the run's own
// partition on the same cutoff, so a retained object that survived this
// call would outlive its row and no later round would find it. A nil
// store is a no-op.
func deleteRetainedRunScripts(ctx context.Context, db maintDB, store ObjectDeleter, cutoff time.Time) (int, error) { //nolint:unparam // the count documents the step's work for a test that asserts on it
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
			return total, fmt.Errorf("select runs for retained script expiry: %w", err)
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
			return total, fmt.Errorf("select runs for retained script expiry: %w", err)
		}
		rows.Close()

		for _, runID := range runIDs {
			if err := store.DeletePrefix(ctx, blob.RunStatePrefix(runID)); err != nil {
				return total, fmt.Errorf("delete retained run state for %s: %w", runID, err)
			}
			if err := store.DeletePrefix(ctx, blob.RestartSourcePrefix(runID)); err != nil {
				return total, fmt.Errorf("delete retained restart copy for %s: %w", runID, err)
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

// deleteExpiredRunState deletes the replicated state of every run that
// has been terminal for longer than runStateBackstopAge, through
// deleteRunStateExceptScript, so the run keeps its build.xml. It runs its
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
			SELECT run_id, payload->'mesh'->'files' FROM varroa.runs
			WHERE status IN ('COMPLETED', 'DUPLICATE') AND ended_at IS NOT NULL AND ended_at < $1
			ORDER BY created_at, run_id
			LIMIT $2 OFFSET $3
		`, cutoff, retentionBatchSize, offset)
		if err != nil {
			return total, fmt.Errorf("select runs for run-state expiry: %w", err)
		}
		type expiredRun struct {
			runID string
			files []runStateFile
		}
		var batch []expiredRun
		for rows.Next() {
			var (
				runID    string
				filesRaw []byte
			)
			if err := rows.Scan(&runID, &filesRaw); err != nil {
				rows.Close()
				return total, fmt.Errorf("scan run id: %w", err)
			}
			batch = append(batch, expiredRun{runID: runID, files: runStateFilesOrEmpty(filesRaw)})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, fmt.Errorf("select runs for run-state expiry: %w", err)
		}
		rows.Close()

		for _, r := range batch {
			if err := deleteRunStateExceptScript(ctx, store, r.runID, r.files); err != nil {
				return total, fmt.Errorf("delete run state for %s: %w", r.runID, err)
			}
			total++
		}

		if len(batch) < retentionBatchSize {
			return total, nil
		}
		offset += retentionBatchSize
	}
}

// TestReportPath is the archived path of a run's per-case test report.
// The plugin archives the report under this path, and the BFF names it
// on the run's test read.
const TestReportPath = InternalArtifactPrefix + "tests.json"

// deleteExpiredArtifacts deletes the blob object of every run_artifacts
// row older than cutoff, then the row itself, in bounded batches. An
// artifact is one whole object, so this deletes it through Delete on
// run_artifacts.blob_key and never calls DeletePrefix. A nil store is a
// no-op. When the deleted row is the run's test report, the run's
// test_report_key is cleared too, so a read never offers a download of
// an object that is gone.
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
			// The key clears before the row leaves. A clear that fails keeps
			// the row, so the next round selects it again and retries.
			if r.path == TestReportPath {
				if _, err := db.Exec(ctx, `
					UPDATE varroa.runs SET test_report_key = NULL WHERE run_id = $1 AND test_report_key = $2
				`, r.runID, r.blobKey); err != nil {
					return total, fmt.Errorf("clear test report key: %w", err)
				}
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

// deleteRunStateExceptScript deletes every object the run's own mesh file
// list names, except the newest build.xml. The script replica outlives the
// run, because a replay reads it. The record's file list names each
// object's epoch and generation, so every delete is one key and the store
// needs no prefix listing.
//
// A nil files deletes the whole prefix: the run-state cap passes nil,
// because its purpose is to free the run's whole storage. A caller that
// reads the record passes an empty non-nil list for a NULL or undecodable
// column. Such a list, like any list that names no build.xml, also
// deletes the whole prefix. Nothing is retained then, and the prefix
// delete also removes a superseded object whose own per-key delete a
// dispatched resume held.
//
// A failed delete of one key does not stop the other deletes. The
// function then answers the first failure, and the error names how many
// keys failed out of how many it tried, so the caller's one log line
// carries the whole result. The function itself logs nothing.
func deleteRunStateExceptScript(ctx context.Context, store ObjectDeleter, runID string, files []runStateFile) error {
	if files == nil {
		return store.DeletePrefix(ctx, blob.RunStatePrefix(runID))
	}
	kept, ok := retainedScriptEntry(files)
	if !ok {
		return store.DeletePrefix(ctx, blob.RunStatePrefix(runID))
	}
	var (
		firstKey string
		firstErr error
		failed   int
		tried    int
	)
	for _, f := range files {
		// An entry whose epoch or generation is not positive names no
		// object, because the object key carries both numbers.
		if f.sameObject(kept) || f.Epoch <= 0 || f.StateGen <= 0 {
			continue
		}
		key := blob.RunStateKey(runID, f.Epoch, f.StateGen, f.RelPath)
		tried++
		if err := store.Delete(ctx, key); err != nil {
			failed++
			if firstErr == nil {
				firstKey, firstErr = key, err
			}
		}
	}
	if firstErr == nil {
		return nil
	}
	return fmt.Errorf("delete run-state object %s (%d of %d keys failed): %w", firstKey, failed, tried, firstErr)
}
