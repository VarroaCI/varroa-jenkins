package recordstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// backstopLockKey is the pg_try_advisory_lock key the backstop-and-sweep
// round holds for its whole duration. migrateLockKey and
// maintenanceLockKey are C1's two fixed keys; this is the third.
const backstopLockKey = 8271340192837467

// scheduleTriggerTypeCron is job_schedules.trigger_type's value for a
// cron trigger. It is a separate vocabulary from IntentScheme, which
// happens to allocate the same word for a different concept, an
// intent-key scheme.
const scheduleTriggerTypeCron = "cron"

// BackstopConfig carries one backstop-and-sweep round's tunables.
type BackstopConfig struct {
	// MaxSweepPending is VARROA_SWEEP_MAX_PENDING, D9's interrupted-sweep
	// gate.
	MaxSweepPending uint64
	// ConfirmedGoneGrace is the wait a pod-read confirmation serves
	// before the round counts the instance gone. A zero value applies
	// DefaultConfirmedGoneGrace.
	ConfirmedGoneGrace time.Duration
	// SweepMetrics counts D9's interrupted-run marking.
	SweepMetrics *InterruptedMetrics
	// BackstopMetrics counts D11's fires and misses.
	BackstopMetrics *BackstopMetrics
	// ActionMetrics counts every action this round publishes.
	ActionMetrics *ActionMetrics
	// ClaimExpiryMetrics counts the runs the claim-expiry sweep marks.
	ClaimExpiryMetrics *ClaimExpiryMetrics
	// RunNotifier publishes the live notification of each run the
	// claim-expiry sweep marks. A nil RunNotifier publishes nothing.
	RunNotifier *RunNotifier
	// StateDeleter removes a marked run's replicated state. A nil
	// StateDeleter skips the delete, the same rule MaintenanceConfig's
	// LogStore follows. cmd/recorder passes a blob.Store value.
	StateDeleter ObjectDeleter
	// Now returns the round's reference time. A nil Now defaults to
	// time.Now in UTC, so a test can pin the round to a fixed instant.
	Now func() time.Time
	// Logger receives the round's own warnings. A nil Logger defaults to
	// slog.Default.
	Logger *slog.Logger
}

func (cfg BackstopConfig) now() time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now().UTC()
}

func (cfg BackstopConfig) logger() *slog.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return slog.Default()
}

func (cfg BackstopConfig) grace() time.Duration {
	if cfg.ConfirmedGoneGrace > 0 {
		return cfg.ConfirmedGoneGrace
	}
	return DefaultConfirmedGoneGrace
}

// RunBackstopRound tries the backstop advisory lock on one acquired
// connection, and on success runs D9's interrupted sweep, then the
// claim-expiry sweep, then the unstarted-intent re-fire, then D11's
// backstop cron round, before releasing it. It reports whether this call took the lock; a replica
// that does not returns false, nil.
func RunBackstopRound(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, cfg BackstopConfig) (bool, error) {
	pgConn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire backstop round connection: %w", err)
	}
	defer pgConn.Release()

	var got bool
	if err := pgConn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", backstopLockKey).Scan(&got); err != nil {
		return false, fmt.Errorf("try backstop round lock: %w", err)
	}
	if !got {
		return false, nil
	}
	defer func() {
		_, _ = pgConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", backstopLockKey)
	}()

	now := cfg.now()
	logger := cfg.logger()

	grace := cfg.grace()
	// The mesh takeover runs before the sweep, so an armed run is
	// shielded before the sweep reads it as a candidate.
	if err := runTakeoverRound(ctx, pool, conn, cfg.SweepMetrics, cfg.ActionMetrics, now, grace, logger); err != nil {
		logger.Warn("resume takeover round failed", "error", err)
	}

	if _, err := SweepInterruptedRuns(ctx, pool, conn, cfg.SweepMetrics, cfg.ActionMetrics,
		cfg.StateDeleter, cfg.MaxSweepPending, now, grace, logger); err != nil {
		logger.Warn("interrupted sweep failed", "error", err)
	}

	if _, err := ExpireUnclaimedRuns(ctx, pool, cfg.RunNotifier, cfg.ClaimExpiryMetrics, now, logger); err != nil {
		logger.Warn("claim-expiry sweep failed", "error", err)
	}

	if err := runRefireRound(ctx, pool, conn, cfg.SweepMetrics, cfg.ActionMetrics, now, grace, logger); err != nil {
		logger.Warn("unstarted-intent re-fire round failed", "error", err)
	}

	// The home recompute runs after the sweep. A job that first appears
	// between two announces waits at most one round for its home, and the
	// announce resync window is ten minutes.
	if _, err := RefreshJobHomes(ctx, pool, cfg.ActionMetrics, logger); err != nil {
		logger.Warn("job home recompute round failed", "error", err)
	}

	if err := runBackstopSchedule(ctx, pool, conn, cfg, now); err != nil {
		logger.Warn("backstop schedule round failed", "error", err)
	}

	return true, nil
}

