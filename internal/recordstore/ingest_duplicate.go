package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// runDuplicateBackstop stops a second live run of one revision. It skips
// an empty revision, because a freestyle SCM job reports none. It takes
// pg_advisory_xact_lock on the 64-bit hash Postgres's own
// hashtextextended computes over the tuple, joined with a unit separator
// so two different tuples with the same concatenation never collide. The
// lock is transaction-scoped and needs no unlock.
func runDuplicateBackstop(ctx context.Context, tx pgx.Tx, intent *intentRow, revision string) (*LedgerAction, error) {
	if revision == "" {
		return nil, nil
	}

	key := intent.cluster + "\x1f" + intent.tenant + "\x1f" + intent.jobPath + "\x1f" + revision
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return nil, fmt.Errorf("take duplicate backstop lock: %w", err)
	}

	var otherRunID uuid.UUID
	var otherCreatedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT run_id, created_at FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND scm_revision = $4
		  AND status IN ('QUEUED', 'RUNNING') AND run_id <> $5
		ORDER BY created_at
		LIMIT 1
	`, intent.cluster, intent.tenant, intent.jobPath, revision, intent.runID).Scan(&otherRunID, &otherCreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("find live revision duplicate: %w", err)
	}

	newerRunID, newerCreatedAt := newerOf(intent.runID, intent.createdAt, otherRunID, otherCreatedAt)

	var cell *string
	var buildNumber *int64
	var jobPath, namespace string
	err = tx.QueryRow(ctx, `
		UPDATE varroa.runs
		SET status = 'DUPLICATE'
		WHERE run_id = $1 AND created_at = $2 AND status IN ('QUEUED', 'RUNNING')
		RETURNING cell, build_number, job_path, namespace
	`, newerRunID, newerCreatedAt).Scan(&cell, &buildNumber, &jobPath, &namespace)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Another transaction already resolved this pair; nothing to publish.
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("mark duplicate run: %w", err)
	}

	controller := ""
	if cell != nil {
		controller = *cell
	}
	return &LedgerAction{
		Kind:        LedgerActionCancelRun,
		Cluster:     intent.cluster,
		Namespace:   namespace,
		Controller:  controller,
		Actor:       LedgerActionRecorder,
		RunID:       newerRunID.String(),
		JobPath:     jobPath,
		BuildNumber: buildNumber,
	}, nil
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
