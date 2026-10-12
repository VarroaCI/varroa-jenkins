package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// JobBranchesPayload is the payload of a job.branches envelope. Every
// project carries its own tenant and placement generation, because one
// envelope describes a whole cell that hosts many tenants.
type JobBranchesPayload struct {
	Hash      string                  `json:"hash"`
	Truncated bool                    `json:"truncated"`
	Total     int                     `json:"total"`
	Projects  []JobBranchProjectInput `json:"projects"`
}

// JobBranchProjectInput is one multibranch project of the payload.
type JobBranchProjectInput struct {
	Tenant              string               `json:"tenant"`
	PlacementGeneration int64                `json:"placement_generation"`
	JobPath             string               `json:"job_path"`
	IndexedAt           string               `json:"indexed_at"`
	IndexResult         string               `json:"index_result"`
	Heads               []JobBranchHeadInput `json:"heads"`
	HeadsTruncated      bool                 `json:"heads_truncated"`
	LastScan            *BranchScanInput     `json:"last_scan"`
}

// JobBranchHeadInput is one head of a project in the payload.
type JobBranchHeadInput struct {
	Name          string                    `json:"name"`
	JobPath       string                    `json:"job_path"`
	Kind          string                    `json:"kind"`
	Orphaned      bool                      `json:"orphaned"`
	Revision      string                    `json:"revision"`
	ChangeRequest *ChangeRequestBranchInput `json:"change_request,omitempty"`
}

// ChangeRequestBranchInput is the change request of a head.
type ChangeRequestBranchInput struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	URL          string `json:"url"`
}

// BranchScanInput is the last scan of a project. The scan path fills it;
// the inventory export carries null until then.
type BranchScanInput struct {
	ScanID        string   `json:"scan_id"`
	State         string   `json:"state"`
	Actor         string   `json:"actor"`
	RequestedAt   string   `json:"requested_at"`
	EndedAt       string   `json:"ended_at"`
	Result        string   `json:"result"`
	NewHeads      []string `json:"new_heads"`
	ChangedHeads  []string `json:"changed_heads"`
	OrphanedHeads []string `json:"orphaned_heads"`
	Message       string   `json:"message"`
}

// applyJobBranches applies one job.branches envelope in its own
// transaction. It follows applyJobSchedules: it row-locks the cell's
// watermark, applies only a newer envelope, and deletes what the cell
// stopped exporting unless the envelope is truncated.
func (g *Ingester) applyJobBranches(ctx context.Context, env IngestEnvelope) (ApplyResult, error) {
	var payload JobBranchesPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: decode job.branches payload: %v", ErrMalformedEnvelope, err)
	}

	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin job.branches transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	result, err := applyJobBranchesTx(ctx, tx, env, payload)
	if err != nil {
		return ApplyResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit job.branches transaction: %w", err)
	}
	return result, nil
}

// applyJobBranchesTx runs the guard-then-write sequence for one
// job.branches envelope inside tx. The watermark row is the one
// job_schedule_cells row that job.schedules also locks. The branch
// watermark is its own column, so the two kinds never reject each other.
func applyJobBranchesTx(ctx context.Context, tx pgx.Tx, env IngestEnvelope, payload JobBranchesPayload) (ApplyResult, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.job_schedule_cells (cluster, namespace, cell, observed_at)
		VALUES ($1, $2, $3, '-infinity')
		ON CONFLICT (cluster, namespace, cell) DO NOTHING
	`, env.Cluster, env.Namespace, env.Cell); err != nil {
		return ApplyResult{}, fmt.Errorf("insert job branch cell watermark: %w", err)
	}

	var isNewer bool
	if err := tx.QueryRow(ctx, `
		SELECT $4::timestamptz > branches_observed_at FROM varroa.job_schedule_cells
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
		FOR UPDATE
	`, env.Cluster, env.Namespace, env.Cell, env.OccurredAt).Scan(&isNewer); err != nil {
		return ApplyResult{}, fmt.Errorf("lock job branch cell watermark: %w", err)
	}

	if !isNewer {
		return ApplyResult{BranchesStale: true}, nil
	}

	homeByTenant := map[string]placementMembers{}
	for _, project := range payload.Projects {
		home, known, err := jobScheduleHomeCell(ctx, tx, env.Cluster, project.Tenant, project.JobPath, homeByTenant)
		if err != nil {
			return ApplyResult{}, err
		}
		if known && (home.Namespace != env.Namespace || home.Cell != env.Cell) {
			// Only the job's home slot holder owns its branch rows, as
			// for schedule rows. The envelope does not fail: its other
			// projects can be valid.
			continue
		}
		indexedAt, err := parseOptionalRFC3339(project.IndexedAt)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("%w: decode indexed_at: %v", ErrMalformedEnvelope, err)
		}
		heads := project.Heads
		if heads == nil {
			heads = []JobBranchHeadInput{}
		}
		headsJSON, err := json.Marshal(heads)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("marshal job branch heads: %w", err)
		}
		var lastScanJSON *string
		if project.LastScan != nil {
			b, err := json.Marshal(project.LastScan)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("marshal job branch last scan: %w", err)
			}
			s := string(b)
			lastScanJSON = &s
		}
		// A repeat envelope carries last_scan as null until the scan path
		// fills it, so COALESCE keeps the last scan that was stored.
		if _, err := tx.Exec(ctx, `
			INSERT INTO varroa.job_branch_projects AS jb (
				cluster, namespace, cell, tenant, job_path, placement_generation,
				observed_at, indexed_at, index_result, heads, heads_truncated, last_scan
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12::jsonb
			)
			ON CONFLICT (cluster, tenant, job_path) DO UPDATE SET
				namespace            = EXCLUDED.namespace,
				cell                 = EXCLUDED.cell,
				placement_generation = EXCLUDED.placement_generation,
				observed_at          = EXCLUDED.observed_at,
				indexed_at           = EXCLUDED.indexed_at,
				index_result         = EXCLUDED.index_result,
				heads                = EXCLUDED.heads,
				heads_truncated      = EXCLUDED.heads_truncated,
				last_scan            = COALESCE(EXCLUDED.last_scan, jb.last_scan)
		`,
			env.Cluster, env.Namespace, env.Cell, project.Tenant, project.JobPath,
			project.PlacementGeneration, env.OccurredAt, indexedAt, project.IndexResult,
			string(headsJSON), project.HeadsTruncated, lastScanJSON,
		); err != nil {
			return ApplyResult{}, fmt.Errorf("upsert job branch project: %w", err)
		}
	}

	result := ApplyResult{}
	if !payload.Truncated {
		// A truncated set is not evidence that an omitted project left
		// the cell, so nothing is deleted.
		if _, err := tx.Exec(ctx, `
			DELETE FROM varroa.job_branch_projects
			WHERE cluster = $1 AND namespace = $2 AND cell = $3 AND observed_at < $4
		`, env.Cluster, env.Namespace, env.Cell, env.OccurredAt); err != nil {
			return ApplyResult{}, fmt.Errorf("delete stale job branch projects: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE varroa.job_schedule_cells SET branches_observed_at = $4
		WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, env.Cluster, env.Namespace, env.Cell, env.OccurredAt); err != nil {
		return ApplyResult{}, fmt.Errorf("raise job branch cell watermark: %w", err)
	}

	return result, nil
}

// parseOptionalRFC3339 parses an RFC 3339 instant into UTC. An empty
// value answers nil, which the column stores as NULL.
func parseOptionalRFC3339(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	u := t.UTC()
	return &u, nil
}
