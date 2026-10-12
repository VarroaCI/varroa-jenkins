package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// tenantRetentionJobBatch caps the runs one job loses in one round. A
// job with a large backlog drains over several hourly rounds.
const tenantRetentionJobBatch = 1000

// tenantRetentionRoundBudget caps the runs the whole tenant step deletes
// in one round. It bounds the blob deletes and the row transactions that
// one maintenance round holds.
const tenantRetentionRoundBudget = 10000

// maintTxDB is a maintDB that also opens a transaction. Both
// *pgxpool.Conn and *pgxpool.Pool satisfy it.
type maintTxDB interface {
	maintDB
	Begin(ctx context.Context) (pgx.Tx, error)
}

// tenantPolicyRow is one tenant_placements row that carries retention
// windows.
type tenantPolicyRow struct {
	cluster string
	tenant  string
	policy  TenantRetentionPolicy
}

// deleteTenantExpiredRuns deletes the terminal runs that a tenant or job
// window expired. A store-wide window still drives the partition drop.
// The step runs after the activity drop and before the partition drops.
//
// It deletes only runs whose status is COMPLETED or DUPLICATE. It never
// touches run_intents or job_numbers. The build-number watermark and the
// claim ledger must outlive every run, so a run that is deleted here
// never frees its number.
//
// It deletes at most tenantRetentionJobBatch runs per job and budget runs
// in total. It returns the number of runs it deleted.
func deleteTenantExpiredRuns(ctx context.Context, db maintTxDB, store ObjectDeleter, now time.Time, storeRuns time.Duration, budget int) (int, error) {
	rows, err := db.Query(ctx, `
		SELECT cluster, tenant, policy FROM varroa.tenant_placements
		ORDER BY cluster, tenant
	`)
	if err != nil {
		return 0, fmt.Errorf("select tenant retention policies: %w", err)
	}
	var tenants []tenantPolicyRow
	for rows.Next() {
		var row tenantPolicyRow
		var raw []byte
		if err := rows.Scan(&row.cluster, &row.tenant, &raw); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan tenant retention policy: %w", err)
		}
		row.policy = decodeRetention(raw)
		if !row.policy.IsZero() {
			tenants = append(tenants, row)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("select tenant retention policies: %w", err)
	}
	rows.Close()

	// The first tenant visited changes every hour. A tenant with a large
	// backlog then cannot use the whole budget on every round.
	used := 0
	start := 0
	if n := len(tenants); n > 0 {
		start = int((now.Unix() / 3600) % int64(n))
	}
	for i := range tenants {
		if used >= budget {
			break
		}
		t := tenants[(start+i)%len(tenants)]
		short := t.policy.shortest(storeRuns)
		if short >= storeRuns {
			continue
		}
		jobs, err := selectExpiredJobPaths(ctx, db, t.cluster, t.tenant, now.Add(-short))
		if err != nil {
			return used, err
		}
		for _, job := range jobs {
			if used >= budget {
				break
			}
			window := EffectiveRunRetention(storeRuns, t.policy, t.tenant, job)
			if window >= storeRuns {
				continue
			}
			limit := min(tenantRetentionJobBatch, budget-used)
			cutoff := now.Add(-window)
			runIDs, err := selectExpiredRunIDs(ctx, db, t.cluster, t.tenant, job, cutoff, limit)
			if err != nil {
				return used, err
			}
			if len(runIDs) == 0 {
				continue
			}
			n, err := purgeRunBatch(ctx, db, store, runIDs, cutoff)
			used += n
			if err != nil {
				return used, fmt.Errorf("purge tenant expired runs of %s/%s: %w", t.tenant, job, err)
			}
		}
	}
	return used, nil
}

