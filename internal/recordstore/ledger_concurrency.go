package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// concurrencyCancelInProgress is the Team.spec.pipelines.concurrency
// value that makes a granted claim cancel its group's older runs.
const concurrencyCancelInProgress = "cancelInProgress"

// concurrencyCancelLimit is the concurrency-groups capability's eight-run
// bound (design D10).
const concurrencyCancelLimit = 8

// concurrencyCancelTimeout bounds the concurrency cancel's own database
// and publish work, run in a goroutine with its own background context
// so a granted claim is answered first.
const concurrencyCancelTimeout = 10 * time.Second

// policyConcurrency decodes the one field of a tenant_placements policy
// column the concurrency cancel needs. An empty or malformed policy
// answers the empty string, which never equals
// concurrencyCancelInProgress, so an unannounced or legacy tenant runs
// concurrently by default.
type policyConcurrency struct {
	Concurrency string `json:"concurrency"`
}

func decodeConcurrency(policy json.RawMessage) string {
	if len(policy) == 0 {
		return ""
	}
	var p policyConcurrency
	if err := json.Unmarshal(policy, &p); err != nil {
		return ""
	}
	return p.Concurrency
}

// concurrencyCancelCandidate is one older run's data a cancel_run action
// needs.
type concurrencyCancelCandidate struct {
	runID       uuid.UUID
	namespace   string
	cell        string
	jobPath     string
	buildNumber *int64
	createdAt   time.Time
}

// selectConcurrencyCancelCandidates reads every non-COMPLETED run of
// cluster/tenant/jobPath older than olderThan, oldest first: design
// D10's concurrency group, `<cluster>/<tenant>/<job_path>`.
func selectConcurrencyCancelCandidates(ctx context.Context, pool *pgxpool.Pool, cluster, tenant, jobPath string, olderThan time.Time) ([]concurrencyCancelCandidate, error) {
	rows, err := pool.Query(ctx, `
		SELECT run_id, namespace, cell, job_path, build_number, created_at
		FROM varroa.runs
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
		  AND status <> 'COMPLETED' AND created_at < $4
		ORDER BY created_at ASC
	`, cluster, tenant, jobPath, olderThan)
	if err != nil {
		return nil, fmt.Errorf("select concurrency cancel candidates: %w", err)
	}
	defer rows.Close()

	var out []concurrencyCancelCandidate
	for rows.Next() {
		var c concurrencyCancelCandidate
		if err := rows.Scan(&c.runID, &c.namespace, &c.cell, &c.jobPath, &c.buildNumber, &c.createdAt); err != nil {
			return nil, fmt.Errorf("scan concurrency cancel candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select concurrency cancel candidates: %w", err)
	}
	return out, nil
}

// runConcurrencyCancel is Ledger's fire-and-forget side effect for a
// granted claim (design D10): it reads the claim's tenant policy, and on
// cancelInProgress publishes a cancel_run action for at most eight
// non-COMPLETED older runs of the claim's group, oldest first, counting
// every run above the limit. It never changes the older run's record;
// the cell reports the abort through the ordinary build-event path.
func runConcurrencyCancel(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, metrics *ConcurrencyMetrics,
	actionMetrics *ActionMetrics, req ClaimRequest, createdAt time.Time, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithTimeout(ctx, concurrencyCancelTimeout)
	defer cancel()

	row, exists, err := readTenantPlacementLivenessRow(ctx, pool, req.Cluster, req.Tenant)
	if err != nil {
		logger.Warn("concurrency cancel: read tenant policy failed", "cluster", req.Cluster, "tenant", req.Tenant, "error", err)
		return
	}
	concurrency := ""
	if exists {
		concurrency = decodeConcurrency(row.policy)
	}
	if concurrency != concurrencyCancelInProgress {
		return
	}

	candidates, err := selectConcurrencyCancelCandidates(ctx, pool, req.Cluster, req.Tenant, req.JobPath, createdAt)
	if err != nil {
		logger.Warn("concurrency cancel: select candidates failed", "cluster", req.Cluster, "tenant", req.Tenant,
			"job_path", req.JobPath, "error", err)
		return
	}

	for i, c := range candidates {
		if i >= concurrencyCancelLimit {
			metrics.addSkipped(ctx, int64(len(candidates)-concurrencyCancelLimit))
			break
		}
		action := LedgerAction{
			Kind:        LedgerActionCancelRun,
			Cluster:     req.Cluster,
			Namespace:   c.namespace,
			Controller:  c.cell,
			Actor:       LedgerActionRecorder,
			RunID:       c.runID.String(),
			JobPath:     c.jobPath,
			BuildNumber: c.buildNumber,
		}
		if err := PublishLedgerAction(conn, action); err != nil {
			logger.Warn("concurrency cancel: publish cancel_run failed", "run_id", c.runID, "error", err)
			continue
		}
		metrics.addCancel(ctx)
		actionMetrics.addPublished(ctx, action.Kind)
	}
}
