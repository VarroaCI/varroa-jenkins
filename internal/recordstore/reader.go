package recordstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/pgdial"
)

// minGooseSchemaVersion is the lowest applied goose migration version
// this Reader accepts. Every method returns ErrSchemaTooOld below it until
// a check passes.
const minGooseSchemaVersion = 1

// defaultRunPageLimit is ListRuns' page size when the caller gives no
// limit, or a limit of zero or less.
const defaultRunPageLimit = 50

// ErrSchemaTooOld means the record store's applied schema version is
// below this Reader's compiled minimum, or the version table is missing
// or unreadable for a reason other than the database being unreachable.
var ErrSchemaTooOld = errors.New("record store schema is older than this reader requires")

// ErrUnavailable means the record store did not answer: a connect failure
// or a deadline. It is never a schema problem. The store may return, so a
// caller answers "unreachable" and retries, not "not configured".
var ErrUnavailable = errors.New("record store unreachable")

// ErrRunNotFound means GetRun found no run_intents row for the given
// run id.
var ErrRunNotFound = errors.New("run not found")

// Reader is the BFF's read surface over the record tables. C1 owns the
// Go queries; C4 owns the HTTP shapes and the JSON.
type Reader interface {
	// ChangeRequestJobs answers the job paths whose live change-request head
	// has the pull-request URL changeURL.
	ChangeRequestJobs(ctx context.Context, cluster, tenant, changeURL string) ([]string, error)
	// RevisionJobs answers the job paths whose live branch head holds the
	// commit revision.
	RevisionJobs(ctx context.Context, cluster, tenant, revision string) ([]string, error)
	ListJobSummaries(ctx context.Context, cluster, tenant string, since time.Time) ([]JobSummary, error)
	GetJobSummary(ctx context.Context, cluster, tenant, jobPath string) (JobSummary, error)
	ListRuns(ctx context.Context, q RunQuery) (RunPage, error)
	// ListRunsAcross pages the runs of every scope in q by keyset. A
	// cursor of another sort or order answers ErrInvalidCursor.
	ListRunsAcross(ctx context.Context, q RunsAcrossQuery) (RunPage, error)
	// RunsOverview answers the aggregate counts and the recent runs of every
	// scope in q, for the 24 hours before q.Now.
	RunsOverview(ctx context.Context, q RunsOverviewQuery) (RunsOverview, error)
	// ListDownstreamRuns pages the runs whose upstream run id is
	// q.UpstreamRunID, newest first.
	ListDownstreamRuns(ctx context.Context, q DownstreamRunQuery) (RunPage, error)
	GetRun(ctx context.Context, runID string) (Run, []Stage, error)
	// GetRunWait answers the live concurrency wait of one run, if a claim
	// refreshed one within the live window.
	GetRunWait(ctx context.Context, runID string) (RunWait, bool, error)
	// ListJobRunWaits answers the live concurrency waits of one job, the
	// longest waiting first.
	ListJobRunWaits(ctx context.Context, cluster, tenant, jobPath string) ([]RunWait, error)

	// TenantAgentSpend answers the Tenant agent rollup row of a Tenant on one UTC day.
	TenantAgentSpend(ctx context.Context, tenant string, day time.Time) (TenantAgentSpendRow, error)

	// TenantRetention answers the tenant's record-store windows. A tenant
	// with no placement row, or no retention key, answers the zero policy.
	TenantRetention(ctx context.Context, cluster, tenant string) (TenantRetentionPolicy, error)

	// CurrentStages answers the name of the top-level stage in progress
	// for each run in runIDs, keyed by run id. A run that holds no
	// top-level stage in progress carries no entry. One call answers every
	// run in the list, so a caller that needs many runs reads the store
	// once.
	CurrentStages(ctx context.Context, runIDs []string) (map[string]string, error)

	// ListArtifacts reads the run's user artifacts, ordered by path. It
	// omits every internal key (see IsInternalArtifactPath).
	ListArtifacts(ctx context.Context, runID string) ([]Artifact, error)
	// GetArtifact reads the one run_artifacts row at path, an internal
	// key included. found is false when the run holds no row at path.
	GetArtifact(ctx context.Context, runID, path string) (artifact Artifact, found bool, err error)
	ListTests(ctx context.Context, runID string) ([]TestSuite, error)

	// ListTestCases reads the run's run_test_cases rows: every row that
	// is not SKIPPED first, then every SKIPPED row, each group ordered by
	// suite, class name, and test name.
	ListTestCases(ctx context.Context, runID string) ([]TestCase, error)
	// ListTestCaseHistory reads one test case's outcomes across one job's
	// runs, newest first, capped at q.Limit.
	ListTestCaseHistory(ctx context.Context, q TestCaseHistoryQuery) ([]TestCaseOutcome, error)

	// RunQuality answers the coverage and warnings rows of one run, each
	// list ordered by report id. Each row carries its base: the newest
	// earlier row of the same cluster, tenant, job path, and report id. A
	// run with no row answers two empty lists.
	RunQuality(ctx context.Context, runID string) (RunQuality, error)
	// JobQualityTrend answers the coverage and warnings rows of the listed
	// runs of one job. No row carries a base. A run with no row has no row
	// in the answer.
	JobQualityTrend(ctx context.Context, cluster, tenant, jobPath string, runIDs []string) (RunQuality, error)

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

	// JobInventory reads the live job inventory of a tenant: which declared
	// jobs and multibranch projects the active member cells last reported,
	// and whether each report is complete. A kind that is not known must
	// not be read as absent.
	JobInventory(ctx context.Context, cluster, tenant string) (JobInventory, error)

	// GetJobBranches reads the stored branch and change-request inventory
	// of one multibranch job of tenant. The bool is false when no row
	// exists, which is the state before the cell's first export.
	GetJobBranches(ctx context.Context, cluster, tenant, jobPath string) (JobBranchProject, bool, error)

	// LatestRunsForJobs answers the newest run of each listed job path of
	// tenant, keyed by job path. A job path with no run has no entry.
	LatestRunsForJobs(ctx context.Context, cluster, tenant string, jobPaths []string) (map[string]Run, error)

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

	// JobHome answers the member that holds jobPath's home slot.
	//
	// A trigger write resolves the home before it publishes the imperative
	// command, so a job homed on slot 1 reaches slot 1's holder and not
	// slot 0's. The answer separates two states a caller must not confuse:
	// a tenant that holds no placement row at all, and a tenant whose home
	// slot holds no cell yet. The first keeps the caller's own coordinate,
	// which is what a controller with no cell role needs. The second is a
	// retryable hold, because a trigger sent to the wrong member answers
	// the permanent NOT_HOME and cancels the build.
	JobHome(ctx context.Context, cluster, tenant, jobPath string) (JobHome, error)

	// ListFleetLocks answers every fleet_locks row of cluster whose name
	// prefix is tenant plus "/", plus every row of cluster whose own
	// tenant column is tenant, as one de-duplicated set. The caller
	// decides whether a row of another tenant also discloses a link to
	// its run.
	ListFleetLocks(ctx context.Context, cluster, tenant string) ([]FleetLock, error)

	// ListRunNotes answers the notes of one run, oldest first. A run is
	// identified by run id and created_at together, as the runs key is.
	ListRunNotes(ctx context.Context, runID uuid.UUID, runCreatedAt time.Time) ([]RunNote, error)

	// ListJobNotes answers the newest notes of one job, newest first,
	// limited to limit rows (1 to 10; other values fall back to 10).
	ListJobNotes(ctx context.Context, cluster, tenant, jobPath string, limit int) ([]RunNote, error)

	// ListAllFleetLocks answers every fleet_locks row of every cluster,
	// with no tenant filter.
	ListAllFleetLocks(ctx context.Context) ([]FleetLock, error)

	// ListTenantPlacements answers the slot-0 coordinate of every
	// placement row of tenant, one row for each cluster. A member
	// cluster's operator writes its tenants' rows through the core
	// recorder, so this is the core's only view of a Tenant placed there.
	ListTenantPlacements(ctx context.Context, tenant string) ([]TenantPlacement, error)

	// TenantBadgeToken answers the badge token that a placement row holds for
	// one tenant on one cluster. It answers "" with a nil error for a missing
	// row or a NULL column. The BFF calls it only for a member-placed badge.
	TenantBadgeToken(ctx context.Context, cluster, tenant string) (string, error)

	// ListAllTenantPlacements answers the slot-0 coordinate of every
	// placement row of every tenant, ordered by tenant, then by cluster.
	// The core BFF lists member-placed tenants on the pipelines landing
	// page from it.
	ListAllTenantPlacements(ctx context.Context) ([]TenantPlacement, error)
}

