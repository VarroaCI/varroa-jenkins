package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// JobInventory is a tenant's live job inventory, as the tenant's active
// member cells last reported it. A Known flag is false when the record
// store cannot prove the set is complete. A reader must then treat a
// missing path as unknown, never as absent.
type JobInventory struct {
	// JobsKnown is true when every active member cell reported a complete
	// job.schedules set.
	JobsKnown bool
	// Jobs holds the distinct job paths of the tenant's job_schedules rows.
	Jobs map[string]struct{}
	// ProjectsKnown is true when every active member cell reported a
	// complete job.branches set.
	ProjectsKnown bool
	// Projects holds the job paths of the tenant's job_branch_projects rows.
	Projects map[string]struct{}
}

// JobInventory reads the job inventory of cluster/tenant. A tenant with no
// placement row answers the zero value, which holds no known set. A
// standby member never contributes. Each active member must report a
// complete set of a kind for that kind to be known.
func (r *pgReader) JobInventory(ctx context.Context, cluster, tenant string) (JobInventory, error) {
	if err := r.checkSchema(ctx); err != nil {
		return JobInventory{}, err
	}

	row, exists, err := readTenantPlacementMembers(ctx, r.pool, cluster, tenant)
	if err != nil {
		return JobInventory{}, err
	}
	if !exists {
		return JobInventory{}, nil
	}

	inv := JobInventory{JobsKnown: true, ProjectsKnown: true}
	active := 0
	for _, m := range row.members {
		if m.Role != MemberRoleActive || m.Slot == nil {
			continue
		}
		active++
		if m.Cell == "" {
			inv.JobsKnown = false
			inv.ProjectsKnown = false
			continue
		}
		var hasSchedules, hasBranches, schedulesTruncated, branchesTruncated bool
		err := r.pool.QueryRow(ctx, `
			SELECT observed_at > '-infinity'::timestamptz,
			       branches_observed_at > '-infinity'::timestamptz,
			       schedules_truncated, branches_truncated
			FROM varroa.job_schedule_cells
			WHERE cluster = $1 AND namespace = $2 AND cell = $3
		`, cluster, m.Namespace, m.Cell).Scan(&hasSchedules, &hasBranches, &schedulesTruncated, &branchesTruncated)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			inv.JobsKnown = false
			inv.ProjectsKnown = false
			continue
		case err != nil:
			return JobInventory{}, fmt.Errorf("read job inventory cell watermark: %w", err)
		}
		if !hasSchedules || schedulesTruncated {
			inv.JobsKnown = false
		}
		if !hasBranches || branchesTruncated {
			inv.ProjectsKnown = false
		}
	}
	if active == 0 {
		return JobInventory{}, nil
	}

	inv.Jobs, err = r.readInventoryPaths(ctx,
		`SELECT DISTINCT job_path FROM varroa.job_schedules WHERE cluster = $1 AND tenant = $2`,
		cluster, tenant)
	if err != nil {
		return JobInventory{}, fmt.Errorf("list job schedule paths: %w", err)
	}
	inv.Projects, err = r.readInventoryPaths(ctx,
		`SELECT DISTINCT job_path FROM varroa.job_branch_projects WHERE cluster = $1 AND tenant = $2`,
		cluster, tenant)
	if err != nil {
		return JobInventory{}, fmt.Errorf("list job branch project paths: %w", err)
	}
	return inv, nil
}

// readInventoryPaths runs one path query and answers its rows as a set.
func (r *pgReader) readInventoryPaths(ctx context.Context, sql string, args ...any) (map[string]struct{}, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out[path] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
