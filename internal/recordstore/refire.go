// Package recordstore: the unstarted-intent re-fire, design D7. A
// run_intents row in state GRANTED whose build never started holds no
// runs row, so the interrupted sweep never sees it. Without this round
// that build stays lost until a person runs the job again.
package recordstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// emptyParamsDigest is the SHA-256 of an empty parameter set. The store
// holds no parameters for a build that never started, so the recorder
// re-fires only an intent this digest, an empty value, or a null value
// proves parameterless. A re-fire of a parameterized intent would run
// with each declared default and not with what the caller asked for.
const emptyParamsDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Re-fire outcome and reason labels recorder_intent_refired_total
// carries.
const (
	refireOutcomePublished    = "published"
	refireOutcomeSkipped      = "skipped"
	refireOutcomeCleared      = "cleared"
	refireReasonNone          = ""
	refireReasonParams        = "parameterized"
	refireReasonNoPlace       = "no-placement"
	refireReasonNotRehydrated = "not-rehydrated"
	refireReasonPublishFailed = "publish-failed"
	refireReasonExhausted     = "attempts-exhausted"
	refireReasonRejectedTemp  = "rejected-transient"
	refireReasonRejectedPerm  = "rejected-permanent"
)

// maxRefireAttempts bounds the retry loop a rejected trigger opens. A
// retried run keeps its own run identifier, so the runs table holds one
// row for every attempt and cannot count them; run_intents.refire_attempts
// is the only bound.
const maxRefireAttempts = 5

// refireCandidate is one GRANTED run_intents row the re-fire reads.
type refireCandidate struct {
	intentKey      string
	runID          string
	jobPath        string
	actor          string
	paramsDigest   string
	cellInstance   string
	refireAttempts int
}

// refirePlacement is the tenant's current placement identity. The action
// must name it, because the intent's own stored cell is the dead one.
type refirePlacement struct {
	namespace          string
	cell               string
	cellInstance       string
	rehydratedInstance string
}

// ready reports whether the placement can run a re-fired build now. A
// non-empty cell and instance are not enough: a replacement pod reports
// its UID as soon as the container starts, while its mite still holds no
// Jenkins token. A trigger that lands in that window gets a 401 from the
// security realm and comes back rejected.
//
// A rehydrated instance holds a token, because the rehydration reload
// itself needs one. Matching the reported seed against the live instance
// is therefore the one placement read that proves the cell can accept a
// trigger.
func (p refirePlacement) ready() bool {
	return p.cell != "" && p.cellInstance != "" && p.rehydratedInstance == p.cellInstance
}

