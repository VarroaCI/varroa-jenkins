package recordstore

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// TestBuildSearchTSQuery covers an empty query, one token, several
// tokens, and a query with punctuation only, per D9's token-prefix
// tsquery builder.
func TestBuildSearchTSQuery(t *testing.T) {
	tests := []struct {
		name   string
		q      string
		want   string
		wantOK bool
	}{
		{"empty", "", "", false},
		{"punctuation only", "!!! ---", "", false},
		{"one token", "service", "service:*", true},
		{"several tokens", "team-a service", "team & a & service:*", true},
		{"mixed case and punctuation", "Team-A/Service", "team & a & service:*", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := buildSearchTSQuery(tt.q)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("tsquery = %q, want %q", got, tt.want)
			}
		})
	}
}

// searchTestCluster is the fixed "cluster" column value every row this
// file seeds uses. Cross-cluster search behavior isn't this file's
// concern (D9's queries carry no cluster filter of their own), so one
// fixed value keeps every insert call shorter.
const searchTestCluster = "core"

// insertJobScheduleRow seeds one varroa.job_schedules row directly, with
// trigger_type fixed to "cron" since these search tests never vary it,
// so control over namespace/cell/tenant/job_path needs no full
// job.schedules ingest pipeline.
func insertJobScheduleRow(t *testing.T, db *sql.DB, namespace, cell, tenant, jobPath string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedules (cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload)
		VALUES ($1, $2, $3, $4, $5, 'cron', now(), '{}')
	`, searchTestCluster, namespace, cell, tenant, jobPath); err != nil {
		t.Fatalf("insert job_schedules row: %v", err)
	}
}

// insertRunRow seeds one varroa.runs row directly, since these search
// tests need control over namespace/cell/tenant/job_path without driving
// the full run.started ingest pipeline.
func insertRunRow(t *testing.T, db *sql.DB, namespace, cell, tenant, jobPath string) {
	t.Helper()
	runID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, now(), $2, $3, $4, $5, $6, $7, 'QUEUED')
	`, runID, "manual:"+runID.String(), searchTestCluster, namespace, cell, tenant, jobPath); err != nil {
		t.Fatalf("insert runs row: %v", err)
	}
}

// TestSearchFetchLimitExceedsPerKindCap asserts searchFetchLimit stays
// larger than searchPerKindCap, the invariant that gives the handler's
// per-row authorization check headroom over the response cap it applies.
func TestSearchFetchLimitExceedsPerKindCap(t *testing.T) {
	if searchFetchLimit <= searchPerKindCap {
		t.Fatalf("searchFetchLimit (%d) must exceed searchPerKindCap (%d)", searchFetchLimit, searchPerKindCap)
	}
}

// TestSearchRecords_FetchLimit seeds more matching rows than
// searchFetchLimit for each of the three record kinds and asserts
// SearchRecords returns exactly searchFetchLimit for each. The response
// cap searchPerKindCap documents is the handler's job (rankSearchResults
// in internal/api/handlers.go), applied after per-row authorization; this
// test proves only the reader's own fetch bound.
func TestSearchRecords_FetchLimit(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ctx := context.Background()

	seedCount := searchFetchLimit + 3
	for i := 0; i < seedCount; i++ {
		// "capjob" leads the job path so the token-prefix query below
		// matches it: Postgres's 'simple' parser keeps a path segment
		// after the first "/" as one lexeme, so only a prefix of that
		// whole lexeme (or of the first segment) matches.
		jobPath := fmt.Sprintf("capjob%d/build", i)
		insertJobScheduleRow(t, db, "jenkins", fmt.Sprintf("cell-%d", i), "acme", jobPath)
		insertRunRow(t, db, "jenkins", fmt.Sprintf("cell-%d", i), "acme", jobPath)
	}
	ing := NewActivityIngester(pool)
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < seedCount; i++ {
		e := activity.Event{Timestamp: base.Add(time.Duration(i) * time.Second), Cluster: "core", Namespace: "jenkins",
			Controller: fmt.Sprintf("cell-%d", i), Source: "user", Severity: "info", Type: "controller.capcheck", Message: "capcheck row"}
		e.Normalize()
		if _, err := ing.Insert(ctx, e, EventKey(e)); err != nil {
			t.Fatalf("insert activity event %d: %v", i, err)
		}
	}

	got, err := r.SearchRecords(ctx, SearchQuery{Query: "capjob", AllTenants: true})
	if err != nil {
		t.Fatalf("SearchRecords jobs/runs: %v", err)
	}
	if len(got.Jobs) != searchFetchLimit {
		t.Fatalf("got %d jobs, want %d", len(got.Jobs), searchFetchLimit)
	}
	if len(got.Runs) != searchFetchLimit {
		t.Fatalf("got %d runs, want %d", len(got.Runs), searchFetchLimit)
	}

	got, err = r.SearchRecords(ctx, SearchQuery{Query: "capcheck", Scope: activity.Scope{All: true}})
	if err != nil {
		t.Fatalf("SearchRecords activity: %v", err)
	}
	if len(got.Activity) != searchFetchLimit {
		t.Fatalf("got %d activity rows, want %d", len(got.Activity), searchFetchLimit)
	}
}

