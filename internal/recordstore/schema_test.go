package recordstore

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"
)

// columnSpec is the expected shape of one column, read back from
// information_schema.columns.
type columnSpec struct {
	name     string
	dataType string
	nullable bool
}

// expectedColumns mirrors the record store's fourteen tables, column for
// column: migration 00001's seven, plus migration 00002's "activity",
// migration 00003's "job_schedules" and "job_schedule_cells", migration
// 00005's "tenant_placements", migration 00006's "fleet_locks", and
// migration 00017's "run_test_cases".
// Migration 00007 adds three columns to "job_schedules" and creates no
// table. Migration 00011 adds two columns to "tenant_placements" and
// creates no table. Migration 00012 adds one column to "runs" and creates
// no table. Migration 00013 adds one column to "tenant_placements" and
// creates no table. Migration 00015 adds one column to "runs" and creates
// no table. Migration 00016 adds one column to "runs" and creates no
// table. Migration 00017 also adds one column to "runs". Migration 00018 adds
// one column to "runs" and creates no table. Migration 00019 adds one column
// to "fleet_locks" and creates no table. Migration 00020 drops two columns
// from "run_stages" and creates no table.
var expectedColumns = map[string][]columnSpec{
	"job_numbers": {
		{"cluster", "text", false},
		{"tenant", "text", false},
		{"job_path", "text", false},
		{"next_number", "bigint", false},
		{"updated_at", "timestamp with time zone", false},
	},
	"run_intents": {
		{"intent_key", "text", false},
		{"run_id", "uuid", false},
		{"created_at", "timestamp with time zone", false},
		{"cluster", "text", false},
		{"namespace", "text", false},
		{"tenant", "text", false},
		{"job_path", "text", false},
		{"cell", "text", true},
		{"build_number", "bigint", true},
		{"placement_generation", "bigint", false},
		{"state", "text", false},
		{"actor", "text", true},
		{"cause", "text", true},
		{"params_digest", "text", true},
		{"consumed_at", "timestamp with time zone", true},
		{"cell_instance", "text", true},
		{"refired_at", "timestamp with time zone", true},
		{"refire_attempts", "integer", false},
		{"granted_at", "timestamp with time zone", true},
		{"start_marked_at", "timestamp with time zone", true},
	},
	"runs": {
		{"run_id", "uuid", false},
		{"created_at", "timestamp with time zone", false},
		{"intent_key", "text", false},
		{"cluster", "text", false},
		{"namespace", "text", false},
		{"cell", "text", false},
		{"tenant", "text", false},
		{"job_path", "text", false},
		{"build_number", "bigint", true},
		{"branch", "text", true},
		{"scm_revision", "text", true},
		{"status", "text", false},
		{"result", "text", true},
		{"queued_at", "timestamp with time zone", true},
		{"started_at", "timestamp with time zone", true},
		{"ended_at", "timestamp with time zone", true},
		{"last_heartbeat_at", "timestamp with time zone", true},
		{"heartbeat_received_at", "timestamp with time zone", true},
		{"interrupted_superseded_at", "timestamp with time zone", true},
		{"duration_ms", "bigint", true},
		{"actor", "text", true},
		{"cause", "jsonb", true},
		{"params", "jsonb", true},
		{"changes", "jsonb", true},
		{"source", "jsonb", true},
		{"tests_total", "integer", true},
		{"tests_failed", "integer", true},
		{"tests_skipped", "integer", true},
		{"payload", "jsonb", true},
		{"summary", "text", true},
		{"inputs", "jsonb", true},
		{"test_report_key", "text", true},
		{"replay_of", "uuid", true},
		{"upstream_run_id", "uuid", true},
		{"upstream_tenant", "text", true},
		{"upstream_job", "text", true},
		{"upstream_number", "bigint", true},
	},
	"run_stages": {
		{"run_id", "uuid", false},
		{"stage_id", "text", false},
		{"created_at", "timestamp with time zone", false},
		{"parent_id", "text", true},
		{"name", "text", false},
		{"node_type", "text", false},
		{"status", "text", false},
		{"started_at", "timestamp with time zone", true},
		{"ended_at", "timestamp with time zone", true},
		{"duration_ms", "bigint", true},
		{"error", "text", true},
		{"payload", "jsonb", true},
	},
	"run_tests": {
		{"run_id", "uuid", false},
		{"suite", "text", false},
		{"created_at", "timestamp with time zone", false},
		{"total", "integer", false},
		{"failed", "integer", false},
		{"skipped", "integer", false},
		{"duration_ms", "bigint", true},
	},
	"run_test_cases": {
		{"run_id", "uuid", false},
		{"suite", "text", false},
		{"class_name", "text", false},
		{"name", "text", false},
		{"cluster", "text", false},
		{"tenant", "text", false},
		{"job_path", "text", false},
		{"status", "text", false},
		{"duration_ms", "bigint", true},
		{"error_details", "text", false},
		{"created_at", "timestamp with time zone", false},
	},
	"run_artifacts": {
		{"run_id", "uuid", false},
		{"path", "text", false},
		{"created_at", "timestamp with time zone", false},
		{"size_bytes", "bigint", false},
		{"digest", "text", true},
		{"content_type", "text", true},
		{"blob_key", "text", false},
	},
	"log_chunks": {
		{"object_key", "text", false},
		{"chunk_seq", "bigint", false},
		{"created_at", "timestamp with time zone", false},
		{"object_class", "text", false},
		{"content_type", "text", true},
		{"digest", "text", false},
		{"bytes", "bytea", false},
		{"writer", "text", false},
	},
	"activity": {
		{"event_key", "text", false},
		{"ts", "timestamp with time zone", false},
		{"type", "text", false},
		{"source", "text", false},
		{"severity", "text", false},
		{"cluster", "text", false},
		{"namespace", "text", false},
		{"controller", "text", false},
		{"actor", "text", false},
		{"message", "text", false},
		{"phase", "text", false},
		{"reason", "text", false},
		{"item_path", "text", false},
		{"build_number", "bigint", true},
		{"result", "text", false},
		{"url", "text", false},
		{"payload", "jsonb", false},
		{"search_tsv", "tsvector", false},
	},
	"job_schedules": {
		{"cluster", "text", false},
		{"namespace", "text", false},
		{"cell", "text", false},
		{"tenant", "text", false},
		{"job_path", "text", false},
		{"trigger_type", "text", false},
		{"spec", "text", false},
		{"timezone", "text", false},
		{"disabled", "boolean", false},
		{"next_fire_times", "ARRAY", false},
		{"observed_at", "timestamp with time zone", false},
		{"placement_generation", "bigint", false},
		{"payload", "jsonb", false},
		{"upstream_tenant", "text", false},
		{"upstream_job", "text", false},
		{"upstream_result", "text", false},
	},
	"job_schedule_cells": {
		{"cluster", "text", false},
		{"namespace", "text", false},
		{"cell", "text", false},
		{"observed_at", "timestamp with time zone", false},
	},
	"tenant_placements": {
		{"cluster", "text", false},
		{"tenant", "text", false},
		{"namespace", "text", false},
		{"cell", "text", false},
		{"generation", "bigint", false},
		{"cell_instance", "text", false},
		{"policy", "jsonb", false},
		{"rehydrated_instance", "text", false},
		{"pending_jobs", "jsonb", false},
		{"cell_alive", "boolean", false},
		{"cell_alive_at", "timestamp with time zone", true},
		{"cell_last_seen_at", "timestamp with time zone", true},
		{"dead_instances", "jsonb", false},
		{"announced_at", "timestamp with time zone", false},
		{"members", "jsonb", false},
		{"job_homes", "jsonb", false},
		{"incarnation", "text", false},
		{"agent_namespace", "text", false},
		{"retired_incarnations", "jsonb", false},
		{"notification_webhook", "jsonb", true},
	},
	"webhook_deliveries": {
		{"delivery_id", "uuid", false},
		{"run_id", "uuid", false},
		{"cluster", "text", false},
		{"tenant", "text", false},
		{"incarnation", "text", false},
		{"body", "bytea", false},
		{"state", "text", false},
		{"attempts", "integer", false},
		{"next_attempt_at", "timestamp with time zone", false},
		{"last_status", "integer", true},
		{"last_error", "text", true},
		{"created_at", "timestamp with time zone", false},
		{"updated_at", "timestamp with time zone", false},
	},
	"fleet_locks": {
		{"name", "text", false},
		{"run_id", "text", false},
		{"cluster", "text", false},
		{"tenant", "text", false},
		{"job_path", "text", false},
		{"cell_instance", "text", false},
		{"state", "text", false},
		{"fence_token", "bigint", false},
		{"waiting_since", "timestamp with time zone", false},
		{"acquired_at", "timestamp with time zone", true},
		{"releasing_since", "timestamp with time zone", true},
		{"expires_at", "timestamp with time zone", false},
		{"placement_generation", "bigint", false},
		{"owner_tenant", "text", false},
	},
}

