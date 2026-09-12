package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// baseActivityMaintenanceConfig returns a MaintenanceConfig with every
// non-activity retention pinned far in the future, so only the activity
// steps under test can delete anything.
func baseActivityMaintenanceConfig(now time.Time) MaintenanceConfig {
	return MaintenanceConfig{
		RunRetention:      3650 * 24 * time.Hour,
		LogRetention:      3650 * 24 * time.Hour,
		ArtifactRetention: 3650 * 24 * time.Hour,
		ActivityRetention: 30 * 24 * time.Hour,
		FinalizeWait:      time.Hour,
		Now:               func() time.Time { return now },
	}
}

func insertActivityRow(t *testing.T, db *sql.DB, ts time.Time, controller string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO varroa.activity (event_key, ts, type, controller, message, payload)
		VALUES ($1, $2, 'controller.created', $3, 'created', '{}')
	`, "key-"+controller+"-"+ts.Format(time.RFC3339Nano), ts, controller); err != nil {
		t.Fatalf("insert activity row for %s: %v", controller, err)
	}
}

// TestMaintenanceRoundDeletesExpiredActivityRow asserts a 31-day-old row
// is deleted when retention is 30d (step 5a), proving activity expiry is
// exact rather than rounded to a partition boundary.
func TestMaintenanceRoundDeletesExpiredActivityRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	if err := EnsurePartitions(ctx, pool, now, 40*24*time.Hour); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	insertActivityRow(t, db, now.Add(-31*24*time.Hour), "expired")
	insertActivityRow(t, db, now.Add(-1*time.Hour), "fresh")

	cfg := baseActivityMaintenanceConfig(now)
	if ran, err := RunMaintenanceRound(ctx, pool, cfg); err != nil || !ran {
		t.Fatalf("run maintenance round: ran=%v err=%v", ran, err)
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'expired'").Scan(&count); err != nil {
		t.Fatalf("count expired rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("want the 31-day-old row deleted under a 30d retention, got %d rows", count)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'fresh'").Scan(&count); err != nil {
		t.Fatalf("count fresh rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("want the fresh row kept, got %d rows", count)
	}
}

// TestMaintenanceRoundKeepsPartitionWithLiveRows proves step 5b drops no
// partition that still holds a live row: a partition holding both an
// expired and a fresh row survives once the expired row is deleted by
// step 5a.
func TestMaintenanceRoundKeepsPartitionWithLiveRows(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	if err := EnsurePartitions(ctx, pool, now, 40*24*time.Hour); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	// Both rows land in the current month's partition.
	insertActivityRow(t, db, now.Add(-1*time.Hour), "still-fresh")

	cfg := baseActivityMaintenanceConfig(now)
	if ran, err := RunMaintenanceRound(ctx, pool, cfg); err != nil || !ran {
		t.Fatalf("run maintenance round: ran=%v err=%v", ran, err)
	}

	var exists bool
	suffix := now.Format("200601")
	if err := db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relnamespace = 'varroa'::regnamespace)",
		"activity_p"+suffix,
	).Scan(&exists); err != nil {
		t.Fatalf("check partition: %v", err)
	}
	if !exists {
		t.Fatal("want the current month's activity partition to survive because it still holds a live row")
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'still-fresh'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("want the live row kept, got %d", count)
	}
}

// TestMaintenanceRoundDropsFullyExpiredActivityPartition proves step 5b
// drops a partition whose whole range is older than retention.activity.
func TestMaintenanceRoundDropsFullyExpiredActivityPartition(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	// A partition four months back is entirely older than a 30d
	// retention, and step 1 must create it since it reaches back that
	// far.
	if err := EnsurePartitions(ctx, pool, now, 120*24*time.Hour); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	oldMonth := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	insertActivityRow(t, db, oldMonth, "long-gone")

	cfg := baseActivityMaintenanceConfig(now)
	if ran, err := RunMaintenanceRound(ctx, pool, cfg); err != nil || !ran {
		t.Fatalf("run maintenance round: ran=%v err=%v", ran, err)
	}

	var exists bool
	suffix := oldMonth.Format("200601")
	if err := db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relnamespace = 'varroa'::regnamespace)",
		"activity_p"+suffix,
	).Scan(&exists); err != nil {
		t.Fatalf("check partition: %v", err)
	}
	if exists {
		t.Fatal("want the fully-expired activity partition dropped")
	}
}

// TestMaintenanceRoundLeavesJobScheduleCellsAlone asserts the round
// never deletes a job_schedule_cells row by age: a one-year-old
// watermark row survives, because the watermark must outlive an empty
// set to keep rejecting a stale envelope.
func TestMaintenanceRoundLeavesJobScheduleCellsAlone(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO varroa.job_schedule_cells (cluster, namespace, cell, observed_at)
		VALUES ('core', 'jenkins', 'old-cell', $1)
	`, now.AddDate(-1, 0, 0)); err != nil {
		t.Fatalf("insert job_schedule_cells row: %v", err)
	}

	cfg := baseActivityMaintenanceConfig(now)
	if ran, err := RunMaintenanceRound(ctx, pool, cfg); err != nil || !ran {
		t.Fatalf("run maintenance round: ran=%v err=%v", ran, err)
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.job_schedule_cells WHERE cell = 'old-cell'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("want the one-year-old watermark row kept, got %d rows", count)
	}
}

// TestMaintenanceRoundLeavesJobSchedulesAlone asserts the round never
// deletes a job_schedules row by age: a one-year-old row and a
// hibernated cell's rows (a long-since-observed cell that has sent no
// newer snapshot) both survive. job_schedules describes live state;
// only a later whole-cell set that omits a row, or C5's tenant purge,
// deletes one.
func TestMaintenanceRoundLeavesJobSchedulesAlone(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	insert := func(jobPath, cell string, observedAt time.Time) {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO varroa.job_schedules
				(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload)
			VALUES ('core', 'jenkins', $1, 'acme', $2, 'cron', $3, '{}')
		`, cell, jobPath, observedAt); err != nil {
			t.Fatalf("insert job_schedules row for %s: %v", jobPath, err)
		}
	}
	// A row one year old.
	insert("acme/old-job", "cell-a", now.AddDate(-1, 0, 0))
	// A hibernated controller's cell: its rows were last observed long
	// ago and the cell has sent no newer snapshot since.
	insert("acme/hibernated-job", "cell-hibernated", now.AddDate(0, -6, 0))

	cfg := baseActivityMaintenanceConfig(now)
	if ran, err := RunMaintenanceRound(ctx, pool, cfg); err != nil || !ran {
		t.Fatalf("run maintenance round: ran=%v err=%v", ran, err)
	}

	for _, jobPath := range []string{"acme/old-job", "acme/hibernated-job"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.job_schedules WHERE job_path = $1", jobPath).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", jobPath, err)
		}
		if count != 1 {
			t.Fatalf("want job_schedules row %s kept, got %d rows", jobPath, count)
		}
	}
}
