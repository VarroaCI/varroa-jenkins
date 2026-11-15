package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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

// ScheduledRun names the earliest future cron occurrence of a job in scope.
type ScheduledRun struct {
	Cluster, Tenant, JobPath string
	At                       time.Time
}

// NextCronRun answers the earliest future cron occurrence after now of an
// enabled cron job in the scopes. A job that matches no scope pattern is left
// out. It answers false when no such occurrence is listed. It issues no
// statement for an empty scope list.
func (r *pgReader) NextCronRun(ctx context.Context, scopes []RunScope, now time.Time) (ScheduledRun, bool, error) {
	if len(scopes) == 0 {
		return ScheduledRun{}, false, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return ScheduledRun{}, false, err
	}
	raw, err := scopesJSON(scopes)
	if err != nil {
		return ScheduledRun{}, false, fmt.Errorf("encode cron scopes: %w", err)
	}
	var out ScheduledRun
	err = r.pool.QueryRow(ctx, scopesCTE+`
		SELECT js.cluster, js.tenant, js.job_path, f.at FROM scopes s
		JOIN varroa.job_schedules js ON js.cluster = s.cluster AND js.tenant = s.tenant
			AND (s.patterns IS NULL OR js.job_path ~ ANY(s.patterns))
		CROSS JOIN LATERAL (SELECT min(t) AS at FROM unnest(js.next_fire_times) t WHERE t > $2) f
		WHERE js.trigger_type = 'cron' AND NOT js.disabled AND f.at IS NOT NULL
		ORDER BY f.at, js.cluster, js.tenant, js.job_path
		LIMIT 1`, string(raw), now).Scan(&out.Cluster, &out.Tenant, &out.JobPath, &out.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return ScheduledRun{}, false, nil
	}
	if err != nil {
		return ScheduledRun{}, false, fmt.Errorf("read next cron run: %w", err)
	}
	return out, true, nil
}
