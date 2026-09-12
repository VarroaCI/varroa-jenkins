package recordstore

import (
	"context"
	"testing"
)

// TestTenantPlacementsPrimaryKey asserts the (cluster, tenant) primary
// key rejects a duplicate row.
func TestTenantPlacementsPrimaryKey(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	insert := func() error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO varroa.tenant_placements (cluster, tenant) VALUES ('core', 'acme')
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

// TestTenantPlacementsSeedAndLivenessDefaults asserts a row inserted with
// no explicit value for the two seed columns and the four liveness
// columns lands on D7's defaults: an empty rehydrated instance, an empty
// pending-jobs list, a dead cell, no liveness timestamp, and an empty
// dead-instances list.
func TestTenantPlacementsSeedAndLivenessDefaults(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.tenant_placements (cluster, tenant) VALUES ('core', 'acme')
	`); err != nil {
		t.Fatalf("insert tenant_placements row: %v", err)
	}

	var (
		rehydratedInstance string
		pendingJobs        string
		cellAlive          bool
		cellAliveAtNull    bool
		cellLastSeenAtNull bool
		deadInstances      string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT
			rehydrated_instance,
			pending_jobs::text,
			cell_alive,
			cell_alive_at IS NULL,
			cell_last_seen_at IS NULL,
			dead_instances::text
		FROM varroa.tenant_placements WHERE cluster = 'core' AND tenant = 'acme'
	`).Scan(&rehydratedInstance, &pendingJobs, &cellAlive, &cellAliveAtNull, &cellLastSeenAtNull, &deadInstances); err != nil {
		t.Fatalf("read defaults: %v", err)
	}

	if rehydratedInstance != "" {
		t.Fatalf("rehydrated_instance default: want empty, got %q", rehydratedInstance)
	}
	if pendingJobs != "[]" {
		t.Fatalf("pending_jobs default: want [], got %q", pendingJobs)
	}
	if cellAlive {
		t.Fatal("cell_alive default: want false, got true")
	}
	if !cellAliveAtNull {
		t.Fatal("cell_alive_at default: want NULL, got a value")
	}
	if !cellLastSeenAtNull {
		t.Fatal("cell_last_seen_at default: want NULL, got a value")
	}
	if deadInstances != "[]" {
		t.Fatalf("dead_instances default: want [], got %q", deadInstances)
	}
}
