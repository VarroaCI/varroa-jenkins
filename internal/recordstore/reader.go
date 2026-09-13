package recordstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// minGooseSchemaVersion is the lowest applied goose migration version
// this Reader accepts. Every method returns ErrSchemaTooOld below it.
const minGooseSchemaVersion = 1

// defaultRunPageLimit is ListRuns' page size when the caller gives no
// limit, or a limit of zero or less.
const defaultRunPageLimit = 50

// ErrSchemaTooOld means the record store's applied schema version is
// below this Reader's compiled minimum, or the version could not be
// read at all. A missing goose_db_version table and a database error
// both count, because both mean the reader cannot trust the store.
var ErrSchemaTooOld = errors.New("record store schema is older than this reader requires")

// ErrRunNotFound means GetRun found no run_intents row for the given
// run id.
var ErrRunNotFound = errors.New("run not found")

// Reader is the BFF's read surface over the record tables. C1 owns the
// Go queries; C4 owns the HTTP shapes and the JSON.
type Reader interface {
	ListJobSummaries(ctx context.Context, cluster, tenant string) ([]JobSummary, error)
	GetJobSummary(ctx context.Context, cluster, tenant, jobPath string) (JobSummary, error)
	ListRuns(ctx context.Context, q RunQuery) (RunPage, error)
	GetRun(ctx context.Context, runID string) (Run, []Stage, error)
	ListArtifacts(ctx context.Context, runID string) ([]Artifact, error)
	ListTests(ctx context.Context, runID string) ([]TestSuite, error)
	GetIntentByKey(ctx context.Context, intentKey string) (Intent, bool, error)
	NextBuildNumber(ctx context.Context, cluster, tenant, jobPath string) (int64, error)
	SchemaVersion(ctx context.Context) (int64, error)

	// ListActivity pages varroa.activity by keyset on (ts, event_key).
	ListActivity(ctx context.Context, q ActivityQuery) (ActivityPage, error)
	// ActivityRetentionDays reports the BFF's configured activity retention
	// window in days, from VARROA_ACTIVITY_RETENTION_DAYS.
	ActivityRetentionDays(ctx context.Context) (int, error)

	// ListJobSchedules lists a tenant's job_schedules rows for cluster,
	// ordered by job path then trigger type. It is the complete job
	// inventory the tenant's cell(s) report, including a job that has
	// never run and carries no trigger.
	ListJobSchedules(ctx context.Context, cluster, tenant string) ([]JobSchedule, error)

	// SearchRecords returns up to five job, run, and activity rows
	// matching q.Query, scoped by q.Scope. It backs the "job", "run", and
	// "activity" global-search kinds.
	SearchRecords(ctx context.Context, q SearchQuery) (SearchResults, error)

	// JobWindowStats summarizes jobPath's runs created at or after
	// since: the pipeline job page's "Last 30 days" card.
	JobWindowStats(ctx context.Context, cluster, tenant, jobPath string, since time.Time) (JobWindowStats, error)

	// JobDependencies reports jobPath's cross-tenant dependency edges:
	// its own varroa-upstream trigger row, and every other job's
	// varroa-upstream row that names it as the producer. It backs the
	// pipeline job page's dependency edge (design D13).
	JobDependencies(ctx context.Context, cluster, tenant, jobPath string) (JobDependencies, error)

	// CellUsage reports every tenant the placement table binds to cell,
	// each tenant's four load counts, and the same four summed. The
	// window is a fixed 24 hours. No caller chooses another window.
	CellUsage(ctx context.Context, cluster, namespace, cell string) (CellUsage, error)

	// CellUsageMany reports the same value for each of cells, in one
	// statement. The map key is the cell name. A cell the placement
	// table binds to no tenant still carries an entry, with no tenant
	// and four zero totals.
	CellUsageMany(ctx context.Context, cluster, namespace string, cells []string) (map[string]CellUsage, error)

	// CellTenantUsage reports one row for each of a tenant's jobs on one
	// cell. The row set is the tenant's job inventory plus every job the
	// window's runs and the running runs name. It answers no row for a
	// tenant the placement table does not bind to cell, never an error:
	// the caller decides whether the tenant belongs there. A caller that
	// may read only part of the tenant's jobs sums the rows it may read.
	CellTenantUsage(ctx context.Context, cluster, namespace, cell, tenant string) ([]CellJobUsage, error)
}

// pgReader is the Postgres-backed Reader implementation.
type pgReader struct {
	pool *pgxpool.Pool
	// activityRetentionDays is the BFF's configured activity retention
	// window, from VARROA_ACTIVITY_RETENTION_DAYS. It bounds how old an
	// incoming ListActivity cursor may be before ErrCursorExpired.
	activityRetentionDays int
}

