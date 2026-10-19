package recordstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// instanceAgentReleaseWindow is how long after the grace the backstop round
// publishes release_instance_agents for one confirmed instance. The round
// runs once a minute, so the action repeats about five times. The repeats
// replace a lost publish, and they let the operator's three-minute
// refresh floor pass for a pod that the dead JVM refreshed just before it died.
const instanceAgentReleaseWindow = 5 * time.Minute

// releasableDeadInstances answers the entries of dead that the instance
// release may act on at now. An entry qualifies when it names an instance,
// its pod-read confirmation is older than grace, and the confirmation is
// no older than grace plus instanceAgentReleaseWindow. The input order is
// kept.
func releasableDeadInstances(dead []TenantSyncDeadInstance, now time.Time, grace time.Duration) []TenantSyncDeadInstance {
	var out []TenantSyncDeadInstance
	for _, d := range dead {
		if d.Instance == "" || !confirmedGone(d, now, grace) {
			continue
		}
		if !d.ConfirmedGoneAt.After(now.Add(-grace - instanceAgentReleaseWindow)) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// ReleaseGoneInstanceAgents publishes one release_instance_agents action
// for each releasable dead instance of each tenant placement, and answers
// the number of actions it published. Only a pod-read confirmation counts
// here. The moved and silent arms release nothing, and the ordinary reaper
// covers them. The keep set comes from heldAgentNodes. This function never
// publishes when that read fails, because an empty keep set would delete
// pods that a resume can adopt.
func ReleaseGoneInstanceAgents(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn,
	actionMetrics *ActionMetrics, now time.Time, grace time.Duration, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	type placement struct {
		cluster, tenant, namespace, cell, agentNamespace string
		dead                                             []byte
	}
	rows, err := pool.Query(ctx, `
		SELECT cluster, tenant, namespace, cell, agent_namespace, dead_instances
		  FROM varroa.tenant_placements
		 WHERE agent_namespace <> ''
		   AND CASE WHEN jsonb_typeof(dead_instances) = 'array'
		            THEN jsonb_array_length(dead_instances) > 0
		            ELSE false END
	`)
	if err != nil {
		return 0, err
	}
	var placements []placement
	for rows.Next() {
		var p placement
		if err := rows.Scan(&p.cluster, &p.tenant, &p.namespace, &p.cell, &p.agentNamespace, &p.dead); err != nil {
			rows.Close()
			return 0, err
		}
		placements = append(placements, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	published := 0
	for _, p := range placements {
		var dead []TenantSyncDeadInstance
		if err := json.Unmarshal(p.dead, &dead); err != nil {
			logger.Warn("decode dead instances failed", "tenant", p.tenant, "error", err)
			continue
		}
		entries := releasableDeadInstances(dead, now, grace)
		if len(entries) == 0 {
			continue
		}
		held, err := heldAgentNodes(ctx, pool, p.cluster, p.tenant, uuid.Nil)
		if err != nil {
			logger.Warn("read held agent nodes failed", "tenant", p.tenant, "error", err)
			continue
		}
		keep := make([]string, 0, len(held))
		for name := range held {
			keep = append(keep, name)
		}
		sort.Strings(keep)
		for _, d := range entries {
			ns, cell := d.Namespace, d.Cell
			if ns == "" {
				ns = p.namespace
			}
			if cell == "" {
				cell = p.cell
			}
			confirmed := *d.ConfirmedGoneAt
			action := LedgerAction{
				Kind:            LedgerActionReleaseInstanceAgents,
				Cluster:         p.cluster,
				Namespace:       ns,
				Controller:      cell,
				Actor:           LedgerActionRecorder,
				Instance:        d.Instance,
				Tenant:          p.tenant,
				AgentNamespace:  p.agentNamespace,
				KeepPods:        keep,
				RefreshedBefore: &confirmed,
			}
			if err := PublishLedgerAction(conn, action); err != nil {
				logger.Warn("publish release_instance_agents action failed", "tenant", p.tenant,
					"instance", d.Instance, "error", err)
				continue
			}
			actionMetrics.addPublished(ctx, action.Kind)
			published++
		}
	}
	return published, nil
}
