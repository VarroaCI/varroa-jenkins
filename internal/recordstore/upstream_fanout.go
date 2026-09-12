// Package recordstore: the upstream fan-out, design D9. RunUpstreamFanout
// runs the six ordered steps after the recorder commits an ingested
// run.completed for the run that owns a producer job. It never runs
// inside that transaction: it reads the grant evaluator and publishes on
// the bus, and a lost publish must never roll a stored run back.
package recordstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// upstreamFanoutCap is the 32-row bound design.md D9 fixes on one round.
// A subscriber beyond it counts in recorder_upstream_fanout_skipped_total
// with reason over_cap.
const upstreamFanoutCap = 32

// upstreamFanoutActorPrefix names the action's actor for every trigger_run
// the fan-out publishes: varroa-upstream/<producer-tenant>. It never
// matches svc-<name>, so the subscriber's own claim reaches grant order 6
// and needs no second grant lookup.
const upstreamFanoutActorPrefix = "varroa-upstream/"

// upstreamFanoutSeqHexLen is the twelve-hex-character length design.md D6
// fixes for the fan-out's "t<digest>" seq segment.
const upstreamFanoutSeqHexLen = 12

// The five recorder_upstream_fanout_skipped_total reason values design.md
// D15 fixes. A subscriber whose placement resolves to another cluster
// than the producer's completion counts as unplaced: from this cluster's
// fan-out it holds no placement here.
const (
	FanoutSkipOverCap       = "over_cap"
	FanoutSkipDenied        = "denied"
	FanoutSkipUnplaced      = "unplaced"
	FanoutSkipDisabled      = "disabled"
	FanoutSkipPublishFailed = "publish_failed"
)

// The Jenkins Result ordinals design.md D9's threshold test compares. A
// lower ordinal is a better result; a threshold names the worst
// acceptable outcome, and an actual result qualifies when its own
// ordinal is no worse. INTERRUPTED carries no ordinal: it is Varroa's
// own synthetic marker, never a Jenkins Result, and it meets no
// threshold.
var jenkinsResultOrdinal = map[string]int{
	"SUCCESS":   0,
	"UNSTABLE":  1,
	"FAILURE":   2,
	"NOT_BUILT": 3,
	"ABORTED":   4,
}

// resultMeetsThreshold reports whether actual is threshold "or better",
// ReverseBuildTrigger's own rule: actual's ordinal must be no worse than
// threshold's. A result outside the allocated set never meets any
// threshold.
func resultMeetsThreshold(actual, threshold string) bool {
	actualOrdinal, ok := jenkinsResultOrdinal[actual]
	if !ok {
		return false
	}
	thresholdOrdinal, ok := jenkinsResultOrdinal[threshold]
	if !ok {
		return false
	}
	return actualOrdinal <= thresholdOrdinal
}

// upstreamFanoutSeq derives the fan-out's stable intent-key seq segment,
// design.md D6: "t" plus the first twelve hexadecimal characters of the
// SHA-256 of the subscriber's own <cluster>/<tenant>/<job_path>. The
// digest depends on identity alone, never on a position in an ordered
// list, so a redelivery whose subscriber set changed still derives the
// same seq for every subscriber still present.
func upstreamFanoutSeq(cluster, tenant, jobPath string) string {
	sum := sha256.Sum256([]byte(cluster + "/" + tenant + "/" + jobPath))
	return "t" + hex.EncodeToString(sum[:])[:upstreamFanoutSeqHexLen]
}

// FanoutProducer is the just-completed run the fan-out round reads:
// intent identity, never the envelope's own fields, matching the
// ingest write path's rule that the ledger's stored identity is
// authoritative.
type FanoutProducer struct {
	Cluster string
	Tenant  string
	JobPath string
	RunID   string
	Result  string
}

// FanoutDeps carries every dependency RunUpstreamFanout needs.
type FanoutDeps struct {
	Pool      *pgxpool.Pool
	Evaluator *GrantEvaluator
	Conn      *bus.Conn
	Metrics   *UpstreamFanoutMetrics
}

