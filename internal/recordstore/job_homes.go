package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recomputeJobHomes answers the tenant's home map over members, from the
// union of its job_numbers and job_schedules paths. The recorder is the
// map's only writer. It counts each moved path in
// recorder_job_home_changed_total, labelled by cause.
func recomputeJobHomes(ctx context.Context, tx pgx.Tx, cluster, tenant string, members []member,
	before map[string]int32, metrics *ActionMetrics) (map[string]int32, error) {
	paths, err := tenantJobPaths(ctx, tx, cluster, tenant)
	if err != nil {
		return nil, err
	}
	after := computeJobHomes(paths, members)
	countJobHomeChanges(ctx, metrics, before, after, activeSlotCount(members) != slotCountOfMap(before))
	return after, nil
}

// slotCountOfMap answers the highest slot the stored map names, plus one.
// It is the active slot count the previous recompute used, which names a
// scale apart from a takeover.
func slotCountOfMap(homes map[string]int32) int32 {
	highest := int32(0)
	for _, slot := range homes {
		if slot > highest {
			highest = slot
		}
	}
	return highest + 1
}

// countJobHomeChanges adds one count for each moved path. A path absent
// from the previous map counts as a new job. A path whose slot changed
// counts as a scale when the active slot count changed, and as a takeover
// otherwise.
func countJobHomeChanges(ctx context.Context, metrics *ActionMetrics, before, after map[string]int32, scaled bool) {
	for _, path := range movedJobPaths(before, after) {
		cause := jobHomeCauseTakeover
		if _, ok := before[path]; !ok {
			cause = jobHomeCauseNewJob
		} else if scaled {
			cause = jobHomeCauseScale
		}
		metrics.addJobHomeChanged(ctx, cause)
	}
}

// tenantJobPaths answers the union of the tenant's job_numbers paths and
// its job_schedules paths. A path in neither has no home entry, and every
// reader resolves such a path to slot 0.
func tenantJobPaths(ctx context.Context, tx pgx.Tx, cluster, tenant string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT job_path FROM varroa.job_numbers WHERE cluster = $1 AND tenant = $2
		UNION
		SELECT job_path FROM varroa.job_schedules WHERE cluster = $1 AND tenant = $2
	`, cluster, tenant)
	if err != nil {
		return nil, fmt.Errorf("list tenant job paths: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan tenant job path: %w", err)
		}
		out = append(out, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant job paths: %w", err)
	}
	return out, nil
}

// RefreshJobHomes recomputes every tenant's home map, one tenant at a
// time. It runs in the once-a-minute backstop round, because a new job
// appears between two announces and the announce resync window is ten
// minutes.
//
// It writes a new map only when the map changes, so a repeat round writes
// nothing. It answers the number of tenants whose map changed.
func RefreshJobHomes(ctx context.Context, pool *pgxpool.Pool, metrics *ActionMetrics,
	logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	pairs, err := listPlacementPairs(ctx, pool)
	if err != nil {
		return 0, err
	}

	changed := 0
	for _, pair := range pairs {
		ok, err := refreshOneTenantJobHomes(ctx, pool, metrics, pair.cluster, pair.tenant)
		if err != nil {
			logger.Warn("job home recompute failed",
				"cluster", pair.cluster, "tenant", pair.tenant, "error", err)
			continue
		}
		if ok {
			changed++
		}
	}
	return changed, nil
}

// placementPair names one stored tenant placement.
type placementPair struct {
	cluster string
	tenant  string
}

// listPlacementPairs answers every stored placement's cluster and tenant.
func listPlacementPairs(ctx context.Context, pool *pgxpool.Pool) ([]placementPair, error) {
	rows, err := pool.Query(ctx, `
		SELECT cluster, tenant FROM varroa.tenant_placements ORDER BY cluster, tenant
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenant placements: %w", err)
	}
	defer rows.Close()

	var out []placementPair
	for rows.Next() {
		var p placementPair
		if err := rows.Scan(&p.cluster, &p.tenant); err != nil {
			return nil, fmt.Errorf("scan tenant placement: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant placements: %w", err)
	}
	return out, nil
}

// refreshOneTenantJobHomes recomputes one tenant's home map inside a
// transaction that locks the placement row, so the announce handler and
// this round never write the column at the same time. It reports whether
// the map changed.
func refreshOneTenantJobHomes(ctx context.Context, pool *pgxpool.Pool, metrics *ActionMetrics,
	cluster, tenant string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin job home transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	old, err := readTenantPlacementForUpdate(ctx, tx, cluster, tenant)
	if err != nil {
		return false, err
	}
	after, err := recomputeJobHomes(ctx, tx, cluster, tenant, old.members, old.jobHomes, metrics)
	if err != nil {
		return false, err
	}
	if jobHomesEqual(old.jobHomes, after) {
		return false, nil
	}
	encoded, err := json.Marshal(nonNilJobHomes(after))
	if err != nil {
		return false, fmt.Errorf("encode job homes: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.tenant_placements SET job_homes = $3::jsonb
		WHERE cluster = $1 AND tenant = $2
	`, cluster, tenant, string(encoded)); err != nil {
		return false, fmt.Errorf("update job homes: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit job home transaction: %w", err)
	}
	return true, nil
}

// readTenantPlacementMembers reads cluster/tenant's member view with no
// lock. It answers exists=false, with no error, when no row is stored.
// Every reader of a member's identity starts here.
func readTenantPlacementMembers(ctx context.Context, db livenessQuerier, cluster, tenant string) (placementMembers, bool, error) {
	var (
		cell               string
		namespace          string
		cellInstance       string
		rehydratedInstance string
		pendingRaw         json.RawMessage
		membersRaw         json.RawMessage
		jobHomesRaw        json.RawMessage
	)
	err := db.QueryRow(ctx, `
		SELECT cell, namespace, cell_instance, rehydrated_instance, pending_jobs, members, job_homes
		FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2
	`, cluster, tenant).Scan(&cell, &namespace, &cellInstance, &rehydratedInstance,
		&pendingRaw, &membersRaw, &jobHomesRaw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return placementMembers{jobHomes: map[string]int32{}}, false, nil
	case err != nil:
		return placementMembers{}, false, fmt.Errorf("read tenant placement members: %w", err)
	}

	var pending []string
	if len(pendingRaw) > 0 {
		if err := json.Unmarshal(pendingRaw, &pending); err != nil {
			return placementMembers{}, false, fmt.Errorf("decode pending jobs: %w", err)
		}
	}
	decoded, err := decodePlacementMembers(membersRaw, jobHomesRaw, placementScalars{
		cell: cell, namespace: namespace, cellInstance: cellInstance,
		rehydratedInstance: rehydratedInstance, pendingJobs: pending,
	})
	if err != nil {
		return placementMembers{}, false, err
	}
	return decoded, true, nil
}
