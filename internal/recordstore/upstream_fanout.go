// Package recordstore: the upstream fan-out, design D9. RunUpstreamFanout
// runs the ordered steps after the recorder commits an ingested
// run.completed for the run that owns a producer job. It never runs
// inside that transaction: it reads the grant evaluator and publishes on
// the bus, and a lost publish must never roll a stored run back.
//
// The round selects two candidate sets. Set A holds a varroaUpstream
// subscription. Set B holds a native Jenkins upstream trigger inside the
// producer's own tenant.
package recordstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	// FanoutSkipCrossTenant counts one subscriber whose native upstream
	// list holds at least one path outside its own tenant and that
	// matched no path inside it. The count is one for each such
	// subscriber, never one for each foreign path.
	FanoutSkipCrossTenant = "cross_tenant"
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

// upstreamFanoutCandidate is one job_schedules row the fan-out selects:
// a subscriber's own declared upstream trigger, of either kind. namespace
// and cell are the coordinate of the export the recorder last accepted.
// The fan-out addresses them only for a tenant with no placement row.
// A placed tenant's job home can move after that export.
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

// nativeUpstreamTriggerType is the trigger type a Jenkins
// ReverseBuildTrigger exports.
const nativeUpstreamTriggerType = "upstream"

// nativeFanoutCandidatesQuery reads every native upstream row of the
// producer's own tenant and cluster. A native edge never crosses a
// tenant, so the tenant filter is the whole boundary.
//
// The query seeks on the (tenant, trigger_type) index. The
// (upstream_tenant, upstream_job) index serves a varroaUpstream
// subscription alone, because a native row names several jobs and holds
// them in its specification field.
const nativeFanoutCandidatesQuery = `
	SELECT cluster, namespace, cell, tenant, job_path, disabled, upstream_result, spec
	FROM varroa.job_schedules
	WHERE cluster = $1 AND tenant = $2 AND trigger_type = $3
`

// claimFanoutRound marks producer's run row as fanned out, and reports
// whether this transaction is the one that claimed it.
//
// The build stream is at least once, so one completion can reach the
// ingest twice. Every other write of the terminal path is idempotent,
// and the fan-out is not: it mints a fresh run identifier for each
// subscriber, so a second round publishes a second trigger_run action
// under the same intent key. The target's claim then answers duplicate,
// and the accepted-trigger row of the second run records a build that
// never runs.
//
// The marker is written in the transaction that records the terminal
// write, so the claim is durable with it. A repeat delivery waits on the
// same row, reads the committed marker, and claims nothing.
func claimFanoutRound(ctx context.Context, tx pgx.Tx, createdAt time.Time, runID uuid.UUID) (bool, error) {
	var claimed bool
	err := tx.QueryRow(ctx, `
		UPDATE varroa.runs
		SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('fanout_at', to_jsonb(now()))
		WHERE created_at = $1 AND run_id = $2 AND NOT COALESCE(payload ? 'fanout_at', false)
		RETURNING true
	`, createdAt, runID).Scan(&claimed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("claim upstream fan-out round for %s: %w", runID, err)
	}
	return claimed, nil
}

// RunUpstreamFanout runs one fan-out round for producer, design.md D9's
// ordered steps. The caller runs it after the ingest transaction that
// recorded producer's completion has committed, never inside it, and
// only when claimFanoutRound claimed that completion.
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
	subscriptions, err := scanUpstreamFanoutCandidates(rows)
	if err != nil {
		return fmt.Errorf("scan upstream fan-out candidates for %s/%s: %w", producer.Tenant, relativeJob, err)
	}

	native, err := selectNativeFanoutCandidates(ctx, deps, producer)
	if err != nil {
		return err
	}

	// Step 3: the two sets join. A job that declares both trigger kinds
	// contributes one candidate for each, with its own threshold and its
	// own disabled flag.
	candidates := make([]upstreamFanoutCandidate, 0, len(subscriptions)+len(native))
	for _, c := range subscriptions {
		if resultMeetsThreshold(producer.Result, c.upstreamResult) {
			candidates = append(candidates, c)
		}
	}
	candidates = append(candidates, native...)
	if len(candidates) == 0 {
		return nil
	}

	placements, err := selectTenantPlacementClusters(ctx, deps.Pool, producer.Tenant)
	if err != nil {
		return fmt.Errorf("select producer tenant placements for %s: %w", producer.Tenant, err)
	}

	active := make([]upstreamFanoutCandidate, 0, len(candidates))
	for _, c := range candidates {
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

	active = dedupeFanoutCandidates(active)

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

	homes := newJobHomeCache()
	for _, c := range active {
		fanOutOne(ctx, deps, producer, c, homes, logger)
	}
	return nil
}

