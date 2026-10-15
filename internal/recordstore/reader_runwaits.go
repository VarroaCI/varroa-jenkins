package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// runWaitListLimit bounds the waits one job answers.
const runWaitListLimit = 50

// runWaitLiveSQL selects the wait rows a claim refreshed inside the live
// window. The window is two minutes, wider than the 10-second claim retry,
// so a run that is still queued never drops out of its own read.
const runWaitLiveSQL = `
	SELECT run_id::text, cluster, tenant, job_path, cell, cap_kind, cap_name,
	       cap_max, running, first_seen_at
	FROM varroa.run_waits
	WHERE last_seen_at > now() - interval '2 minutes'`

// GetRunWait answers the live wait of one run. A run with no live row
// answers false.
func (r *pgReader) GetRunWait(ctx context.Context, runID string) (RunWait, bool, error) {
	if err := r.checkSchema(ctx); err != nil {
		return RunWait{}, false, err
	}
	row := r.pool.QueryRow(ctx, runWaitLiveSQL+` AND run_id = $1::uuid`, runID)
	w, err := scanRunWait(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunWait{}, false, nil
	}
	if err != nil {
		return RunWait{}, false, fmt.Errorf("read run wait: %w", err)
	}
	return w, true, nil
}

// ListJobRunWaits answers the live waits of one job, ordered by how long
// each run has waited.
func (r *pgReader) ListJobRunWaits(ctx context.Context, cluster, tenant, jobPath string) ([]RunWait, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, runWaitLiveSQL+`
		AND cluster = $1 AND tenant = $2 AND job_path = $3
		ORDER BY first_seen_at, run_id
		LIMIT $4`, cluster, tenant, jobPath, runWaitListLimit)
	if err != nil {
		return nil, fmt.Errorf("list job run waits: %w", err)
	}
	defer rows.Close()
	var out []RunWait
	for rows.Next() {
		w, err := scanRunWait(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job run wait: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list job run waits: %w", err)
	}
	return out, nil
}

// scanRunWait reads one wait row in the column order of runWaitLiveSQL.
func scanRunWait(row pgx.Row) (RunWait, error) {
	var w RunWait
	var maxRuns, running int32
	if err := row.Scan(&w.RunID, &w.Cluster, &w.Tenant, &w.JobPath, &w.Cell, &w.Kind, &w.Limit,
		&maxRuns, &running, &w.Since); err != nil {
		return RunWait{}, err
	}
	w.Max = maxRuns
	w.Running = running
	return w, nil
}
