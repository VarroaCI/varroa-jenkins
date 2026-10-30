package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// acrossSeed holds the optional columns of a seeded run.
type acrossSeed struct {
	branch, actor, scmRevision *string
	inputs                     string
	durationMs                 *int64
	queuedAt, startedAt        *time.Time
}

// seedAcrossRun inserts one varroa.runs row for the fleet run read. The
// mods set the optional columns, so a test names only the columns it reads.
func seedAcrossRun(t *testing.T, db *sql.DB, tenant, jobPath string, createdAt time.Time, status string, result *string, mods ...func(*acrossSeed)) uuid.UUID {
	t.Helper()
	var s acrossSeed
	for _, mod := range mods {
		mod(&s)
	}
	var inputs any
	if s.inputs != "" {
		inputs = s.inputs
	}
	runID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, result, branch, actor, scm_revision, inputs, duration_ms, queued_at, started_at)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	`, runID, createdAt, "manual:across-"+runID.String(), testCluster, testNamespace, tenant, jobPath,
		status, result, s.branch, s.actor, s.scmRevision, inputs, s.durationMs, s.queuedAt, s.startedAt); err != nil {
		t.Fatalf("seed run for %s: %v", jobPath, err)
	}
	return runID
}

// acrossJobPaths returns the job paths of a page, in page order.
func acrossJobPaths(page RunPage) []string {
	out := make([]string, 0, len(page.Runs))
	for _, run := range page.Runs {
		out = append(out, run.JobPath)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListRunsAcrossScopesAndJobPatterns(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	success := "SUCCESS"
	seedAcrossRun(t, db, "pay", "pay/release/v1.2+a", base, "COMPLETED", &success)
	seedAcrossRun(t, db, "pay", "pay/release/v1x2+a", base.Add(time.Minute), "COMPLETED", &success)
	seedAcrossRun(t, db, "ops", "ops/deploy", base.Add(2*time.Minute), "RUNNING", nil)
	seedAcrossRun(t, db, "hidden", "hidden/x", base.Add(3*time.Minute), "RUNNING", nil)

	page, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{Scopes: []RunScope{
		{Cluster: "core", Tenant: "pay", JobPatterns: []string{`^pay/release/v1\.2\+[^/]*$`}},
		{Cluster: "core", Tenant: "ops"},
	}})
	if err != nil {
		t.Fatalf("ListRunsAcross: %v", err)
	}
	want := []string{"ops/deploy", "pay/release/v1.2+a"}
	if got := acrossJobPaths(page); !sameStrings(got, want) {
		t.Fatalf("job paths = %v; want %v", got, want)
	}
}

func TestListRunsAcrossEmptyScopesAnswersNothing(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	page, err := NewReader(pool).ListRunsAcross(context.Background(), RunsAcrossQuery{})
	if err != nil {
		t.Fatalf("ListRunsAcross: %v", err)
	}
	if len(page.Runs) != 0 || page.NextCursor != "" {
		t.Fatalf("page = %+v; want an empty page", page)
	}
}

func TestListRunsAcrossDefaultKeysetPages(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		seedAcrossRun(t, db, "pay", "pay/job", base.Add(time.Duration(i)*time.Minute), "COMPLETED", nil)
	}

	scope := []RunScope{{Cluster: "core", Tenant: "pay"}}
	seen := map[string]bool{}
	var sizes []int
	cursor := ""
	for {
		page, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{Scopes: scope, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListRunsAcross page: %v", err)
		}
		sizes = append(sizes, len(page.Runs))
		for _, run := range page.Runs {
			if seen[run.RunID] {
				t.Fatalf("run %s repeated across pages", run.RunID)
			}
			seen[run.RunID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if len(sizes) > 5 {
			t.Fatalf("paging did not end: sizes %v", sizes)
		}
	}
	if !sameInts(sizes, []int{2, 2, 1}) {
		t.Fatalf("page sizes = %v; want [2 2 1]", sizes)
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListRunsAcrossFilters(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	success, failure := "SUCCESS", "FAILURE"
	seedAcrossRun(t, db, "pay", "pay/a", base, "COMPLETED", &success,
		func(s *acrossSeed) { s.branch, s.actor = strPtr("main"), strPtr("alice") })
	seedAcrossRun(t, db, "pay", "pay/b", base.Add(time.Minute), "COMPLETED", &failure,
		func(s *acrossSeed) { s.branch, s.actor = strPtr("feat"), strPtr("bob") })
	seedAcrossRun(t, db, "pay", "pay/c", base.Add(2*time.Minute), "QUEUED", nil,
		func(s *acrossSeed) { s.branch, s.actor = strPtr("main"), strPtr("alice") })
	seedAcrossRun(t, db, "pay", "pay/d", base.Add(3*time.Minute), "RUNNING", nil,
		func(s *acrossSeed) { s.inputs = `[{"id":"a"}]` })
	seedAcrossRun(t, db, "pay", "pay/e", base.Add(4*time.Minute), "RUNNING", nil,
		func(s *acrossSeed) { s.inputs = `[]` })

	scope := []RunScope{{Cluster: "core", Tenant: "pay"}}
	cases := []struct {
		name  string
		query RunsAcrossQuery
		want  []string
	}{
		{"job path", RunsAcrossQuery{JobPath: "pay/b"}, []string{"pay/b"}},
		{"branch", RunsAcrossQuery{Branch: "main"}, []string{"pay/c", "pay/a"}},
		{"actor", RunsAcrossQuery{Actor: "bob"}, []string{"pay/b"}},
		{"result", RunsAcrossQuery{Result: "FAILURE"}, []string{"pay/b"}},
		{"status queued", RunsAcrossQuery{Status: "QUEUED"}, []string{"pay/c"}},
		{"waiting input", RunsAcrossQuery{Status: RunStatusWaitingInput}, []string{"pay/d"}},
		{"since", RunsAcrossQuery{Since: base.Add(2 * time.Minute)}, []string{"pay/e", "pay/d", "pay/c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.query
			q.Scopes = scope
			page, err := r.ListRunsAcross(context.Background(), q)
			if err != nil {
				t.Fatalf("ListRunsAcross: %v", err)
			}
			if got := acrossJobPaths(page); !sameStrings(got, tc.want) {
				t.Fatalf("job paths = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestListRunsAcrossSearchIsLiteral(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	scope := []RunScope{{Cluster: "core", Tenant: "pay"}}
	seedAcrossRun(t, db, "pay", "pay/x", base, "COMPLETED", nil,
		func(s *acrossSeed) { s.branch = strPtr("feat_x") })
	seedAcrossRun(t, db, "pay", "pay/y", base.Add(time.Minute), "COMPLETED", nil,
		func(s *acrossSeed) { s.branch = strPtr("featyx") })
	seedAcrossRun(t, db, "pay", "pay/z", base.Add(2*time.Minute), "COMPLETED", nil,
		func(s *acrossSeed) { s.scmRevision = strPtr("100%") })

	page, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{Scopes: scope, Search: "feat_x"})
	if err != nil {
		t.Fatalf("search feat_x: %v", err)
	}
	if got := acrossJobPaths(page); !sameStrings(got, []string{"pay/x"}) {
		t.Fatalf("search feat_x: job paths = %v; want [pay/x]", got)
	}
	page, err = r.ListRunsAcross(context.Background(), RunsAcrossQuery{Scopes: scope, Search: "100%"})
	if err != nil {
		t.Fatalf("search 100%%: %v", err)
	}
	if got := acrossJobPaths(page); !sameStrings(got, []string{"pay/z"}) {
		t.Fatalf("search 100%%: job paths = %v; want [pay/z]", got)
	}
}

func TestListRunsAcrossLimitClamped(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		seedAcrossRun(t, db, "pay", "pay/job", base.Add(time.Duration(i)*time.Minute), "COMPLETED", nil)
	}
	page, err := r.ListRunsAcross(context.Background(), RunsAcrossQuery{
		Scopes: []RunScope{{Cluster: "core", Tenant: "pay"}},
		Limit:  1000,
	})
	if err != nil {
		t.Fatalf("ListRunsAcross: %v", err)
	}
	if len(page.Runs) != 3 {
		t.Fatalf("got %d runs; want 3", len(page.Runs))
	}
}
