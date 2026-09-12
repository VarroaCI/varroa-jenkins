package recordstore

import (
	"context"
	"fmt"
)

// ListJobSchedules lists cluster/tenant's job_schedules rows, ordered by
// job path then trigger type, matching the (tenant, trigger_type) index
// D9 defines. These queries return record-derived data only: no job
// definition and no trigger state the record tables do not carry.
func (r *pgReader) ListJobSchedules(ctx context.Context, cluster, tenant string) ([]JobSchedule, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, `
		SELECT job_path, trigger_type, spec, timezone, disabled, next_fire_times, observed_at,
		       namespace, cell, placement_generation,
		       upstream_tenant, upstream_job, upstream_result
		FROM varroa.job_schedules
		WHERE cluster = $1 AND tenant = $2
		ORDER BY job_path, trigger_type
	`, cluster, tenant)
	if err != nil {
		return nil, fmt.Errorf("list job schedules: %w", err)
	}
	defer rows.Close()

	var out []JobSchedule
	for rows.Next() {
		var js JobSchedule
		if err := rows.Scan(&js.JobPath, &js.TriggerType, &js.Spec, &js.Timezone,
			&js.Disabled, &js.NextFireTimes, &js.ObservedAt,
			&js.Namespace, &js.Cell, &js.PlacementGeneration,
			&js.UpstreamTenant, &js.UpstreamJob, &js.UpstreamResult); err != nil {
			return nil, fmt.Errorf("scan job schedule: %w", err)
		}
		out = append(out, js)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list job schedules: %w", err)
	}
	return out, nil
}
