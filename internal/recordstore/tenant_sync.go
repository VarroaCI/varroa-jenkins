package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// tenantSyncQueueGroup is the NATS queue group the recorder's tenant
// sync responder joins, so more than one recorder replica shares the
// subscription.
const tenantSyncQueueGroup = "recorder"

// tenantSyncTimeout bounds one tenant.sync call's database work, so a
// stalled Postgres connection does not hold the responder goroutine open
// indefinitely.
const tenantSyncTimeout = 10 * time.Second

// maxDeadInstances bounds the dead_instances entries the responder
// keeps for a reason other than a RUNNING run naming them.
const maxDeadInstances = 10

// RegisterTenantSyncResponder subscribes to bus.TenantSyncSubject with
// queue group "recorder". The returned Sub stops the subscription when
// unsubscribed. It registers its own InterruptedMetrics for the
// interrupted-run marking an "announce" request triggers (design D9)
// and its own ActionMetrics for the actions that marking publishes;
// registration failure is logged and the responder still starts, with
// marking and publishing counting nothing.
//
// grace is the confirmed-gone grace the announce hook's own marking,
// fleet-lock release, and intent re-fire apply.
func RegisterTenantSyncResponder(conn *bus.Conn, pool *pgxpool.Pool, grace time.Duration,
	logger *slog.Logger) (*bus.Sub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		logger.Warn("interrupted-run metrics registration failed; the tenant sync responder starts anyway", "error", err)
	}
	actionMetrics, err := NewActionMetrics()
	if err != nil {
		logger.Warn("action metrics registration failed; the tenant sync responder starts anyway", "error", err)
	}
	handler := func(data []byte) []byte {
		return handleTenantSync(conn, logger, pool, metrics, actionMetrics, grace, data)
	}
	return conn.SubscribeRequest(bus.TenantSyncSubject, tenantSyncQueueGroup, handler)
}

// handleTenantSync decodes a TenantSyncRequest and dispatches it by
// Kind. A body the decoder rejects, and a kind outside the two known
// values, both answer an error reply and write nothing.
func handleTenantSync(conn *bus.Conn, logger *slog.Logger, pool *pgxpool.Pool, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, grace time.Duration, data []byte) []byte {
	var req TenantSyncRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("tenant sync request decode failed", "error", err)
		return marshalTenantSyncResponse(logger, TenantSyncResponse{Error: "malformed tenant sync request"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), tenantSyncTimeout)
	defer cancel()

	switch req.Kind {
	case TenantSyncKindAnnounce:
		resp, err := handleTenantSyncAnnounce(ctx, conn, pool, metrics, actionMetrics, grace, logger, req)
		if err != nil {
			logger.Error("tenant sync announce failed", "cluster", req.Cluster, "tenant", req.Tenant, "error", err)
			return marshalTenantSyncResponse(logger, TenantSyncResponse{Error: "tenant sync announce failed"})
		}
		return marshalTenantSyncResponse(logger, resp)
	case TenantSyncKindRecords:
		resp, err := handleTenantSyncRecords(ctx, pool, req)
		if err != nil {
			logger.Error("tenant sync records failed", "cluster", req.Cluster, "tenant", req.Tenant, "error", err)
			return marshalTenantSyncResponse(logger, TenantSyncResponse{Error: "tenant sync records failed"})
		}
		return marshalTenantSyncResponse(logger, resp)
	default:
		return marshalTenantSyncResponse(logger, TenantSyncResponse{Error: fmt.Sprintf("unknown tenant sync kind %q", req.Kind)})
	}
}

// marshalTenantSyncResponse encodes resp, falling back to an encode-error
// reply when resp itself cannot be marshaled.
func marshalTenantSyncResponse(logger *slog.Logger, resp TenantSyncResponse) []byte {
	out, err := json.Marshal(resp)
	if err != nil {
		logger.Error("tenant sync response encode failed", "error", err)
		out, _ = json.Marshal(TenantSyncResponse{Error: "tenant sync response encode failed"})
	}
	return out
}

// handleTenantSyncRecords calls ListRehydrationRecords and shapes its
// answer as a TenantSyncResponse.
func handleTenantSyncRecords(ctx context.Context, pool *pgxpool.Pool, req TenantSyncRequest) (TenantSyncResponse, error) {
	if req.Cluster == "" || req.Tenant == "" {
		return TenantSyncResponse{}, errors.New("records request needs a cluster and a tenant")
	}

	jobs, err := ListRehydrationRecords(ctx, pool, req.Cluster, req.Tenant, req.JobPaths, req.PerJob)
	if err != nil {
		return TenantSyncResponse{}, err
	}

	out := make([]TenantSyncJobRecords, 0, len(jobs))
	for _, j := range jobs {
		// RehydrationRecord and TenantSyncRecord share the same field
		// names, order, and types by design: this handler is the only
		// place that turns a store-internal record into a wire record.
		records := make([]TenantSyncRecord, 0, len(j.Records))
		for _, r := range j.Records {
			records = append(records, TenantSyncRecord(r))
		}
		out = append(out, TenantSyncJobRecords{JobPath: j.JobPath, NextBuildNumber: j.NextBuildNumber, Records: records})
	}
	return TenantSyncResponse{Jobs: out}, nil
}