// NewReader returns a Reader over pool. activityRetentionDays is the BFF's
// configured VARROA_ACTIVITY_RETENTION_DAYS value; omit it (or pass 0) when
// the caller has none, e.g. every non-activity Reader use today.
func NewReader(pool *pgxpool.Pool, activityRetentionDays ...int) Reader {
	days := 0
	if len(activityRetentionDays) > 0 {
		days = activityRetentionDays[0]
	}
	return &pgReader{pool: pool, activityRetentionDays: days}
}

// SchemaVersion reads the highest applied migration version from
// varroa.goose_db_version. It returns ErrSchemaTooOld, wrapped with the
// observed version, when that version is below minGooseSchemaVersion. It
// returns ErrSchemaTooOld, wrapped with the underlying failure, when the
// version table is missing or the database returns any other error,
// because both mean this Reader cannot trust the store.
func (r *pgReader) SchemaVersion(ctx context.Context) (int64, error) {
	var version int64
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(version_id), 0) FROM varroa.goose_db_version WHERE is_applied
	`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("%w: read schema version: %v", ErrSchemaTooOld, err) //nolint:errorlint // %v, not %w: the wrapped ErrSchemaTooOld is the one sentinel callers match on
	}
	if version < minGooseSchemaVersion {
		return 0, fmt.Errorf("%w: applied version %d is below %d", ErrSchemaTooOld, version, minGooseSchemaVersion)
	}
	return version, nil
}

// checkSchema returns SchemaVersion's error unchanged. Every Reader
// method but SchemaVersion itself calls this first.
func (r *pgReader) checkSchema(ctx context.Context) error {
	_, err := r.SchemaVersion(ctx)
	return err
}

// runColumns lists varroa.runs' columns in the order scanRun expects.
const runColumns = `run_id, created_at, cluster, namespace, cell, tenant, job_path, build_number, branch,
	scm_revision, status, result, queued_at, started_at, ended_at, last_heartbeat_at,
	interrupted_superseded_at, duration_ms, actor, cause, params, changes,
	tests_total, tests_failed, tests_skipped, payload`

// qualifiedRunColumns is the same columns as runColumns, each qualified
// with the "runs" alias, so ListRuns' query stays unambiguous once it
// joins run_intents.
const qualifiedRunColumns = `runs.run_id, runs.created_at, runs.cluster, runs.namespace, runs.cell, runs.tenant,
	runs.job_path, runs.build_number, runs.branch, runs.scm_revision, runs.status, runs.result,
	runs.queued_at, runs.started_at, runs.ended_at, runs.last_heartbeat_at,
	runs.interrupted_superseded_at, runs.duration_ms, runs.actor, runs.cause, runs.params,
	runs.changes, runs.tests_total, runs.tests_failed, runs.tests_skipped, runs.payload`

// scanRun scans one row shaped by runColumns into a Run.
func scanRun(row pgx.Row) (Run, error) {
	var run Run
	var runID uuid.UUID
	err := row.Scan(
		&runID, &run.CreatedAt, &run.Cluster, &run.Namespace, &run.Cell, &run.Tenant, &run.JobPath,
		&run.BuildNumber, &run.Branch, &run.SCMRevision, &run.Status, &run.Result, &run.QueuedAt,
		&run.StartedAt, &run.EndedAt, &run.LastHeartbeatAt, &run.InterruptedSupersededAt, &run.DurationMs,
		&run.Actor, &run.Cause, &run.Params, &run.Changes, &run.TestsTotal, &run.TestsFailed,
		&run.TestsSkipped, &run.Payload,
	)
	if err != nil {
		return Run{}, err
	}
	run.RunID = runID.String()
	return run, nil
}

// GetIntentByKey is one indexed read on run_intents by intentKey. It
// returns found false, with no error, for an unknown key.
func (r *pgReader) GetIntentByKey(ctx context.Context, intentKey string) (Intent, bool, error) {
	if err := r.checkSchema(ctx); err != nil {
		return Intent{}, false, err
	}
	var it Intent
	var runID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT run_id, state, created_at FROM varroa.run_intents WHERE intent_key = $1
	`, intentKey).Scan(&runID, &it.State, &it.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Intent{}, false, nil
	case err != nil:
		return Intent{}, false, fmt.Errorf("get intent by key: %w", err)
	}
	it.IntentKey = intentKey
	it.RunID = runID.String()
	return it, true, nil
}

