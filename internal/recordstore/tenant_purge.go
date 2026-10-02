package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// Incarnation is the identity of the Team this purge tears down: the
	// object's metadata.uid. A teardown purge that names another
	// incarnation than the stored one leaves the placement row alone,
	// because a newer tenant of the same name already owns it. An empty
	// value means the caller names no incarnation, which keeps the reset.
	Incarnation string `json:"incarnation,omitempty"`
	// TeamDeleted reports that the Team object is under deletion, and not
	// only without spec.pipelines. A teardown for a deleted Team retires
	// its incarnation, because a deleted object's uid never comes back. A
	// Team that only drops spec.pipelines keeps its uid and can add the
	// field again, so its teardown retires nothing. A slot move never sets
	// this field.
	TeamDeleted bool `json:"team_deleted,omitempty"`
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
// queue group "recorder". The returned Responder subscribes again after a denial and stops when
// unsubscribed.
func RegisterTenantPurgeResponder(conn *bus.Conn, pool *pgxpool.Pool, logger *slog.Logger) (*bus.Responder, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := func(data []byte) []byte {
		return handleTenantPurge(logger, pool, data)
	}
	return conn.ServeRequests(bus.TenantPurgeSubject, tenantPurgeQueueGroup, handler)
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
	if req.Cell != "" && req.TeamDeleted {
		// A request that names a cell is a slot move, and the tenant lives
		// on. A move that retires the incarnation would refuse the
		// tenant's own next announcement.
		out, _ := json.Marshal(TenantPurgeResponse{Error: "a slot move purge cannot report a deleted team"})
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), tenantPurgeTimeout)
	defer cancel()

	if err := purgeTenant(ctx, logger, pool, req); err != nil {
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
// where it is. A move needs no incarnation test: the tenant that sends it
// is the tenant the row names.
func purgeTenant(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, req TenantPurgeRequest) error {
	if req.Cell != "" {
		return purgeTenantJobSchedules(ctx, pool, req)
	}
	return purgeTornDownTenant(ctx, logger, pool, req)
}

// purgeTornDownTenant applies a teardown purge. It deletes the tenant's
// job_schedules rows and resets the tenant's placement generation to
// zero. One transaction holds both writes, and one incarnation test
// guards both.
//
// Teardown ends the tenant, but it leaves the tenant_placements row in
// place. A Team created again under the same name starts at generation 1,
// and the announce lowers a stored generation for one reason alone: an
// announcement that names a new incarnation. The reset serves the row
// whose incarnation is unknown, where that take-over cannot apply: it
// lets the first announcement of the new tenant own the row again.
//
// The row itself survives, because the interrupted-run sweep reads its
// cell-liveness fields to mark a run the teardown left behind. The reset
// moves the generation alone: the first announcement of the new tenant
// raises the generation, and that raise clears every seed field on its
// own. Until it arrives, the generation orders refuse every claim.
//
// One teardown request can still be applied after the teardown completes.
// Two teardown paths release the Team finalizer with no successful purge:
// an absent cell, and the tenth failed attempt. The purge rides core NATS
// request-reply, which redelivers nothing, and both ends bound one call
// at ten seconds, so that window is at most ten seconds wide. A tenant of
// the same name created inside it owns the row already, and its own rows
// sit at generation 1, which every generation test of the old tenant
// admits. The incarnation test is what keeps both writes off them: the
// stored incarnation is the new tenant's, and the request names the old
// one.
//
// The test reads the row under FOR UPDATE, which is the lock the announce
// takes, so a purge and an announcement of one tenant never interleave. An
// empty value on either side means unknown, and an unknown value matches:
// a row written before the incarnation column existed still answers the
// purge, and so does a request from a caller that names no incarnation. A
// tenant with no row answers the purge too, because the schedule rows can
// outlive the placement row.
//
// A purge for a deleted Team (TeamDeleted) also adds the incarnation to
// retired_incarnations, in the same transaction. A late announcement of
// that Team is then stale, and it cannot write the old placement back
// after the reset. A purge for a Team that only drops spec.pipelines
// retires nothing: the Team keeps its uid, and it can add the field again
// under that same uid. A retiring purge with no placement row writes no
// row, so a deleted tenant never gets a row of its own.
//
// A purge the test refuses writes nothing at all. The rows the torn-down
// tenant left are already gone: the announcement that took the row over
// deleted every job_schedules row of the name, because at that moment
// every one of them belonged to the tenant before it.
func purgeTornDownTenant(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool,
	req TenantPurgeRequest) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tenant purge transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, exists, err := readPlacementIncarnation(ctx, tx, req)
	if err != nil {
		return err
	}
	if exists && incarnationChanged(stored.incarnation, req.Incarnation) {
		logger.Info("tenant purge keeps the rows a newer tenant owns",
			"cluster", req.Cluster, "tenant", req.Tenant,
			"stored_incarnation", stored.incarnation, "purged_incarnation", req.Incarnation)
		return nil
	}

	if err := purgeTenantJobSchedules(ctx, tx, req); err != nil {
		return err
	}
	if err := resetTenantPlacementGeneration(ctx, tx, req); err != nil {
		return err
	}
	if exists && req.TeamDeleted {
		if err := retireDeletedTeam(ctx, tx, req, stored.retired); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// purgeIdentity is the identity part of a tenant_placements row that a
// teardown purge reads: the stored incarnation and the retired list.
type purgeIdentity struct {
	incarnation string
	retired     []string
}

// retireDeletedTeam adds the incarnation of a deleted Team to the row's
// retired list. The list keeps the take-over's order and bound.
func retireDeletedTeam(ctx context.Context, tx pgx.Tx, req TenantPurgeRequest, retired []string) error {
	next := retireIncarnation(retired, req.Incarnation)
	data, err := json.Marshal(nonNilStrings(next))
	if err != nil {
		return fmt.Errorf("encode retired incarnations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.tenant_placements
		SET retired_incarnations = $3::jsonb
		WHERE cluster = $1 AND tenant = $2
	`, req.Cluster, req.Tenant, string(data)); err != nil {
		return fmt.Errorf("retire the deleted team's incarnation: %w", err)
	}
	return nil
}

// readPlacementIncarnation reads the tenant's stored incarnation and
// retired list, and locks its placement row for the rest of tx. It
// reports whether the row exists.
func readPlacementIncarnation(ctx context.Context, tx pgx.Tx, req TenantPurgeRequest) (purgeIdentity, bool, error) {
	var (
		id         purgeIdentity
		retiredRaw []byte
	)
	err := tx.QueryRow(ctx, `
		SELECT incarnation, retired_incarnations FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2
		FOR UPDATE
	`, req.Cluster, req.Tenant).Scan(&id.incarnation, &retiredRaw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return purgeIdentity{}, false, nil
	case err != nil:
		return purgeIdentity{}, false, fmt.Errorf("read tenant placement incarnation: %w", err)
	}
	if len(retiredRaw) > 0 {
		if err := json.Unmarshal(retiredRaw, &id.retired); err != nil {
			return purgeIdentity{}, false, fmt.Errorf("decode retired incarnations: %w", err)
		}
	}
	return id, true, nil
}

// resetTenantPlacementGeneration sets the tenant's stored placement
// generation to zero.
//
// The generation test matches the job_schedules delete: a teardown retry
// that arrives after the tenant is created again, and announces a higher
// generation, resets nothing. An exact-equality test would be wrong here:
// a lost announcement leaves the stored generation below the one the
// teardown names, and the row would then keep a generation the new tenant
// can never reach.
func resetTenantPlacementGeneration(ctx context.Context, db purgeExecer, req TenantPurgeRequest) error {
	if _, err := db.Exec(ctx, `
		UPDATE varroa.tenant_placements
		SET generation = 0
		WHERE cluster = $1 AND tenant = $2 AND generation <= $3
	`, req.Cluster, req.Tenant, req.PlacementGeneration); err != nil {
		return fmt.Errorf("reset tenant placement generation: %w", err)
	}
	return nil
}

// purgeExecer is the subset of pgx.Tx and *pgxpool.Pool this file's two
// writes need: one Exec call. A slot move sends its delete straight to
// the pool, and a teardown sends both writes to one transaction, so both
// shapes share one copy of each statement.
type purgeExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
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
func purgeTenantJobSchedules(ctx context.Context, db purgeExecer, req TenantPurgeRequest) error {
	if req.Cell != "" {
		if _, err := db.Exec(ctx, `
			DELETE FROM varroa.job_schedules
			WHERE cluster = $1 AND tenant = $2 AND placement_generation <= $3
			  AND namespace = $4 AND cell = $5
		`, req.Cluster, req.Tenant, req.PlacementGeneration, req.CellNamespace, req.Cell); err != nil {
			return fmt.Errorf("delete the cell's job schedule rows: %w", err)
		}
		return nil
	}
	if _, err := db.Exec(ctx, `
		DELETE FROM varroa.job_schedules
		WHERE cluster = $1 AND tenant = $2 AND placement_generation <= $3
	`, req.Cluster, req.Tenant, req.PlacementGeneration); err != nil {
		return fmt.Errorf("delete the tenant's job schedule rows: %w", err)
	}
	return nil
}