// JobHome is one job's home member. Placed reports whether the tenant
// holds a placement row. Cell is empty when the home slot holds no cell.
type JobHome struct {
	Placed    bool
	Namespace string
	Cell      string
}

// pgReader is the Postgres-backed Reader implementation.
type pgReader struct {
	pool *pgxpool.Pool
	// activityRetentionDays is the BFF's configured activity retention
	// window, from VARROA_ACTIVITY_RETENTION_DAYS. It bounds how old an
	// incoming ListActivity cursor may be before ErrCursorExpired.
	activityRetentionDays int
	// schemaOK is true after one schema check passes. Migrations only go
	// up, so it stays true for the life of the Reader. A failed check
	// never sets it.
	schemaOK atomic.Bool
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
// version table is missing or unreadable. It returns ErrUnavailable,
// wrapped with the underlying failure, when the database did not answer.
func (r *pgReader) SchemaVersion(ctx context.Context) (int64, error) {
	var version int64
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(version_id), 0) FROM varroa.goose_db_version WHERE is_applied
	`).Scan(&version)
	if err != nil {
		// The caller's own context ended the call. The store may be healthy,
		// so the error is neither a schema problem nor unreachable.
		if ctx.Err() != nil {
			return 0, fmt.Errorf("read schema version: %w", err)
		}
		if pgdial.IsUnreachable(err) {
			return 0, fmt.Errorf("%w: read schema version: %w", ErrUnavailable, err)
		}
		return 0, fmt.Errorf("%w: read schema version: %v", ErrSchemaTooOld, err) //nolint:errorlint // %v, not %w: the wrapped ErrSchemaTooOld is the one sentinel callers match on
	}
	if version < minGooseSchemaVersion {
		return 0, fmt.Errorf("%w: applied version %d is below %d", ErrSchemaTooOld, version, minGooseSchemaVersion)
	}
	r.schemaOK.Store(true)
	return version, nil
}

// checkSchema returns nil at once after one schema check has passed. Until
// then it runs SchemaVersion and returns that error unchanged. Every Reader
// method but SchemaVersion itself calls this first.
func (r *pgReader) checkSchema(ctx context.Context) error {
	if r.schemaOK.Load() {
		return nil
	}
	_, err := r.SchemaVersion(ctx)
	return err
}

// runColumns lists varroa.runs' columns in the order scanRun expects.
const runColumns = `run_id, created_at, cluster, namespace, cell, tenant, job_path, build_number, branch,
	scm_revision, status, result, queued_at, started_at, ended_at, last_heartbeat_at,
	interrupted_superseded_at, duration_ms, actor, cause, params, changes, source, inputs, summary,
	test_report_key, tests_total, tests_failed, tests_skipped, payload, replay_of,
	upstream_run_id, upstream_tenant, upstream_job, upstream_number, restart_of, restart_stage`

// qualifiedRunColumns is the same columns as runColumns, each qualified
// with the "runs" alias, so ListRuns' query stays unambiguous once it
// joins run_intents.
const qualifiedRunColumns = `runs.run_id, runs.created_at, runs.cluster, runs.namespace, runs.cell, runs.tenant,
	runs.job_path, runs.build_number, runs.branch, runs.scm_revision, runs.status, runs.result,
	runs.queued_at, runs.started_at, runs.ended_at, runs.last_heartbeat_at,
	runs.interrupted_superseded_at, runs.duration_ms, runs.actor, runs.cause, runs.params,
	runs.changes, runs.source, runs.inputs, runs.summary, runs.test_report_key, runs.tests_total,
	runs.tests_failed, runs.tests_skipped, runs.payload, runs.replay_of,
	runs.upstream_run_id, runs.upstream_tenant, runs.upstream_job, runs.upstream_number,
	runs.restart_of, runs.restart_stage`

// scanRun scans one row shaped by runColumns into a Run.
func scanRun(row pgx.Row) (Run, error) {
	var run Run
	var runID uuid.UUID
	err := row.Scan(
		&runID, &run.CreatedAt, &run.Cluster, &run.Namespace, &run.Cell, &run.Tenant, &run.JobPath,
		&run.BuildNumber, &run.Branch, &run.SCMRevision, &run.Status, &run.Result, &run.QueuedAt,
		&run.StartedAt, &run.EndedAt, &run.LastHeartbeatAt, &run.InterruptedSupersededAt, &run.DurationMs,
		&run.Actor, &run.Cause, &run.Params, &run.Changes, &run.Source, &run.Inputs, &run.Summary,
		&run.TestReportKey, &run.TestsTotal, &run.TestsFailed, &run.TestsSkipped, &run.Payload,
		&run.ReplayOf, &run.UpstreamRunID, &run.UpstreamTenant, &run.UpstreamJob, &run.UpstreamNumber,
		&run.RestartOf, &run.RestartStage,
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

// listStages reads every run_stages row for runID in stage-start order.
// Every reader of the stage list relies on this order, so no caller
// sorts again. Ingest writes the run's created_at on every stage row, so
// created_at alone gives no order. A row with no started_at comes last.
// Two rows with the same started_at come in numeric order of their
// Jenkins node ids, which the length-then-text comparison gives.
func (r *pgReader) listStages(ctx context.Context, runID string) ([]Stage, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, stage_id, created_at, parent_id, name, node_type, status,
		       started_at, ended_at, duration_ms, error, payload
		FROM varroa.run_stages
		WHERE run_id = $1
		ORDER BY started_at ASC NULLS LAST, created_at, length(stage_id), stage_id
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
			&s.Status, &s.StartedAt, &s.EndedAt, &s.DurationMs, &s.Error, &s.Payload); err != nil {
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

// CurrentStages answers one run_stages name for each run in runIDs: the
// top-level stage in progress with the latest started_at, and the latest
// created_at among those when the started_at values tie or are absent. A
// run_stages row nested in a parallel branch or in another stage carries
// a parent_id, so the predicate here keeps the top level alone. It issues
// one statement for the whole list, never one statement per run. An empty
// runIDs answers an empty map and issues no statement.
func (r *pgReader) CurrentStages(ctx context.Context, runIDs []string) (map[string]string, error) {
	if len(runIDs) == 0 {
		return map[string]string{}, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT ON (run_id) run_id, name
		FROM varroa.run_stages
		WHERE run_id = ANY($1::uuid[]) AND status = 'IN_PROGRESS'
		  AND node_type = 'stage' AND parent_id IS NULL
		ORDER BY run_id, started_at DESC NULLS LAST, created_at DESC
	`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("current stages: %w", err)
	}
	defer rows.Close()

	stages := make(map[string]string, len(runIDs))
	for rows.Next() {
		var runID uuid.UUID
		var name string
		if err := rows.Scan(&runID, &name); err != nil {
			return nil, fmt.Errorf("scan current stage: %w", err)
		}
		stages[runID.String()] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("current stages: %w", err)
	}
	return stages, nil
}

