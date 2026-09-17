package recordstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fleetLockDefaultLeaseSeconds is the lease design.md D11 fixes when a
// request carries no lease_seconds, or a non-positive one.
const fleetLockDefaultLeaseSeconds = 300

// fleetLockOneHolderConstraint is the partial unique index migration
// 00006 places on varroa.fleet_locks. A violation of it is the
// concurrent-first-acquire race design.md D11 names.
const fleetLockOneHolderConstraint = "fleet_locks_one_holder"

// splitLockName splits name, the wire-level lock name a caller supplied,
// into its owning tenant and bare name. A name holding a "/" already
// names another tenant's namespace explicitly; a bare name with no "/"
// names the caller's own namespace, and callerTenant supplies the
// missing prefix. crossesPrefix is false exactly when the resulting
// owner is the caller's own tenant.
func splitLockName(name, callerTenant string) (ownerTenant, bareName string, crossesPrefix bool) {
	if prefix, rest, ok := strings.Cut(name, "/"); ok {
		return prefix, rest, prefix != callerTenant
	}
	return callerTenant, name, false
}

// handleCrossTenantLockAcquire implements the lock_acquire kind: the
// CheckPlacement call, the cross-prefix grant, and the five-step
// acquire transaction design.md D11 defines.
func handleCrossTenantLockAcquire(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	if veto, err := checkCrossTenantLockPlacement(ctx, deps, req); err != nil || veto != nil {
		return derefCrossTenantResponse(veto), err
	}

	ownerTenant, bareName, crossesPrefix := splitLockName(req.Name, req.CallerTenant)
	if crossesPrefix {
		grant, err := checkCrossTenantLockGrant(ctx, deps, req, ownerTenant, bareName)
		if err != nil {
			return CrossTenantResponse{}, err
		}
		if !grant.Allow {
			return CrossTenantResponse{ReasonCode: grant.ReasonCode, Message: grant.Message}, nil
		}
	}

	cellInstance, err := readTenantCellInstance(ctx, deps.Pool, req.Cluster, req.CallerTenant, req.JobPath)
	if err != nil {
		return CrossTenantResponse{}, err
	}

	lease := req.LeaseSeconds
	if lease <= 0 {
		lease = fleetLockDefaultLeaseSeconds
	}

	outcome, err := acquireFleetLock(ctx, deps.Pool, deps.LockMetrics, fleetLockAcquireParams{
		Name:                ownerTenant + "/" + bareName,
		RunID:               req.RunID,
		Cluster:             req.Cluster,
		Tenant:              req.CallerTenant,
		JobPath:             req.JobPath,
		CellInstance:        cellInstance,
		PlacementGeneration: req.PlacementGeneration,
		Lease:               time.Duration(lease) * time.Second,
	})
	if err != nil {
		return CrossTenantResponse{}, err
	}

	resp := CrossTenantResponse{
		State:      outcome.state,
		Holder:     outcome.holder,
		Position:   outcome.position,
		FenceToken: outcome.fenceToken,
	}
	if !outcome.expiresAt.IsZero() {
		expiresAt := outcome.expiresAt
		resp.ExpiresAt = &expiresAt
	}
	return resp, nil
}

// checkCrossTenantLockPlacement runs GrantEvaluator.CheckPlacement for
// the lock path, shared by acquire and renew. veto is non-nil exactly
// when the caller should answer it as-is.
func checkCrossTenantLockPlacement(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (*CrossTenantResponse, error) {
	result, err := deps.Evaluator.CheckPlacement(ctx, PlacementInput{
		Path:                GrantPathLock,
		Cluster:             req.Cluster,
		Namespace:           req.Namespace,
		Cell:                req.Cell,
		Tenant:              req.CallerTenant,
		PlacementGeneration: req.PlacementGeneration,
	})
	if err != nil {
		return nil, err
	}
	if !result.Allow {
		return &CrossTenantResponse{ReasonCode: result.ReasonCode, Message: result.Message}, nil
	}
	return nil, nil
}

// checkCrossTenantLockGrant runs GrantEvaluator.Check for a lock acquire
// or renew that crosses into another tenant's prefix.
func checkCrossTenantLockGrant(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest, ownerTenant, bareName string) (GrantResult, error) {
	return deps.Evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			Path:                GrantPathLock,
			Cluster:             req.Cluster,
			Namespace:           req.Namespace,
			Cell:                req.Cell,
			Tenant:              req.CallerTenant,
			PlacementGeneration: req.PlacementGeneration,
		},
		CallerTenant: req.CallerTenant,
		TargetTenant: ownerTenant,
		ResourceKind: GrantResourceLock,
		Resource:     bareName,
	})
}

