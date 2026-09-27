package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// ClaimExpiryAge is how long an accepted run can wait for its first
// claim before the sweep marks it TRIGGER_FAILED. The plugin stops
// claiming one hour after it first tries. The margin of 15 minutes
// covers the plugin's last backoff step and the round interval.
const ClaimExpiryAge = 75 * time.Minute

// claimExpiryBatch bounds the runs one round marks. Each mark commits its
// own transaction under the backstop lock, so a large backlog must drain
// over several rounds and never hold the lock for the whole backlog.
const claimExpiryBatch = 500

// claimExpiredMessage is the trigger_error text an expired run holds.
const claimExpiredMessage = "no build claim reached the record store within 75 minutes of acceptance"

// ClaimExpiryMetrics holds the claim-expiry sweep's counter on the
// "varroa-recorder" meter.
type ClaimExpiryMetrics struct {
	expired metric.Int64Counter
}

// NewClaimExpiryMetrics registers recorder_claim_expired_total.
func NewClaimExpiryMetrics() (*ClaimExpiryMetrics, error) {
	meter := otel.Meter("varroa-recorder")
	expired, err := meter.Int64Counter("recorder_claim_expired_total",
		metric.WithDescription("Accepted runs marked TRIGGER_FAILED because no claim reached the ledger"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_claim_expired_total: %w", err)
	}
	return &ClaimExpiryMetrics{expired: expired}, nil
}

// addExpired counts one expired run. A nil receiver is a no-op.
func (m *ClaimExpiryMetrics) addExpired(ctx context.Context) {
	if m == nil || m.expired == nil {
		return
	}
	m.expired.Add(ctx, 1)
}

// claimExpiryCandidate is one accepted run the sweep can expire.
type claimExpiryCandidate struct {
	runID     uuid.UUID
	createdAt time.Time
	cluster   string
	namespace string
	cell      string
	tenant    string
	jobPath   string
	actor     string
}

// claimExpiryGuardSQL is the test both the candidate read and the
// conditional write apply. A run qualifies only while it is QUEUED, holds
// no build number, holds an accepted_at older than the cutoff ($1),
// holds no mesh resume record, and has no ledger intent. The write
// repeats the test, so a claim or a real event that lands between the
// read and the write keeps the run.
//
// The age test is the only part that the veto mark does not share.
const claimExpiryGuardSQL = `
	(r.payload->>'accepted_at')::timestamptz <= $1
	AND ` + unclaimedQueuedRunSQL

// unclaimedQueuedRunSQL matches a run row "r" that waits for its first
// claim: it is QUEUED, holds no build number, holds no mesh resume
// record, and has no ledger intent. The claim-expiry sweep and the veto
// mark both apply it in the write itself, so a claim or a real event
// that commits first keeps the run.
//
// The resume test says IS DISTINCT FROM, never <>: a missing mesh key
// makes jsonb_typeof answer SQL NULL.
const unclaimedQueuedRunSQL = `
	r.status = 'QUEUED'
	AND r.build_number IS NULL
	AND jsonb_typeof(r.payload->'mesh'->'resume') IS DISTINCT FROM 'object'
	AND NOT EXISTS (SELECT 1 FROM varroa.run_intents ri WHERE ri.run_id = r.run_id)`

// selectClaimExpiryCandidates reads the oldest claimExpiryBatch runs the
// guard admits at cutoff.
func selectClaimExpiryCandidates(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) ([]claimExpiryCandidate, error) {
	rows, err := pool.Query(ctx, `
		SELECT r.run_id, r.created_at, r.cluster, r.namespace, r.cell, r.tenant, r.job_path, r.actor
		FROM varroa.runs r
		WHERE `+claimExpiryGuardSQL+`
		ORDER BY (r.payload->>'accepted_at')::timestamptz
		LIMIT $2`, cutoff, claimExpiryBatch)
	if err != nil {
		return nil, fmt.Errorf("select claim-expiry candidates: %w", err)
	}
	defer rows.Close()

	var out []claimExpiryCandidate
	for rows.Next() {
		var (
			c     claimExpiryCandidate
			actor *string
		)
		if err := rows.Scan(&c.runID, &c.createdAt, &c.cluster, &c.namespace, &c.cell, &c.tenant,
			&c.jobPath, &actor); err != nil {
			return nil, fmt.Errorf("scan claim-expiry candidate: %w", err)
		}
		c.actor = derefString(actor)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select claim-expiry candidates: %w", err)
	}
	return out, nil
}

// markClaimExpired writes one run's TRIGGER_FAILED record and its
// build.claim_expired activity row in one transaction. It reports
// whether the conditional update changed the row. A miss means a claim
// or a real event won the race.
//
// The write keeps the accepted keys, so every reader that asks for a
// real build still excludes the row. A late run.started replaces the
// row through the ordinary TRIGGER_FAILED supersede.
func markClaimExpired(ctx context.Context, pool *pgxpool.Pool, c claimExpiryCandidate, cutoff, now time.Time) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin claim-expiry marking: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE varroa.runs r
		SET status = 'COMPLETED',
		    result = '`+ResultTriggerFailed+`',
		    ended_at = $4,
		    payload = COALESCE(r.payload, '{}'::jsonb) || jsonb_build_object(
		        'trigger_error', $5::text,
		        'trigger_reason_code', $6::text,
		        'trigger_failed_at', $4::timestamptz)
		WHERE r.run_id = $2 AND r.created_at = $3 AND `+claimExpiryGuardSQL,
		cutoff, c.runID, c.createdAt, now, claimExpiredMessage, ReasonClaimExpired)
	if err != nil {
		return false, fmt.Errorf("mark run claim-expired: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	event := activity.Event{
		Timestamp:  now,
		Type:       "build.claim_expired",
		Source:     "recorder",
		Severity:   "warning",
		Cluster:    c.cluster,
		Namespace:  c.namespace,
		Controller: c.cell,
		Actor:      c.actor,
		// The run id keeps the event key unique. Two runs of one job share
		// the round time, the cell, and an empty build number.
		Message: fmt.Sprintf("%s run %s never claimed a build number and was marked failed (%s)",
			c.jobPath, c.runID, ReasonClaimExpired),
		Reason:   ReasonClaimExpired,
		ItemPath: c.jobPath,
		Result:   ResultTriggerFailed,
	}
	if err := insertRunActivityTx(ctx, tx, event); err != nil {
		return false, fmt.Errorf("claim-expiry activity row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit claim-expiry marking: %w", err)
	}
	return true, nil
}

// insertRunActivityTx normalizes event and writes it as one activity row
// through db, which is the caller's transaction. The row then commits or
// rolls back with the run write it records. A row with the same key is
// kept as it is.
func insertRunActivityTx(ctx context.Context, db DBTX, event activity.Event) error {
	event.Normalize()
	payloadJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal activity payload: %w", err)
	}
	if _, err := db.Exec(ctx, `
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
		return fmt.Errorf("insert activity row: %w", err)
	}
	return nil
}

