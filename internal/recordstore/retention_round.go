package recordstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maintenanceLockKey is the pg_advisory_lock key one maintenance round
// holds for its whole duration. It is distinct from migrateLockKey, so a
// migration and a maintenance round never contend for the same lock.
const maintenanceLockKey = 8271340192837466

// maintDB is the subset of *pgxpool.Pool and *pgxpool.Conn every
// maintenance step needs. A step function takes this interface so a test
// can call it directly against a pool, while RunMaintenanceRound passes
// one connection held for the whole round.
type maintDB interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// MaintenanceConfig carries one maintenance round's tunables.
type MaintenanceConfig struct {
	// RunRetention is retention.runs, parsed by ParseRetention.
	RunRetention time.Duration
	// LogRetention is retention.logs, parsed by ParseRetention.
	LogRetention time.Duration
	// ArtifactRetention is retention.artifacts, parsed by ParseRetention.
	ArtifactRetention time.Duration
	// ActivityRetention is recordStore.retention.activity, parsed by
	// ParseRetention. It replaces the activity.retention dial.
	ActivityRetention time.Duration
	// LogStore deletes log and artifact blob objects. A nil LogStore
	// skips both object deletes, which a test that only exercises the
	// row-level steps relies on. cmd/recorder passes a blob.Store value,
	// which satisfies this interface.
	LogStore ObjectDeleter
	// Liveness judges whether a run whose status is not yet terminal
	// still counts as terminal, because its owning cell is gone.
	Liveness Liveness
	// FinalizeWait is VARROA_RECORDSTORE_LOG_FINALIZE_WAIT, C3's
	// finalize wait.
	FinalizeWait time.Duration
	// FleetLockMetrics counts an expired fleet_locks row this round's
	// sweep deletes. A nil FleetLockMetrics counts nothing.
	FleetLockMetrics *FleetLockMetrics
	// LockNotifier publishes one lock notification per fleet_locks row
	// this round's sweep moves or deletes. A nil LockNotifier publishes
	// nothing, which a test with no bus relies on.
	LockNotifier *RunNotifier
	// Now returns the round's reference time. A nil Now defaults to
	// time.Now, so a test can pin the round to a fixed instant.
	Now func() time.Time
	// testBarrier runs once the round's steps finish, while the round
	// still holds the maintenance advisory lock. A nil testBarrier runs
	// nothing. It is unexported so no production caller can set it; a
	// test reaches it only through WithTestBarrier in export_test.go, to
	// prove a second round cannot take the lock while the first holds it.
	testBarrier func()
}

// now returns cfg.Now(), or time.Now in UTC when cfg.Now is nil.
func (cfg MaintenanceConfig) now() time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now().UTC()
}

// RunMaintenanceRound tries the maintenance advisory lock on one
// connection acquired from pool, and runs one round when it gets the
// lock. It reports whether this call took the lock and ran the round; a
// replica that does not take the lock returns false, nil.
func RunMaintenanceRound(ctx context.Context, pool *pgxpool.Pool, cfg MaintenanceConfig) (bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire maintenance connection: %w", err)
	}
	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", maintenanceLockKey).Scan(&got); err != nil {
		return false, fmt.Errorf("try maintenance lock: %w", err)
	}
	if !got {
		return false, nil
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", maintenanceLockKey)
	}()

	if err := runMaintenanceSteps(ctx, conn, cfg); err != nil {
		return true, err
	}
	if cfg.testBarrier != nil {
		cfg.testBarrier()
	}
	return true, nil
}

