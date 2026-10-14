package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// defaultRestartSourceRetention is the window a restart copy lasts when the
// configuration names none.
const defaultRestartSourceRetention = 7 * 24 * time.Hour

// deleteExpiredRestartSources deletes the restart copy of every run whose
// end, or creation when the run never ended, is before cutoff. For each run
// it deletes the copy's whole prefix, then clears the run's manifest. A
// failed delete returns at once, so the next round retries that run while
// its manifest still names the copy. It reports the number of runs it
// cleared.
func deleteExpiredRestartSources(ctx context.Context, db maintDB, store ObjectDeleter, cutoff time.Time) (int, error) {
	if store == nil {
		return 0, nil
	}
	type expiredCopy struct {
		runID     string
		createdAt time.Time
	}
	total := 0
	for {
		// Each cleared row leaves the predicate, so no offset is needed.
		rows, err := db.Query(ctx, `
			SELECT run_id, created_at FROM varroa.runs
			WHERE payload ? 'restart_source' AND COALESCE(ended_at, created_at) < $1
			ORDER BY created_at, run_id
			LIMIT $2
		`, cutoff, retentionBatchSize)
		if err != nil {
			return total, fmt.Errorf("select runs for restart copy expiry: %w", err)
		}
		var batch []expiredCopy
		for rows.Next() {
			var c expiredCopy
			if err := rows.Scan(&c.runID, &c.createdAt); err != nil {
				rows.Close()
				return total, fmt.Errorf("scan restart copy run: %w", err)
			}
			batch = append(batch, c)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, fmt.Errorf("select runs for restart copy expiry: %w", err)
		}
		rows.Close()

		for _, c := range batch {
			if err := store.DeletePrefix(ctx, blob.RestartSourcePrefix(c.runID)); err != nil {
				return total, fmt.Errorf("delete restart copy for %s: %w", c.runID, err)
			}
			if _, err := db.Exec(ctx, `
				UPDATE varroa.runs SET payload = payload - 'restart_source'
				 WHERE run_id = $1 AND created_at = $2
			`, c.runID, c.createdAt); err != nil {
				return total, fmt.Errorf("clear restart copy manifest for %s: %w", c.runID, err)
			}
			total++
		}

		if len(batch) < retentionBatchSize {
			return total, nil
		}
	}
}
