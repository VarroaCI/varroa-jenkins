package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// maxRehydrationJobPaths bounds one ListRehydrationRecords call to a
// non-empty job-path list, so one reply fits one bus message.
const maxRehydrationJobPaths = 32

// maxRehydrationChanges bounds the change entries one RehydrationRecord
// carries, so one changelog file stays small inside the bundle budget.
const maxRehydrationChanges = 100

// RehydrationRecord is one thin build record ListRehydrationRecords
// returns. It carries no stage row, no test row, and no artifact row: a
// thin record needs none of them.
type RehydrationRecord struct {
	BuildNumber          int64
	QueuedAt             *time.Time
	StartedAt            *time.Time
	EndedAt              *time.Time
	DurationMs           *int64
	Result               string
	CauseDescription     string
	Params               json.RawMessage
	MaskedParameterNames []string
	Changes              json.RawMessage
}

// RehydrationJobRecords is one job path's answer: its next build number
// and the records ListRehydrationRecords selected for it, newest first.
type RehydrationJobRecords struct {
	JobPath         string
	NextBuildNumber int64
	Records         []RehydrationRecord
}

// ListRehydrationRecords reads the record store's rehydration seed for
// cluster and tenant. It is not a Reader method: only the tenant.sync
// responder calls it, per specs/build-record-store/spec.md.
//
// A non-empty jobPaths, at most maxRehydrationJobPaths entries, returns
// the newest completed runs of each job path up to perJob, plus the
// newest SUCCESS run of that job path when it falls outside the limit.
// An empty jobPaths enumerates: it returns every distinct job path of
// the tenant that holds a job_numbers row or a completed run, with its
// next build number and no record.
func ListRehydrationRecords(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string, jobPaths []string, perJob int) ([]RehydrationJobRecords, error) {
	if len(jobPaths) > maxRehydrationJobPaths {
		return nil, fmt.Errorf("rehydration records request names %d job paths, more than the %d cap", len(jobPaths), maxRehydrationJobPaths)
	}
	if len(jobPaths) == 0 {
		return enumerateRehydrationJobs(ctx, pool, cluster, tenant)
	}
	return rehydrationRecordsForPaths(ctx, pool, cluster, tenant, jobPaths, perJob)
}

// enumerateRehydrationJobs lists every distinct job path of cluster and
// tenant that holds a job_numbers row or a completed run. The
// job_numbers half is necessary: a job whose every claimed build was
// lost before it completed holds a watermark above one and no completed
// run, and a completed-run-only enumeration would miss it.
//
// A TRIGGER_FAILED row is no completed run. It records a build that never
// started, it holds a null build number, and the mite refuses a seeded
// build numbered zero, which fails the whole rehydration bundle.
func enumerateRehydrationJobs(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string) ([]RehydrationJobRecords, error) {
	rows, err := pool.Query(ctx, `
		SELECT job_path FROM varroa.job_numbers WHERE cluster = $1 AND tenant = $2
		UNION
		SELECT job_path FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND status = 'COMPLETED' AND `+notTriggerFailedSQL+`
		ORDER BY job_path
	`, cluster, tenant)
	if err != nil {
		return nil, fmt.Errorf("enumerate rehydration job paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan rehydration job path: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enumerate rehydration job paths: %w", err)
	}

	out := make([]RehydrationJobRecords, 0, len(paths))
	for _, p := range paths {
		next, err := NextBuildNumber(ctx, pool, cluster, tenant, p)
		if err != nil {
			return nil, fmt.Errorf("next build number for %s: %w", p, err)
		}
		out = append(out, RehydrationJobRecords{JobPath: p, NextBuildNumber: next})
	}
	return out, nil
}

