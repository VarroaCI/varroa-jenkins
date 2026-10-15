package recordstore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// reasonTriggerUnreachable is the run.trigger_failed reason code of a
// trigger that reached no Jenkins. It is the only code the backstop
// re-fires: a booting cell answers it, and a later fire can succeed.
const reasonTriggerUnreachable = "TRIGGER_UNREACHABLE"

// maxBackstopRefires bounds the re-fires of one occurrence. The first fire
// does not count, so an occurrence holds at most four failed runs.
const maxBackstopRefires = 3

// backstopAnswerWait bounds the wait for a fire that has not answered. A
// trigger waits at most five minutes for Jenkins on the mite, so a fire
// with no answer after this wait never reached its cell, for example
// because the job's home moved away from it.
const backstopAnswerWait = 10 * time.Minute

// backstopIntentPrefix starts every intent key the backstop writes.
const backstopIntentPrefix = "cron:"

// backstopRefireSpentNote is the text a failed run carries when its
// failure spends the last re-fire of its occurrence.
var backstopRefireSpentNote = fmt.Sprintf("the backstop stopped after %d re-fires of this occurrence", maxBackstopRefires)

// isTransientTriggerReason reports whether a run.trigger_failed reason
// code lets a later fire of the same build succeed. TRIGGER_REJECTED is
// transient for the unstarted-intent re-fire only; the backstop re-fires
// TRIGGER_UNREACHABLE alone.
func isTransientTriggerReason(code string) bool {
	return code == reasonTriggerRejected || code == reasonTriggerUnreachable
}

// backstopRefireSpent reports whether a backstop occurrence whose failed
// runs number attempts has used every re-fire.
func backstopRefireSpent(attempts int) bool {
	return attempts > maxBackstopRefires
}

// backstopOccurrenceOfKey parses the occurrence minute out of a backstop
// intent key for jobPath. It answers false for a key that is not
// cronIntentKey(jobPath, occ) for some occurrence.
func backstopOccurrenceOfKey(intentKey, jobPath string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(intentKey, backstopIntentPrefix+jobPath+":")
	if !ok {
		return time.Time{}, false
	}
	occ, err := time.Parse(time.RFC3339, rest)
	if err != nil || cronIntentKey(jobPath, occ) != intentKey {
		return time.Time{}, false
	}
	return occ, true
}

// hasTriggerFailure reports whether a TRIGGER_FAILED run of cluster
// holds intentKey. A cron key names no cluster, so the test filters on
// it. The predicate names the literal result, so the query uses the
// partial index runs_trigger_failed_intent_key_idx.
func hasTriggerFailure(ctx context.Context, pool *pgxpool.Pool, cluster, intentKey string) (bool, error) {
	var exists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM varroa.runs
			WHERE intent_key = $1 AND result = 'TRIGGER_FAILED' AND cluster = $2
		)
	`, intentKey, cluster).Scan(&exists); err != nil {
		return false, fmt.Errorf("read failed triggers of %q: %w", intentKey, err)
	}
	return exists, nil
}

// backstopFiresOf answers the build.scheduled.backstop rows of one
// occurrence of cluster/jobPath in the last 24 hours: their count and the
// newest row's time. Each row is one published fire.
func backstopFiresOf(ctx context.Context, pool *pgxpool.Pool, cluster, jobPath, occurrenceKey string, now time.Time) (int, time.Time, error) {
	var count int
	var newest *time.Time
	err := pool.QueryRow(ctx, `
		SELECT count(*), max(ts) FROM varroa.activity
		WHERE type = 'build.scheduled.backstop' AND cluster = $1 AND item_path = $2 AND reason = $3 AND ts > $4
	`, cluster, jobPath, occurrenceKey, now.Add(-maxBackstopOccurrenceAge)).Scan(&count, &newest)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("read backstop fires of %q: %w", jobPath, err)
	}
	if newest == nil {
		return count, time.Time{}, nil
	}
	return count, *newest, nil
}

// backstopRetryRow is one backstop occurrence whose fires all failed as
// unreachable, with at least one re-fire left.
type backstopRetryRow struct {
	cluster   string
	tenant    string
	namespace string
	cell      string
	jobPath   string
	intentKey string
	attempts  int
	// newest is the newest failed run's created_at: the UUIDv7 time of
	// its run identifier, which the recorder minted at the fire.
	newest time.Time
}

// listBackstopRetries reads every backstop occurrence of the last 24 hours
// whose failed runs all carry TRIGGER_UNREACHABLE and number at most
// maxBackstopRefires. A permanent failure removes its occurrence for good.
// namespace and cell are the newest failed run's own coordinate.
func listBackstopRetries(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]backstopRetryRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT cluster, tenant, job_path, intent_key,
		       (array_agg(namespace ORDER BY created_at DESC))[1],
		       (array_agg(cell ORDER BY created_at DESC))[1],
		       count(*), max(created_at)
		FROM varroa.runs
		WHERE result = 'TRIGGER_FAILED' AND intent_key LIKE 'cron:%' AND created_at > $1
		GROUP BY cluster, tenant, job_path, intent_key
		HAVING bool_and(payload->>'trigger_reason_code' = $2) AND count(*) <= $3
	`, now.Add(-maxBackstopOccurrenceAge), reasonTriggerUnreachable, maxBackstopRefires)
	if err != nil {
		return nil, fmt.Errorf("list backstop retries: %w", err)
	}
	defer rows.Close()

	var out []backstopRetryRow
	for rows.Next() {
		var r backstopRetryRow
		if err := rows.Scan(&r.cluster, &r.tenant, &r.jobPath, &r.intentKey,
			&r.namespace, &r.cell, &r.attempts, &r.newest); err != nil {
			return nil, fmt.Errorf("scan backstop retry: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list backstop retries: %w", err)
	}
	return out, nil
}