// RefireLostIntents publishes one trigger_run action for each GRANTED
// intent of cluster/tenant whose owning cell instance is confirmed gone
// and whose build never started. It answers how many actions it
// published.
//
// The action carries the stored intent_key and run_id, so the repeat
// claim from the new cell takes the existing re-answer path and needs no
// new ledger rule.
func RefireLostIntents(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn,
	metrics *InterruptedMetrics, actionMetrics *ActionMetrics, cluster, tenant string,
	grace time.Duration, now time.Time, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	candidates, err := selectRefireCandidates(ctx, pool, cluster, tenant)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	row, exists, err := readTenantPlacementLivenessRow(ctx, pool, cluster, tenant)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	published := 0
	for _, c := range candidates {
		// Rule 1: only a pod-read confirmation re-fires. A stream loss
		// alone is never enough, and a live cell still holds the queue.
		if !confirmedGoneInstance(row.deadInstances, c.cellInstance, now, grace) {
			continue
		}
		// Rule 2: the intent must carry no parameters.
		if !parameterlessIntent(c.paramsDigest) {
			metrics.addRefired(ctx, refireOutcomeSkipped, refireReasonParams)
			continue
		}
		// Rule 3: the job's home slot must hold a live, rehydrated
		// holder. The action must name that holder, because the
		// intent's own stored cell is the dead one. The
		// operator clears its stored cell instance at the moment of the
		// confirmation, and a replacement pod reports its UID well
		// before its mite holds a Jenkins token. This rule therefore
		// defers the re-fire twice over. It never drops one: refired_at
		// stays null and a later round retries.
		placement := refirePlacementForJob(row.members, c.jobPath)
		if !placement.ready() {
			reason := refireReasonNoPlace
			if placement.cell != "" && placement.cellInstance != "" {
				reason = refireReasonNotRehydrated
			}
			metrics.addRefired(ctx, refireOutcomeSkipped, reason)
			continue
		}
		// Rule 4: the conditional marker is the whole idempotency
		// mechanism. Two recorder replicas therefore publish one action
		// between them.
		marked, err := markIntentRefired(ctx, pool, c.intentKey, now)
		if err != nil {
			logger.Warn("mark intent refired failed", "intent_key", c.intentKey, "error", err)
			continue
		}
		if !marked {
			continue
		}

		action := LedgerAction{
			Kind:       LedgerActionTriggerRun,
			Cluster:    cluster,
			Namespace:  placement.namespace,
			Controller: placement.cell,
			Actor:      c.actor,
			RunID:      c.runID,
			IntentKey:  c.intentKey,
			JobPath:    c.jobPath,
		}
		if err := PublishLedgerAction(conn, action); err != nil {
			// The marker must be cleared, not left behind. Both the
			// candidate query and the round's own tenant list filter on
			// refired_at IS NULL, and so does the dead-instances trim's
			// keep set. A marker left set by a failed publish therefore
			// loses the queued build forever and stops protecting the
			// evidence its next attempt needs.
			logger.Warn("publish re-fire trigger_run action failed", "intent_key", c.intentKey, "error", err)
			if clearErr := clearIntentRefired(ctx, pool, c.intentKey); clearErr != nil {
				logger.Error("clear the re-fire marker after a failed publish",
					"intent_key", c.intentKey, "error", clearErr)
			}
			metrics.addRefired(ctx, refireOutcomeSkipped, refireReasonPublishFailed)
			continue
		}
		metrics.addRefired(ctx, refireOutcomePublished, refireReasonNone)
		actionMetrics.addPublished(ctx, action.Kind)
		published++
	}
	return published, nil
}

// confirmedGoneInstance reports whether dead holds an entry for instance
// with a pod-read confirmation older than grace.
func confirmedGoneInstance(dead []TenantSyncDeadInstance, instance string, now time.Time,
	grace time.Duration) bool {
	if instance == "" {
		return false
	}
	for _, d := range dead {
		if d.Instance == instance {
			return confirmedGone(d, now, grace)
		}
	}
	return false
}

// parameterlessIntent reports whether digest proves an empty parameter
// set. A null or empty digest names an intent that carried none.
func parameterlessIntent(digest string) bool {
	return digest == "" || digest == emptyParamsDigest
}