// runMaintenanceSteps runs the spec's numbered steps 1 through 8 (activity
// adds steps 5a and 5b, renumbering the frozen 00001-era steps 5 to 8 as
// 6 to 9 in this function's order) and step 11 (the log-finalization
// sweep), all against db. Step 10, EnsureGrants, is not one of these:
// RunMaintenance calls it every tick regardless of which replica holds
// the advisory lock, because it is idempotent and every replica
// benefits from running it as soon as the client role appears.
func runMaintenanceSteps(ctx context.Context, db maintTxDB, cfg MaintenanceConfig) error {
	now := cfg.now()

	if err := EnsurePartitions(ctx, db, now, cfg.ActivityRetention); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}

	logsCutoff := now.Add(-cfg.LogRetention)
	if _, err := deleteExpiredLogObjects(ctx, db, cfg.LogStore, logsCutoff); err != nil {
		return fmt.Errorf("delete expired log objects: %w", err)
	}

	if _, err := deleteExpiredRunState(ctx, db, cfg.LogStore, now); err != nil {
		return fmt.Errorf("delete expired run state: %w", err)
	}

	artifactsCutoff := now.Add(-cfg.ArtifactRetention)
	if _, err := deleteExpiredArtifacts(ctx, db, cfg.LogStore, artifactsCutoff); err != nil {
		return fmt.Errorf("delete expired artifacts: %w", err)
	}

	runsCutoff := now.Add(-cfg.RunRetention)
	if _, _, _, err := deleteExpiredChildRows(ctx, db, runsCutoff); err != nil {
		return fmt.Errorf("delete expired child rows: %w", err)
	}

	// Step 5a: delete expired activity rows by exact range, before the
	// partition drop in step 5b, so effective activity retention is
	// exact.
	activityCutoff := now.Add(-cfg.ActivityRetention)
	if _, err := deleteExpiredActivity(ctx, db, activityCutoff); err != nil {
		return fmt.Errorf("delete expired activity rows: %w", err)
	}
	// Step 5b: drop a fully expired activity partition.
	if _, err := dropExpiredPartitions(ctx, db, "activity", activityCutoff); err != nil {
		return fmt.Errorf("drop expired activity partitions: %w", err)
	}

	// Step 5c: delete terminal runs that a tenant or job window expired.
	// It runs before the runs partition drop, so a window shorter than
	// the store-wide one still removes each run's blobs and rows.
	if _, err := deleteTenantExpiredRuns(ctx, db, cfg.LogStore, now, cfg.RunRetention, tenantRetentionRoundBudget); err != nil {
		return fmt.Errorf("delete tenant expired runs: %w", err)
	}

	// The retained script replica leaves on the record cutoff, before the
	// runs partition drop, so no retained object outlives its row.
	if _, err := deleteRetainedRunScripts(ctx, db, cfg.LogStore, runsCutoff); err != nil {
		return fmt.Errorf("delete retained run scripts: %w", err)
	}

	if _, err := dropExpiredPartitions(ctx, db, "runs", runsCutoff); err != nil {
		return fmt.Errorf("drop expired runs partitions: %w", err)
	}

	logChunksCutoff := logsCutoff
	if artifactsCutoff.Before(logChunksCutoff) {
		logChunksCutoff = artifactsCutoff
	}
	if _, err := dropExpiredPartitions(ctx, db, "log_chunks", logChunksCutoff); err != nil {
		return fmt.Errorf("drop expired log_chunks partitions: %w", err)
	}

	if _, err := deleteFinishedWebhookDeliveries(ctx, db, now.Add(-webhookDeliveryRetention)); err != nil {
		return fmt.Errorf("delete finished webhook deliveries: %w", err)
	}

	if _, _, err := sweepIntents(ctx, db, now); err != nil {
		return fmt.Errorf("sweep intents: %w", err)
	}

	if _, err := sweepRunWaits(ctx, db, now); err != nil {
		return fmt.Errorf("sweep run waits: %w", err)
	}

	if _, err := finalizeStaleLogs(ctx, db, cfg.Liveness, cfg.FinalizeWait, now); err != nil {
		return fmt.Errorf("finalize stale logs: %w", err)
	}

	_, expiredLocks, err := sweepFleetLocks(ctx, db, now, cfg.LockNotifier)
	if err != nil {
		return fmt.Errorf("sweep fleet locks: %w", err)
	}
	cfg.FleetLockMetrics.addExpired(ctx, expiredLocks)

	// No step here deletes a job_schedules, job_schedule_cells, or
	// job_branch_projects row. A branch row describes the live inventory
	// of a multibranch job, as a schedule row does. Only a later cell
	// export that omits it, or the tenant purge, deletes one.
	// job_schedules describes live state: only a later whole-cell set
	// that omits a row, or C5's tenant purge, deletes one. The
	// job_schedule_cells watermark must outlive an empty set, so a
	// deleted row would accept a stale envelope.

	return nil
}

// RunMaintenance runs one maintenance round every hour until ctx is
// cancelled, and calls EnsureGrants on every tick, before the round.
// EnsureGrants runs regardless of which replica takes the round's
// advisory lock, because CloudNativePG can create the client role at any
// time and every replica needs to notice.
func RunMaintenance(ctx context.Context, pool *pgxpool.Pool, sqlDB *sql.DB, cfg MaintenanceConfig, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := EnsureGrants(ctx, sqlDB); err != nil {
			logger.Warn("ensure grants failed", "error", err)
		}
		ran, err := RunMaintenanceRound(ctx, pool, cfg)
		switch {
		case err != nil:
			logger.Error("maintenance round failed", "error", err)
		case ran:
			logger.Info("maintenance round completed")
		default:
			logger.Debug("maintenance round skipped; another replica holds the lock")
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