// dedupeFanoutCandidates keeps one candidate for each
// (cluster, tenant, job_path), in the order the joined list holds them.
//
// The de-duplication runs after the threshold test and the disabled test.
// A job that declares both trigger kinds therefore builds when either
// declaration passes, and one declaration cannot hide the other.
func dedupeFanoutCandidates(in []upstreamFanoutCandidate) []upstreamFanoutCandidate {
	seen := make(map[string]bool, len(in))
	out := make([]upstreamFanoutCandidate, 0, len(in))
	for _, c := range in {
		key := c.cluster + "\x00" + c.tenant + "\x00" + c.jobPath
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// selectNativeFanoutCandidates answers set B: every native upstream row
// of the producer's own tenant whose specification list holds the
// producer's tenant-qualified job path and whose threshold the run's
// result meets.
//
// The comparison uses the qualified path on both sides. The export writes
// each resolved upstream job's full path, which on a cell opens with the
// tenant folder, so the relative form set A matches would never match
// here.
func selectNativeFanoutCandidates(ctx context.Context, deps FanoutDeps,
	producer FanoutProducer) ([]upstreamFanoutCandidate, error) {
	rows, err := deps.Pool.Query(ctx, nativeFanoutCandidatesQuery,
		producer.Cluster, producer.Tenant, nativeUpstreamTriggerType)
	if err != nil {
		return nil, fmt.Errorf("select native upstream fan-out candidates for %s: %w", producer.JobPath, err)
	}
	defer rows.Close()

	var out []upstreamFanoutCandidate
	for rows.Next() {
		var (
			c    upstreamFanoutCandidate
			spec string
		)
		if err := rows.Scan(&c.cluster, &c.namespace, &c.cell, &c.tenant, &c.jobPath,
			&c.disabled, &c.upstreamResult, &spec); err != nil {
			return nil, fmt.Errorf("scan job_schedules native upstream row: %w", err)
		}
		matched, foreign := matchNativeUpstreamList(spec, c.tenant, producer.JobPath)
		if !matched {
			if foreign {
				deps.Metrics.addSkipped(ctx, FanoutSkipCrossTenant)
			}
			continue
		}
		if !resultMeetsThreshold(producer.Result, c.upstreamResult) {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select native upstream fan-out rows for %s: %w", producer.JobPath, err)
	}
	return out, nil
}

// matchNativeUpstreamList reads one native row's comma-separated path
// list. matched is true when a listed path of tenant equals producerJob.
// foreign is true when the list holds at least one path whose first
// segment is another tenant; the recorder drops such a path, because a
// native edge is intra-tenant by definition.
func matchNativeUpstreamList(spec, tenant, producerJob string) (matched, foreign bool) {
	for _, raw := range strings.Split(spec, ",") {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		if first, _, _ := strings.Cut(path, "/"); first != tenant {
			foreign = true
			continue
		}
		if path == producerJob {
			matched = true
		}
	}
	return matched, foreign
}

// fanOutOne evaluates and, when granted, publishes one subscriber's
// trigger_run action. It never returns an error: a per-subscriber fault
// counts and moves to the next row, because one bad subscriber must
// never block the rest of the round.
//
// For a placed tenant, the action names the member that holds the
// subscriber job's home slot. The claim fence reads the same member, so a
// trigger sent to another cell is refused. A tenant with no placement row
// keeps the schedule row's own namespace and cell. The round cannot retry a skip, because the producer's
// run already carries its fan-out marker.
func fanOutOne(ctx context.Context, deps FanoutDeps, producer FanoutProducer, c upstreamFanoutCandidate,
	homes *jobHomeCache, logger *slog.Logger) {
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

	namespace, cell := c.namespace, c.cell
	home, err := homes.resolve(ctx, deps.Pool, c.cluster, c.tenant, c.jobPath)
	if err != nil {
		logger.Warn("upstream fan-out home read failed", "subscriber_tenant", c.tenant, "subscriber_job", c.jobPath, "error", err)
		deps.Metrics.addSkipped(ctx, FanoutSkipUnplaced)
		return
	}
	if home.Placed {
		if home.Cell == "" {
			logger.Warn("upstream fan-out skipped: the job's home slot holds no cell",
				"subscriber_tenant", c.tenant, "subscriber_job", c.jobPath)
			deps.Metrics.addSkipped(ctx, FanoutSkipUnplaced)
			return
		}
		namespace, cell = home.Namespace, home.Cell
	}

	runID, err := uuid.NewV7()
	if err != nil {
		logger.Warn("mint fan-out run id failed", "subscriber_tenant", c.tenant, "subscriber_job", c.jobPath, "error", err)
		deps.Metrics.addSkipped(ctx, FanoutSkipPublishFailed)
		return
	}

	seq := upstreamFanoutSeq(c.cluster, c.tenant, c.jobPath)
	key := fmt.Sprintf("%s:%s:%s", IntentSchemeUpstream, producer.RunID, seq)
	if err := recordUpstreamOrigin(ctx, deps.Pool, key, c.cluster, c.tenant, c.jobPath); err != nil {
		// The trigger still publishes. Its run then starts without an
		// upstream link, which only loses the Triggered by link.
		logger.Warn("record fan-out upstream origin failed", "subscriber_tenant", c.tenant, "subscriber_job", c.jobPath, "error", err)
	}
	action := LedgerAction{
		Kind:       LedgerActionTriggerRun,
		Cluster:    c.cluster,
		Namespace:  namespace,
		Controller: cell,
		Actor:      upstreamFanoutActorPrefix + producer.Tenant,
		RunID:      runID.String(),
		IntentKey:  key,
		JobPath:    c.jobPath,
	}
	if err := PublishLedgerAction(deps.Conn, action); err != nil {
		logger.Warn("publish fan-out trigger_run action failed", "subscriber_tenant", c.tenant, "subscriber_job", c.jobPath, "error", err)
		deps.Metrics.addSkipped(ctx, FanoutSkipPublishFailed)
		return
	}
	deps.Metrics.addPublished(ctx)
	logger.Info("upstream fan-out published", "subscriber_tenant", c.tenant, "subscriber_job", c.jobPath,
		"controller", namespace+"/"+cell, "run_id", action.RunID, "intent_key", action.IntentKey)
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
