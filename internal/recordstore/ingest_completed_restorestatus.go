package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxRestoreRuns bounds the runs of one job and one revision that one
// restore check reads, newest first.
const maxRestoreRuns = 20

// restoreReadTimeout bounds the restore read after a completion commits. The
// read holds up the consumer's next message, and the completion it serves is
// already stored.
const restoreReadTimeout = 5 * time.Second

// restoreQuerier is the read surface of restoreStatusAction. A pool and a
// transaction both satisfy it.
type restoreQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// readRestoreAction reads the restore_commit_status action of an owner
// completion, on the pool, after the completion committed. A failed read
// is logged at Warn and answers nil: the completion stays stored, and only
// the repair of the commit status is lost.
//
// The read sees the record as it stands after the commit, not as the
// completion's own transaction saw it. A newer run that started since then
// makes the newest run live, so no action follows; that run writes its own
// status last. A concurrent completion of the same revision can make both
// reads publish one action, and the second forge write then finds nothing
// to change.
func (g *Ingester) readRestoreAction(ctx context.Context, env IngestEnvelope, intent *intentRow) *LedgerAction {
	read := g.restoreRead
	if read == nil {
		read = restoreStatusAction
	}
	rctx, cancel := context.WithTimeout(ctx, restoreReadTimeout)
	defer cancel()
	action, err := read(rctx, g.pool, intent)
	if err != nil {
		g.logger.Warn("restore_commit_status read failed; the completion is stored",
			"runID", env.RunID, "jobPath", intent.jobPath, "error", err)
		return nil
	}
	return action
}

// restoreRun is one run of a job and a revision that a restore check reads.
type restoreRun struct {
	runID       uuid.UUID
	status      string
	result      string
	buildNumber *int64
	namespace   string
	cell        string
	startedAt   time.Time
	endedAt     *time.Time
	repo        string
}

// ownsStatus reports whether r can own the forge status context of its
// revision. A live run and a run that Jenkins completed can. A DUPLICATE
// run, an INTERRUPTED run, and a TRIGGER_FAILED run cannot, because none of
// them gives the outcome of the revision.
func (r restoreRun) ownsStatus() bool {
	switch r.status {
	case StatusQueued, StatusRunning:
		return true
	case StatusCompleted:
		return r.result != "" && r.result != resultInterrupted && r.result != ResultTriggerFailed
	}
	return false
}

// restoreStatusAction builds the restore_commit_status action after the
// owner completion of a run. The newest run of a job and a revision owns
// each forge status context of that job on the commit. The order is the
// run id order, (created_at, run_id), the same order as the duplicate
// backstop. Each completion writes its forge status from the plugin, so an
// older run that ends last can leave its result on the commit.
//
// It returns nil while the newest owning run is live, because that run
// writes its own status last. It also returns nil when no other run of the
// job and the revision ended at or after the start of the newest run, so a
// later rebuild of an old revision costs no forge read. The action names
// the newest run and the build numbers of those other runs.
func restoreStatusAction(ctx context.Context, q restoreQuerier, intent *intentRow) (*LedgerAction, error) {
	var revision string
	err := q.QueryRow(ctx, `
		SELECT COALESCE(scm_revision, '') FROM varroa.runs WHERE run_id = $1 AND created_at = $2`,
		intent.runID, intent.createdAt).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read restore-status revision: %w", err)
	}
	if revision == "" {
		return nil, nil
	}
	runs, err := readRestoreRuns(ctx, q, intent, revision)
	if err != nil {
		return nil, err
	}
	var newest *restoreRun
	for i := range runs {
		if runs[i].ownsStatus() {
			newest = &runs[i]
			break
		}
	}
	if newest == nil || newest.status != StatusCompleted || newest.buildNumber == nil || newest.repo == "" {
		return nil, nil
	}
	var superseded []int64
	for _, r := range runs {
		if r.runID == newest.runID || r.buildNumber == nil {
			continue
		}
		if r.endedAt != nil && r.endedAt.Before(newest.startedAt) {
			continue
		}
		superseded = append(superseded, *r.buildNumber)
	}
	if len(superseded) == 0 {
		return nil, nil
	}
	return &LedgerAction{
		Kind:             LedgerActionRestoreCommitStatus,
		Cluster:          intent.cluster,
		Namespace:        newest.namespace,
		Controller:       newest.cell,
		Actor:            LedgerActionRecorder,
		RunID:            newest.runID.String(),
		JobPath:          intent.jobPath,
		BuildNumber:      newest.buildNumber,
		Tenant:           intent.tenant,
		Repo:             newest.repo,
		Revision:         revision,
		Result:           newest.result,
		SupersededBuilds: superseded,
	}, nil
}

// readRestoreRuns reads the newest maxRestoreRuns runs of the intent's job
// with the given revision, newest first.
func readRestoreRuns(ctx context.Context, q restoreQuerier, intent *intentRow, revision string) ([]restoreRun, error) {
	rows, err := q.Query(ctx, `
		SELECT run_id, status, COALESCE(result, ''), build_number, namespace, cell,
		       COALESCE(started_at, created_at), ended_at,
		       COALESCE(NULLIF(payload->'run.source'->>'scm_url', ''), payload->'run.started'->'payload'->>'scm_url', '')
		FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3 AND scm_revision = $4
		ORDER BY created_at DESC, run_id DESC
		LIMIT $5`,
		intent.cluster, intent.tenant, intent.jobPath, revision, maxRestoreRuns)
	if err != nil {
		return nil, fmt.Errorf("read restore-status runs: %w", err)
	}
	defer rows.Close()
	var runs []restoreRun
	for rows.Next() {
		var r restoreRun
		if err := rows.Scan(&r.runID, &r.status, &r.result, &r.buildNumber, &r.namespace, &r.cell,
			&r.startedAt, &r.endedAt, &r.repo); err != nil {
			return nil, fmt.Errorf("scan restore-status run: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read restore-status runs: %w", err)
	}
	return runs, nil
}
