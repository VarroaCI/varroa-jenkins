package recordstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// nodeRelPathPrefix and nodeRelPathSuffix bracket the run-state file that
// names one Jenkins agent node: nodes/<name>/config.xml.
const (
	nodeRelPathPrefix = "nodes/"
	nodeRelPathSuffix = "/config.xml"
)

// agentNodeNames answers the sorted, unique agent node names a run's
// state files name. A path names a node only when it is exactly
// nodes/<name>/config.xml with a name that holds no slash. A nested path
// under a job directory names no node.
func agentNodeNames(files []runStateFile) []string {
	seen := map[string]struct{}{}
	for _, f := range files {
		if !strings.HasPrefix(f.RelPath, nodeRelPathPrefix) || !strings.HasSuffix(f.RelPath, nodeRelPathSuffix) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(f.RelPath, nodeRelPathPrefix), nodeRelPathSuffix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// releaseRunAgentsAfterMark runs releaseRunAgents for a mark that ends a
// RUNNING run. A provisional mark never releases: its owner can still
// hold the build and resume through the same agent pods.
func releaseRunAgentsAfterMark(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn,
	actionMetrics *ActionMetrics, run runInterruptCandidate, reason string, logger *slog.Logger) {
	if run.status != StatusRunning || provisionalMark(reason) {
		return
	}
	releaseRunAgents(ctx, pool, conn, actionMetrics, run, logger)
}

// releaseRunAgents publishes one release_agents action for a run the
// recorder just ended. The action names the run's agent nodes and the
// tenant's agent namespace, so the operator can delete the pods the run
// held. The run's terminal state is the guard: a resume that could still
// adopt these pods has been cancelled, or never existed. The publish is
// best effort. A lost action leaves the pods to the operator's ordinary
// reaper, and the recorder never republishes.
func releaseRunAgents(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn,
	actionMetrics *ActionMetrics, run runInterruptCandidate, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	var (
		raw            []byte
		agentNamespace string
	)
	err := pool.QueryRow(ctx, `
		SELECT r.payload->'mesh'->'files', COALESCE(p.agent_namespace, '')
		  FROM varroa.runs r
		  LEFT JOIN varroa.tenant_placements p ON p.cluster = r.cluster AND p.tenant = r.tenant
		 WHERE r.run_id = $1 AND r.created_at = $2
	`, run.runID, run.createdAt).Scan(&raw, &agentNamespace)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("read run agent nodes failed", "run_id", run.runID, "error", err)
		}
		return
	}
	pods := agentNodeNames(runStateFilesOrEmpty(raw))
	if len(pods) == 0 || agentNamespace == "" {
		return
	}
	pods, err = dropNodesHeldByOtherRuns(ctx, pool, run, pods)
	if err != nil {
		logger.Warn("read other runs' agent nodes failed", "run_id", run.runID, "error", err)
		return
	}
	if len(pods) == 0 {
		return
	}
	action := LedgerAction{
		Kind:           LedgerActionReleaseAgents,
		Cluster:        run.cluster,
		Namespace:      run.namespace,
		Controller:     run.cell,
		Actor:          LedgerActionRecorder,
		RunID:          run.runID.String(),
		Tenant:         run.tenant,
		AgentNamespace: agentNamespace,
		AgentPods:      pods,
	}
	if err := PublishLedgerAction(conn, action); err != nil {
		logger.Warn("publish release_agents action failed", "run_id", run.runID, "error", err)
		return
	}
	actionMetrics.addPublished(ctx, action.Kind)
}

// dropNodesHeldByOtherRuns removes the node names that another run of the
// same tenant also lists. Such a run is RUNNING, or carries a heartbeat-lost
// mark, whose owner can still hold the build. The filter has no time bound:
// keeping a name costs one pod until the plugin collects it, and releasing
// a name still in use would kill a live build. An agent pod can outlive the run that
// named it, and a later run of the tenant can reuse it while the ended run
// still lists its name. The pods of such a name belong to the later run.
func dropNodesHeldByOtherRuns(ctx context.Context, pool *pgxpool.Pool, run runInterruptCandidate,
	names []string) ([]string, error) {
	held, err := heldAgentNodes(ctx, pool, run.cluster, run.tenant, run.runID)
	if err != nil {
		return nil, err
	}
	out := names[:0:0]
	for _, name := range names {
		if _, ok := held[name]; !ok {
			out = append(out, name)
		}
	}
	return out, nil
}

// heldAgentNodes answers the agent node names that a RUNNING run or a
// heartbeat-lost run of tenant stores, except run except. A resume can
// still adopt each name. The answer has no time bound: a name that a
// pod still holds stays in the set until its run ends.
func heldAgentNodes(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string,
	except uuid.UUID) (map[string]struct{}, error) {
	rows, err := pool.Query(ctx, `
		SELECT payload->'mesh'->'files' FROM varroa.runs
		 WHERE cluster = $1 AND tenant = $2 AND run_id <> $3
		   AND (status = 'RUNNING' OR payload->>'interrupted_reason' = '`+ReasonHeartbeatLost+`')
	`, cluster, tenant, except)
	if err != nil {
		return nil, fmt.Errorf("select other running runs: %w", err)
	}
	defer rows.Close()
	held := map[string]struct{}{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan other running run: %w", err)
		}
		// A malformed list fails the read. An empty answer would let a
		// caller release a node that a resume can still adopt.
		var files []runStateFile
		if len(raw) != 0 && string(raw) != "null" {
			decoded, err := decodeRunStateFiles(raw)
			if err != nil {
				return nil, fmt.Errorf("decode held agent nodes: %w", err)
			}
			files = decoded
		}
		for _, name := range agentNodeNames(files) {
			held[name] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select other running runs: %w", err)
	}
	return held, nil
}