// selectExpiredJobPaths lists the job paths of one tenant that hold a
// terminal run older than cutoff. The rows are read in full before the
// caller runs another statement, because the round holds one connection.
func selectExpiredJobPaths(ctx context.Context, db maintDB, cluster, tenant string, cutoff time.Time) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT DISTINCT job_path FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND created_at < $3
		  AND status IN ('COMPLETED', 'DUPLICATE')
		ORDER BY job_path
	`, cluster, tenant, cutoff)
	if err != nil {
		return nil, fmt.Errorf("select expired job paths: %w", err)
	}
	defer rows.Close()
	var jobs []string
	for rows.Next() {
		var job string
		if err := rows.Scan(&job); err != nil {
			return nil, fmt.Errorf("scan expired job path: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select expired job paths: %w", err)
	}
	return jobs, nil
}

// selectExpiredRunIDs lists up to limit terminal runs of one job that are
// older than cutoff, oldest first.
func selectExpiredRunIDs(ctx context.Context, db maintDB, cluster, tenant, job string, cutoff time.Time, limit int) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT run_id::text FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND created_at < $4
		  AND status IN ('COMPLETED', 'DUPLICATE')
		ORDER BY created_at, run_id
		LIMIT $5
	`, cluster, tenant, job, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("select expired runs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired run id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select expired runs: %w", err)
	}
	return ids, nil
}

// purgeRunBatch deletes the blobs and then the rows of one batch of runs.
//
// Blobs go first. A row that outlives its blob would point at nothing,
// but a blob that outlives its row is only a leak. If a blob delete fails,
// the function returns before it deletes any row. The next round retries,
// and every blob delete is idempotent. A nil store skips the blob deletes.
//
// The transaction locks each run row and rechecks its status before any
// blob delete. A run can leave a terminal status while the round works:
// a late run.started reopens a start-lost run. A locked row cannot change
// until the commit or rollback, so the status read here stays true. The
// blob deletes run inside the transaction, so a failed blob delete rolls
// back every row change. The created_at term on the runs delete lets the
// planner prune the monthly partitions.
func purgeRunBatch(ctx context.Context, db maintTxDB, store ObjectDeleter, runIDs []string, cutoff time.Time) (int, error) {
	if len(runIDs) == 0 {
		return 0, nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tenant run purge: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx, `
		SELECT run_id::text FROM varroa.runs
		WHERE run_id = ANY($1::uuid[]) AND created_at < $2
		  AND status IN ('COMPLETED', 'DUPLICATE')
		FOR UPDATE
	`, runIDs, cutoff)
	if err != nil {
		return 0, fmt.Errorf("lock expired runs: %w", err)
	}
	var locked []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan locked run: %w", err)
		}
		locked = append(locked, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("lock expired runs: %w", err)
	}
	rows.Close()
	if len(locked) == 0 {
		return 0, nil
	}

	if store != nil {
		if err := deleteRunBlobs(ctx, tx, store, locked); err != nil {
			return 0, err
		}
	}

	for _, stmt := range []string{
		`DELETE FROM varroa.run_stages WHERE run_id = ANY($1::uuid[])`,
		`DELETE FROM varroa.run_tests WHERE run_id = ANY($1::uuid[])`,
		`DELETE FROM varroa.run_test_cases WHERE run_id = ANY($1::uuid[])`,
		`DELETE FROM varroa.run_artifacts WHERE run_id = ANY($1::uuid[])`,
	} {
		if _, err := tx.Exec(ctx, stmt, locked); err != nil {
			return 0, fmt.Errorf("delete run children: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM varroa.runs WHERE run_id = ANY($1::uuid[]) AND created_at < $2`, locked, cutoff); err != nil {
		return 0, fmt.Errorf("delete runs: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit tenant run purge: %w", err)
	}
	return len(locked), nil
}

// deleteRunBlobs deletes every artifact, log, log index and run-state
// object of one batch of runs.
func deleteRunBlobs(ctx context.Context, db maintDB, store ObjectDeleter, runIDs []string) error {
	rows, err := db.Query(ctx, `SELECT blob_key FROM varroa.run_artifacts WHERE run_id = ANY($1::uuid[])`, runIDs)
	if err != nil {
		return fmt.Errorf("select run artifact blobs: %w", err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return fmt.Errorf("scan run artifact blob: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("select run artifact blobs: %w", err)
	}
	rows.Close()

	for _, key := range keys {
		if err := store.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete run artifact %s: %w", key, err)
		}
	}
	for _, id := range runIDs {
		if err := store.Delete(ctx, blob.LogKey(id)); err != nil {
			return fmt.Errorf("delete run log %s: %w", id, err)
		}
		if err := store.Delete(ctx, blob.LogIndexKey(id)); err != nil {
			return fmt.Errorf("delete run log index %s: %w", id, err)
		}
		if err := store.DeletePrefix(ctx, blob.RunStatePrefix(id)); err != nil {
			return fmt.Errorf("delete run state %s: %w", id, err)
		}
	}
	return nil
}