// selectBackstopRetries answers the round's re-fires. A re-fire needs
// every earlier fire answered, one failed run for each activity row of
// the occurrence, or backstopAnswerWait since the newest fire. A fire
// still waiting on a booting cell therefore gets no second fire. The grace window paces the re-fires, and the job's home
// addresses them, as it does a first fire.
func selectBackstopRetries(ctx context.Context, pool *pgxpool.Pool, reader Reader, homes *jobHomeCache,
	now time.Time, logger *slog.Logger) ([]backstopFireCandidate, error) {
	rows, err := listBackstopRetries(ctx, pool, now)
	if err != nil {
		return nil, err
	}

	policies := map[[2]string]tenantSchedulePolicy{}
	var out []backstopFireCandidate
	for _, r := range rows {
		occ, ok := backstopOccurrenceOfKey(r.intentKey, r.jobPath)
		if !ok || occ.Before(now.Add(-maxBackstopOccurrenceAge)) {
			continue
		}

		key := [2]string{r.cluster, r.tenant}
		policy, cached := policies[key]
		if !cached {
			policy, err = readTenantSchedulePolicy(ctx, pool, r.cluster, r.tenant)
			if err != nil {
				logger.Warn("backstop retry: read tenant policy failed", "cluster", r.cluster, "tenant", r.tenant, "error", err)
				continue
			}
			policies[key] = policy
		}
		if !policy.cronBackstop {
			continue
		}

		// A granted intent means some fire reached a claim, so the
		// build runs or ran.
		_, found, err := reader.GetIntentByKey(ctx, r.intentKey)
		if err != nil {
			logger.Warn("backstop retry: read intent failed", "intent_key", r.intentKey, "error", err)
			continue
		}
		if found {
			continue
		}

		fires, lastFire, err := backstopFiresOf(ctx, pool, r.cluster, r.jobPath, occurrenceMinuteKey(occ), now)
		if err != nil {
			logger.Warn("backstop retry: read fires failed", "job_path", r.jobPath, "error", err)
			continue
		}
		// A fire whose activity row failed to write leaves no row, so the
		// newest failed run's mint time also paces the next fire.
		if r.newest.After(lastFire) {
			lastFire = r.newest
		}
		// A fire that never answered still spends its attempt, so the
		// bound counts the fires as well as the failed runs.
		total := max(fires, r.attempts)
		if total > maxBackstopRefires {
			continue
		}
		// Each fire answers with at most one failed run, so fewer failed
		// runs than fires means a fire can still wait on its cell.
		if r.attempts < fires && lastFire.After(now.Add(-backstopAnswerWait)) {
			continue
		}
		if lastFire.After(now.Add(-policy.cronGrace)) {
			continue
		}

		kept, err := dropSupersededCandidates(ctx, pool, r.cluster, r.jobPath, []time.Time{occ}, now)
		if err != nil {
			logger.Warn("backstop retry: read superseding fire failed", "job_path", r.jobPath, "error", err)
			continue
		}
		if len(kept) == 0 {
			continue
		}

		namespace, cell := r.namespace, r.cell
		home, err := homes.resolve(ctx, pool, r.cluster, r.tenant, r.jobPath)
		if err != nil {
			logger.Warn("backstop retry: read job home failed", "job_path", r.jobPath, "error", err)
			continue
		}
		if home.Placed {
			if home.Cell == "" {
				continue
			}
			namespace, cell = home.Namespace, home.Cell
		}

		out = append(out, backstopFireCandidate{
			cluster:    r.cluster,
			tenant:     r.tenant,
			namespace:  namespace,
			cell:       cell,
			jobPath:    r.jobPath,
			occurrence: occ,
			grace:      policy.cronGrace,
			attempt:    total + 1,
		})
	}
	return out, nil
}

// mergeBackstopRetries adds each re-fire to the round's first fires. It
// drops a re-fire of a job whose first fire this round names a newer
// occurrence, because cronCatchUp fires the newest miss of a job.
func mergeBackstopRetries(fires, retries []backstopFireCandidate) []backstopFireCandidate {
	newest := map[[2]string]time.Time{}
	for _, f := range fires {
		key := [2]string{f.cluster, f.jobPath}
		if cur, ok := newest[key]; !ok || f.occurrence.After(cur) {
			newest[key] = f.occurrence
		}
	}
	for _, r := range retries {
		if first, ok := newest[[2]string{r.cluster, r.jobPath}]; ok && !r.occurrence.After(first) {
			continue
		}
		fires = append(fires, r)
	}
	return fires
}