// runBackstopSchedule runs D11's whole backstop cron round: it lists
// every distinct (cluster, tenant) pair job_schedules holds, reads each
// pair's policy, drops a pair whose cronBackstop is off, applies D11's
// candidate and fire-decision rules to every surviving cron row, then
// fires the round's ready candidates, oldest occurrence first, bounded
// at 100.
func runBackstopSchedule(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, cfg BackstopConfig, now time.Time) error {
	logger := cfg.logger()
	reader := NewReader(pool)

	pairs, err := listJobSchedulePairs(ctx, pool)
	if err != nil {
		return err
	}

	var fireCandidates []backstopFireCandidate
	homes := newJobHomeCache()
	for _, pair := range pairs {
		policy, err := readTenantSchedulePolicy(ctx, pool, pair.cluster, pair.tenant)
		if err != nil {
			logger.Warn("backstop round: read tenant policy failed", "cluster", pair.cluster, "tenant", pair.tenant, "error", err)
			continue
		}
		if !policy.cronBackstop {
			continue
		}

		rows, err := reader.ListJobSchedules(ctx, pair.cluster, pair.tenant)
		if err != nil {
			logger.Warn("backstop round: list job schedules failed", "cluster", pair.cluster, "tenant", pair.tenant, "error", err)
			continue
		}

		for _, row := range rows {
			if row.TriggerType != scheduleTriggerTypeCron || row.Disabled || len(row.NextFireTimes) == 0 {
				continue
			}
			if row.PlacementGeneration < policy.generation {
				continue
			}

			candidates, err := selectCandidateOccurrences(ctx, reader, pool, pair.cluster, row.JobPath, row.NextFireTimes, now, policy.cronGrace)
			if err != nil {
				logger.Warn("backstop round: select candidates failed", "job_path", row.JobPath, "error", err)
				continue
			}
			candidates, err = dropSupersededCandidates(ctx, pool, pair.cluster, row.JobPath, candidates, now)
			if err != nil {
				logger.Warn("backstop round: read newest fire failed", "job_path", row.JobPath, "error", err)
				continue
			}

			fire, missed := selectFire(candidates, policy.cronCatchUp)
			if missed > 0 {
				cfg.BackstopMetrics.addMissed(ctx, int64(missed))
			}
			if fire == nil {
				continue
			}

			// The fire goes to the job's home slot, the member the claim
			// fence reads. The row's own cell can name a former home. A
			// tenant with no placement row keeps the row's coordinate.
			namespace, cell := row.Namespace, row.Cell
			home, err := homes.resolve(ctx, pool, pair.cluster, pair.tenant, row.JobPath)
			if err != nil {
				logger.Warn("backstop round: read job home failed", "job_path", row.JobPath, "error", err)
				continue
			}
			if home.Placed {
				if home.Cell == "" {
					// No activity row is written, so a later round fires
					// this occurrence once the slot holds a cell.
					logger.Warn("backstop round: the job's home slot holds no cell", "job_path", row.JobPath)
					continue
				}
				namespace, cell = home.Namespace, home.Cell
			}

			fireCandidates = append(fireCandidates, backstopFireCandidate{
				cluster:    pair.cluster,
				tenant:     pair.tenant,
				namespace:  namespace,
				cell:       cell,
				jobPath:    row.JobPath,
				occurrence: *fire,
				grace:      policy.cronGrace,
			})
		}
	}

	for _, c := range selectReadyFires(fireCandidates, now) {
		fireOne(ctx, pool, conn, cfg.BackstopMetrics, cfg.ActionMetrics, c, now, logger)
	}
	return nil
}

// RunBackstopScheduler runs RunBackstopRound once a minute until ctx is
// cancelled, logging every outcome.
func RunBackstopScheduler(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, cfg BackstopConfig) {
	logger := cfg.logger()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		ran, err := RunBackstopRound(ctx, pool, conn, cfg)
		switch {
		case err != nil:
			logger.Error("backstop round failed", "error", err)
		case ran:
			logger.Debug("backstop round completed")
		default:
			logger.Debug("backstop round skipped; another replica holds the lock")
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