// ExpireUnclaimedRuns marks every accepted run that no claim reached
// within ClaimExpiryAge as TRIGGER_FAILED with reason CLAIM_EXPIRED. It
// answers how many runs it marked.
//
// Such a run holds no ledger intent, so neither the interrupted sweep
// nor the re-fire can end it while its cell stays alive. The plugin
// stops claiming after one hour, and after that no event ever moves
// the row.
//
// The age limit does not prove that the plugin tried to claim. A queue
// item that waits longer than the limit before its first claim, for
// example behind a long previous build, is marked too. Its later claim
// and run.started then replace the row with the real build.
//
// A nil notifier and nil metrics are no-ops.
func ExpireUnclaimedRuns(ctx context.Context, pool *pgxpool.Pool, notifier *RunNotifier,
	metrics *ClaimExpiryMetrics, now time.Time, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cutoff := now.Add(-ClaimExpiryAge)

	candidates, err := selectClaimExpiryCandidates(ctx, pool, cutoff)
	if err != nil {
		return 0, err
	}

	marked := 0
	for _, c := range candidates {
		changed, err := markClaimExpired(ctx, pool, c, cutoff, now)
		if err != nil {
			logger.Warn("claim-expiry sweep: mark failed", "run_id", c.runID, "error", err)
			continue
		}
		if !changed {
			continue
		}
		metrics.addExpired(ctx)
		notifier.Notify(RunNotification{
			Kind:    "run",
			Cluster: c.cluster,
			Tenant:  c.tenant,
			JobPath: c.jobPath,
			RunID:   c.runID.String(),
			Status:  StatusCompleted,
			Result:  ResultTriggerFailed,
		})
		logger.Debug("accepted run marked claim-expired", "run_id", c.runID, "job_path", c.jobPath)
		marked++
	}
	if marked > 0 {
		logger.Info("claim-expiry sweep marked accepted runs", "count", marked)
	}
	return marked, nil
}
