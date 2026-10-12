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
	"go.opentelemetry.io/otel/metric"

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
// queue group "recorder". The returned Responder subscribes again after a denial and stops when
// unsubscribed. It registers on mp its own InterruptedMetrics for the
// interrupted-run marking an "announce" request triggers (design D9)
// and its own ActionMetrics for the actions that marking publishes;
// registration failure is logged and the responder still starts, with
// marking and publishing counting nothing.
//
// grace is the confirmed-gone grace the announce hook's own marking,
// fleet-lock release, and intent re-fire apply. notifier publishes the
// lock notifications the announce hook's fleet-lock release owes, and a
// nil notifier publishes nothing. logs reads a moved run's stored
// console log for the resume action the announce hook's takeover
// publishes, and a nil logs sends no log sequence.
func RegisterTenantSyncResponder(mp metric.MeterProvider, conn *bus.Conn, pool *pgxpool.Pool, grace time.Duration,
	stateDeleter ObjectDeleter, logs LogIndexReader, notifier *RunNotifier, logger *slog.Logger) (*bus.Responder, error) {
	if logger == nil {
		logger = slog.Default()
	}
	metrics, err := NewInterruptedMetrics(mp)
	if err != nil {
		logger.Warn("interrupted-run metrics registration failed; the tenant sync responder starts anyway", "error", err)
	}
	actionMetrics, err := NewActionMetrics(mp)
	if err != nil {
		logger.Warn("action metrics registration failed; the tenant sync responder starts anyway", "error", err)
	}
	handler := func(data []byte) []byte {
		return handleTenantSync(conn, logger, pool, metrics, actionMetrics, stateDeleter, logs, grace, notifier, data)
	}
	return conn.ServeRequests(bus.TenantSyncSubject, tenantSyncQueueGroup, handler)
}

// handleTenantSync decodes a TenantSyncRequest and dispatches it by
// Kind. A body the decoder rejects, and a kind outside the three known
// values, both answer an error reply and write nothing.
func handleTenantSync(conn *bus.Conn, logger *slog.Logger, pool *pgxpool.Pool, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, stateDeleter ObjectDeleter, logs LogIndexReader, grace time.Duration,
	notifier *RunNotifier, data []byte) []byte {
	var req TenantSyncRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("tenant sync request decode failed", "error", err)
		return marshalTenantSyncResponse(logger, TenantSyncResponse{Error: "malformed tenant sync request"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), tenantSyncTimeout)
	defer cancel()

	switch req.Kind {
	case TenantSyncKindAnnounce:
		resp, err := handleTenantSyncAnnounce(ctx, conn, pool, metrics, actionMetrics, stateDeleter, logs, grace, notifier, logger, req)
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
	case TenantSyncKindHomes:
		resp, err := handleTenantSyncHomes(ctx, pool, req)
		if err != nil {
			logger.Error("tenant sync homes failed", "cluster", req.Cluster, "tenant", req.Tenant, "error", err)
			return marshalTenantSyncResponse(logger, TenantSyncResponse{Error: "tenant sync homes failed"})
		}
		return marshalTenantSyncResponse(logger, resp)
	default:
		return marshalTenantSyncResponse(logger, TenantSyncResponse{Error: fmt.Sprintf("unknown tenant sync kind %q", req.Kind)})
	}
}

// handleTenantSyncHomes answers the tenant's member list and its job home
// map. The recorder is the map's only owner, and this kind is how the
// operator reads it for the desired state.
//
// A tenant with no stored row answers an empty list and an empty map. A
// tenant whose stored list is empty answers the synthetic slot-0 member
// every reader resolves, so the caller needs no branch.
func handleTenantSyncHomes(ctx context.Context, pool *pgxpool.Pool, req TenantSyncRequest) (TenantSyncResponse, error) {
	if req.Cluster == "" || req.Tenant == "" {
		return TenantSyncResponse{}, errors.New("homes request needs a cluster and a tenant")
	}
	row, exists, err := readTenantPlacementMembers(ctx, pool, req.Cluster, req.Tenant)
	if err != nil {
		return TenantSyncResponse{}, err
	}
	if !exists {
		return TenantSyncResponse{Ok: true, JobHomes: map[string]int32{}}, nil
	}
	return TenantSyncResponse{Ok: true, Members: row.members, JobHomes: nonNilJobHomes(row.jobHomes)}, nil
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
	generation         int64
	cell               string
	cellInstance       string
	rehydratedInstance string
	pendingJobs        []string
	deadInstances      []TenantSyncDeadInstance
	// incarnation identifies the tenant object the row describes. An
	// empty value means unknown: the row was written before the column
	// existed, or by a caller that names no incarnation.
	incarnation string
	// retiredIncarnations holds the incarnations a take-over replaced,
	// newest first. An announcement that names one is stale.
	retiredIncarnations []string
	// members holds the stored member list, resolved through the row's
	// own scalar columns. A row with an empty stored list therefore
	// answers one synthetic slot-0 member, so an upgraded tenant keeps
	// its seed state on its first member-carrying announcement.
	members  []member
	jobHomes map[string]int32
}

