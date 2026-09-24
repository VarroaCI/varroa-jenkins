package recordstore

import (
	"context"
	"testing"
)

// TestTenantPlacementsMemberColumnDefaults asserts that migration 00011
// adds "members" and "job_homes" with the empty non-null defaults every
// reader depends on. An empty "members" list resolves to one synthetic
// slot-0 member, and an empty "job_homes" map resolves every job to slot 0.
func TestTenantPlacementsMemberColumnDefaults(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.tenant_placements (cluster, tenant) VALUES ('core', 'acme')
	`); err != nil {
		t.Fatalf("insert tenant_placements row: %v", err)
	}

	var members, jobHomes string
	if err := db.QueryRowContext(ctx, `
		SELECT members::text, job_homes::text
		FROM varroa.tenant_placements WHERE cluster = 'core' AND tenant = 'acme'
	`).Scan(&members, &jobHomes); err != nil {
		t.Fatalf("read the member columns: %v", err)
	}
	if members != "[]" {
		t.Fatalf("members default: want [], got %q", members)
	}
	if jobHomes != "{}" {
		t.Fatalf("job_homes default: want {}, got %q", jobHomes)
	}
}

// TestTenantPlacementsMemberColumnsAreNotNull asserts both new columns
// refuse a null. A reader decodes them without a null test.
func TestTenantPlacementsMemberColumnsAreNotNull(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	for _, column := range []string{"members", "job_homes"} {
		var nullable string
		if err := db.QueryRowContext(ctx, `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = 'varroa' AND table_name = 'tenant_placements'
			  AND column_name = $1
		`, column).Scan(&nullable); err != nil {
			t.Fatalf("read the null rule for %s: %v", column, err)
		}
		if nullable != "NO" {
			t.Fatalf("%s is_nullable: want NO, got %q", column, nullable)
		}
	}
}

// TestSchemaHoldsThirteenTables asserts migration 00011 adds no table.
// The record store holds exactly thirteen tables in schema varroa after
// every migration runs.
func TestSchemaHoldsThirteenTables(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	got := queryTableNames(ctx, t, db)
	if len(got) != 13 {
		t.Fatalf("table count: want 13, got %d: %v", len(got), got)
	}
}