// upstreamFanoutCandidate is one job_schedules row the fan-out's step 1
// selects: a subscriber's own declared varroa-upstream trigger.
type upstreamFanoutCandidate struct {
	cluster, namespace, cell, tenant, jobPath string
	disabled                                  bool
	upstreamResult                            string
}

// upstreamFanoutCandidatesQuery reads every job_schedules row that
// subscribes to producer.Tenant/relativeJob, design.md D9 step 1's own
// selection before the threshold, the disabled skip, the order, and the
// cap.
const upstreamFanoutCandidatesQuery = `
	SELECT cluster, namespace, cell, tenant, job_path, disabled, upstream_result
	FROM varroa.job_schedules
	WHERE trigger_type = $1 AND upstream_tenant = $2 AND upstream_job = $3
`

// RunUpstreamFanout runs one fan-out round for producer, design.md D9's
// six ordered steps. The caller runs it after the ingest transaction
// that recorded producer's completion has committed, never inside it.
func RunUpstreamFanout(ctx context.Context, deps FanoutDeps, producer FanoutProducer, now time.Time, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	relativeJob, ok := stripTenantPrefix(producer.Tenant, producer.JobPath)
	if !ok {
		relativeJob = producer.JobPath
	}

	rows, err := deps.Pool.Query(ctx, upstreamFanoutCandidatesQuery, varroaUpstreamTriggerType, producer.Tenant, relativeJob)
	if err != nil {
		return fmt.Errorf("select upstream fan-out candidates for %s/%s: %w", producer.Tenant, relativeJob, err)
	}
	candidates, err := scanUpstreamFanoutCandidates(rows)
	if err != nil {
		return fmt.Errorf("scan upstream fan-out candidates for %s/%s: %w", producer.Tenant, relativeJob, err)
	}
	if len(candidates) == 0 {
		return nil
	}

	placements, err := selectTenantPlacementClusters(ctx, deps.Pool, producer.Tenant)
	if err != nil {
		return fmt.Errorf("select producer tenant placements for %s: %w", producer.Tenant, err)
	}

	active := make([]upstreamFanoutCandidate, 0, len(candidates))
	for _, c := range candidates {
		if !resultMeetsThreshold(producer.Result, c.upstreamResult) {
			continue
		}
		if resolveFanoutTargetCluster(placements, c.cluster) != producer.Cluster {
			// This subscriber's own resolution of producer.Tenant never
			// lands on the cluster the run actually finished in, so it
			// never fires for this completion.
			deps.Metrics.addSkipped(ctx, FanoutSkipUnplaced)
			continue
		}
		if c.disabled {
			deps.Metrics.addSkipped(ctx, FanoutSkipDisabled)
			continue
		}
		active = append(active, c)
	}

	sort.Slice(active, func(i, j int) bool {
		if active[i].cluster != active[j].cluster {
			return active[i].cluster < active[j].cluster
		}
		if active[i].tenant != active[j].tenant {
			return active[i].tenant < active[j].tenant
		}
		return active[i].jobPath < active[j].jobPath
	})

	if len(active) > upstreamFanoutCap {
		deps.Metrics.addSkippedN(ctx, FanoutSkipOverCap, len(active)-upstreamFanoutCap)
		active = active[:upstreamFanoutCap]
	}

	for _, c := range active {
		fanOutOne(ctx, deps, producer, c, logger)
	}
	return nil
}