// handleTenantSyncAnnounce upserts cluster/tenant's tenant_placements
// row. It never lowers generation.
//
// A request staleAnnounce judges stale writes nothing and runs no
// sweep. It answers Stale and no Ok, so the sender stores no cache entry
// and writes no placement status from its older reads. The seed fields, rehydrated_instance
// and pending_jobs, follow D6/D7: a generation-zero row seeds itself
// from the announced cell instance at once; a generation raise or a
// cell-instance change clears the seed, because the new cell holds
// none yet; otherwise the announcement's own seed report is stored
// as-is.
//
// The handler adds the dead instances deriveDeadInstances derives from a
// changed or dropped member, inside the same transaction. Before that it
// removes every unconfirmed entry for an instance the request names.
//
// An announcement whose cell or cell_instance differs from the row
// stored before this call, or that derived a dead instance, runs the
// interrupted-run moved test for every RUNNING run of the tenant, once
// the write commits, and reports the count in the response's
// InterruptedMarked field (design D9). conn and
// metrics feed that marking; a nil metrics counts nothing. notifier
// carries the lock notifications of the fleet-lock move and release,
// and a nil notifier publishes nothing.
func handleTenantSyncAnnounce(ctx context.Context, conn *bus.Conn, pool *pgxpool.Pool, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, stateDeleter ObjectDeleter, logs LogIndexReader, grace time.Duration,
	notifier *RunNotifier, logger *slog.Logger, req TenantSyncRequest) (TenantSyncResponse, error) {
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

	// A tenant created again under one name carries a new incarnation. The
	// row it finds belongs to the tenant before it, so this announcement
	// takes the row over: it adopts the announced generation, even below
	// the stored one, and clears every seed the old tenant left. Without
	// the take-over the announce could never lower the generation, and the
	// new tenant's claims would wait for the teardown purge to reset the
	// row.
	//
	// A row that names no incarnation needs the second test. Such a row
	// was written before the column existed, so the incarnations cannot
	// be compared, and a generation below the stored one is the proof
	// instead: a tenant never lowers its own generation, so a lower one
	// comes from a tenant that started again.
	takeOver := incarnationChanged(old.incarnation, req.Incarnation) ||
		(old.incarnation == "" && req.Incarnation != "" && req.Generation < old.generation)

	// The first announcement to name an incarnation stamps a row that
	// names none. Such a row can hold the schedules of the tenant before
	// this one, at any generation, and no test tells them from this
	// tenant's own. The stamp therefore drops every one of them, exactly
	// once per tenant: the cell exports the live set again within its own
	// ten-minute poll, and the rows of a tenant that is gone never come
	// back.
	stampsIncarnation := old.incarnation == "" && req.Incarnation != ""

	if staleAnnounce(staleAnnounceInput{
		takeOver: takeOver, retired: old.retiredIncarnations, storedGeneration: old.generation,
		prior: old.members, stored: old.deadInstances, req: req,
	}) {
		reason := "older state"
		if retiredIncarnation(old.retiredIncarnations, req.Incarnation) {
			reason = "retired incarnation"
		}
		logger.Info("tenant sync announce refused as stale",
			"cluster", req.Cluster, "tenant", req.Tenant, "reason", reason,
			"incarnation", req.Incarnation, "storedIncarnation", old.incarnation,
			"generation", req.Generation, "storedGeneration", old.generation,
			"cellInstance", req.CellInstance, "storedCellInstance", old.cellInstance)
		return TenantSyncResponse{Stale: true}, nil
	}

	newGeneration := old.generation
	if takeOver || req.Generation > newGeneration {
		newGeneration = req.Generation
	}

	generationRaised := takeOver || req.Generation > old.generation

	var rehydratedInstance string
	var pendingJobs []string
	switch {
	case old.generation == 0 && req.Generation == 0:
		// A controller with no cell role keeps its own home and needs
		// no seed: it is ready the moment it is announced.
		rehydratedInstance = req.CellInstance
	case generationRaised || req.CellInstance != old.cellInstance:
		// A generation raise or a cell-instance change clears the seed
		// state: the new cell holds no seed yet.
		rehydratedInstance = ""
		pendingJobs = nil
	default:
		// Same generation, same instance: store the seed state this
		// announcement itself reports, except where keepAppliedSeed
		// judges the report stale.
		rehydratedInstance = keepAppliedSeed(old.rehydratedInstance, req.RehydratedInstance)
		pendingJobs = keepDeferredJobs(old.pendingJobs, req.PendingJobs,
			req.RehydratedInstance, rehydratedInstance)
	}

	live, err := liveCellInstances(ctx, tx, req.Cluster, req.Tenant)
	if err != nil {
		return TenantSyncResponse{}, err
	}
	members := resolveAnnouncedMembers(old.members, req.Members, generationRaised)

	announced := newPlacementMembers(members, nil, placementScalars{
		cell: req.Cell, namespace: req.Namespace, cellInstance: req.CellInstance,
	}).members
	// An accepted announcement proves every instance it names live. An
	// unconfirmed entry for such an instance leaves the list before the
	// derivation reads it.
	listed := pruneNamedUnconfirmed(mergeDeadInstances(old.deadInstances, req.DeadInstances),
		announcedInstances(req.CellInstance, announced))
	derived := deriveDeadInstances(deadInstanceDerivation{
		prior:           old.members,
		announced:       announced,
		requestInstance: req.CellInstance,
		listed:          listed,
		now:             time.Now(),
	})
	deadInstances := trimDeadInstances(append(listed, derived...), live, maxDeadInstances)

	jobHomes, err := recomputeJobHomes(ctx, tx, req.Cluster, req.Tenant, members, old.jobHomes, actionMetrics)
	if err != nil {
		return TenantSyncResponse{}, err
	}

	if takeOver || stampsIncarnation {
		// On a take-over every job_schedules row of this name belongs to
		// the tenant before this one. A cell exports a tenant's jobs only
		// after the announce that placed it, and this is that announce,
		// so the new tenant has exported none yet. A row left here would
		// outlive its own tenant: the backstop admits a row by its job's
		// home, and the new tenant's home can name the same cell, so the
		// old tenant's cron would keep firing under the new tenant's name.
		if err := deleteTenantJobSchedules(ctx, tx, req.Cluster, req.Tenant); err != nil {
			return TenantSyncResponse{}, err
		}
		// The branch inventory follows the same rule. Left in place, the
		// rehome step would adopt the previous tenant's projects.
		if _, err := tx.Exec(ctx, `
			DELETE FROM varroa.job_branch_projects
			WHERE cluster = $1 AND tenant = $2
		`, req.Cluster, req.Tenant); err != nil {
			return TenantSyncResponse{}, fmt.Errorf("delete the previous tenant's job branch inventory: %w", err)
		}
	}

	// A take-over retires the stored incarnation, so a late announcement
	// of the replaced tenant object can never take the row back. The
	// second take-over arm names no stored incarnation and retires none.
	retired := old.retiredIncarnations
	if takeOver {
		retired = retireIncarnation(retired, old.incarnation)
	}

	if err := writeTenantPlacement(ctx, tx, req, newGeneration, rehydratedInstance, pendingJobs,
		deadInstances, members, jobHomes, placementIdentity{
			incarnation: keptIncarnation(old.incarnation, req.Incarnation),
			retired:     retired,
		}); err != nil {
		return TenantSyncResponse{}, err
	}
	homeView := newPlacementMembers(members, jobHomes, placementScalars{cell: req.Cell, namespace: req.Namespace})
	if err := rehomeJobSchedules(ctx, tx, req.Cluster, req.Tenant, homeView, newGeneration); err != nil {
		return TenantSyncResponse{}, err
	}
	if err := rehomeJobBranches(ctx, tx, req.Cluster, req.Tenant, homeView, newGeneration); err != nil {
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
		// The built row carries the member view this announce just
		// wrote, so the marking resolves a run's owning instance the
		// same way a fresh read would.
		members: newPlacementMembers(members, jobHomes, placementScalars{
			cell: req.Cell, namespace: req.Namespace, cellInstance: req.CellInstance,
			rehydratedInstance: rehydratedInstance, pendingJobs: pendingJobs,
		}),
	}

	// The mesh takeover runs before the marking, so an armed run is
	// shielded before the marking sees it. No rule depends on that
	// order: the dispatch tests its own window, and the marking write
	// tests the same window against the committed record.
	if _, takeoverErr := TakeOverLostRuns(ctx, pool, conn, metrics, actionMetrics, logs, req.Cluster, req.Tenant,
		grace, time.Now(), logger); takeoverErr != nil {
		logger.Error("resume takeover after announce failed",
			"cluster", req.Cluster, "tenant", req.Tenant, "error", takeoverErr)
	}

	var interruptedMarked int
	if old.cell != req.Cell || old.cellInstance != req.CellInstance || len(derived) > 0 {
		n, markErr := MarkMovedRunsForTenant(ctx, pool, conn, metrics, actionMetrics, stateDeleter,
			req.Cluster, req.Tenant, row, time.Now(), grace, logger)
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

	// The held-to-releasing move runs after the marking and before the
	// release confirmation. A cell-lost mark of this announce then
	// releases its lock in the same announce, when the grace already
	// elapsed. The move is fleet-wide, so it also reaches a run that
	// another path marked.
	if _, moveErr := moveInterruptedFleetLockHolders(ctx, pool, time.Now(), notifier); moveErr != nil {
		logger.Error("fleet lock holder move after announce failed",
			"cluster", req.Cluster, "tenant", req.Tenant, "error", moveErr)
	}

	// The fleet lock's second release confirmation (design.md D11) can
	// become newly true on any announce, not only a cell or an instance
	// change: cell_alive can flip to false while the tenant stays on the
	// same cell, and dead_instances can gain an entry either way. This
	// runs on every announce, against the row this announce just wrote.
	if _, releaseErr := ReleaseConfirmedFleetLocks(ctx, pool, req.Cluster, req.Tenant, row, time.Now(),
		grace, notifier, logger); releaseErr != nil {
		logger.Error("fleet lock release confirmation after announce failed",
			"cluster", req.Cluster, "tenant", req.Tenant, "error", releaseErr)
	}

	return TenantSyncResponse{Ok: true, InterruptedMarked: interruptedMarked,
		Unconfirmed: unconfirmedDeadInstances(deadInstances)}, nil
}

// unconfirmedDeadInstances answers the entries of dead that name a cell and
// carry no confirmed_gone_at. The announce reply hands them back to the
// operator, which reads each pod again until the pod is gone.
func unconfirmedDeadInstances(dead []TenantSyncDeadInstance) []TenantSyncDeadInstance {
	var out []TenantSyncDeadInstance
	for _, d := range dead {
		if d.ConfirmedGoneAt == nil && d.Cell != "" && d.Namespace != "" {
			out = append(out, d)
		}
	}
	return out
}

// readTenantPlacementForUpdate reads cluster/tenant's tenant_placements
// row and locks it for the rest of the transaction. It answers a zero
// tenantPlacementRow, with no error, when no row exists yet.
func readTenantPlacementForUpdate(ctx context.Context, tx pgx.Tx, cluster, tenant string) (tenantPlacementRow, error) {
	var (
		generation         int64
		cell               string
		namespace          string
		cellInstance       string
		rehydratedInstance string
		deadRaw            json.RawMessage
		pendingRaw         json.RawMessage
		membersRaw         json.RawMessage
		jobHomesRaw        json.RawMessage
		incarnation        string
		retiredRaw         json.RawMessage
	)
	err := tx.QueryRow(ctx, `
		SELECT generation, cell, namespace, cell_instance, rehydrated_instance,
		       dead_instances, pending_jobs, members, job_homes, incarnation,
		       retired_incarnations
		FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2
		FOR UPDATE
	`, cluster, tenant).Scan(&generation, &cell, &namespace, &cellInstance, &rehydratedInstance,
		&deadRaw, &pendingRaw, &membersRaw, &jobHomesRaw, &incarnation, &retiredRaw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return tenantPlacementRow{jobHomes: map[string]int32{}}, nil
	case err != nil:
		return tenantPlacementRow{}, fmt.Errorf("read tenant placement: %w", err)
	}

	var dead []TenantSyncDeadInstance
	if len(deadRaw) > 0 {
		if err := json.Unmarshal(deadRaw, &dead); err != nil {
			return tenantPlacementRow{}, fmt.Errorf("decode dead instances: %w", err)
		}
	}
	var retired []string
	if len(retiredRaw) > 0 {
		if err := json.Unmarshal(retiredRaw, &retired); err != nil {
			return tenantPlacementRow{}, fmt.Errorf("decode retired incarnations: %w", err)
		}
	}
	var pending []string
	if len(pendingRaw) > 0 {
		if err := json.Unmarshal(pendingRaw, &pending); err != nil {
			return tenantPlacementRow{}, fmt.Errorf("decode pending jobs: %w", err)
		}
	}
	decoded, err := decodePlacementMembers(membersRaw, jobHomesRaw, placementScalars{
		cell: cell, namespace: namespace, cellInstance: cellInstance,
		rehydratedInstance: rehydratedInstance, pendingJobs: pending,
	})
	if err != nil {
		return tenantPlacementRow{}, err
	}
	return tenantPlacementRow{
		generation: generation, cell: cell, cellInstance: cellInstance,
		rehydratedInstance: rehydratedInstance, pendingJobs: pending, deadInstances: dead,
		incarnation: incarnation, retiredIncarnations: retired,
		members: decoded.members, jobHomes: decoded.jobHomes,
	}, nil
}

// liveCellInstances answers the set of cell instances the tenant's live
// work names: every QUEUED or RUNNING run's own owning instance, and the
// owner a GRANTED intent with a null refired_at names. The
// dead-instances trim in writeTenantPlacement never drops an entry in
// this set.
//
// The first arm resolves the owning instance exactly as candidatesQuery
// does, over the same left join: the ledger's cell_instance when the
// intent carries one, else the accepted trigger's own accepted_instance
// payload value. The keep set and the sweep therefore read one
// owning-instance source. A narrower keep set trims away the only
// evidence the sweep can mark such a run with, and the run then stays
// RUNNING for good.
//
// The second arm exists because the first one joins runs. A GRANTED
// intent whose build never started holds no runs row at all, so a busy
// pool would otherwise trim away the evidence the re-fire needs. An
// intent the recorder already re-fired holds a non-null refired_at, so
// it stops protecting its dead instance at once, and so does one past
// maxRefireAttempts, whose retry loop has ended.
//
// The third arm keeps the owning instance of every held or releasing
// fleet lock. The run of a releasing lock is INTERRUPTED, so the first arm
// does not hold it. A trimmed entry is the lock's only release evidence,
// and the lock would then stay releasing for good.
func liveCellInstances(ctx context.Context, tx pgx.Tx, cluster, tenant string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT COALESCE(NULLIF(ri.cell_instance, ''), r.payload->>'accepted_instance')
		FROM varroa.runs r
		LEFT JOIN varroa.run_intents ri ON ri.run_id = r.run_id
		WHERE r.cluster = $1 AND r.tenant = $2 AND r.status IN ('QUEUED', 'RUNNING')
		  AND COALESCE(NULLIF(ri.cell_instance, ''), r.payload->>'accepted_instance', '') <> ''
		UNION
		SELECT DISTINCT ri.cell_instance
		FROM varroa.run_intents ri
		WHERE ri.cluster = $1 AND ri.tenant = $2 AND ri.state = 'GRANTED'
		  AND ri.refired_at IS NULL
		  AND ri.refire_attempts < $3
		  AND ri.cell_instance IS NOT NULL AND ri.cell_instance <> ''
		UNION
		SELECT DISTINCT fl.cell_instance
		FROM varroa.fleet_locks fl
		WHERE fl.cluster = $1 AND fl.tenant = $2 AND fl.state IN ('held', 'releasing')
		  AND fl.cell_instance <> ''
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
//
// On a collision the incoming Derived value wins. An operator entry
// carries none, so the operator's own report clears the recorder's tag.
// The incoming cell wins too, but an incoming entry with no cell keeps
// the stored one, so the operator can still read that cell again.
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
		merged.Derived = d.Derived
		merged.Cell, merged.Namespace = d.Cell, d.Namespace
		if d.Cell == "" {
			merged.Cell, merged.Namespace = existing.Cell, existing.Namespace
		}
		byInstance[d.Instance] = merged
	}
	merged := make([]TenantSyncDeadInstance, 0, len(byInstance))
	for _, d := range byInstance {
		merged = append(merged, d)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].LastSeenAt.After(merged[j].LastSeenAt) })
	return merged
}

