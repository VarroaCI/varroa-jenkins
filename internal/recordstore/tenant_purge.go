package recordstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// tenantPurgeQueueGroup is the NATS queue group the recorder's tenant
// purge responder joins, so more than one recorder replica shares the
// subscription.
const tenantPurgeQueueGroup = "recorder"

// tenantPurgeTimeout bounds one purge's database work, so a stalled
// Postgres connection does not hold the responder goroutine open
// indefinitely.
const tenantPurgeTimeout = 10 * time.Second

// TenantPurgeRequest is the request body of a
// varroa.recordstore.tenant.purge call. The operator's placement pass
// sends this whenever a tenant leaves a cell, carrying the tenant's
// placement generation from before the move or the teardown. C2 owns
// the matching mite-side field name; the two packages communicate over
// the wire only, with no shared Go type.
type TenantPurgeRequest struct {
	Cluster             string `json:"cluster"`
	Tenant              string `json:"tenant"`
	PlacementGeneration int64  `json:"placement_generation"`
	// Cell narrows the delete to one cell's rows. A slot move raises the
	// tenant generation, so a whole-tenant delete would drop every other
	// slot's schedule rows. A teardown leaves this field empty, which
	// keeps the whole-tenant delete.
	Cell string `json:"cell,omitempty"`
	// CellNamespace names that cell's namespace. A job_schedules row
	// carries both columns, and two cells in two namespaces can hold the
	// same name, so the cell name alone names no one cell. A request
	// that names a cell must name its namespace.
	CellNamespace string `json:"cell_namespace,omitempty"`
}

// TenantPurgeResponse is the reply body of a
// varroa.recordstore.tenant.purge call, following the bus.WakeResponse
// precedent. An empty Error field means the delete succeeded, whether
// or not it matched any row: a repeated purge for a tenant with no
// remaining rows still answers success.
type TenantPurgeResponse struct {
	Error string `json:"error,omitempty"`
}

// RegisterTenantPurgeResponder subscribes to bus.TenantPurgeSubject with
// queue group "recorder". The returned Sub stops the subscription when
// unsubscribed.
func RegisterTenantPurgeResponder(conn *bus.Conn, pool *pgxpool.Pool, logger *slog.Logger) (*bus.Sub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := func(data []byte) []byte {
		return handleTenantPurge(logger, pool, data)
	}
	return conn.SubscribeRequest(bus.TenantPurgeSubject, tenantPurgeQueueGroup, handler)
}

// handleTenantPurge decodes a TenantPurgeRequest, deletes the matching
// job_schedules rows, and encodes the answer. A body the decoder
// rejects, or a request naming no cluster or no tenant, answers an
// error: an unscoped delete would otherwise match nothing and silently
// report success, hiding a caller-side defect.
func handleTenantPurge(logger *slog.Logger, pool *pgxpool.Pool, data []byte) []byte {
	var req TenantPurgeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("tenant purge request decode failed", "error", err)
		out, _ := json.Marshal(TenantPurgeResponse{Error: "malformed tenant purge request"})
		return out
	}
	if req.Cluster == "" || req.Tenant == "" {
		out, _ := json.Marshal(TenantPurgeResponse{Error: "tenant purge request needs a cluster and a tenant"})
		return out
	}
	if req.Cell != "" && req.CellNamespace == "" {
		// A cell name with no namespace names no one cell. The delete
		// would drop the rows of a same-named cell in another namespace,
		// so the request answers an error and the caller retries.
		out, _ := json.Marshal(TenantPurgeResponse{Error: "tenant purge request needs the cell's namespace"})
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), tenantPurgeTimeout)
	defer cancel()

	if err := purgeTenant(ctx, pool, req); err != nil {
		logger.Error("tenant purge failed", "cluster", req.Cluster, "tenant", req.Tenant, "error", err)
		out, _ := json.Marshal(TenantPurgeResponse{Error: "tenant purge failed"})
		return out
	}

	out, err := json.Marshal(TenantPurgeResponse{})
	if err != nil {
		logger.Error("tenant purge response encode failed", "error", err)
		out, _ = json.Marshal(TenantPurgeResponse{Error: "tenant purge response encode failed"})
	}
	return out
}