// TestSearchRecords_TokenPrefixMatchesJobPath covers the spec scenario
// "A prefix matches a job path token": a caller searching a prefix finds
// a readable job path that starts with it.
func TestSearchRecords_TokenPrefixMatchesJobPath(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ctx := context.Background()

	insertJobScheduleRow(t, db, "team-a", "cell-a", "team-a", "team-a/service/main")

	got, err := r.SearchRecords(ctx, SearchQuery{Query: "serv", AllTenants: true})
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].JobPath != "team-a/service/main" {
		t.Fatalf("jobs = %+v, want one match on team-a/service/main", got.Jobs)
	}
}

// TestSearchRecords_ScopePushdown seeds job and run rows in two
// namespaces and asserts a JobRunNamespaces list admits only the
// readable namespace, proving the job/run namespace pushdown.
func TestSearchRecords_ScopePushdown(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ctx := context.Background()

	// "scopecheck" leads each job path so the token-prefix query matches
	// it directly, for the same reason TestSearchRecords_PerKindCap's
	// job paths lead with "capjob".
	insertJobScheduleRow(t, db, "team-a", "cell-a", "team-a", "scopecheck/team-a")
	insertJobScheduleRow(t, db, "team-b", "cell-b", "team-b", "scopecheck/team-b")
	insertRunRow(t, db, "team-a", "cell-a", "team-a", "scopecheck/team-a")
	insertRunRow(t, db, "team-b", "cell-b", "team-b", "scopecheck/team-b")

	got, err := r.SearchRecords(ctx, SearchQuery{Query: "scopecheck", JobRunNamespaces: []string{"team-a"}})
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Namespace != "team-a" {
		t.Fatalf("jobs = %+v, want one row in team-a only", got.Jobs)
	}
	if len(got.Runs) != 1 || got.Runs[0].Namespace != "team-a" {
		t.Fatalf("runs = %+v, want one row in team-a only", got.Runs)
	}

	all, err := r.SearchRecords(ctx, SearchQuery{Query: "scopecheck", AllTenants: true})
	if err != nil {
		t.Fatalf("SearchRecords all: %v", err)
	}
	if len(all.Jobs) != 2 || len(all.Runs) != 2 {
		t.Fatalf("all-scope jobs/runs = %d/%d, want 2/2", len(all.Jobs), len(all.Runs))
	}
}

// TestSearchRecords_TenantPushdown seeds job and run rows in a namespace
// outside the caller's scope, tenanted either inside or outside the
// caller's tenant grants, and asserts SearchRecords returns the
// in-tenant row but not the out-of-tenant row (ruling y).
func TestSearchRecords_TenantPushdown(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ctx := context.Background()

	// Neither row's namespace ("outside-ns") is in the caller's scope.
	insertJobScheduleRow(t, db, "outside-ns", "cell-a", "team-a", "tenantcheck/team-a")
	insertJobScheduleRow(t, db, "outside-ns", "cell-b", "team-b", "tenantcheck/team-b")
	insertRunRow(t, db, "outside-ns", "cell-a", "team-a", "tenantcheck/team-a")
	insertRunRow(t, db, "outside-ns", "cell-b", "team-b", "tenantcheck/team-b")

	got, err := r.SearchRecords(ctx, SearchQuery{
		Query:            "tenantcheck",
		JobRunNamespaces: []string{"in-scope-ns"},
		Tenants:          []string{"team-a"},
	})
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Tenant != "team-a" {
		t.Fatalf("jobs = %+v, want one row tenanted team-a only", got.Jobs)
	}
	if len(got.Runs) != 1 || got.Runs[0].Tenant != "team-a" {
		t.Fatalf("runs = %+v, want one row tenanted team-a only", got.Runs)
	}
}

// TestSearchRecords_NamespaceFilterAppliesInSQL seeds one tenant-granted
// job row in "ns-a" and searchFetchLimit tenant-granted job rows in
// "ns-b" that outrank it (equal rank, job_path tie-break ascending, and
// the ns-b rows' job paths sort ahead of the ns-a row's), then asserts a
// Namespace-filtered query still returns the ns-a row. Both namespaces
// are admitted only through Tenants, not Scope, so the fix must apply
// the namespace filter inside the SQL query itself: an unfiltered fetch
// would exhaust searchFetchLimit on the 25 outranking ns-b rows before
// the ns-a row is ever read, and a post-fetch namespace drop could not
// recover it.
func TestSearchRecords_NamespaceFilterAppliesInSQL(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ctx := context.Background()

	for i := 0; i < searchFetchLimit; i++ {
		insertJobScheduleRow(t, db, "ns-b", fmt.Sprintf("cell-%02d", i), "team-a", fmt.Sprintf("nsfiltercheck%02d/build", i))
	}
	insertJobScheduleRow(t, db, "ns-a", "cell-a", "team-a", "nsfilterchecklast/build")

	got, err := r.SearchRecords(ctx, SearchQuery{
		Query:            "nsfiltercheck",
		JobRunNamespaces: []string{"nowhere"},
		Tenants:          []string{"team-a"},
		Namespace:        "ns-a",
	})
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Namespace != "ns-a" {
		t.Fatalf("jobs = %+v, want the single ns-a row despite %d outranking ns-b rows sharing the same tenant grant", got.Jobs, searchFetchLimit)
	}
}