// incarnationChanged reports whether two incarnations name two different
// tenant objects. An empty value on either side means unknown, and an
// unknown value never differs: a row written before the incarnation
// column existed, and a caller that names no incarnation, both keep the
// behavior they had before the column.
func incarnationChanged(stored, reported string) bool {
	return stored != "" && reported != "" && stored != reported
}

// deleteTenantJobSchedules deletes every job_schedules row of one
// tenant, inside tx. The announce is its only caller, and it runs the
// delete and the placement write together, so no reader sees the new
// tenant's row beside the old tenant's schedules.
func deleteTenantJobSchedules(ctx context.Context, tx pgx.Tx, cluster, tenant string) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM varroa.job_schedules WHERE cluster = $1 AND tenant = $2
	`, cluster, tenant); err != nil {
		return fmt.Errorf("delete the previous tenant's job schedules: %w", err)
	}
	return nil
}

// keptIncarnation answers the incarnation to store. An announcement that
// names none keeps the stored value, because an empty report proves
// nothing about the tenant's identity.
func keptIncarnation(stored, reported string) string {
	if reported == "" {
		return stored
	}
	return reported
}

// keepAppliedSeed answers the rehydrated instance to store at the same
// generation and the same cell instance. An announcement that reports
// none keeps the stored one.
//
// A cell that applied a tenant's seed holds it until the pod or the
// generation changes, and each of those clears the seed on its own arm.
// An empty report at the same generation and instance therefore never
// proves the seed gone: it is the operator reading its own rehydration
// state one tick late. Storing it would fence every build of the tenant
// until the next announcement raises the value again.
func keepAppliedSeed(stored, reported string) string {
	if reported == "" {
		return stored
	}
	return reported
}

// keepDeferredJobs answers the deferred job list to store beside a seed
// keepAppliedSeed decided. It holds the two halves of one seed report
// together: a report keepAppliedSeed judged stale never clears a job
// either side still calls deferred, so the answer is the union.
//
// The cell clears its own deferred list at the moment it starts a fresh
// bundle for a job it already deferred, and it reports that bundle as not
// applied. An announcement in that window carries no seed and no deferred
// job, so taking its list would leave the job admitted by both the seed
// order and the deferred order, and the job would claim a build number
// against a folder that holds no record for it.
//
// The union never grows without bound: the next announcement that reports
// a seed of its own replaces the whole list.
func keepDeferredJobs(stored, reported []string, reportedSeed, storedSeed string) []string {
	if reportedSeed != "" || storedSeed == "" {
		return reported
	}
	out := append([]string(nil), stored...)
	for _, job := range reported {
		if !containsJobPath(out, job) {
			out = append(out, job)
		}
	}
	return out
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
// cell instances a RUNNING run or a held or releasing fleet lock still
// names, so neither the interrupted-run sweep nor the lock release loses
// the evidence it depends on.
//
// The input can arrive in any order, so the function sorts it newest first
// before it trims. A derived entry joins the end of the merged list, and a
// trim by position would drop it first although it is the newest.
func trimDeadInstances(merged []TenantSyncDeadInstance, keep map[string]bool, limit int) []TenantSyncDeadInstance {
	merged = append([]TenantSyncDeadInstance(nil), merged...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].LastSeenAt.After(merged[j].LastSeenAt) })
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

// placementIdentity holds the identity columns writeTenantPlacement
// stores: the tenant object's incarnation and the incarnations a take-over
// retired.
type placementIdentity struct {
	incarnation string
	retired     []string
}

// writeTenantPlacement upserts cluster/tenant's row with the computed
// generation, seed fields, and identity, inside tx. A NOT NULL jsonb
// column never receives a Go nil: an empty policy becomes "{}", and empty
// pending_jobs and dead_instances lists become "[]".
func writeTenantPlacement(ctx context.Context, tx pgx.Tx, req TenantSyncRequest, generation int64,
	rehydratedInstance string, pendingJobs []string, deadInstances []TenantSyncDeadInstance,
	members []member, jobHomes map[string]int32, identity placementIdentity) error {
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
	membersJSON, err := json.Marshal(nonNilMembers(members))
	if err != nil {
		return fmt.Errorf("encode members: %w", err)
	}
	jobHomesJSON, err := json.Marshal(nonNilJobHomes(jobHomes))
	if err != nil {
		return fmt.Errorf("encode job homes: %w", err)
	}
	retiredJSON, err := json.Marshal(nonNilStrings(identity.retired))
	if err != nil {
		return fmt.Errorf("encode retired incarnations: %w", err)
	}
	// The webhook is stored as it is announced, and a nil value clears
	// the stored one. Its encoding never reaches a log line.
	var webhookJSON *string
	if req.NotificationWebhook != nil {
		encoded, encErr := json.Marshal(req.NotificationWebhook)
		if encErr != nil {
			return fmt.Errorf("encode notification webhook: %w", encErr)
		}
		s := string(encoded)
		webhookJSON = &s
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.tenant_placements (
			cluster, tenant, namespace, cell, generation, cell_instance, policy,
			rehydrated_instance, pending_jobs, cell_alive, cell_alive_at, cell_last_seen_at,
			dead_instances, announced_at, members, job_homes, incarnation, agent_namespace,
			retired_incarnations, notification_webhook
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9::jsonb, $10, $11, $12, $13::jsonb, now(),
			$14::jsonb, $15::jsonb, $16, $17, $18::jsonb, $19::jsonb
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
			announced_at        = now(),
			members             = EXCLUDED.members,
			job_homes           = EXCLUDED.job_homes,
			incarnation         = EXCLUDED.incarnation,
			agent_namespace     = EXCLUDED.agent_namespace,
			retired_incarnations = EXCLUDED.retired_incarnations,
			notification_webhook = EXCLUDED.notification_webhook
	`,
		req.Cluster, req.Tenant, req.Namespace, req.Cell, generation, req.CellInstance, string(policy),
		rehydratedInstance, string(pendingJobsJSON), req.CellAlive, req.CellAliveAt, req.CellLastSeenAt,
		string(deadInstancesJSON), string(membersJSON), string(jobHomesJSON), identity.incarnation,
		req.AgentNamespace, string(retiredJSON), webhookJSON,
	); err != nil {
		return fmt.Errorf("upsert tenant placement: %w", err)
	}
	return nil
}

