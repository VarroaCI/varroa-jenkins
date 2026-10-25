package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// JobBranchProject is one multibranch job of a cell with its discovered
// heads, as the recorder stored the last job.branches envelope.
type JobBranchProject struct {
	JobPath        string
	ObservedAt     time.Time
	IndexedAt      *time.Time
	IndexResult    string
	Heads          []JobBranchHead
	HeadsTruncated bool
	LastScan       *BranchScan
}

// JobBranchHead is one head of a multibranch job: a branch, a change
// request, or a tag.
type JobBranchHead struct {
	Name          string               `json:"name"`
	JobPath       string               `json:"job_path"`
	Kind          string               `json:"kind"`
	Orphaned      bool                 `json:"orphaned"`
	Revision      string               `json:"revision"`
	ChangeRequest *ChangeRequestBranch `json:"change_request,omitempty"`
}

// ChangeRequestBranch is the change request of a head.
type ChangeRequestBranch struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	URL          string `json:"url"`
}

// BranchScan is the last scan of a multibranch job.
type BranchScan struct {
	ScanID           string           `json:"scan_id"`
	State            string           `json:"state"`
	Actor            string           `json:"actor"`
	RequestedAt      string           `json:"requested_at"`
	EndedAt          string           `json:"ended_at"`
	Result           string           `json:"result"`
	NewHeadRefs      []BranchScanHead `json:"new_head_refs"`
	ChangedHeadRefs  []BranchScanHead `json:"changed_head_refs"`
	OrphanedHeadRefs []BranchScanHead `json:"orphaned_head_refs"`
	Message          string           `json:"message"`
}

// BranchScanHead names one head of a scan and the job that holds it. The
// job path is the key the read gate checks.
type BranchScanHead struct {
	Name    string `json:"name"`
	JobPath string `json:"job_path"`
}

// GetJobBranches reads the stored branch inventory of one multibranch job
// of tenant on cluster. The bool is false when no row exists.
func (r *pgReader) GetJobBranches(ctx context.Context, cluster, tenant, jobPath string) (JobBranchProject, bool, error) {
	if err := r.checkSchema(ctx); err != nil {
		return JobBranchProject{}, false, err
	}

	var (
		project    JobBranchProject
		indexedAt  *time.Time
		headsRaw   []byte
		lastScanRa []byte
	)
	err := r.pool.QueryRow(ctx, `
		SELECT job_path, observed_at, indexed_at, index_result, heads, heads_truncated, last_scan
		FROM varroa.job_branch_projects
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, cluster, tenant, jobPath).Scan(
		&project.JobPath, &project.ObservedAt, &indexedAt, &project.IndexResult,
		&headsRaw, &project.HeadsTruncated, &lastScanRa,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobBranchProject{}, false, nil
	}
	if err != nil {
		return JobBranchProject{}, false, fmt.Errorf("get job branches: %w", err)
	}
	project.IndexedAt = indexedAt

	if err := json.Unmarshal(headsRaw, &project.Heads); err != nil {
		return JobBranchProject{}, false, fmt.Errorf("decode job branch heads: %w", err)
	}
	if lastScanRa != nil {
		var scan BranchScan
		if err := json.Unmarshal(lastScanRa, &scan); err != nil {
			return JobBranchProject{}, false, fmt.Errorf("decode job branch last scan: %w", err)
		}
		project.LastScan = &scan
	}
	return project, true, nil
}

// ChangeRequestJobs answers the job paths of tenant on cluster whose live
// change-request head has the pull-request URL changeURL. The match ignores
// case. An orphaned head never matches. The answer is sorted and holds at
// most 16 paths.
func (r *pgReader) ChangeRequestJobs(ctx context.Context, cluster, tenant, changeURL string) ([]string, error) {
	if changeURL == "" {
		return nil, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT h->>'job_path' FROM varroa.job_branch_projects p,
		  jsonb_array_elements(p.heads) h
		WHERE p.cluster = $1 AND p.tenant = $2 AND h->>'kind' = 'changeRequest'
		  AND NOT COALESCE((h->>'orphaned')::boolean, false)
		  AND lower(h->'change_request'->>'url') = lower($3)
		ORDER BY 1 LIMIT 16
	`, cluster, tenant, changeURL)
	if err != nil {
		return nil, fmt.Errorf("change request jobs: %w", err)
	}
	return scanJobPaths(rows)
}

// RevisionJobs answers the job paths of tenant on cluster whose live branch
// head holds revision. The revision must be a 40-character hex commit SHA.
// The match is exact, and an orphaned head never matches. The answer is
// sorted and holds at most 16 paths.
func (r *pgReader) RevisionJobs(ctx context.Context, cluster, tenant, revision string) ([]string, error) {
	if !isFullSHA(revision) {
		return nil, fmt.Errorf("revision %q is not a full commit SHA", revision)
	}
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT h->>'job_path' FROM varroa.job_branch_projects p,
		  jsonb_array_elements(p.heads) h
		WHERE p.cluster = $1 AND p.tenant = $2 AND h->>'kind' = 'branch'
		  AND NOT COALESCE((h->>'orphaned')::boolean, false)
		  AND h->>'revision' = $3
		ORDER BY 1 LIMIT 16
	`, cluster, tenant, revision)
	if err != nil {
		return nil, fmt.Errorf("revision jobs: %w", err)
	}
	return scanJobPaths(rows)
}

// isFullSHA reports whether s is a 40-character lowercase or uppercase hex string.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// scanJobPaths reads one column of job paths and closes rows. A NULL path
// is skipped.
func scanJobPaths(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path *string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan job path: %w", err)
		}
		if path != nil && *path != "" {
			out = append(out, *path)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job paths: %w", err)
	}
	return out, nil
}

// LatestRunsForJobs answers the newest run of each listed job path of
// tenant on cluster, keyed by job path. A job path with no run has no
// entry. One query serves the whole list, so a job page with many
// branches runs one read for its last runs.
func (r *pgReader) LatestRunsForJobs(ctx context.Context, cluster, tenant string, jobPaths []string) (map[string]Run, error) {
	out := make(map[string]Run, len(jobPaths))
	if len(jobPaths) == 0 {
		return out, nil
	}
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT ON (job_path) `+runColumns+`
		FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND job_path = ANY($3)
		ORDER BY job_path, created_at DESC, run_id DESC
	`, cluster, tenant, jobPaths)
	if err != nil {
		return nil, fmt.Errorf("latest runs for jobs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan latest run: %w", err)
		}
		out[run.JobPath] = run
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest runs: %w", err)
	}
	return out, nil
}