// TestSearchRecords_AllTenantsWithNamespaceFilter seeds job and run rows
// in two namespaces, with no namespace or tenant naming either row, and
// asserts AllTenants admits both, while a Namespace filter still narrows
// the admitted set to the one requested namespace (ruling y: a
// cluster-wide pipelines grant reaches every tenant, but a Namespace
// filter still applies as an AND'd exact match).
func TestSearchRecords_AllTenantsWithNamespaceFilter(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ctx := context.Background()

	insertJobScheduleRow(t, db, "ns-a", "cell-a", "team-a", "alltenantscheck/team-a")
	insertJobScheduleRow(t, db, "ns-b", "cell-b", "team-b", "alltenantscheck/team-b")
	insertRunRow(t, db, "ns-a", "cell-a", "team-a", "alltenantscheck/team-a")
	insertRunRow(t, db, "ns-b", "cell-b", "team-b", "alltenantscheck/team-b")

	all, err := r.SearchRecords(ctx, SearchQuery{Query: "alltenantscheck", AllTenants: true})
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(all.Jobs) != 2 || len(all.Runs) != 2 {
		t.Fatalf("all-tenants jobs/runs = %d/%d, want 2/2", len(all.Jobs), len(all.Runs))
	}

	got, err := r.SearchRecords(ctx, SearchQuery{Query: "alltenantscheck", AllTenants: true, Namespace: "ns-a"})
	if err != nil {
		t.Fatalf("SearchRecords namespace-filtered: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Namespace != "ns-a" {
		t.Fatalf("jobs = %+v, want the single ns-a row despite AllTenants", got.Jobs)
	}
	if len(got.Runs) != 1 || got.Runs[0].Namespace != "ns-a" {
		t.Fatalf("runs = %+v, want the single ns-a row despite AllTenants", got.Runs)
	}
}

// TestSearchRecords_EmptyQueryMatchesNothing asserts buildSearchTSQuery's
// ok=false case (an empty or punctuation-only query) returns no record
// results without sending a query Postgres cannot parse.
func TestSearchRecords_EmptyQueryMatchesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	ctx := context.Background()
	insertJobScheduleRow(t, db, "jenkins", "cell-a", "acme", "acme/emptycheck")

	got, err := r.SearchRecords(ctx, SearchQuery{Query: "!!!", Scope: activity.Scope{All: true}})
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(got.Jobs) != 0 || len(got.Runs) != 0 || len(got.Activity) != 0 {
		t.Fatalf("got = %+v, want an empty SearchResults", got)
	}
}

// TestSearchRecordsExpressionIndexesAreUsed asserts SearchRecords' three
// queries repeat the pinned expressions identically, so Postgres answers
// each with an index scan rather than a sequential scan.
func TestSearchRecordsExpressionIndexesAreUsed(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	insertJobScheduleRow(t, db, "jenkins", "cell-a", "acme", "acme/idxcheck")
	insertRunRow(t, db, "jenkins", "cell-a", "acme", "acme/idxcheck")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.activity (event_key, ts, type, message, actor, controller, namespace, item_path, payload)
		VALUES ('idxcheck', now(), 'controller.idxcheck', 'idxcheck message', '', 'cell-a', 'jenkins', '', '{}')
	`); err != nil {
		t.Fatalf("insert into activity: %v", err)
	}

	// A single row is too small for the planner to prefer an index scan
	// over a sequential scan; disable sequential scans so the assertion
	// proves the expression is repeated identically, not that the table
	// happens to be large.
	if _, err := db.ExecContext(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}

	assertIndexScan(ctx, t, db, fmt.Sprintf(`
		SELECT job_path FROM varroa.job_schedules
		WHERE %s @@ to_tsquery('simple', 'idxcheck')
	`, jobSearchExprSQL))
	assertIndexScan(ctx, t, db, fmt.Sprintf(`
		SELECT run_id FROM varroa.runs
		WHERE %s @@ to_tsquery('simple', 'idxcheck')
	`, runSearchExprSQL))
	assertIndexScan(ctx, t, db, `
		SELECT event_key FROM varroa.activity
		WHERE search_tsv @@ to_tsquery('simple', 'idxcheck')
	`)
}