// derefCrossTenantResponse answers *resp, or the zero CrossTenantResponse
// for a nil resp.
func derefCrossTenantResponse(resp *CrossTenantResponse) CrossTenantResponse {
	if resp == nil {
		return CrossTenantResponse{}
	}
	return *resp
}

// readTenantCellInstance answers the cell instance of the member that
// holds jobPath's home slot. It answers "" for a tenant with no row,
// which a caller reaches only when GrantEvaluator already vetoed the
// request: this is a best-effort read for a value fleet_locks records
// for diagnosis, and it never gates the acquire on its own.
//
// The lock's stored instance is the renew's identity gate, so it must
// name the member that actually runs the build, not slot 0.
func readTenantCellInstance(ctx context.Context, pool *pgxpool.Pool, cluster, tenant, jobPath string) (string, error) {
	row, exists, err := readTenantPlacementMembers(ctx, pool, cluster, tenant)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	return memberForJob(row, jobPath).Instance, nil
}

// fleetLockAcquireParams carries one lock_acquire request's identity,
// already resolved to the qualified lock name.
type fleetLockAcquireParams struct {
	Name                string
	RunID               string
	Cluster             string
	Tenant              string
	JobPath             string
	CellInstance        string
	PlacementGeneration int64
	Lease               time.Duration
}

// fleetLockAcquireOutcome is what one acquire attempt answers.
type fleetLockAcquireOutcome struct {
	state      string
	holder     string
	position   int
	fenceToken int64
	expiresAt  time.Time
}

// acquireFleetLock runs the five-step acquire transaction, with the one
// retry design.md D11 names after a unique violation on
// fleet_locks_one_holder. A second violation answers "waiting" with no
// further attempt: the winner now holds a row step 4 can see. metrics
// counts every row the transaction's own expiry delete removes; a nil
// metrics counts nothing.
func acquireFleetLock(ctx context.Context, pool *pgxpool.Pool, metrics *FleetLockMetrics, p fleetLockAcquireParams) (fleetLockAcquireOutcome, error) {
	outcome, err := acquireFleetLockOnce(ctx, pool, metrics, p)
	if err == nil {
		return outcome, nil
	}
	if !isFleetLockOneHolderViolation(err) {
		return fleetLockAcquireOutcome{}, err
	}

	outcome, err = acquireFleetLockOnce(ctx, pool, metrics, p)
	if err == nil {
		return outcome, nil
	}
	if !isFleetLockOneHolderViolation(err) {
		return fleetLockAcquireOutcome{}, err
	}

	return readFleetLockWaitOutcome(ctx, pool, p.Name, p.RunID, p.Lease)
}

