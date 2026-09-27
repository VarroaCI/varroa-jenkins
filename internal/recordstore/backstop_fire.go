package recordstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// maxBackstopFiresPerRound is D11's 100-fire bound. The round carries
// every candidate above it to the next round.
const maxBackstopFiresPerRound = 100

// selectFire applies Team.spec.pipelines.cronCatchUp to one job's
// candidate occurrences, already filtered by isBackstopCandidate. "one"
// (the default) fires the newest and counts the rest missed; "none"
// fires nothing and counts every one missed. A nil fire means the round
// makes no claim for this job this round.
func selectFire(candidates []time.Time, catchUp string) (fire *time.Time, missed int) {
	if len(candidates) == 0 {
		return nil, 0
	}
	if catchUp == cronCatchUpNone {
		return nil, len(candidates)
	}

	newest := candidates[0]
	for _, c := range candidates[1:] {
		if c.After(newest) {
			newest = c
		}
	}
	return &newest, len(candidates) - 1
}

// spreadDelay computes D11's sha256(cluster|tenant|job_path) mod
// grace_seconds delay, so triggers selected during one occurrence minute
// spread across the grace window rather than firing in the same round.
func spreadDelay(cluster, tenant, jobPath string, grace time.Duration) time.Duration {
	graceSeconds := int64(grace / time.Second)
	if graceSeconds <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(cluster + "|" + tenant + "|" + jobPath))
	n := binary.BigEndian.Uint64(sum[:8])
	return time.Duration(int64(n%uint64(graceSeconds))) * time.Second //nolint:gosec // a spreading hash, not a security boundary
}

// backstopFireCandidate is one job's selected occurrence, still subject
// to the spread delay and the round's 100-fire bound.
type backstopFireCandidate struct {
	cluster    string
	tenant     string
	namespace  string
	cell       string
	jobPath    string
	occurrence time.Time
	grace      time.Duration
}

// spreadEligible reports whether c's spread delay has elapsed: now is at
// or past the occurrence plus the grace window plus the spreading hash's
// delay.
func spreadEligible(c backstopFireCandidate, now time.Time) bool {
	delay := spreadDelay(c.cluster, c.tenant, c.jobPath, c.grace)
	return !now.Before(c.occurrence.Add(c.grace).Add(delay))
}

// selectReadyFires filters candidates to those whose spread delay has
// elapsed, orders them by occurrence minute oldest first, and bounds the
// result at maxBackstopFiresPerRound. The round carries every candidate
// past the bound to its next run with no further action.
func selectReadyFires(candidates []backstopFireCandidate, now time.Time) []backstopFireCandidate {
	var ready []backstopFireCandidate
	for _, c := range candidates {
		if spreadEligible(c, now) {
			ready = append(ready, c)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].occurrence.Before(ready[j].occurrence) })
	if len(ready) > maxBackstopFiresPerRound {
		ready = ready[:maxBackstopFiresPerRound]
	}
	return ready
}

// fireOne publishes D11's fire for one candidate: a wake_cell action,
// then a retry_run action carrying the cron:<job-path>:<occurrence-
// minute> intent key and a fresh UUIDv7 run id, then one
// build.scheduled.backstop activity row. Both actions name c's namespace
// and cell. For a placed tenant, they are the member of the job's home
// slot. For a tenant with no placement row, they are the schedule row's
// own coordinate. It counts
// recorder_cron_fired_total and recorder_actions_published_total only
// once the retry_run action, the fire itself, publishes without error.
func fireOne(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, metrics *BackstopMetrics, actionMetrics *ActionMetrics,
	c backstopFireCandidate, now time.Time, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	wake := LedgerAction{
		Kind:       LedgerActionWakeCell,
		Cluster:    c.cluster,
		Namespace:  c.namespace,
		Controller: c.cell,
		Actor:      LedgerActionRecorder,
	}
	if err := PublishLedgerAction(conn, wake); err != nil {
		logger.Warn("backstop fire: publish wake_cell failed", "job_path", c.jobPath, "error", err)
	} else {
		actionMetrics.addPublished(ctx, wake.Kind)
	}

	runID, err := uuid.NewV7()
	if err != nil {
		logger.Warn("backstop fire: generate run id failed", "job_path", c.jobPath, "error", err)
		return
	}
	retry := LedgerAction{
		Kind:       LedgerActionRetryRun,
		Cluster:    c.cluster,
		Namespace:  c.namespace,
		Controller: c.cell,
		Actor:      LedgerActionRecorder,
		RunID:      runID.String(),
		IntentKey:  cronIntentKey(c.jobPath, c.occurrence),
		JobPath:    c.jobPath,
	}
	if err := PublishLedgerAction(conn, retry); err != nil {
		logger.Warn("backstop fire: publish retry_run failed", "job_path", c.jobPath, "error", err)
		return
	}
	actionMetrics.addPublished(ctx, retry.Kind)
	metrics.addFired(ctx)

	if err := writeBackstopActivity(ctx, pool, c, now); err != nil {
		logger.Warn("backstop fire: write activity row failed", "job_path", c.jobPath, "error", err)
	}
}

// writeBackstopActivity writes the fire's build.scheduled.backstop
// activity row, naming the job path and the occurrence minute so a
// later round's candidate suppression test can find it.
func writeBackstopActivity(ctx context.Context, pool *pgxpool.Pool, c backstopFireCandidate, now time.Time) error {
	event := activity.Event{
		Timestamp:  now,
		Type:       "build.scheduled.backstop",
		Source:     "recorder",
		Severity:   "info",
		Cluster:    c.cluster,
		Namespace:  c.namespace,
		Controller: c.cell,
		Actor:      LedgerActionRecorder,
		Message:    fmt.Sprintf("%s missed its scheduled build; the backstop retried it", c.jobPath),
		Reason:     occurrenceMinuteKey(c.occurrence),
		ItemPath:   c.jobPath,
	}
	event.Normalize()
	payloadJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal backstop activity payload: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO varroa.activity
			(event_key, ts, type, source, severity, cluster, namespace, controller,
			 actor, message, phase, reason, item_path, build_number, result, url, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (ts, event_key) DO NOTHING
	`,
		EventKey(event), event.Timestamp, event.Type, event.Source, event.Severity, event.Cluster, event.Namespace,
		event.Controller, event.Actor, event.Message, event.Phase, event.Reason, event.ItemPath,
		nullableBuildNumber(event.BuildNumber), event.Result, event.URL, string(payloadJSON),
	); err != nil {
		return fmt.Errorf("insert backstop activity row: %w", err)
	}
	return nil
}
