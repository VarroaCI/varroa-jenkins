package recordstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestActivitySearchTSVGenerated inserts a row with no explicit
// search_tsv write and asserts the generated column is populated from
// the pinned expression.
func TestActivitySearchTSVGenerated(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.activity (event_key, ts, type, message, actor, controller, namespace, item_path, payload)
		VALUES ('deadbeef', now(), 'controller.created', 'jenkins ready', 'nb', 'foo', 'jenkins', '', '{}')
	`); err != nil {
		t.Fatalf("insert into activity: %v", err)
	}

	var tsv string
	if err := db.QueryRowContext(ctx, `
		SELECT search_tsv::text FROM varroa.activity WHERE event_key = 'deadbeef'
	`).Scan(&tsv); err != nil {
		t.Fatalf("read search_tsv: %v", err)
	}
	if tsv == "" {
		t.Fatal("want search_tsv populated with no explicit write, got empty")
	}
	if !strings.Contains(tsv, "'jenkins'") || !strings.Contains(tsv, "'ready'") {
		t.Fatalf("want search_tsv to index message tokens, got %q", tsv)
	}
}

// TestSearchExpressionIndexesAreUsed asserts a query that repeats the
// pinned expression against runs and job_schedules is answered by an
// index scan, proving the expression index accelerates it.
func TestSearchExpressionIndexesAreUsed(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.runs
			(run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, now(), 'manual:test', 'core', 'jenkins', 'cell-a', 'acme', 'acme/app', 'QUEUED')
	`, uuid.New()); err != nil {
		t.Fatalf("insert into runs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.job_schedules
			(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload)
		VALUES ('core', 'jenkins', 'cell-a', 'acme', 'acme/app', 'cron', now(), '{}')
	`); err != nil {
		t.Fatalf("insert into job_schedules: %v", err)
	}
	// A single row is too small for the planner to prefer an index scan
	// over a sequential scan, so the search-index scenarios disable
	// sequential scans to prove the expression is repeated identically
	// (an expression index accelerates only a query that repeats it).
	if _, err := db.ExecContext(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}

	assertIndexScan(ctx, t, db, fmt.Sprintf(`
		SELECT run_id FROM varroa.runs
		WHERE %s @@ to_tsquery('simple', 'acme')
	`, runSearchExprSQL))
	assertIndexScan(ctx, t, db, fmt.Sprintf(`
		SELECT job_path FROM varroa.job_schedules
		WHERE %s @@ to_tsquery('simple', 'acme')
	`, jobSearchExprSQL))
}

// assertIndexScan runs EXPLAIN on query and fails the test unless the
// plan names an index scan, proving the query repeats the indexed
// expression rather than falling back to a sequential scan.
func assertIndexScan(ctx context.Context, t *testing.T, db *sql.DB, query string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN "+query)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain line: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate explain lines: %v", err)
	}
	if !strings.Contains(plan.String(), "Index Scan") {
		t.Fatalf("want an index scan in the plan, got:\n%s", plan.String())
	}
}

// TestJobSchedulesPrimaryKey asserts the (cluster, tenant, job_path,
// trigger_type) primary key rejects a duplicate row.
func TestJobSchedulesPrimaryKey(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	insert := func() error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO varroa.job_schedules
				(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload)
			VALUES ('core', 'jenkins', 'cell-a', 'acme', 'acme/app', 'cron', now(), '{}')
		`)
		return err
	}

	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("want a primary key violation on the second insert, got none")
	}
}

// TestJobScheduleCellsPrimaryKeyAndSurvival asserts the (cluster,
// namespace, cell) primary key rejects a duplicate row, and that the
// watermark row survives an empty set deleting every job_schedules row
// of that cell.
func TestJobScheduleCellsPrimaryKeyAndSurvival(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	insert := func() error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO varroa.job_schedule_cells (cluster, namespace, cell, observed_at)
			VALUES ('core', 'jenkins', 'cell-a', now())
		`)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("want a primary key violation on the second insert, got none")
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.job_schedules
			(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload)
		VALUES ('core', 'jenkins', 'cell-a', 'acme', 'acme/app', 'cron', now(), '{}')
	`); err != nil {
		t.Fatalf("insert job_schedules row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM varroa.job_schedules WHERE cluster = 'core' AND namespace = 'jenkins' AND cell = 'cell-a'`); err != nil {
		t.Fatalf("delete job_schedules rows: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM varroa.job_schedule_cells WHERE cluster = 'core' AND namespace = 'jenkins' AND cell = 'cell-a'
	`).Scan(&count); err != nil {
		t.Fatalf("count job_schedule_cells: %v", err)
	}
	if count != 1 {
		t.Fatalf("want the watermark row to survive an empty set, got count=%d", count)
	}
}
