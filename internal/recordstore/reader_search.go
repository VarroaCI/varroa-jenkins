package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// searchPerKindCap documents the response cap D9 requires: at most this
// many rows per kind reach the caller's response, with no redistribution
// between kinds. The handler applies this cap itself, after its own
// per-row authorization check.
const searchPerKindCap = 5

// searchFetchLimit bounds how many rows each SQL query in this file
// fetches per kind. It is larger than searchPerKindCap so the caller's
// per-row authorization check keeps headroom: an unreadable row that
// outranks a readable one must not push the readable row out of the
// fetched set before authorization runs.
const searchFetchLimit = 25

// SearchQuery is Reader.SearchRecords' input: the raw search string, the
// caller's D5 scope, and the caller's job/run pushdown, pushed down
// together as a sound superset. The "activity" query uses Scope alone.
// The "job" and "run" queries use JobRunNamespaces, Tenants, and
// AllTenants instead of Scope: a cluster-wide binding's role never
// appears in Scope (Scope only reflects a controllers:read verb) or in
// Tenants (a cluster-wide binding names no tenant), so a caller holding
// only a cluster-wide pipelines:read grant would otherwise push down
// nothing for a synthetic or deleted-Tenant tenant (ruling y). AllTenants
// carries that case: when true, every job/run row is admitted regardless
// of namespace or tenant. JobRunNamespaces carries a namespace-scoped
// pipelines grant's namespaces, unioned with Scope's own namespaces
// (kept for parity with a controllers:read namespace grant, itself a
// safe over-admission since the caller's per-row check still gates the
// response). Namespace, when set, narrows every kind to that one
// namespace inside the SQL query itself: the admission clause still
// decides whether the caller may read the row at all, but the row must
// also carry that namespace, so a foreign-namespace row an admission
// clause allows never occupies a fetched-row slot another namespace's
// row needs. The caller still applies its own authoritative per-row
// check to every returned row, then applies the searchPerKindCap
// response cap, before a row enters a response.
type SearchQuery struct {
	Query            string
	Scope            activity.Scope
	Tenants          []string
	Namespace        string
	AllTenants       bool
	JobRunNamespaces []string
}

// JobSearchResult is one varroa.job_schedules row SearchRecords returns
// for the "job" kind: the columns a search response and its
// authorization check need, not the full JobSchedule shape ListJobSchedules
// returns.
type JobSearchResult struct {
	Cluster   string
	Namespace string
	Cell      string
	Tenant    string
	JobPath   string
}

// RunSearchResult is one varroa.runs row SearchRecords returns for the
// "run" kind.
type RunSearchResult struct {
	RunID       string
	Cluster     string
	Namespace   string
	Cell        string
	Tenant      string
	JobPath     string
	Branch      *string
	Actor       *string
	BuildNumber *int64
}

// SearchResults is Reader.SearchRecords' output: at most searchFetchLimit
// rows for each of the three record-backed search kinds. The caller
// authorizes each row, then applies the searchPerKindCap response cap.
type SearchResults struct {
	Jobs     []JobSearchResult
	Runs     []RunSearchResult
	Activity []activity.Event
}

// buildSearchTSQuery converts a raw search string into the operand for
// to_tsquery('simple', ...). It lowercases and trims the string first,
// splits on any character that is not a letter or a digit, and drops
// every empty token. The remaining tokens join with "&", and the last
// token gains the ":*" prefix operator. It returns ok false for a token
// list with no token, so a caller with nothing to match sends no query.
func buildSearchTSQuery(q string) (tsquery string, ok bool) {
	q = strings.ToLower(strings.TrimSpace(q))
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(tokens) == 0 {
		return "", false
	}
	tokens[len(tokens)-1] += ":*"
	return strings.Join(tokens, " & "), true
}

// jobSearchExprSQL and runSearchExprSQL repeat migration 00004's index
// expressions verbatim. An expression index accelerates only a query
// that repeats the identical expression.
const (
	jobSearchExprSQL = `to_tsvector('simple', replace(job_path, '/', ' '))`
	runSearchExprSQL = `to_tsvector('simple', replace(job_path, '/', ' ')||' '||coalesce(branch,'')||' '||coalesce(actor,'')||' '||coalesce(build_number::text,''))`
)

// jobRunScopeClause returns the SQL fragment a job or run search query
// adds for namespaces union'd with tenants, and the args slice with
// every bound value appended. A job or run row is never global. When
// allTenants is true, every row is admitted regardless of namespace or
// tenant — a cluster-wide pipelines grant reaches every synthetic and
// deleted-Tenant tenant, which no namespace or tenant list can enumerate
// (ruling y). Otherwise the row is admitted when its namespace lies in
// namespaces OR its tenant lies in tenants — an empty tenants array
// binds to an array that matches no row. When namespace is set, the
// admission clause gains an AND'd exact-namespace match: the OR clause
// still decides whether the caller may read the row at all, but a
// granted tenant's row outside the requested namespace never reaches
// the fetched set.
func jobRunScopeClause(namespaces []string, tenants []string, allTenants bool, namespace string, args []any) (string, []any) {
	var clause string
	if allTenants {
		clause = "TRUE"
	} else {
		args = append(args, namespaces)
		nsIdx := len(args)
		args = append(args, tenants)
		tenantIdx := len(args)
		clause = fmt.Sprintf("(namespace = ANY($%d) OR tenant = ANY($%d))", nsIdx, tenantIdx)
	}
	if namespace == "" {
		return clause, args
	}
	args = append(args, namespace)
	nsFilterIdx := len(args)
	return fmt.Sprintf("(%s) AND namespace = $%d", clause, nsFilterIdx), args
}