// TestSchemaMatchesDesign runs every migration and compares the resulting
// schema against the record store's fourteen tables, column for column.
func TestSchemaMatchesDesign(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	wantTables := tableNames()
	gotTables := queryTableNames(ctx, t, db)
	if !slices.Equal(wantTables, gotTables) {
		t.Fatalf("table set mismatch: want %v, got %v", wantTables, gotTables)
	}

	for _, table := range wantTables {
		assertColumns(ctx, t, db, table, expectedColumns[table])
	}
}

func tableNames() []string {
	names := make([]string, 0, len(expectedColumns))
	for name := range expectedColumns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func queryTableNames(ctx context.Context, t *testing.T, db *sql.DB) []string {
	t.Helper()
	// pg_class, not information_schema, because a monthly partition and the
	// goose version table are both "BASE TABLE"s in information_schema; the
	// design's seven tables are the schema's non-partition relations minus
	// that version table.
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'varroa'
		  AND c.relkind IN ('r', 'p')
		  AND NOT c.relispartition
		  AND c.relname <> 'goose_db_version'
	`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table names: %v", err)
	}
	sort.Strings(names)
	return names
}

func assertColumns(ctx context.Context, t *testing.T, db *sql.DB, table string, want []columnSpec) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'varroa' AND table_name = $1
	`, table)
	if err != nil {
		t.Fatalf("list columns for %s: %v", table, err)
	}
	defer rows.Close()

	got := map[string]columnSpec{}
	for rows.Next() {
		var name, dataType, nullable string
		if err := rows.Scan(&name, &dataType, &nullable); err != nil {
			t.Fatalf("scan column for %s: %v", table, err)
		}
		got[name] = columnSpec{name: name, dataType: dataType, nullable: nullable == "YES"}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns for %s: %v", table, err)
	}

	if len(got) != len(want) {
		t.Fatalf("table %s: want %d columns, got %d (%v)", table, len(want), len(got), got)
	}
	for _, w := range want {
		g, ok := got[w.name]
		if !ok {
			t.Fatalf("table %s: missing column %s", table, w.name)
		}
		if g.dataType != w.dataType || g.nullable != w.nullable {
			t.Fatalf("table %s column %s: want {%s nullable=%v}, got {%s nullable=%v}",
				table, w.name, w.dataType, w.nullable, g.dataType, g.nullable)
		}
	}
}