// resolveAnnouncedMembers answers the member list to store. It mirrors
// the scalar seed rule, per member: a generation raise, or a member whose
// cell instance changed, loses that member's own rehydrated_instance and
// pending_jobs, because the cell holds no seed for the new cycle. A
// member the raise and the instance test both leave alone keeps the seed
// it already reported when this announcement reports none, which is
// keepAppliedSeed's rule for one member.
//
// The generation arm is what a teardown and a new tenant of the same name
// need: the cells can be the same and the pods can still be running, so
// the instance test alone would hand the new tenant the seed state of the
// old one, and its first build would pass the seed order against an
// unseeded folder.
//
// The prior list is resolved through the row's scalar columns, so a
// tenant the operator announces with a member list for the first time
// keeps slot 0's seed state.
func resolveAnnouncedMembers(prior, incoming []member, generationRaised bool) []member {
	if len(incoming) == 0 {
		return nil
	}
	priorRow := placementMembers{members: prior, jobHomes: map[string]int32{}}
	out := make([]member, 0, len(incoming))
	for _, m := range incoming {
		before, ok := memberForCell(priorRow, m.Namespace, m.Cell)
		switch {
		case generationRaised || !ok || before.Instance != m.Instance:
			m.RehydratedInstance = ""
			m.PendingJobs = nil
		default:
			reportedSeed := m.RehydratedInstance
			m.RehydratedInstance = keepAppliedSeed(before.RehydratedInstance, reportedSeed)
			m.PendingJobs = keepDeferredJobs(before.PendingJobs, m.PendingJobs,
				reportedSeed, m.RehydratedInstance)
		}
		out = append(out, m)
	}
	return out
}

// nonNilMembers turns a nil slice into an empty one, so json.Marshal
// writes "[]" instead of "null" into a NOT NULL jsonb column.
func nonNilMembers(m []member) []member {
	if m == nil {
		return []member{}
	}
	return m
}

// nonNilJobHomes is nonNilMembers for the home map.
func nonNilJobHomes(h map[string]int32) map[string]int32 {
	if h == nil {
		return map[string]int32{}
	}
	return h
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
