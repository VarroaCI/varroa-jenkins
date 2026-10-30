package recordstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RunScope names one tenant of the fleet run read. JobPatterns holds
// anchored POSIX regular expressions over the full job path. A nil slice
// admits every job of the tenant.
type RunScope struct {
	Cluster string
	Tenant  string
	// JobPatterns holds anchored POSIX regexps over the full job path. Nil admits every job.
	JobPatterns []string
}

// RunSort names the key that orders the fleet run read.
type RunSort string

// The six keys of the fleet run read. An empty RunSort means RunSortStarted.
const (
	RunSortStarted  RunSort = "started"
	RunSortDuration RunSort = "duration"
	RunSortTenant   RunSort = "tenant"
	RunSortJob      RunSort = "job"
	RunSortStatus   RunSort = "status"
	RunSortActor    RunSort = "actor"
)

// RunStatusWaitingInput is the status filter of a RUNNING run that holds
// at least one live input.
const RunStatusWaitingInput = "WAITING_INPUT"

// maxRunsAcrossLimit caps the page size of the fleet run read.
const maxRunsAcrossLimit = 200

// ErrInvalidCursor means a fleet run cursor does not decode, or it was
// issued under another sort or order. A caller maps it to a client error.
var ErrInvalidCursor = errors.New("invalid run cursor")

// RunsAcrossQuery filters and pages the fleet run read. Every scope is read
// separately. The results are merged in sort order and cut to Limit.
type RunsAcrossQuery struct {
	Scopes                         []RunScope
	JobPath, Branch, Actor, Result string
	// Status is QUEUED, RUNNING, COMPLETED, or RunStatusWaitingInput.
	Status string
	// Since is the lower bound on created_at. Zero applies no bound.
	Since time.Time
	// Search is a literal ILIKE match over job_path, branch, and scm_revision.
	Search string
	// Sort orders the page. Empty means RunSortStarted.
	Sort      RunSort
	Ascending bool
	// Limit is the page size. A value of zero or less means defaultRunPageLimit.
	// A value above maxRunsAcrossLimit is clamped to it.
	Limit  int
	Cursor string
}

// waitingInputSQL is true for a RUNNING run with a non-empty inputs array.
// The COALESCE guards a non-array value. Postgres does not fix the order of
// AND operands, so the array test must not depend on it.
const waitingInputSQL = `COALESCE(CASE WHEN jsonb_typeof(runs.inputs) = 'array' THEN jsonb_array_length(runs.inputs) END, 0) > 0`

// runStatusRankSQL orders runs by severity. The lower rank is the more
// severe state. runStatusRank holds the same rules in Go.
const runStatusRankSQL = `CASE
	WHEN runs.result = 'FAILURE' THEN 0
	WHEN runs.result = 'TRIGGER_FAILED' THEN 1
	WHEN runs.result = 'INTERRUPTED' THEN 2
	WHEN runs.result = 'UNSTABLE' THEN 3
	WHEN runs.status = 'RUNNING' AND runs.result IS NULL THEN 4
	WHEN runs.status = 'QUEUED' THEN 5
	WHEN runs.result = 'ABORTED' THEN 6
	WHEN runs.result = 'SUCCESS' THEN 7
	ELSE 8
END`

// runStatusRank is the Go mirror of runStatusRankSQL.
func runStatusRank(run Run) int {
	if run.Result != nil {
		switch *run.Result {
		case "FAILURE":
			return 0
		case "TRIGGER_FAILED":
			return 1
		case "INTERRUPTED":
			return 2
		case "UNSTABLE":
			return 3
		}
	}
	if run.Status == "RUNNING" && run.Result == nil {
		return 4
	}
	if run.Status == "QUEUED" {
		return 5
	}
	if run.Result != nil && *run.Result == "ABORTED" {
		return 6
	}
	if run.Result != nil && *run.Result == "SUCCESS" {
		return 7
	}
	return 8
}

// runSortSpec holds the SQL sort expression of one key. The cursor carries
// the key value of the last run, typed to match this expression.
type runSortSpec struct {
	expr string
}

var runSortSpecs = map[RunSort]runSortSpec{
	RunSortStarted:  {expr: "runs.created_at"},
	RunSortDuration: {expr: "COALESCE(runs.duration_ms, -1)"},
	RunSortTenant:   {expr: "runs.tenant"},
	RunSortJob:      {expr: "runs.job_path"},
	RunSortStatus:   {expr: runStatusRankSQL},
	RunSortActor:    {expr: "COALESCE(runs.actor, '')"},
}