// rehydrationRecordsForPaths answers a non-empty job-path request. It
// reads the next build number of every requested path through
// NextBuildNumber, never from run_intents or runs, then runs one query
// with a window function per job path to select each path's records.
//
// It excludes a TRIGGER_FAILED row, which seeds no build. Such a row is
// always the newest record of its job, so it would otherwise take the
// newest slot of every per-job budget.
func rehydrationRecordsForPaths(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string, jobPaths []string, perJob int) ([]RehydrationJobRecords, error) {
	byPath := make(map[string]*RehydrationJobRecords, len(jobPaths))
	order := make([]string, 0, len(jobPaths))
	for _, p := range jobPaths {
		if _, ok := byPath[p]; ok {
			continue
		}
		next, err := NextBuildNumber(ctx, pool, cluster, tenant, p)
		if err != nil {
			return nil, fmt.Errorf("next build number for %s: %w", p, err)
		}
		byPath[p] = &RehydrationJobRecords{JobPath: p, NextBuildNumber: next}
		order = append(order, p)
	}

	rows, err := pool.Query(ctx, `
		WITH ranked AS (
			SELECT
				job_path, created_at, build_number, queued_at, started_at, ended_at, duration_ms,
				result, cause, params, changes, payload,
				ROW_NUMBER() OVER (PARTITION BY job_path ORDER BY created_at DESC) AS newest_rank,
				ROW_NUMBER() OVER (PARTITION BY job_path ORDER BY (result = 'SUCCESS') DESC, created_at DESC) AS success_rank
			FROM varroa.runs
			WHERE cluster = $1 AND tenant = $2 AND job_path = ANY($3) AND status = 'COMPLETED'
			  AND `+notTriggerFailedSQL+`
		)
		SELECT job_path, build_number, queued_at, started_at, ended_at, duration_ms, result, cause, params, changes, payload
		FROM ranked
		WHERE newest_rank <= $4 OR (success_rank = 1 AND result = 'SUCCESS')
		ORDER BY job_path, created_at DESC
	`, cluster, tenant, jobPaths, perJob)
	if err != nil {
		return nil, fmt.Errorf("list rehydration records: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			jobPath     string
			buildNumber *int64
			queuedAt    *time.Time
			startedAt   *time.Time
			endedAt     *time.Time
			durationMs  *int64
			result      *string
			cause       json.RawMessage
			params      json.RawMessage
			changes     json.RawMessage
			payload     json.RawMessage
		)
		if err := rows.Scan(&jobPath, &buildNumber, &queuedAt, &startedAt, &endedAt, &durationMs,
			&result, &cause, &params, &changes, &payload); err != nil {
			return nil, fmt.Errorf("scan rehydration record: %w", err)
		}
		target, ok := byPath[jobPath]
		if !ok {
			// The window function ranked a job path this call did not
			// request. That cannot happen given the WHERE clause above,
			// but skipping is cheaper than a panic.
			continue
		}
		rec := RehydrationRecord{
			QueuedAt:             queuedAt,
			StartedAt:            startedAt,
			EndedAt:              endedAt,
			DurationMs:           durationMs,
			Params:               params,
			Changes:              truncateRehydrationChanges(changes),
			CauseDescription:     decodeCauseDescription(cause),
			MaskedParameterNames: decodeMaskedParameterNames(payload),
		}
		if buildNumber != nil {
			rec.BuildNumber = *buildNumber
		}
		if result != nil {
			rec.Result = *result
		}
		target.Records = append(target.Records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rehydration records: %w", err)
	}

	out := make([]RehydrationJobRecords, 0, len(order))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	return out, nil
}

// decodeCauseDescription decodes runs.cause, a JSON-encoded plain
// string carrying the run's recorded cause description, into that
// string. It answers empty for a null or malformed value.
func decodeCauseDescription(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// decodeMaskedParameterNames reads the masked parameter names the mite
// recorded on the run.started envelope, stored at
// runs.payload."run.started".payload.masked_params. It answers nil when
// the envelope carried none.
func decodeMaskedParameterNames(payload json.RawMessage) []string {
	if len(payload) == 0 {
		return nil
	}
	var envelope struct {
		RunStarted struct {
			Payload struct {
				MaskedParams []string `json:"masked_params"`
			} `json:"payload"`
		} `json:"run.started"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil
	}
	return envelope.RunStarted.Payload.MaskedParams
}

// truncateRehydrationChanges caps raw, a JSON array of change entries
// stored newest first, to maxRehydrationChanges entries. It answers raw
// unchanged when it holds no array or fewer entries than the cap.
func truncateRehydrationChanges(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return raw
	}
	if len(entries) <= maxRehydrationChanges {
		return raw
	}
	truncated, err := json.Marshal(entries[:maxRehydrationChanges])
	if err != nil {
		return raw
	}
	return truncated
}