// GetRun resolves created_at from run_intents first, using its unique
// run_id index, then queries runs with that exact value, so Postgres
// prunes to one partition. For a run that holds an intent, the run_id
// timestamp is never used to derive the partition, because the caller
// mints the identifier and the recorder writes created_at.
//
// A run may hold no intent. A failed trigger writes a runs row and no
// run_intents row, so the first read finds nothing. GetRun then falls
// back in two steps: a bounded query around the identifier's own UUIDv7
// time, then an unbounded one. The bound is a fast path alone, and it
// never decides whether a row is readable, because nothing bounds the
// time from a command's delivery to its result.
func (r *pgReader) GetRun(ctx context.Context, runID string) (Run, []Stage, error) {
	if err := r.checkSchema(ctx); err != nil {
		return Run{}, nil, err
	}

	var createdAt time.Time
	err := r.pool.QueryRow(ctx, `SELECT created_at FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&createdAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return r.getRunWithoutIntent(ctx, runID)
	case err != nil:
		return Run{}, nil, fmt.Errorf("resolve run created_at: %w", err)
	}

	run, err := scanRun(r.pool.QueryRow(ctx, `
		SELECT `+runColumns+`
		FROM varroa.runs WHERE run_id = $1 AND created_at = $2
	`, runID, createdAt))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Run{}, nil, ErrRunNotFound
	case err != nil:
		return Run{}, nil, fmt.Errorf("read run: %w", err)
	}

	stages, err := r.listStages(ctx, runID)
	if err != nil {
		return Run{}, nil, err
	}
	return run, stages, nil
}

// getRunWithoutIntent answers a run that holds no run_intents row. It runs
// the bounded query first, and the unbounded one on a miss. An identifier
// that is not a UUIDv7 skips the bounded step.
func (r *pgReader) getRunWithoutIntent(ctx context.Context, runID string) (Run, []Stage, error) {
	if from, to, ok := runWindow(runID); ok {
		run, err := scanRun(r.pool.QueryRow(ctx, `
			SELECT `+runColumns+`
			FROM varroa.runs
			WHERE run_id = $1 AND created_at >= $2 AND created_at <= $3
			ORDER BY created_at DESC
			LIMIT 1
		`, runID, from, to))
		switch {
		case err == nil:
			return r.runWithStages(ctx, runID, run)
		case !errors.Is(err, pgx.ErrNoRows):
			return Run{}, nil, fmt.Errorf("read run in window: %w", err)
		}
	}

	run, err := scanRun(r.pool.QueryRow(ctx, `
		SELECT `+runColumns+`
		FROM varroa.runs
		WHERE run_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, runID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Run{}, nil, ErrRunNotFound
	case err != nil:
		return Run{}, nil, fmt.Errorf("read run: %w", err)
	}
	return r.runWithStages(ctx, runID, run)
}

// runWithStages returns run beside its stage list.
func (r *pgReader) runWithStages(ctx context.Context, runID string, run Run) (Run, []Stage, error) {
	stages, err := r.listStages(ctx, runID)
	if err != nil {
		return Run{}, nil, err
	}
	return run, stages, nil
}

// listStages reads every run_stages row for runID, ordered by
// created_at.
func (r *pgReader) listStages(ctx context.Context, runID string) ([]Stage, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, stage_id, created_at, parent_id, name, node_type, status,
		       started_at, ended_at, duration_ms, error, log_offset, log_length, payload
		FROM varroa.run_stages
		WHERE run_id = $1
		ORDER BY created_at
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list stages: %w", err)
	}
	defer rows.Close()

	var stages []Stage
	for rows.Next() {
		var s Stage
		var stageRunID uuid.UUID
		if err := rows.Scan(&stageRunID, &s.StageID, &s.CreatedAt, &s.ParentID, &s.Name, &s.NodeType,
			&s.Status, &s.StartedAt, &s.EndedAt, &s.DurationMs, &s.Error, &s.LogOffset, &s.LogLength, &s.Payload); err != nil {
			return nil, fmt.Errorf("scan stage: %w", err)
		}
		s.RunID = stageRunID.String()
		stages = append(stages, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stages: %w", err)
	}
	return stages, nil
}

