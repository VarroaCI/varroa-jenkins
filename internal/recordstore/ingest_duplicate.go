package recordstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// scmIntentKeyPrefix is the prefix of every intent key an SCM build holds.
const scmIntentKeyPrefix = string(IntentSchemeSCM) + ":"

// backstopSchemes are the intent-key schemes that the plugin mints when
// Jenkins schedules a build on its own: a branch event, a branch scan, a
// timer, or a trigger with no revision. Varroa supplies every other scheme,
// for a run that a person or a pipeline requests. Such a run can build a
// revision again on purpose, with other parameters and its own caller.
var backstopSchemes = []string{string(IntentSchemeSCM), string(IntentSchemeCron), string(IntentSchemeAPI)}

// isBackstopKey reports whether intentKey holds one of backstopSchemes.
// A key with no colon names no scheme, so it never matches, also when the
// whole key equals a scheme name.
func isBackstopKey(intentKey string) bool {
	scheme, _, found := strings.Cut(intentKey, ":")
	return found && slices.Contains(backstopSchemes, scheme)
}

// runDuplicateBackstop stops a second live run of one revision. It skips
// an empty revision, because a freestyle SCM job reports none. It compares
// only runs whose intent key holds one of backstopSchemes. A run with any
// other key is never marked and never makes another run a duplicate. Two
// runs that both hold an `scm:` intent key are not duplicates. The ledger
// gives one claim revision one key, so two such keys name two accepted
// revisions. A change-request merge build against a new base is one
// example: it shares the head commit with the older build. It takes
// pg_advisory_xact_lock on the 64-bit hash Postgres's own
// hashtextextended computes over the tuple, joined with a unit separator
// so two different tuples with the same concatenation never collide. The
// lock is transaction-scoped and needs no unlock.
//
// It also answers the run notification of the row it marked, built from
// that row's own identity, so the caller publishes it after the commit.
// Both answers are nil when it marks no row.
func runDuplicateBackstop(ctx context.Context, tx pgx.Tx, intent *intentRow, revision string) (*LedgerAction, *RunNotification, error) {
	if revision == "" || !isBackstopKey(intent.intentKey) {
		return nil, nil, nil
	}
	arrivingSCM := strings.HasPrefix(intent.intentKey, scmIntentKeyPrefix)

	key := intent.cluster + "\x1f" + intent.tenant + "\x1f" + intent.jobPath + "\x1f" + revision
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return nil, nil, fmt.Errorf("take duplicate backstop lock: %w", err)
	}

	var otherRunID uuid.UUID
	var otherCreatedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT run_id, created_at FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND scm_revision = $4
		  AND status IN ('QUEUED', 'RUNNING') AND run_id <> $5
		  AND strpos(intent_key, ':') > 0 AND split_part(intent_key, ':', 1) = ANY($8::text[])
		  AND NOT ($6::boolean AND starts_with(intent_key, $7))
		ORDER BY created_at
		LIMIT 1
	`, intent.cluster, intent.tenant, intent.jobPath, revision, intent.runID, arrivingSCM, scmIntentKeyPrefix,
		backstopSchemes).Scan(&otherRunID, &otherCreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("find live revision duplicate: %w", err)
	}

	newerRunID, newerCreatedAt := newerOf(intent.runID, intent.createdAt, otherRunID, otherCreatedAt)

	var cell *string
	var buildNumber *int64
	var cluster, tenant, jobPath, namespace string
	err = tx.QueryRow(ctx, `
		UPDATE varroa.runs
		SET status = 'DUPLICATE'
		WHERE run_id = $1 AND created_at = $2 AND status IN ('QUEUED', 'RUNNING')
		RETURNING cell, build_number, cluster, tenant, job_path, namespace
	`, newerRunID, newerCreatedAt).Scan(&cell, &buildNumber, &cluster, &tenant, &jobPath, &namespace)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Another transaction already resolved this pair; nothing to publish.
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("mark duplicate run: %w", err)
	}

	controller := ""
	if cell != nil {
		controller = *cell
	}
	marked := duplicateNotification(cluster, tenant, jobPath, newerRunID.String())
	return &LedgerAction{
		Kind:        LedgerActionCancelRun,
		Cluster:     intent.cluster,
		Namespace:   namespace,
		Controller:  controller,
		Actor:       LedgerActionRecorder,
		RunID:       newerRunID.String(),
		JobPath:     jobPath,
		BuildNumber: buildNumber,
	}, &marked, nil
}

// newerOf orders two (created_at, run_id) pairs by design's total order
// and returns the newer one. Two runs that share a timestamp still order,
// because uuid.UUID compares byte for byte.
func newerOf(runA uuid.UUID, createdA time.Time, runB uuid.UUID, createdB time.Time) (uuid.UUID, time.Time) {
	if createdA.After(createdB) {
		return runA, createdA
	}
	if createdB.After(createdA) {
		return runB, createdB
	}
	if string(runA[:]) > string(runB[:]) {
		return runA, createdA
	}
	return runB, createdB
}