// tenantPlacementRow is the row handleTenantSyncAnnounce reads under
// FOR UPDATE before it decides the new stored values. A zero value
// means no row exists yet, which this responder treats the same as a
// generation-zero row with no cell instance.
type tenantPlacementRow struct {
	generation    int64
	cell          string
	cellInstance  string
	deadInstances []TenantSyncDeadInstance
}

// handleTenantSyncAnnounce upserts cluster/tenant's tenant_placements
// row. It never lowers generation. The seed fields, rehydrated_instance
// and pending_jobs, follow D6/D7: a generation-zero row seeds itself
// from the announced cell instance at once; a generation raise or a
// cell-instance change clears the seed, because the new cell holds
// none yet; otherwise the announcement's own seed report is stored
// as-is.
//
// An announcement whose cell or cell_instance differs from the row
// stored before this call runs the interrupted-run moved test for every
// RUNNING run of the tenant, once the write commits, and reports the
// count in the response's InterruptedMarked field (design D9). conn and
// metrics feed that marking; a nil metrics counts nothing.
func handleTenantSyncAnnounce(ctx context.Context, conn *bus.Conn, pool *pgxpool.Pool, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, grace time.Duration, logger *slog.Logger,
	req TenantSyncRequest) (TenantSyncResponse, error) {
	if req.Cluster == "" || req.Tenant == "" {
		return TenantSyncResponse{}, errors.New("announce request needs a cluster and a tenant")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return TenantSyncResponse{}, fmt.Errorf("begin tenant sync transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	old, err := readTenantPlacementForUpdate(ctx, tx, req.Cluster, req.Tenant)
	if err != nil {
		return TenantSyncResponse{}, err
	}

	newGeneration := old.generation
	if req.Generation > newGeneration {
		newGeneration = req.Generation
	}

	var rehydratedInstance string
	var pendingJobs []string
	switch {
	case old.generation == 0 && req.Generation == 0:
		// A controller with no cell role keeps its own home and needs
		// no seed: it is ready the moment it is announced.
		rehydratedInstance = req.CellInstance
	case req.Generation > old.generation || req.CellInstance != old.cellInstance:
		// A generation raise or a cell-instance change clears the seed
		// state: the new cell holds no seed yet.
		rehydratedInstance = ""
		pendingJobs = nil
	default:
		// Same generation, same instance: store the seed state this
		// announcement itself reports.
		rehydratedInstance = req.RehydratedInstance
		pendingJobs = req.PendingJobs
	}

	live, err := liveCellInstances(ctx, tx, req.Cluster, req.Tenant)
	if err != nil {
		return TenantSyncResponse{}, err
	}
	deadInstances := trimDeadInstances(mergeDeadInstances(old.deadInstances, req.DeadInstances), live, maxDeadInstances)

	if err := writeTenantPlacement(ctx, tx, req, newGeneration, rehydratedInstance, pendingJobs, deadInstances); err != nil {
		return TenantSyncResponse{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TenantSyncResponse{}, fmt.Errorf("commit tenant sync transaction: %w", err)
	}

	policy := req.Policy
	if len(policy) == 0 {
		policy = json.RawMessage(`{}`)
	}
	row := tenantPlacementLivenessRow{
		cell:           req.Cell,
		cellInstance:   req.CellInstance,
		cellAlive:      req.CellAlive,
		cellAliveAt:    req.CellAliveAt,
		cellLastSeenAt: req.CellLastSeenAt,
		deadInstances:  deadInstances,
		policy:         policy,
	}

	var interruptedMarked int
	if old.cell != req.Cell || old.cellInstance != req.CellInstance {
		n, markErr := MarkMovedRunsForTenant(ctx, pool, conn, metrics, actionMetrics, req.Cluster, req.Tenant,
			row, time.Now(), grace, logger)
		if markErr != nil {
			logger.Error("interrupted-run marking after announce failed",
				"cluster", req.Cluster, "tenant", req.Tenant, "error", markErr)
		} else {
			interruptedMarked = n
		}
	}

	// The unstarted-intent re-fire runs after the marking, for this one
	// tenant. A granted intent whose build never started holds no runs
	// row, so the marking above can never see it.
	if _, refireErr := RefireLostIntents(ctx, pool, conn, metrics, actionMetrics, req.Cluster, req.Tenant,
		grace, time.Now(), logger); refireErr != nil {
		logger.Error("unstarted-intent re-fire after announce failed",
			"cluster", req.Cluster, "tenant", req.Tenant, "error", refireErr)
	}

	// The fleet lock's second release confirmation (design.md D11) can
	// become newly true on any announce, not only a cell or an instance
	// change: cell_alive can flip to false while the tenant stays on the
	// same cell, and dead_instances can gain an entry either way. This
	// runs on every announce, against the row this announce just wrote.
	if _, releaseErr := ReleaseConfirmedFleetLocks(ctx, pool, req.Cluster, req.Tenant, row, time.Now(),
		grace, logger); releaseErr != nil {
		logger.Error("fleet lock release confirmation after announce failed",
			"cluster", req.Cluster, "tenant", req.Tenant, "error", releaseErr)
	}

	return TenantSyncResponse{Ok: true, InterruptedMarked: interruptedMarked}, nil
}

// readTenantPlacementForUpdate reads cluster/tenant's tenant_placements
// row and locks it for the rest of the transaction. It answers a zero
// tenantPlacementRow, with no error, when no row exists yet.
func readTenantPlacementForUpdate(ctx context.Context, tx pgx.Tx, cluster, tenant string) (tenantPlacementRow, error) {
	var (
		generation   int64
		cell         string
		cellInstance string
		deadRaw      json.RawMessage
	)
	err := tx.QueryRow(ctx, `
		SELECT generation, cell, cell_instance, dead_instances
		FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2
		FOR UPDATE
	`, cluster, tenant).Scan(&generation, &cell, &cellInstance, &deadRaw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return tenantPlacementRow{}, nil
	case err != nil:
		return tenantPlacementRow{}, fmt.Errorf("read tenant placement: %w", err)
	}

	var dead []TenantSyncDeadInstance
	if len(deadRaw) > 0 {
		if err := json.Unmarshal(deadRaw, &dead); err != nil {
			return tenantPlacementRow{}, fmt.Errorf("decode dead instances: %w", err)
		}
	}
	return tenantPlacementRow{generation: generation, cell: cell, cellInstance: cellInstance, deadInstances: dead}, nil
}

// liveCellInstances answers the set of cell instances the tenant's live
// work names: an intent-joined RUNNING or QUEUED run's own
// cell_instance, an unclaimed QUEUED run's accepted_instance payload
// value, and the owner a GRANTED intent with a null refired_at names.
// The dead-instances trim in writeTenantPlacement never drops an entry
// in this set.
//
// The third arm exists because the first two join runs. A GRANTED
// intent whose build never started holds no runs row at all, so a busy
// pool would otherwise trim away the evidence the re-fire needs. An
// intent the recorder already re-fired holds a non-null refired_at, so
// it stops protecting its dead instance at once, and so does one past
// maxRefireAttempts, whose retry loop has ended.
func liveCellInstances(ctx context.Context, tx pgx.Tx, cluster, tenant string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ri.cell_instance
		FROM varroa.run_intents ri
		JOIN varroa.runs r ON r.run_id = ri.run_id
		WHERE ri.cluster = $1 AND ri.tenant = $2 AND r.status IN ('QUEUED', 'RUNNING')
		  AND ri.cell_instance IS NOT NULL AND ri.cell_instance <> ''
		UNION
		SELECT DISTINCT r.payload->>'accepted_instance'
		FROM varroa.runs r
		WHERE r.cluster = $1 AND r.tenant = $2 AND r.status = 'QUEUED' AND r.payload ? 'accepted_instance'
		UNION
		SELECT DISTINCT ri.cell_instance
		FROM varroa.run_intents ri
		WHERE ri.cluster = $1 AND ri.tenant = $2 AND ri.state = 'GRANTED'
		  AND ri.refired_at IS NULL
		  AND ri.refire_attempts < $3
		  AND ri.cell_instance IS NOT NULL AND ri.cell_instance <> ''
	`, cluster, tenant, maxRefireAttempts)
	if err != nil {
		return nil, fmt.Errorf("list live cell instances: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var instance string
		if err := rows.Scan(&instance); err != nil {
			return nil, fmt.Errorf("scan live cell instance: %w", err)
		}
		out[instance] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list live cell instances: %w", err)
	}
	return out, nil
}

// mergeDeadInstances unions stored and incoming by Instance, keeping the
// newer LastSeenAt on a collision, and answers the result sorted newest
// first. The recorder merges rather than replaces, because an operator
// restart loses every deleted pod identity it held in memory.
//
// The merge keeps the EARLIER non-nil ConfirmedGoneAt. The operator
// repeats the same dead instance on every tick, so a later-wins rule
// would hold the timestamp young forever and the confirmed-gone grace
// would never elapse. A stored nil takes the incoming value, and an
// incoming nil never clears a stored one.
func mergeDeadInstances(stored, incoming []TenantSyncDeadInstance) []TenantSyncDeadInstance {
	byInstance := make(map[string]TenantSyncDeadInstance, len(stored)+len(incoming))
	for _, d := range stored {
		byInstance[d.Instance] = d
	}
	for _, d := range incoming {
		existing, ok := byInstance[d.Instance]
		if !ok {
			byInstance[d.Instance] = d
			continue
		}
		merged := existing
		if d.LastSeenAt.After(existing.LastSeenAt) {
			merged = d
		}
		merged.ConfirmedGoneAt = earlierConfirmation(existing.ConfirmedGoneAt, d.ConfirmedGoneAt)
		byInstance[d.Instance] = merged
	}
	merged := make([]TenantSyncDeadInstance, 0, len(byInstance))
	for _, d := range byInstance {
		merged = append(merged, d)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].LastSeenAt.After(merged[j].LastSeenAt) })
	return merged
}

// earlierConfirmation answers the earlier of two confirmation times. A
// nil argument carries no confirmation, so the other one wins.
func earlierConfirmation(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.Before(*a):
		return b
	default:
		return a
	}
}

// trimDeadInstances keeps every entry named in keep without limit, and
// trims every other entry to at most limit, newest first. keep holds the
// cell instances a RUNNING run still names, so the interrupted-run sweep
// never loses the evidence a live build depends on.
func trimDeadInstances(merged []TenantSyncDeadInstance, keep map[string]bool, limit int) []TenantSyncDeadInstance {
	var kept, rest []TenantSyncDeadInstance
	for _, d := range merged {
		if keep[d.Instance] {
			kept = append(kept, d)
		} else {
			rest = append(rest, d)
		}
	}
	if len(rest) > limit {
		rest = rest[:limit]
	}
	out := append(kept, rest...) //nolint:gocritic // kept and rest are both fresh slices this function built
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.After(out[j].LastSeenAt) })
	return out
}

// writeTenantPlacement upserts cluster/tenant's row with the computed
// generation and seed fields, inside tx. A NOT NULL jsonb column never
// receives a Go nil: an empty policy becomes "{}", and empty
// pending_jobs and dead_instances lists become "[]".
func writeTenantPlacement(ctx context.Context, tx pgx.Tx, req TenantSyncRequest, generation int64,
	rehydratedInstance string, pendingJobs []string, deadInstances []TenantSyncDeadInstance) error {
	policy := req.Policy
	if len(policy) == 0 {
		policy = json.RawMessage(`{}`)
	}
	pendingJobsJSON, err := json.Marshal(nonNilStrings(pendingJobs))
	if err != nil {
		return fmt.Errorf("encode pending jobs: %w", err)
	}
	deadInstancesJSON, err := json.Marshal(nonNilDeadInstances(deadInstances))
	if err != nil {
		return fmt.Errorf("encode dead instances: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.tenant_placements (
			cluster, tenant, namespace, cell, generation, cell_instance, policy,
			rehydrated_instance, pending_jobs, cell_alive, cell_alive_at, cell_last_seen_at,
			dead_instances, announced_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9::jsonb, $10, $11, $12, $13::jsonb, now()
		)
		ON CONFLICT (cluster, tenant) DO UPDATE SET
			namespace           = EXCLUDED.namespace,
			cell                = EXCLUDED.cell,
			generation          = EXCLUDED.generation,
			cell_instance       = EXCLUDED.cell_instance,
			policy              = EXCLUDED.policy,
			rehydrated_instance = EXCLUDED.rehydrated_instance,
			pending_jobs        = EXCLUDED.pending_jobs,
			cell_alive          = EXCLUDED.cell_alive,
			cell_alive_at       = EXCLUDED.cell_alive_at,
			cell_last_seen_at   = EXCLUDED.cell_last_seen_at,
			dead_instances      = EXCLUDED.dead_instances,
			announced_at        = now()
	`,
		req.Cluster, req.Tenant, req.Namespace, req.Cell, generation, req.CellInstance, string(policy),
		rehydratedInstance, string(pendingJobsJSON), req.CellAlive, req.CellAliveAt, req.CellLastSeenAt,
		string(deadInstancesJSON),
	); err != nil {
		return fmt.Errorf("upsert tenant placement: %w", err)
	}
	return nil
}

// nonNilStrings turns a nil slice into an empty one, so json.Marshal
// writes "[]" instead of "null" into a NOT NULL jsonb column.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nonNilDeadInstances is nonNilStrings for []TenantSyncDeadInstance.
func nonNilDeadInstances(d []TenantSyncDeadInstance) []TenantSyncDeadInstance {
	if d == nil {
		return []TenantSyncDeadInstance{}
	}
	return d
}