// fanOutOne evaluates and, when granted, publishes one subscriber's
// trigger_run action. It never returns an error: a per-subscriber fault
// counts and moves to the next row, because one bad subscriber must
// never block the rest of the round.
func fanOutOne(ctx context.Context, deps FanoutDeps, producer FanoutProducer, c upstreamFanoutCandidate, logger *slog.Logger) {
	result, err := deps.Evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			Path:    GrantPathFanout,
			Cluster: c.cluster,
			Tenant:  c.tenant,
		},
		CallerTenant:    c.tenant,
		TargetTenant:    producer.Tenant,
		ResourceKind:    GrantResourceJob,
		Resource:        producer.JobPath,
		IntentKeyScheme: IntentSchemeUpstream,
	})
	if err != nil {
		logger.Warn("upstream fan-out grant check failed", "subscriber_tenant", c.tenant, "subscriber_job", c.jobPath, "error", err)
		deps.Metrics.addSkipped(ctx, FanoutSkipDenied)
		return
	}
	if !result.Allow {
		reason := FanoutSkipDenied
		if result.ReasonCode == ReasonNotReady {
			reason = FanoutSkipUnplaced
		}
		deps.Metrics.addSkipped(ctx, reason)
		return
	}

	runID, err := uuid.NewV7()
	if err != nil {
		logger.Warn("mint fan-out run id failed", "subscriber_tenant", c.tenant, "subscriber_job", c.jobPath, "error", err)
		deps.Metrics.addSkipped(ctx, FanoutSkipPublishFailed)
		return
	}

	seq := upstreamFanoutSeq(c.cluster, c.tenant, c.jobPath)
	action := LedgerAction{
		Kind:       LedgerActionTriggerRun,
		Cluster:    c.cluster,
		Namespace:  c.namespace,
		Controller: c.cell,
		Actor:      upstreamFanoutActorPrefix + producer.Tenant,
		RunID:      runID.String(),
		IntentKey:  fmt.Sprintf("%s:%s:%s", IntentSchemeUpstream, producer.RunID, seq),
		JobPath:    c.jobPath,
	}
	if err := PublishLedgerAction(deps.Conn, action); err != nil {
		logger.Warn("publish fan-out trigger_run action failed", "subscriber_tenant", c.tenant, "subscriber_job", c.jobPath, "error", err)
		deps.Metrics.addSkipped(ctx, FanoutSkipPublishFailed)
		return
	}
	deps.Metrics.addPublished(ctx)
}

// scanUpstreamFanoutCandidates reads upstreamFanoutCandidatesQuery's rows.
func scanUpstreamFanoutCandidates(rows pgx.Rows) ([]upstreamFanoutCandidate, error) {
	defer rows.Close()
	var out []upstreamFanoutCandidate
	for rows.Next() {
		var c upstreamFanoutCandidate
		if err := rows.Scan(&c.cluster, &c.namespace, &c.cell, &c.tenant, &c.jobPath, &c.disabled, &c.upstreamResult); err != nil {
			return nil, fmt.Errorf("scan job_schedules upstream row: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select job_schedules upstream rows: %w", err)
	}
	return out, nil
}

// selectTenantPlacementClusters answers every cluster tenant holds a
// tenant_placements row in, for resolveFanoutTargetCluster.
func selectTenantPlacementClusters(ctx context.Context, pool *pgxpool.Pool, tenant string) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT cluster FROM varroa.tenant_placements WHERE tenant = $1`, tenant)
	if err != nil {
		return nil, fmt.Errorf("select tenant placement clusters for %s: %w", tenant, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cluster string
		if err := rows.Scan(&cluster); err != nil {
			return nil, fmt.Errorf("scan tenant placement cluster for %s: %w", tenant, err)
		}
		out = append(out, cluster)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select tenant placement clusters for %s: %w", tenant, err)
	}
	return out, nil
}

// resolveFanoutTargetCluster answers the cluster a subscriber in
// subscriberCluster resolves target's placement to, mirroring
// GrantEvaluator.resolveTargetPolicy's own resolution: subscriberCluster
// itself when target is placed there, the lone other cluster when
// exactly one placement exists, and "" when target has no placement or
// the resolution is ambiguous between two or more clusters that do not
// include the subscriber's own. A subscriber whose own cluster also
// hosts the producer tenant therefore never fires on another cluster's
// completion, design.md D9's own rule.
func resolveFanoutTargetCluster(placements []string, subscriberCluster string) string {
	for _, c := range placements {
		if c == subscriberCluster {
			return subscriberCluster
		}
	}
	if len(placements) == 1 {
		return placements[0]
	}
	return ""
}
