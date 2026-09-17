package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// applyJobSchedules applies one job.schedules envelope in its own
// transaction, per design.md D8 "The table and the write". Unlike every
// run.* kind, it needs no run_intents row: the envelope describes a
// cell, not a run.
//
// It row-locks the cell's job_schedule_cells watermark first, inserting
// it with the sentinel observed_at "-infinity" when absent so a cell's
// first envelope always has a row to lock. It applies the envelope only
// when the envelope's time is newer than the stored watermark; an older
// or equal envelope changes no row and is reported through
// ApplyResult.ScheduleStale. Deleting every row the envelope omits is
// skipped when the envelope reports truncation, reported through
// ApplyResult.Truncated.
func (g *Ingester) applyJobSchedules(ctx context.Context, env IngestEnvelope) (ApplyResult, error) {
	var payload JobSchedulesPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: decode job.schedules payload: %v", ErrMalformedEnvelope, err)
	}

	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin job.schedules transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	result, err := applyJobSchedulesTx(ctx, tx, env, payload)
	if err != nil {
		return ApplyResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit job.schedules transaction: %w", err)
	}
	return result, nil
}

// applyJobSchedulesTx runs the guard-then-write sequence for one
// job.schedules envelope inside tx.
func applyJobSchedulesTx(ctx context.Context, tx pgx.Tx, env IngestEnvelope, payload JobSchedulesPayload) (ApplyResult, error) {
	// The sentinel is older than every envelope, so a cell's first
	// envelope always finds a row to lock and passes the comparison
	// below. ON CONFLICT DO NOTHING makes two concurrent first
	// envelopes for one cell serialize on this insert.
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.job_schedule_cells (cluster, namespace, cell, observed_at)
		VALUES ($1, $2, $3, '-infinity')
		ON CONFLICT (cluster, namespace, cell) DO NOTHING
	`, env.Cluster, env.Namespace, env.Cell); err != nil {
		return ApplyResult{}, fmt.Errorf("insert job schedule cell watermark: %w", err)
	}

	// The comparison runs in SQL rather than scanning observed_at into a
	// Go time.Time: the sentinel is the Postgres value -infinity, which
	// time.Time cannot represent.
	var isNewer bool
	if err := tx.QueryRow(ctx, `
		SELECT $4::timestamptz > observed_at FROM varroa.job_schedule_cells
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
		FOR UPDATE
	`, env.Cluster, env.Namespace, env.Cell, env.OccurredAt).Scan(&isNewer); err != nil {
		return ApplyResult{}, fmt.Errorf("lock job schedule cell watermark: %w", err)
	}

	if !isNewer {
		// A stale or replayed envelope. The watermark rejects it, so a
		// redelivered envelope can never restore a row a later, already
		// applied empty set deleted.
		return ApplyResult{ScheduleStale: true}, nil
	}

	// homeByTenant caches one placement read for each tenant the envelope
	// names, so a cell hosting many tenants runs one read for each.
	homeByTenant := map[string]placementMembers{}

	for _, row := range payload.Rows {
		home, known, err := jobScheduleHomeCell(ctx, tx, env.Cluster, row.Tenant, row.JobPath, homeByTenant)
		if err != nil {
			return ApplyResult{}, err
		}
		if known && home != env.Cell {
			// Only a job's home slot holder owns that job's row. A
			// standby polls SCM and fires cron, so its export would
			// overwrite the row's cell and the backstop would then fire
			// the job on a cell that must not run it. The envelope does
			// not fail: its other rows can be valid.
			continue
		}
		fireTimes, err := parseJobScheduleFireTimes(row.NextFireTimes)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("%w: decode next_fire_times: %v", ErrMalformedEnvelope, err)
		}
		rowJSON, err := json.Marshal(row)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("marshal job schedule row: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO varroa.job_schedules (
				cluster, namespace, cell, tenant, job_path, trigger_type,
				spec, timezone, disabled, next_fire_times, observed_at,
				placement_generation, payload,
				upstream_tenant, upstream_job, upstream_result
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, $16
			)
			ON CONFLICT (cluster, tenant, job_path, trigger_type) DO UPDATE SET
				namespace            = EXCLUDED.namespace,
				cell                 = EXCLUDED.cell,
				spec                 = EXCLUDED.spec,
				timezone             = EXCLUDED.timezone,
				disabled             = EXCLUDED.disabled,
				next_fire_times      = EXCLUDED.next_fire_times,
				observed_at          = EXCLUDED.observed_at,
				placement_generation = EXCLUDED.placement_generation,
				payload              = EXCLUDED.payload,
				upstream_tenant      = EXCLUDED.upstream_tenant,
				upstream_job         = EXCLUDED.upstream_job,
				upstream_result      = EXCLUDED.upstream_result
		`,
			env.Cluster, env.Namespace, env.Cell, row.Tenant, row.JobPath, row.TriggerType,
			row.Spec, row.Timezone, row.Disabled, fireTimes, env.OccurredAt,
			row.PlacementGeneration, string(rowJSON),
			row.UpstreamTenant, row.UpstreamJob, row.UpstreamResult,
		); err != nil {
			return ApplyResult{}, fmt.Errorf("upsert job schedule row: %w", err)
		}
	}

	result := ApplyResult{}
	if payload.Truncated {
		// A truncated set is not evidence that an omitted job lost its
		// trigger, so nothing is deleted.
		result.Truncated = true
	} else if _, err := tx.Exec(ctx, `
		DELETE FROM varroa.job_schedules
		WHERE cluster = $1 AND namespace = $2 AND cell = $3 AND observed_at < $4
	`, env.Cluster, env.Namespace, env.Cell, env.OccurredAt); err != nil {
		return ApplyResult{}, fmt.Errorf("delete stale job schedule rows: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE varroa.job_schedule_cells SET observed_at = $4
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, env.Cluster, env.Namespace, env.Cell, env.OccurredAt); err != nil {
		return ApplyResult{}, fmt.Errorf("raise job schedule cell watermark: %w", err)
	}

	return result, nil
}

// parseJobScheduleFireTimes parses a job schedule row's RFC 3339
// occurrence strings into UTC instants, in the order the plugin exported
// them, for the next_fire_times timestamptz[] column.
func parseJobScheduleFireTimes(values []string) ([]time.Time, error) {
	out := make([]time.Time, 0, len(values))
	for _, v := range values {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, err
		}
		out = append(out, t.UTC())
	}
	return out, nil
}

// jobScheduleHomeCell answers the cell that holds jobPath's home slot
// for tenant, and whether the tenant holds a placement row at all. A
// tenant with no row is unknown, and the caller then stores every row it
// exports, which is the behaviour before slots existed.
//
// The cache holds one decoded placement for each tenant of the envelope,
// so a cell hosting many tenants runs one read for each.
func jobScheduleHomeCell(ctx context.Context, tx pgx.Tx, cluster, tenant, jobPath string,
	cache map[string]placementMembers) (string, bool, error) {
	row, cached := cache[tenant]
	if !cached {
		read, exists, err := readTenantPlacementMembers(ctx, tx, cluster, tenant)
		if err != nil {
			return "", false, err
		}
		if !exists {
			cache[tenant] = placementMembers{}
			return "", false, nil
		}
		cache[tenant] = read
		row = read
	}
	if len(row.members) == 0 {
		return "", false, nil
	}
	return memberForJob(row, jobPath).Cell, true, nil
}