// searchJobs queries varroa.job_schedules for the "job" search kind.
func (r *pgReader) searchJobs(ctx context.Context, tsquery string, namespaces []string, tenants []string, allTenants bool, namespace string) ([]JobSearchResult, error) {
	args := []any{tsquery}
	nsClause, args := jobRunScopeClause(namespaces, tenants, allTenants, namespace, args)
	sql := fmt.Sprintf(`
		SELECT cluster, namespace, cell, tenant, job_path
		FROM varroa.job_schedules
		WHERE %s @@ to_tsquery('simple', $1)
		  AND %s
		ORDER BY ts_rank(%s, to_tsquery('simple', $1)) DESC, job_path
		LIMIT %d
	`, jobSearchExprSQL, nsClause, jobSearchExprSQL, searchFetchLimit)

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search jobs: %w", err)
	}
	defer rows.Close()

	var out []JobSearchResult
	for rows.Next() {
		var j JobSearchResult
		if err := rows.Scan(&j.Cluster, &j.Namespace, &j.Cell, &j.Tenant, &j.JobPath); err != nil {
			return nil, fmt.Errorf("scan job search result: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search jobs: %w", err)
	}
	return out, nil
}

// searchRuns queries varroa.runs for the "run" search kind.
func (r *pgReader) searchRuns(ctx context.Context, tsquery string, namespaces []string, tenants []string, allTenants bool, namespace string) ([]RunSearchResult, error) {
	args := []any{tsquery}
	nsClause, args := jobRunScopeClause(namespaces, tenants, allTenants, namespace, args)
	sql := fmt.Sprintf(`
		SELECT run_id, cluster, namespace, cell, tenant, job_path, branch, actor, build_number
		FROM varroa.runs
		WHERE %s @@ to_tsquery('simple', $1)
		  AND %s
		ORDER BY ts_rank(%s, to_tsquery('simple', $1)) DESC, created_at DESC
		LIMIT %d
	`, runSearchExprSQL, nsClause, runSearchExprSQL, searchFetchLimit)

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search runs: %w", err)
	}
	defer rows.Close()

	var out []RunSearchResult
	for rows.Next() {
		var run RunSearchResult
		var runID uuid.UUID
		if err := rows.Scan(&runID, &run.Cluster, &run.Namespace, &run.Cell, &run.Tenant, &run.JobPath,
			&run.Branch, &run.Actor, &run.BuildNumber); err != nil {
			return nil, fmt.Errorf("scan run search result: %w", err)
		}
		run.RunID = runID.String()
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search runs: %w", err)
	}
	return out, nil
}

// searchActivity queries varroa.activity for the "activity" search kind,
// through the generated search_tsv column migration 00002's GIN index
// covers directly. When namespace is set, the admission clause gains an
// AND'd exact-namespace match, the same way jobRunScopeClause does; a
// global event's namespace column is always empty, so this excludes
// every global event from a namespace-filtered search without needing
// to also clear scope.Global.
func (r *pgReader) searchActivity(ctx context.Context, tsquery string, scope activity.Scope, namespace string) ([]activity.Event, error) {
	args := []any{tsquery}
	var clause string
	if scope.All {
		clause = "TRUE"
	} else {
		args = append(args, scope.Namespaces, scope.Global)
		nsIdx, globalIdx := len(args)-1, len(args)
		clause = fmt.Sprintf("(namespace = ANY($%d) OR ($%d AND controller = ''))", nsIdx, globalIdx)
	}
	if namespace != "" {
		args = append(args, namespace)
		nsFilterIdx := len(args)
		clause = fmt.Sprintf("(%s) AND namespace = $%d", clause, nsFilterIdx)
	}
	sql := fmt.Sprintf(`
		SELECT payload
		FROM varroa.activity
		WHERE search_tsv @@ to_tsquery('simple', $1)
		  AND %s
		ORDER BY ts_rank(search_tsv, to_tsquery('simple', $1)) DESC, ts DESC
		LIMIT %d
	`, clause, searchFetchLimit)

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search activity: %w", err)
	}
	defer rows.Close()

	var out []activity.Event
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan activity search result: %w", err)
		}
		var e activity.Event
		if err := json.Unmarshal(payload, &e); err != nil {
			return nil, fmt.Errorf("unmarshal activity search payload: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search activity: %w", err)
	}
	return out, nil
}

// SearchRecords returns up to searchFetchLimit rows for each of the
// three record-backed search kinds — job, run, and activity — matching
// q.Query. The "job" and "run" queries are scoped by q.JobRunNamespaces
// OR q.Tenants, OR unconditionally admitted when q.AllTenants (ruling
// y); the "activity" query is scoped by q.Scope alone. The caller
// authorizes each row and applies the searchPerKindCap response cap;
// SearchRecords does not apply that cap itself. An empty token list
// (buildSearchTSQuery returns ok false) returns an empty SearchResults
// with no query sent.
func (r *pgReader) SearchRecords(ctx context.Context, q SearchQuery) (SearchResults, error) {
	if err := r.checkSchema(ctx); err != nil {
		return SearchResults{}, err
	}
	tsquery, ok := buildSearchTSQuery(q.Query)
	if !ok {
		return SearchResults{}, nil
	}

	jobs, err := r.searchJobs(ctx, tsquery, q.JobRunNamespaces, q.Tenants, q.AllTenants, q.Namespace)
	if err != nil {
		return SearchResults{}, err
	}
	runs, err := r.searchRuns(ctx, tsquery, q.JobRunNamespaces, q.Tenants, q.AllTenants, q.Namespace)
	if err != nil {
		return SearchResults{}, err
	}
	events, err := r.searchActivity(ctx, tsquery, q.Scope, q.Namespace)
	if err != nil {
		return SearchResults{}, err
	}
	return SearchResults{Jobs: jobs, Runs: runs, Activity: events}, nil
}