// selectRefireCandidates reads every GRANTED, un-refired intent of
// cluster/tenant whose owning instance is named and whose run_id holds
// no runs row that records a build.
//
// The state alone is not enough: an intent moves to CONSUMED only when
// the run.started ingest lands, so a started build's intent stays
// GRANTED while the consumer lags. A re-fire of such an intent would
// start a second build of a run that already runs.
//
// An intent past maxRefireAttempts leaves the candidate set for good, so
// the retry loop ends and its dead instance stops being pinned.
func selectRefireCandidates(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string) ([]refireCandidate, error) {
	rows, err := pool.Query(ctx, `
		SELECT ri.intent_key, ri.run_id, ri.job_path, COALESCE(ri.actor, ''), COALESCE(ri.params_digest, ''),
		       COALESCE(ri.cell_instance, ''), ri.refire_attempts
		FROM varroa.run_intents ri
		WHERE ri.cluster = $1 AND ri.tenant = $2
		  AND ri.state = 'GRANTED'
		  AND ri.refired_at IS NULL
		  AND ri.refire_attempts < $3
		  AND ri.cell_instance IS NOT NULL AND ri.cell_instance <> ''
		  AND NOT EXISTS (
		    SELECT 1 FROM varroa.runs r WHERE r.run_id = ri.run_id AND `+recordsBuildRunSQL+`
		  )
	`, cluster, tenant, maxRefireAttempts)
	if err != nil {
		return nil, fmt.Errorf("select re-fire candidates for %s/%s: %w", cluster, tenant, err)
	}
	defer rows.Close()

	var out []refireCandidate
	for rows.Next() {
		var c refireCandidate
		if err := rows.Scan(&c.intentKey, &c.runID, &c.jobPath, &c.actor, &c.paramsDigest,
			&c.cellInstance, &c.refireAttempts); err != nil {
			return nil, fmt.Errorf("scan re-fire candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select re-fire candidates for %s/%s: %w", cluster, tenant, err)
	}
	return out, nil
}

// refirePlacementForJob answers the identity of the member that holds
// jobPath's home slot. The action names that holder, and never slot 0's
// cell for a job homed on another slot. A slot with no bound member
// answers a zero value, which rule 3 then defers on.
func refirePlacementForJob(row placementMembers, jobPath string) refirePlacement {
	home := memberForJob(row, jobPath)
	return refirePlacement{
		namespace:          home.Namespace,
		cell:               home.Cell,
		cellInstance:       home.Instance,
		rehydratedInstance: home.RehydratedInstance,
	}
}

// clearIntentRefired rolls the marker back after a failed publish, so a
// later round selects the intent again. It matches on the state as well
// as the key, so it never resurrects an intent a claim has consumed
// since the marker was written.
func clearIntentRefired(ctx context.Context, pool *pgxpool.Pool, intentKey string) error {
	if _, err := pool.Exec(ctx, `
		UPDATE varroa.run_intents SET refired_at = NULL
		WHERE intent_key = $1 AND state = 'GRANTED'
	`, intentKey); err != nil {
		return fmt.Errorf("clear the re-fire marker on intent %q: %w", intentKey, err)
	}
	return nil
}

// markIntentRefired writes the at-most-once marker and counts the
// attempt. It reports whether this call changed the row, which is the
// one signal that authorizes the publish.
//
// The marker is written BEFORE the publish, and that order is the
// cross-replica idempotency mechanism: two recorder replicas racing on
// one intent publish one action between them. A publish that then fails
// rolls the marker back through clearIntentRefired.
//
// refire_attempts never decreases, so a rejected trigger that clears the
// marker cannot restart the retry loop from zero. Past
// maxRefireAttempts the update matches nothing and the intent stays for
// a person.
func markIntentRefired(ctx context.Context, pool *pgxpool.Pool, intentKey string, now time.Time) (bool, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE varroa.run_intents SET refired_at = $2, refire_attempts = refire_attempts + 1
		WHERE intent_key = $1 AND refired_at IS NULL AND state = 'GRANTED'
		  AND refire_attempts < $3
	`, intentKey, now, maxRefireAttempts)
	if err != nil {
		return false, fmt.Errorf("mark intent %q refired: %w", intentKey, err)
	}
	return tag.RowsAffected() == 1, nil
}

// runRefireRound runs RefireLostIntents for every tenant that holds a
// re-fire candidate. It enumerates its own pairs from run_intents: a
// tenant can hold a granted intent and no cron schedule, so the backstop
// scheduler's schedule-derived list would miss it.
func runRefireRound(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, now time.Time, grace time.Duration, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	pairs, err := listRefirePairs(ctx, pool)
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if _, err := RefireLostIntents(ctx, pool, conn, metrics, actionMetrics,
			p.cluster, p.tenant, grace, now, logger); err != nil {
			logger.Warn("unstarted-intent re-fire failed", "cluster", p.cluster, "tenant", p.tenant, "error", err)
		}
	}
	return nil
}

// refirePair is one (cluster, tenant) the re-fire round visits.
type refirePair struct {
	cluster string
	tenant  string
}

// listRefirePairs answers every distinct (cluster, tenant) that holds a
// GRANTED, un-refired intent naming an owning instance.
func listRefirePairs(ctx context.Context, pool *pgxpool.Pool) ([]refirePair, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT cluster, tenant FROM varroa.run_intents
		WHERE state = 'GRANTED' AND refired_at IS NULL
		  AND refire_attempts < $1
		  AND cell_instance IS NOT NULL AND cell_instance <> ''
	`, maxRefireAttempts)
	if err != nil {
		return nil, fmt.Errorf("list re-fire tenant pairs: %w", err)
	}
	defer rows.Close()

	var out []refirePair
	for rows.Next() {
		var p refirePair
		if err := rows.Scan(&p.cluster, &p.tenant); err != nil {
			return nil, fmt.Errorf("scan re-fire tenant pair: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list re-fire tenant pairs: %w", err)
	}
	return out, nil
}
