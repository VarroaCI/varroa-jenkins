package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// originExecDB runs one write. The record store passes its pool or a
// transaction.
type originExecDB interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// recordUpstreamOrigin stores that the recorder published an upstream
// intent key for the child run of tenant and jobPath. The row lets the
// run.started ingest stamp an upstream link from that key. A row that
// already exists keeps its first values.
//
// jobPath is the tenant-qualified job path that the claim stores in
// run_intents.job_path.
func recordUpstreamOrigin(ctx context.Context, db originExecDB, intentKey, cluster, tenant, jobPath string) error {
	if _, err := db.Exec(ctx, `
		INSERT INTO varroa.upstream_origins (intent_key, cluster, tenant, job_path)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (intent_key) DO NOTHING
	`, intentKey, cluster, tenant, jobPath); err != nil {
		return fmt.Errorf("record upstream origin: %w", err)
	}
	return nil
}

// upstreamOriginMatches reports whether the recorder wrote an origin row
// for intentKey that names the child run of cluster, tenant, and jobPath.
func upstreamOriginMatches(ctx context.Context, tx pgx.Tx, intentKey, cluster, tenant, jobPath string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM varroa.upstream_origins
		WHERE intent_key = $1 AND cluster = $2 AND tenant = $3 AND job_path = $4
	`, intentKey, cluster, tenant, jobPath).Scan(&one)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read upstream origin: %w", err)
	default:
		return true, nil
	}
}

// callerRunMatches reports whether callerRunID is a run that the ledger
// granted to cluster, tenant, and jobPath. The caller names the run, so the
// recorder checks the run's own intent row before it writes an origin for a
// key built from that run id. A run id that is not a UUIDv7 answers false.
func callerRunMatches(ctx context.Context, db *pgxpool.Pool, callerRunID, cluster, tenant, jobPath string) (bool, error) {
	id, _, err := parseRunUUIDv7(callerRunID)
	if err != nil {
		return false, nil //nolint:nilerr // a run id that is not a UUIDv7 names no granted run
	}
	var intentCluster, intentTenant, intentJob string
	err = db.QueryRow(ctx, `
		SELECT cluster, tenant, job_path FROM varroa.run_intents WHERE run_id = $1
	`, id).Scan(&intentCluster, &intentTenant, &intentJob)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read caller run intent: %w", err)
	default:
		return intentCluster == cluster && intentTenant == tenant && intentJob == jobPath, nil
	}
}

// sweepUpstreamOrigins deletes the origin rows created before the lower
// bound of the oldest surviving runs partition. A child run that is older
// than that bound no longer exists, so its origin row is no longer read.
func sweepUpstreamOrigins(ctx context.Context, db maintDB) (int, error) {
	lowerBound, err := oldestRunsPartitionLowerBound(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("resolve oldest runs partition: %w", err)
	}
	tag, err := db.Exec(ctx, `DELETE FROM varroa.upstream_origins WHERE created_at < $1`, lowerBound)
	if err != nil {
		return 0, fmt.Errorf("delete upstream origins older than the oldest runs partition: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
