package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// maxBackstopOccurrenceAge is D11's 24-hour bound: the operator's
// imperative command rides a work-queue stream whose maximum age is 24
// hours, so an older fire can never reach a controller that is still
// asleep.
const maxBackstopOccurrenceAge = 24 * time.Hour

// occurrenceMinuteKey formats occ as D11's occurrence minute: truncated
// to the minute, in UTC, RFC3339.
func occurrenceMinuteKey(occ time.Time) string {
	return occ.UTC().Truncate(time.Minute).Format(time.RFC3339)
}

// cronIntentKey is D11's ledger key for one job's missed occurrence:
// cron:<job-path>:<occurrence-minute>. A cell that fires the same
// occurrence late produces the same key, so the ledger answers
// duplicate and exactly one build runs.
func cronIntentKey(jobPath string, occ time.Time) string {
	return fmt.Sprintf("cron:%s:%s", jobPath, occurrenceMinuteKey(occ))
}

// isBackstopCandidate applies D11's four candidate rules to one exported
// occurrence of cluster/jobPath: older than grace, not older than 24
// hours, no cron:<job-path>:<occurrence-minute> intent, and no
// build.scheduled.backstop activity row for the same cluster, job path,
// and occurrence minute newer than the grace window.
func isBackstopCandidate(ctx context.Context, reader Reader, pool *pgxpool.Pool, cluster, jobPath string, occ, now time.Time, grace time.Duration) (bool, error) {
	if !occ.Before(now.Add(-grace)) {
		return false, nil
	}
	if occ.Before(now.Add(-maxBackstopOccurrenceAge)) {
		return false, nil
	}

	_, found, err := reader.GetIntentByKey(ctx, cronIntentKey(jobPath, occ))
	if err != nil {
		return false, fmt.Errorf("get intent by key: %w", err)
	}
	if found {
		return false, nil
	}

	logged, err := backstopFireRecentlyLogged(ctx, pool, cluster, jobPath, occurrenceMinuteKey(occ), now.Add(-grace))
	if err != nil {
		return false, err
	}
	return !logged, nil
}

// backstopFireRecentlyLogged reports whether a build.scheduled.backstop
// activity row for cluster, jobPath, and occurrenceKey exists with ts
// newer than since. It matches cluster and jobPath, not jobPath alone,
// because the activity table holds every cluster's rows together and a
// job path is unique only within its own cluster. jobPath's own leading
// segment is the tenant, per the cross-tenant-veto convention, so
// matching jobPath already scopes the match to one tenant; a separate
// tenant column does not exist on this table. The test is time-bounded
// on purpose: an action is at-most-once, so a permanent skip would
// strand a lost fire.
func backstopFireRecentlyLogged(ctx context.Context, pool *pgxpool.Pool, cluster, jobPath, occurrenceKey string, since time.Time) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM varroa.activity
			WHERE type = 'build.scheduled.backstop' AND cluster = $1 AND item_path = $2 AND reason = $3 AND ts > $4
		)
	`, cluster, jobPath, occurrenceKey, since).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read backstop activity row: %w", err)
	}
	return exists, nil
}

// selectCandidateOccurrences applies isBackstopCandidate to every
// exported occurrence of one cluster's job schedule row, in the order
// the plugin exported them.
func selectCandidateOccurrences(ctx context.Context, reader Reader, pool *pgxpool.Pool, cluster, jobPath string, occurrences []time.Time, now time.Time, grace time.Duration) ([]time.Time, error) {
	var out []time.Time
	for _, occ := range occurrences {
		ok, err := isBackstopCandidate(ctx, reader, pool, cluster, jobPath, occ, now, grace)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, occ)
		}
	}
	return out, nil
}
