package recordstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// upstreamLink is the four upstream columns of one run.started write. A
// nil field binds NULL. runID is nil when no upstream run matched.
type upstreamLink struct {
	runID  *string
	tenant *string
	job    *string
	number *int64
}

// upstreamRunFromIntentKey reads the upstream run id, and the created_at
// that its UUIDv7 carries, from an intent key of the form
// upstream:<runId>:<seq>. It answers false for any other key.
func upstreamRunFromIntentKey(key string) (uuid.UUID, time.Time, bool) {
	scheme, rest, found := strings.Cut(key, ":")
	if !found || scheme != string(IntentSchemeUpstream) {
		return uuid.UUID{}, time.Time{}, false
	}
	id, seq, found := strings.Cut(rest, ":")
	if !found || seq == "" {
		return uuid.UUID{}, time.Time{}, false
	}
	parsed, created, err := parseRunUUIDv7(id)
	if err != nil {
		return uuid.UUID{}, time.Time{}, false
	}
	return parsed, created, true
}

// retryRunFromIntentKey reads the source run id, and its created_at, from
// an intent key of the form retry:<runId>. It answers false for any other
// key.
func retryRunFromIntentKey(key string) (uuid.UUID, time.Time, bool) {
	scheme, rest, found := strings.Cut(key, ":")
	if !found || scheme != string(IntentSchemeRetry) {
		return uuid.UUID{}, time.Time{}, false
	}
	parsed, created, err := parseRunUUIDv7(rest)
	if err != nil {
		return uuid.UUID{}, time.Time{}, false
	}
	return parsed, created, true
}

// deriveUpstream answers the upstream columns of a run.started write. The
// rules run in this order:
//
//  1. An upstream intent key names the upstream run. The recorder stamps
//     that key only when it wrote an origin row for the key, with this
//     run's cluster, tenant, and job. A key with no such row falls through to the
//     later rules. The recorder reads that run's tenant, job, and number.
//     When the run row is absent, the link keeps the run id alone.
//  2. A retry intent key copies the four upstream columns of its source
//     run of the same tenant, because a retry repeats the cause of the
//     source. A source row with no upstream columns copies nothing.
//  3. Otherwise the native cause sent by the cell names the upstream job
//     path and build number. The recorder looks for the run in the
//     intent's own cluster and tenant only. A native cause never links
//     across tenants.
//  4. Otherwise the link is empty.
//
// Every lookup reads a row by its primary key, or by the (cluster,
// tenant, job_path, build_number) prefix of runs_job_idx.
func deriveUpstream(ctx context.Context, tx pgx.Tx, intent *intentRow, p RunStartedPayload) (upstreamLink, error) {
	id, created, isUpstreamKey := upstreamRunFromIntentKey(intent.intentKey)
	if isUpstreamKey {
		matched, err := upstreamOriginMatches(ctx, tx, intent.intentKey, intent.cluster, intent.tenant, intent.jobPath)
		if err != nil {
			return upstreamLink{}, err
		}
		isUpstreamKey = matched
	}
	if isUpstreamKey {
		runID := id.String()
		link := upstreamLink{runID: &runID}
		var tenant, job string
		var number *int64
		const producerSQL = `
			SELECT tenant, job_path, build_number FROM varroa.runs
			WHERE created_at = $1 AND run_id = $2
		`
		err := tx.QueryRow(ctx, producerSQL, created, id).Scan(&tenant, &job, &number)
		if errors.Is(err, pgx.ErrNoRows) {
			// The producer's own start may be in another open transaction.
			// Waiting for its lock and reading again closes that window.
			if err := lockProducer(ctx, tx, runID); err != nil {
				return upstreamLink{}, err
			}
			err = tx.QueryRow(ctx, producerSQL, created, id).Scan(&tenant, &job, &number)
		}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return link, nil
		case err != nil:
			return upstreamLink{}, fmt.Errorf("read upstream run %s: %w", id, err)
		}
		link.tenant, link.job, link.number = &tenant, &job, number
		return link, nil
	}

	if source, created, ok := retryRunFromIntentKey(intent.intentKey); ok {
		var link upstreamLink
		err := tx.QueryRow(ctx, `
			SELECT upstream_run_id::text, upstream_tenant, upstream_job, upstream_number
			FROM varroa.runs WHERE created_at = $1 AND run_id = $2 AND cluster = $3 AND tenant = $4
		`, created, source, intent.cluster, intent.tenant).Scan(&link.runID, &link.tenant, &link.job, &link.number)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return upstreamLink{}, fmt.Errorf("read retry source run %s: %w", source, err)
		default:
			return link, nil
		}
	}

	if p.UpstreamJobPath != nil && *p.UpstreamJobPath != "" && p.UpstreamBuildNumber != nil && *p.UpstreamBuildNumber > 0 {
		tenant := intent.tenant
		link := upstreamLink{tenant: &tenant, job: p.UpstreamJobPath, number: p.UpstreamBuildNumber}
		var id string
		err := tx.QueryRow(ctx, `
			SELECT run_id::text FROM varroa.runs
			WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND build_number = $4
			ORDER BY created_at DESC LIMIT 1
		`, intent.cluster, intent.tenant, *p.UpstreamJobPath, *p.UpstreamBuildNumber).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return upstreamLink{}, fmt.Errorf("read native upstream run: %w", err)
		default:
			link.runID = &id
		}
		return link, nil
	}

	return upstreamLink{}, nil
}

// lockProducer takes the transaction-scoped advisory lock of one producer
// run id. A child that finds no producer row and the producer's own start
// both take it, so the two writes serialize: whichever commits second sees
// the other's row.
func lockProducer(ctx context.Context, tx pgx.Tx, runID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('varroa.upstream:' || $1, 0))`, runID); err != nil {
		return fmt.Errorf("lock producer run %s: %w", runID, err)
	}
	return nil
}

// fillDownstreamUpstream completes the upstream columns of the runs that
// name the run this write starts, when those runs were stored before this
// run existed. Such a child keeps its upstream run id with a NULL tenant
// until this write arrives. It only fills NULL columns, so a later start
// never changes a link that already holds its tenant, job, and number.
func fillDownstreamUpstream(ctx context.Context, tx pgx.Tx, intent *intentRow, buildNumber *int64) error {
	if err := lockProducer(ctx, tx, intent.runID.String()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.runs
		SET upstream_tenant = $2, upstream_job = $3, upstream_number = $4
		WHERE upstream_run_id = $1 AND upstream_tenant IS NULL
	`, intent.runID, intent.tenant, intent.jobPath, buildNumber); err != nil {
		return fmt.Errorf("fill downstream upstream columns: %w", err)
	}
	return nil
}