// purgeTenant applies one purge request: it deletes the tenant's
// job_schedules rows, and, for a teardown, resets the tenant's placement
// generation.
//
// A request that names a cell is a slot move. The tenant lives on, so the
// move purges that cell's rows alone and leaves the placement generation
// where it is.
func purgeTenant(ctx context.Context, pool *pgxpool.Pool, req TenantPurgeRequest) error {
	if err := purgeTenantJobSchedules(ctx, pool, req); err != nil {
		return err
	}
	if req.Cell != "" {
		return nil
	}
	return resetTenantPlacementGeneration(ctx, pool, req)
}

// resetTenantPlacementGeneration sets the tenant's stored placement
// generation to zero, for a teardown alone.
//
// Teardown ends the tenant, but it leaves the tenant_placements row in
// place. A Team created again under the same name starts at generation 1,
// and the announce never lowers a stored generation, so the fence would
// answer NOT_READY to every claim of the new tenant for good. The reset
// lets the first announcement of the new tenant own the row again.
//
// The row itself survives, because the interrupted-run sweep reads its
// cell-liveness fields to mark a run the teardown left behind. The reset
// moves the generation alone: the first announcement of the new tenant
// raises the generation, and that raise clears every seed field on its
// own. Until it arrives, the generation orders refuse every claim.
//
// The generation test matches the job_schedules delete: a teardown retry
// that arrives after the tenant is created again, and announces a higher
// generation, resets nothing. An exact-equality test would be wrong here:
// a lost announcement leaves the stored generation below the one the
// teardown names, and the row would then keep a generation the new tenant
// can never reach.
//
// One teardown request can still be applied after the teardown completes.
// Two teardown paths release the Team finalizer with no successful purge:
// an absent cell, and the tenth failed attempt. The purge rides core NATS
// request-reply, which redelivers nothing, and both ends bound one call
// at ten seconds, so that window is at most ten seconds wide. A tenant of
// the same name created inside it reads generation 0 and its claims wait.
//
// The wait usually ends on the next rehydration-state announcement: a new
// tenant's member applied state, its seed, and its deferred job list each
// move once while it rehydrates, and each move is an announce trigger.
// A request that lands after all three have moved waits for the fixed
// ten-minute resync instead. The claims stay on the retryable NOT_READY
// either way, and the seed order holds the same claims while the tenant
// is still rehydrating.
func resetTenantPlacementGeneration(ctx context.Context, pool *pgxpool.Pool, req TenantPurgeRequest) error {
	_, err := pool.Exec(ctx, `
		UPDATE varroa.tenant_placements
		SET generation = 0
		WHERE cluster = $1 AND tenant = $2 AND generation <= $3
	`, req.Cluster, req.Tenant, req.PlacementGeneration)
	return err
}

// purgeTenantJobSchedules deletes every varroa.job_schedules row for
// req's cluster and tenant whose stored placement_generation is at
// most req's. The delete is idempotent: a repeat matches zero rows and
// still succeeds. It never deletes a row at a newer generation, so a
// delayed retry can never remove a row the tenant's new cell already
// reported.
//
// A request that names a cell narrows the delete to that cell's rows, so
// a slot move purges one cell and leaves every other slot's rows in
// place. The narrow delete tests the namespace and the cell together,
// because a row carries both and two namespaces can hold a cell of the
// same name.
func purgeTenantJobSchedules(ctx context.Context, pool *pgxpool.Pool, req TenantPurgeRequest) error {
	if req.Cell != "" {
		_, err := pool.Exec(ctx, `
			DELETE FROM varroa.job_schedules
			WHERE cluster = $1 AND tenant = $2 AND placement_generation <= $3
			  AND namespace = $4 AND cell = $5
		`, req.Cluster, req.Tenant, req.PlacementGeneration, req.CellNamespace, req.Cell)
		return err
	}
	_, err := pool.Exec(ctx, `
		DELETE FROM varroa.job_schedules
		WHERE cluster = $1 AND tenant = $2 AND placement_generation <= $3
	`, req.Cluster, req.Tenant, req.PlacementGeneration)
	return err
}
