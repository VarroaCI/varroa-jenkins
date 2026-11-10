package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// sourceGoneEndStatus builds the end_commit_status action for a completed run
// whose branch job lost its branch source. The branch-source plugins find no
// source for a dead branch job, so they write no terminal status. The action
// needs the commit, the repository, and the build number, which the run
// stored. It returns nil when the run stored none of them. The SQL repeats
// candidatesQuery on purpose, because this path reads a completed run.
func sourceGoneEndStatus(ctx context.Context, tx pgx.Tx, intent *intentRow, result string) (*LedgerAction, error) {
	var revision, repo string
	var buildNumber *int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(r.scm_revision, ''),
		       COALESCE(NULLIF(r.payload->'run.source'->>'scm_url', ''), r.payload->'run.started'->'payload'->>'scm_url', ''),
		       r.build_number
		FROM varroa.runs r WHERE r.run_id = $1 AND r.created_at = $2`,
		intent.runID, intent.createdAt).Scan(&revision, &repo, &buildNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read end-status facts: %w", err)
	}
	if revision == "" || repo == "" || buildNumber == nil {
		return nil, nil
	}
	return &LedgerAction{
		Kind:        LedgerActionEndCommitStatus,
		Cluster:     intent.cluster,
		Namespace:   intent.namespace,
		Controller:  intent.cellValue(),
		Actor:       LedgerActionRecorder,
		RunID:       intent.runID.String(),
		JobPath:     intent.jobPath,
		BuildNumber: buildNumber,
		Tenant:      intent.tenant,
		Repo:        repo,
		Revision:    revision,
		Result:      result,
	}, nil
}