// runsAcrossCursor is the wire form of a fleet run cursor. It is base64url
// over this JSON object. "s" is the sort, "o" the order, "k" the sort key of
// the last returned run, "c" its created_at, and "r" its run id.
type runsAcrossCursor struct {
	S string `json:"s"`
	O string `json:"o"`
	K string `json:"k"`
	C string `json:"c"`
	R string `json:"r"`
}

// decodedRunsAcrossCursor is a cursor after its key has been typed for SQL.
type decodedRunsAcrossCursor struct {
	key       any
	createdAt time.Time
	runID     string
}

// runKeyString formats the sort key of one run for the cursor.
func runKeyString(sort RunSort, run Run) string {
	switch sort {
	case RunSortDuration:
		if run.DurationMs == nil {
			return "-1"
		}
		return strconv.FormatInt(*run.DurationMs, 10)
	case RunSortTenant:
		return run.Tenant
	case RunSortJob:
		return run.JobPath
	case RunSortStatus:
		return strconv.Itoa(runStatusRank(run))
	case RunSortActor:
		if run.Actor == nil {
			return ""
		}
		return *run.Actor
	default:
		return run.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
}

// EncodeRunsAcrossCursor answers the cursor that resumes strictly after run
// under the given sort and order.
func EncodeRunsAcrossCursor(sort RunSort, ascending bool, run Run) string {
	if sort == "" {
		sort = RunSortStarted
	}
	raw, _ := json.Marshal(runsAcrossCursor{
		S: string(sort),
		O: acrossOrderName(ascending),
		K: runKeyString(sort, run),
		C: run.CreatedAt.UTC().Format(time.RFC3339Nano),
		R: run.RunID,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// acrossOrderName names the order of a fleet run cursor.
func acrossOrderName(ascending bool) string {
	if ascending {
		return "asc"
	}
	return "desc"
}

// decodeRunsAcrossCursor reverses EncodeRunsAcrossCursor. Every failure
// wraps ErrInvalidCursor, and a cursor of another sort or order fails too.
func decodeRunsAcrossCursor(cursor string, sort RunSort, ascending bool) (decodedRunsAcrossCursor, error) {
	invalid := func(why string) error {
		return fmt.Errorf("%w: %s", ErrInvalidCursor, why)
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return decodedRunsAcrossCursor{}, invalid("encoding")
	}
	var c runsAcrossCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return decodedRunsAcrossCursor{}, invalid("format")
	}
	if c.S != string(sort) || c.O != acrossOrderName(ascending) {
		return decodedRunsAcrossCursor{}, invalid("sort or order")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, c.C)
	if err != nil {
		return decodedRunsAcrossCursor{}, invalid("timestamp")
	}
	if _, err := uuid.Parse(c.R); err != nil {
		return decodedRunsAcrossCursor{}, invalid("run id")
	}
	out := decodedRunsAcrossCursor{createdAt: createdAt, runID: c.R}
	switch sort {
	case RunSortStarted:
		key, err := time.Parse(time.RFC3339Nano, c.K)
		if err != nil {
			return decodedRunsAcrossCursor{}, invalid("key")
		}
		out.key = key
	case RunSortDuration:
		n, err := strconv.ParseInt(c.K, 10, 64)
		if err != nil {
			return decodedRunsAcrossCursor{}, invalid("key")
		}
		out.key = n
	case RunSortStatus:
		n, err := strconv.ParseInt(c.K, 10, 32)
		if err != nil {
			return decodedRunsAcrossCursor{}, invalid("key")
		}
		out.key = int32(n)
	default:
		out.key = c.K
	}
	return out, nil
}

// acrossScopeJSON is one scope of the scopes CTE.
type acrossScopeJSON struct {
	Cluster  string   `json:"cluster"`
	Tenant   string   `json:"tenant"`
	Patterns []string `json:"patterns"`
}

// scopesJSON marshals scopes for the scopes CTE. A nil pattern list becomes
// JSON null, which the CTE reads as SQL NULL: every job of the tenant.
func scopesJSON(scopes []RunScope) ([]byte, error) {
	out := make([]acrossScopeJSON, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, acrossScopeJSON{Cluster: s.Cluster, Tenant: s.Tenant, Patterns: s.JobPatterns})
	}
	return json.Marshal(out)
}

// scopesRelation names the scope rows as the scopes relation. The one
// parameter is the scopesJSON text. A statement that adds its own CTEs
// writes "WITH " + scopesRelation + ", ...".
const scopesRelation = `scopes AS (
	SELECT * FROM jsonb_to_recordset($1::jsonb) AS s(cluster text, tenant text, patterns text[])
)`

// scopesCTE is scopesRelation as the first CTE of a statement.
const scopesCTE = "WITH " + scopesRelation

// prefixColumns qualifies each name of a comma-separated column list.
func prefixColumns(prefix, columns string) string {
	parts := strings.Split(columns, ",")
	for i := range parts {
		parts[i] = prefix + strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ", ")
}

// lateralRunColumns is runColumns qualified with the lateral alias "r", so
// the outer select stays unambiguous: the scopes relation also has "tenant"
// and "cluster".
var lateralRunColumns = prefixColumns("r.", runColumns)

// escapeLikeLiteral escapes the LIKE metacharacters so that a search matches
// the text literally. The statement uses ESCAPE '\'.
func escapeLikeLiteral(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(text)
}

// ListRunsAcross pages the runs of every scope by keyset. Each scope reads
// through runs_tenant_idx inside a lateral subquery, so the work is bounded
// by len(Scopes) times the page size. A scope with no pattern list admits
// every job of its tenant. The statement reads limit+1 rows, and
// NextCursor is set only when that extra row exists.
func (r *pgReader) ListRunsAcross(ctx context.Context, q RunsAcrossQuery) (RunPage, error) {
	if len(q.Scopes) == 0 {
		return RunPage{}, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return RunPage{}, err
	}

	sortKey := q.Sort
	if sortKey == "" {
		sortKey = RunSortStarted
	}
	spec, ok := runSortSpecs[sortKey]
	if !ok {
		return RunPage{}, fmt.Errorf("unknown run sort %q", sortKey)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultRunPageLimit
	}
	if limit > maxRunsAcrossLimit {
		limit = maxRunsAcrossLimit
	}

	scopes, err := scopesJSON(q.Scopes)
	if err != nil {
		return RunPage{}, fmt.Errorf("encode run scopes: %w", err)
	}
	args := []any{string(scopes)}
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	var filters []string
	if q.JobPath != "" {
		filters = append(filters, "runs.job_path = "+bind(q.JobPath))
	}
	if q.Branch != "" {
		filters = append(filters, "runs.branch = "+bind(q.Branch))
	}
	if q.Result != "" {
		filters = append(filters, "runs.result = "+bind(q.Result))
	}
	if q.Actor != "" {
		filters = append(filters, "runs.actor = "+bind(q.Actor))
	}
	switch q.Status {
	case "":
	case RunStatusWaitingInput:
		filters = append(filters, "runs.status = 'RUNNING' AND "+waitingInputSQL)
	default:
		filters = append(filters, "runs.status = "+bind(q.Status))
	}
	if !q.Since.IsZero() {
		filters = append(filters, "runs.created_at >= "+bind(q.Since))
	}
	if q.Search != "" {
		pattern := bind("%" + escapeLikeLiteral(q.Search) + "%")
		filters = append(filters, fmt.Sprintf(
			`(runs.job_path ILIKE %[1]s ESCAPE '\' OR runs.branch ILIKE %[1]s ESCAPE '\' OR runs.scm_revision ILIKE %[1]s ESCAPE '\')`,
			pattern))
	}

	op, dir := "<", "DESC"
	if q.Ascending {
		op, dir = ">", "ASC"
	}
	if q.Cursor != "" {
		cur, err := decodeRunsAcrossCursor(q.Cursor, sortKey, q.Ascending)
		if err != nil {
			return RunPage{}, err
		}
		filters = append(filters, fmt.Sprintf("(%s, runs.created_at, runs.run_id) %s (%s, %s, %s)",
			spec.expr, op, bind(cur.key), bind(cur.createdAt), bind(cur.runID)))
	}

	where := ""
	if len(filters) > 0 {
		where = "AND " + strings.Join(filters, " AND ")
	}

	sqlText := fmt.Sprintf(`%s
SELECT %s
FROM scopes s
CROSS JOIN LATERAL (
	SELECT runs.*, %s AS sort_key
	FROM varroa.runs
	WHERE runs.cluster = s.cluster AND runs.tenant = s.tenant
		AND (s.patterns IS NULL OR runs.job_path ~ ANY(s.patterns))
		%s
	ORDER BY sort_key %s, runs.created_at %s, runs.run_id %s
	LIMIT %s
) r
ORDER BY r.sort_key %s, r.created_at %s, r.run_id %s
LIMIT %s`,
		scopesCTE, lateralRunColumns, spec.expr, where,
		dir, dir, dir, bind(limit+1),
		dir, dir, dir, bind(limit+1))

	rows, err := r.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return RunPage{}, fmt.Errorf("list runs across scopes: %w", err)
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
		return RunPage{}, fmt.Errorf("list runs across scopes: %w", err)
	}

	if len(page.Runs) > limit {
		last := page.Runs[limit-1]
		page.NextCursor = EncodeRunsAcrossCursor(sortKey, q.Ascending, last)
		page.Runs = page.Runs[:limit]
	}
	return page, nil
}