// acquireFleetLockOnce runs one attempt of the five-step acquire
// transaction.
func acquireFleetLockOnce(ctx context.Context, pool *pgxpool.Pool, metrics *FleetLockMetrics, p fleetLockAcquireParams) (fleetLockAcquireOutcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fleetLockAcquireOutcome{}, fmt.Errorf("begin fleet lock acquire: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()

	if _, err := moveInterruptedHoldersToReleasing(ctx, tx, p.Name, now); err != nil {
		return fleetLockAcquireOutcome{}, err
	}

	expiredTag, err := tx.Exec(ctx, `
		DELETE FROM varroa.fleet_locks WHERE name = $1 AND expires_at < $2 AND state <> 'releasing'
	`, p.Name, now)
	if err != nil {
		return fleetLockAcquireOutcome{}, fmt.Errorf("delete expired fleet lock rows: %w", err)
	}
	metrics.addExpired(ctx, int(expiredTag.RowsAffected()))

	expiresAt := now.Add(p.Lease)
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.fleet_locks
			(name, run_id, cluster, tenant, job_path, state, waiting_since, expires_at, placement_generation)
		VALUES ($1, $2, $3, $4, $5, 'waiting', $6, $7, $8)
		ON CONFLICT (name, run_id) DO UPDATE SET expires_at = EXCLUDED.expires_at
	`, p.Name, p.RunID, p.Cluster, p.Tenant, p.JobPath, now, expiresAt, p.PlacementGeneration); err != nil {
		return fleetLockAcquireOutcome{}, fmt.Errorf("insert waiting fleet lock row: %w", err)
	}

	var fenceToken int64
	err = tx.QueryRow(ctx, `
		WITH caller AS (
			SELECT waiting_since, run_id FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
		)
		UPDATE varroa.fleet_locks fl
		SET state = 'held', fence_token = nextval('varroa.fleet_lock_fence_seq'), acquired_at = $3, cell_instance = $4
		FROM caller
		WHERE fl.name = $1 AND fl.run_id = $2
		  AND NOT EXISTS (SELECT 1 FROM varroa.fleet_locks h WHERE h.name = $1 AND h.state IN ('held', 'releasing'))
		  AND NOT EXISTS (
		      SELECT 1 FROM varroa.fleet_locks w
		      WHERE w.name = $1 AND w.state = 'waiting' AND w.run_id <> $2
		        AND (w.waiting_since, w.run_id) < (caller.waiting_since, caller.run_id)
		  )
		RETURNING fl.fence_token
	`, p.Name, p.RunID, now, p.CellInstance).Scan(&fenceToken)
	switch {
	case err == nil:
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return fleetLockAcquireOutcome{}, fmt.Errorf("commit fleet lock acquire: %w", commitErr)
		}
		return fleetLockAcquireOutcome{state: FleetLockStateHeld, fenceToken: fenceToken, expiresAt: expiresAt}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Not promoted: fall through to the waiting answer below.
	default:
		return fleetLockAcquireOutcome{}, fmt.Errorf("promote waiting fleet lock row: %w", err)
	}

	holder, position, err := readFleetLockWaitStatus(ctx, tx, p.Name, p.RunID)
	if err != nil {
		return fleetLockAcquireOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return fleetLockAcquireOutcome{}, fmt.Errorf("commit fleet lock wait: %w", err)
	}
	return fleetLockAcquireOutcome{state: FleetLockStateWaiting, holder: holder, position: position, expiresAt: expiresAt}, nil
}

// fleetLockExecer is the subset of pgx.Tx, *pgxpool.Pool, *pgxpool.Conn,
// and maintDB that moveInterruptedHoldersToReleasing needs: one Exec
// call. Every caller of the derived held-to-releasing move satisfies it,
// so the acquire transaction (task 3.8) and the maintenance round (task
// 4.4) share one implementation of design.md D11's move, never two
// copies of its SQL.
type fleetLockExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// moveInterruptedHoldersToReleasing implements the derived held-to-
// releasing move design.md D11 and the fleet-locks capability define: a
// held row whose run is INTERRUPTED becomes releasing, stamped with
// releasing_since. It deletes nothing and promotes no waiter. name
// narrows the move to one lock name, for the acquire transaction's own
// step 1, which must run before any expiry test so an expired lock of an
// interrupted run is never deleted; an empty name moves every lock name
// at once, for the maintenance round's own first step. Because the move
// re-derives wherever the table is read, a recorder that stops between
// the interruption marking and this move loses nothing: the next
// acquire or the next maintenance round reaches the same state. It
// answers how many rows it moved.
func moveInterruptedHoldersToReleasing(ctx context.Context, db fleetLockExecer, name string, now time.Time) (int, error) {
	tag, err := db.Exec(ctx, `
		UPDATE varroa.fleet_locks fl
		SET state = 'releasing', releasing_since = $2
		FROM varroa.runs r
		WHERE fl.state = 'held' AND r.run_id::text = fl.run_id
		  AND r.status = 'COMPLETED' AND r.result = 'INTERRUPTED'
		  AND ($1 = '' OR fl.name = $1)
	`, name, now)
	if err != nil {
		return 0, fmt.Errorf("move interrupted holders to releasing: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// readFleetLockWaitStatus reads the current holder (a "held" or a
// "releasing" row leaves at most one) and the caller's 1-based queue
// position, inside tx so both reads see the same snapshot the acquire
// transaction just wrote.
func readFleetLockWaitStatus(ctx context.Context, tx pgx.Tx, name, runID string) (holder string, position int, err error) {
	err = tx.QueryRow(ctx, `
		SELECT run_id FROM varroa.fleet_locks WHERE name = $1 AND state IN ('held', 'releasing') LIMIT 1
	`, name).Scan(&holder)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", 0, fmt.Errorf("read fleet lock holder: %w", err)
	}

	err = tx.QueryRow(ctx, `
		WITH caller AS (
			SELECT waiting_since, run_id FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
		)
		SELECT count(*) + 1 FROM varroa.fleet_locks w, caller
		WHERE w.name = $1 AND w.state = 'waiting' AND w.run_id <> $2
		  AND (w.waiting_since, w.run_id) < (caller.waiting_since, caller.run_id)
	`, name, runID).Scan(&position)
	if err != nil {
		return holder, 0, fmt.Errorf("read fleet lock position: %w", err)
	}
	return holder, position, nil
}

// readFleetLockWaitOutcome is readFleetLockWaitStatus's standalone form,
// for the second-violation path: the caller's own attempt aborted twice,
// so there is no open transaction left to read inside, and this function
// opens its own short read instead.
func readFleetLockWaitOutcome(ctx context.Context, pool *pgxpool.Pool, name, runID string, lease time.Duration) (fleetLockAcquireOutcome, error) {
	var expiresAt time.Time
	err := pool.QueryRow(ctx, `SELECT expires_at FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`, name, runID).Scan(&expiresAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		expiresAt = time.Now().UTC().Add(lease)
	case err != nil:
		return fleetLockAcquireOutcome{}, fmt.Errorf("read fleet lock row after violation: %w", err)
	}

	var holder string
	var position int
	err = pool.QueryRow(ctx, `
		SELECT run_id FROM varroa.fleet_locks WHERE name = $1 AND state IN ('held', 'releasing') LIMIT 1
	`, name).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		holder = ""
	case err != nil:
		return fleetLockAcquireOutcome{}, fmt.Errorf("read fleet lock holder after violation: %w", err)
	}

	err = pool.QueryRow(ctx, `
		WITH caller AS (
			SELECT waiting_since, run_id FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
		)
		SELECT count(*) + 1 FROM varroa.fleet_locks w, caller
		WHERE w.name = $1 AND w.state = 'waiting' AND w.run_id <> $2
		  AND (w.waiting_since, w.run_id) < (caller.waiting_since, caller.run_id)
	`, name, runID).Scan(&position)
	if err != nil {
		return fleetLockAcquireOutcome{}, fmt.Errorf("read fleet lock position after violation: %w", err)
	}

	return fleetLockAcquireOutcome{state: FleetLockStateWaiting, holder: holder, position: position, expiresAt: expiresAt}, nil
}

// isFleetLockOneHolderViolation reports whether err is the UNIQUE
// violation on fleet_locks_one_holder.
func isFleetLockOneHolderViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == fleetLockOneHolderConstraint
}