// ListArtifacts reads every run_artifacts row for runID, ordered by
// path.
func (r *pgReader) ListArtifacts(ctx context.Context, runID string) ([]Artifact, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, path, created_at, size_bytes, digest, content_type, blob_key
		FROM varroa.run_artifacts
		WHERE run_id = $1
		ORDER BY path
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()

	var artifacts []Artifact
	for rows.Next() {
		var a Artifact
		var artifactRunID uuid.UUID
		if err := rows.Scan(&artifactRunID, &a.Path, &a.CreatedAt, &a.SizeBytes, &a.Digest, &a.ContentType, &a.BlobKey); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		a.RunID = artifactRunID.String()
		artifacts = append(artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	return artifacts, nil
}

// ListTests reads every run_tests row for runID, ordered by suite.
func (r *pgReader) ListTests(ctx context.Context, runID string) ([]TestSuite, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, suite, created_at, total, failed, skipped, duration_ms
		FROM varroa.run_tests
		WHERE run_id = $1
		ORDER BY suite
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list tests: %w", err)
	}
	defer rows.Close()

	var suites []TestSuite
	for rows.Next() {
		var s TestSuite
		var suiteRunID uuid.UUID
		if err := rows.Scan(&suiteRunID, &s.Suite, &s.CreatedAt, &s.Total, &s.Failed, &s.Skipped, &s.DurationMs); err != nil {
			return nil, fmt.Errorf("scan test suite: %w", err)
		}
		s.RunID = suiteRunID.String()
		suites = append(suites, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tests: %w", err)
	}
	return suites, nil
}

// NextBuildNumber reads job_numbers.next_number, the permanent
// watermark. It reads no intent and no run, so retention cannot reset
// it.
func (r *pgReader) NextBuildNumber(ctx context.Context, cluster, tenant, jobPath string) (int64, error) {
	if err := r.checkSchema(ctx); err != nil {
		return 0, err
	}
	next, err := NextBuildNumber(ctx, r.pool, cluster, tenant, jobPath)
	if err != nil {
		return 0, fmt.Errorf("next build number: %w", err)
	}
	return next, nil
}

// runCursor is the decoded form of a ListRuns page cursor.
type runCursor struct {
	createdAt time.Time
	runID     string
}

// encodeRunCursor base64-encodes createdAt and runID as ListRuns'
// keyset cursor.
func encodeRunCursor(createdAt time.Time, runID string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + runID
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// decodeRunCursor reverses encodeRunCursor.
func decodeRunCursor(cursor string) (runCursor, error) {
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return runCursor{}, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return runCursor{}, fmt.Errorf("invalid cursor format")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return runCursor{}, fmt.Errorf("invalid cursor timestamp: %w", err)
	}
	return runCursor{createdAt: createdAt, runID: parts[1]}, nil
}

// ListRuns pages varroa.runs by keyset on (created_at DESC, run_id
// DESC). The five filters branch, result, actor, cause, and status all
// combine. cause filters run_intents.cause through a join, because
// runs.cause holds the free-form Jenkins Cause object instead of a
// plain classification.
func (r *pgReader) ListRuns(ctx context.Context, q RunQuery) (RunPage, error) {
	if err := r.checkSchema(ctx); err != nil {
		return RunPage{}, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultRunPageLimit
	}

	args := []any{q.Cluster, q.Tenant}
	clauses := []string{"runs.cluster = $1", "runs.tenant = $2"}
	addFilter := func(column, value string) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if q.JobPath != "" {
		addFilter("runs.job_path", q.JobPath)
	}
	if q.Branch != "" {
		addFilter("runs.branch", q.Branch)
	}
	if q.Result != "" {
		addFilter("runs.result", q.Result)
	}
	if q.Actor != "" {
		addFilter("runs.actor", q.Actor)
	}
	if q.Status != "" {
		addFilter("runs.status", q.Status)
	}

	join := ""
	if q.Cause != "" {
		join = "JOIN varroa.run_intents ri ON ri.intent_key = runs.intent_key"
		addFilter("ri.cause", q.Cause)
	}

	if q.Cursor != "" {
		cur, err := decodeRunCursor(q.Cursor)
		if err != nil {
			return RunPage{}, fmt.Errorf("decode cursor: %w", err)
		}
		args = append(args, cur.createdAt, cur.runID)
		clauses = append(clauses, fmt.Sprintf("(runs.created_at, runs.run_id) < ($%d, $%d)", len(args)-1, len(args)))
	}

	args = append(args, limit+1)
	sql := fmt.Sprintf(`
		SELECT %s
		FROM varroa.runs
		%s
		WHERE %s
		ORDER BY runs.created_at DESC, runs.run_id DESC
		LIMIT $%d
	`, qualifiedRunColumns, join, strings.Join(clauses, " AND "), len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return RunPage{}, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var page RunPage
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return RunPage{}, fmt.Errorf("scan run: %w", err)
		}
		page.Runs = append(page.Runs, run)
	}
	if err := rows.Err(); err != nil {
		return RunPage{}, fmt.Errorf("list runs: %w", err)
	}

	if len(page.Runs) > limit {
		last := page.Runs[limit-1]
		page.NextCursor = encodeRunCursor(last.CreatedAt, last.RunID)
		page.Runs = page.Runs[:limit]
	}
	return page, nil
}