// ListArtifacts reads every run_artifacts row for runID that is not an
// internal key, ordered by path.
func (r *pgReader) ListArtifacts(ctx context.Context, runID string) ([]Artifact, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, path, created_at, size_bytes, digest, content_type, blob_key
		FROM varroa.run_artifacts
		WHERE run_id = $1 AND NOT starts_with(path, $2)
		ORDER BY path
	`, runID, InternalArtifactPrefix)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()

	var artifacts []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		artifacts = append(artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	return artifacts, nil
}

// GetArtifact reads the run_artifacts row at runID and path. It reads an
// internal key too, so the test report download keeps its row.
func (r *pgReader) GetArtifact(ctx context.Context, runID, path string) (Artifact, bool, error) {
	if err := r.checkSchema(ctx); err != nil {
		return Artifact{}, false, err
	}
	a, err := scanArtifact(r.pool.QueryRow(ctx, `
		SELECT run_id, path, created_at, size_bytes, digest, content_type, blob_key
		FROM varroa.run_artifacts
		WHERE run_id = $1 AND path = $2
	`, runID, path))
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, false, nil
	}
	if err != nil {
		return Artifact{}, false, fmt.Errorf("get artifact: %w", err)
	}
	return a, true, nil
}

// scanArtifact scans one run_artifacts row in the column order both
// artifact reads select.
func scanArtifact(row pgx.Row) (Artifact, error) {
	var a Artifact
	var artifactRunID uuid.UUID
	if err := row.Scan(&artifactRunID, &a.Path, &a.CreatedAt, &a.SizeBytes, &a.Digest, &a.ContentType, &a.BlobKey); err != nil {
		return Artifact{}, err
	}
	a.RunID = artifactRunID.String()
	return a, nil
}

// ListTests reads every run_tests row for runID. The rows sort by failed
// count, most failed first, then by suite name, so a caller shows the
// suites that need attention first without sorting again.
func (r *pgReader) ListTests(ctx context.Context, runID string) ([]TestSuite, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT run_id, suite, created_at, total, failed, skipped, duration_ms
		FROM varroa.run_tests
		WHERE run_id = $1
		ORDER BY failed DESC, suite
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
	if q.BuildNumber > 0 {
		args = append(args, q.BuildNumber)
		clauses = append(clauses, fmt.Sprintf("runs.build_number = $%d", len(args)))
	}
	if q.Namespace != "" {
		addFilter("runs.namespace", q.Namespace)
	}
	if q.Cell != "" {
		addFilter("runs.cell", q.Cell)
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