// TestPartitionStrategy asserts runs and log_chunks are RANGE-partitioned
// on created_at, activity is RANGE-partitioned on ts, and that migration
// 00001 and migration 00002 each created the initial four partitions, so
// a fresh install accepts writes right away.
func TestPartitionStrategy(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	for _, table := range []string{"runs", "log_chunks", "activity"} {
		var relkind string
		if err := db.QueryRowContext(ctx,
			"SELECT relkind FROM pg_class WHERE relname = $1 AND relnamespace = 'varroa'::regnamespace",
			table,
		).Scan(&relkind); err != nil {
			t.Fatalf("read relkind for %s: %v", table, err)
		}
		if relkind != "p" {
			t.Fatalf("want %s to be a partitioned table (relkind=p), got %q", table, relkind)
		}

		var partitionCount int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_inherits i
			JOIN pg_class p ON p.oid = i.inhparent
			WHERE p.relname = $1 AND p.relnamespace = 'varroa'::regnamespace
		`, table).Scan(&partitionCount); err != nil {
			t.Fatalf("count partitions for %s: %v", table, err)
		}
		if partitionCount != 4 {
			t.Fatalf("want 4 initial partitions for %s, got %d", table, partitionCount)
		}
	}

	// A row for "now" must land inside a partition the migration created.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.runs
			(run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, now(), 'manual:test', 'core', 'jenkins', 'cell-a', 'acme', 'acme/app', 'QUEUED')
	`, uuid.New()); err != nil {
		t.Fatalf("insert into runs: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.log_chunks (object_key, chunk_seq, created_at, object_class, digest, bytes)
		VALUES ('logs/test-run', 0, now(), 'log', 'sha256:deadbeef', 'x')
	`); err != nil {
		t.Fatalf("insert into log_chunks: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.activity (event_key, ts, type, message, payload)
		VALUES ('deadbeef', now(), 'controller.created', 'created', '{}')
	`); err != nil {
		t.Fatalf("insert into activity: %v", err)
	}
}

// TestNonPartitionedTables asserts the other ten tables carry no RANGE
// partitioning, matching the allocation: only runs, log_chunks, and
// activity are partitioned.
func TestNonPartitionedTables(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	for _, table := range []string{
		"job_numbers", "run_intents", "run_stages", "run_tests",
		"run_artifacts", "job_schedules", "job_schedule_cells", "tenant_placements",
		"fleet_locks", "run_test_cases", "webhook_deliveries",
	} {
		var relkind string
		if err := db.QueryRowContext(ctx,
			"SELECT relkind FROM pg_class WHERE relname = $1 AND relnamespace = 'varroa'::regnamespace",
			table,
		).Scan(&relkind); err != nil {
			t.Fatalf("read relkind for %s: %v", table, err)
		}
		if relkind != "r" {
			t.Fatalf("want %s to be an ordinary table (relkind=r), got %q", table, relkind)
		}
	}
}

// TestIndexesExist asserts every index D1 names is present.
func TestIndexesExist(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	want := map[string][]string{
		"run_intents":    {"run_intents_conflict_idx", "run_intents_orphan_idx"},
		"runs":           {"runs_run_id_idx", "runs_job_idx", "runs_tenant_idx", "runs_scm_revision_idx", "runs_search_tsv_idx", "runs_upstream_run_id_idx", "runs_upstream_edge_idx"},
		"run_stages":     {"run_stages_created_at_idx"},
		"run_tests":      {"run_tests_created_at_idx"},
		"run_artifacts":  {"run_artifacts_created_at_idx"},
		"log_chunks":     {"log_chunks_object_idx", "log_chunks_class_idx"},
		"activity":       {"activity_namespace_controller_idx", "activity_cluster_idx", "activity_search_tsv_idx"},
		"job_schedules":  {"job_schedules_tenant_trigger_idx", "job_schedules_search_tsv_idx", "job_schedules_upstream_idx"},
		"fleet_locks":    {"fleet_locks_one_holder", "fleet_locks_cluster_owner_tenant"},
		"run_test_cases": {"run_test_cases_history_idx", "run_test_cases_created_at_idx"},
	}

	for table, indexes := range want {
		got := map[string]bool{}
		rows, err := db.QueryContext(ctx,
			"SELECT indexname FROM pg_indexes WHERE schemaname = 'varroa' AND tablename = $1", table)
		if err != nil {
			t.Fatalf("list indexes for %s: %v", table, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatalf("scan index name for %s: %v", table, err)
			}
			got[name] = true
		}
		rows.Close()

		for _, name := range indexes {
			if !got[name] {
				t.Fatalf("table %s: want index %s, got %v", table, name, got)
			}
		}
	}
}

// TestLedgerUniqueConstraints asserts both run_intents UNIQUE constraints
// reject a conflicting row.
func TestLedgerUniqueConstraints(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	insert := func(intentKey string, runID uuid.UUID, buildNumber int64) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO varroa.run_intents
				(intent_key, run_id, created_at, cluster, namespace, tenant, job_path, build_number, state)
			VALUES ($1, $2, now(), 'core', 'jenkins', 'acme', 'acme/app', $3, 'GRANTED')
		`, intentKey, runID, buildNumber)
		return err
	}

	runA := uuid.New()
	if err := insert("scm:acme/app:abc123", runA, 1); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	if err := insert("scm:acme/app:def456", runA, 2); err == nil {
		t.Fatal("want a UNIQUE violation on run_id, got none")
	}

	runB := uuid.New()
	if err := insert("scm:acme/app:ghi789", runB, 1); err == nil {
		t.Fatal("want a UNIQUE violation on (cluster, tenant, job_path, build_number), got none")
	}
}

// TestJobNumbersPrimaryKey asserts the (cluster, tenant, job_path)
// primary key rejects a duplicate row.
func TestJobNumbersPrimaryKey(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	insert := func() error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO varroa.job_numbers (cluster, tenant, job_path) VALUES ('core', 'acme', 'acme/app')
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

// TestRunsStatusCheckAllowsDuplicate asserts the runs.status CHECK
// constraint accepts DUPLICATE.
func TestRunsStatusCheckAllowsDuplicate(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.runs
			(run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, now(), 'scm:acme/app:abc123', 'core', 'jenkins', 'cell-a', 'acme', 'acme/app', 'DUPLICATE')
	`, uuid.New()); err != nil {
		t.Fatalf("insert with status DUPLICATE: %v", err)
	}
}

// TestRunStagesStatusCheck asserts the run_stages.status CHECK constraint
// equals C2's nine-value enumeration exactly: every named value inserts,
// and a value outside the set is rejected.
func TestRunStagesStatusCheck(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()

	insert := func(stageID, status string) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO varroa.run_stages (run_id, stage_id, created_at, name, node_type, status)
			VALUES ($1, $2, now(), 'Build', 'stage', $3)
		`, uuid.New(), stageID, status)
		return err
	}

	statuses := []string{
		"IN_PROGRESS", "SUCCESS", "UNSTABLE", "FAILURE", "ABORTED",
		"NOT_BUILT", "SKIPPED", "NOT_EXECUTED", "PAUSED",
	}
	for i, status := range statuses {
		if err := insert(uuid.NewString(), status); err != nil {
			t.Fatalf("insert with status %s (%d): %v", status, i, err)
		}
	}

	if err := insert(uuid.NewString(), "BOGUS"); err == nil {
		t.Fatal("want a CHECK violation for a status outside the enumeration, got none")
	}
}
