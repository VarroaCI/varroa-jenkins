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

// ReasonInvalidName vetoes a lock call whose bare name is empty or holds
// a "/". It is a veto, never a server fault, so it rides ReasonCode, and
// Code keeps its own C1 rule.
const ReasonInvalidName = "INVALID_NAME"

// resolveLockTarget resolves one lock call to the tenant that owns the
// lock and to the bare lock name. Name is always bare: the tenant never
// rides inside it, because a controller tenant already holds a "/" of
// its own. TargetTenant names the owning tenant, and an empty value
// names the caller's own tenant, which the gateway stamps as
// CallerTenant. crossesTenant is false exactly when the owner is the
// caller's own tenant. veto is non-nil for a name the call must refuse,
// and the other results are then empty.
func resolveLockTarget(req CrossTenantRequest) (ownerTenant, bareName string, crossesTenant bool, veto *CrossTenantResponse) {
	if req.Name == "" || strings.Contains(req.Name, "/") {
		return "", "", false, &CrossTenantResponse{
			ReasonCode: ReasonInvalidName,
			Message: fmt.Sprintf("Lock name %q is not valid. Supply a bare name with no \"/\", "+
				"and name another tenant in the tenant field.", req.Name),
		}
	}
	ownerTenant = req.TargetTenant
	if ownerTenant == "" {
		ownerTenant = req.CallerTenant
	}
	return ownerTenant, req.Name, ownerTenant != req.CallerTenant, nil
}

// qualifiedLockName answers the stored fleet_locks.name of one lock: the
// owner tenant, a "/", and the bare name. The owner tenant can itself
// hold a "/", so no reader splits this value: owner_tenant carries the
// owner on its own.
func qualifiedLockName(ownerTenant, bareName string) string {
	return ownerTenant + "/" + bareName
}

// handleCrossTenantLockAcquire implements the lock_acquire kind: the
// CheckPlacement call, the name check, the cross-tenant grant, and the
// five-step acquire transaction design.md D11 defines. The reply names
// the owner tenant, because an empty target_tenant leaves the step with
// no other source for it.
func handleCrossTenantLockAcquire(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	if veto, err := checkCrossTenantLockPlacement(ctx, deps, req); err != nil || veto != nil {
		return derefCrossTenantResponse(veto), err
	}

	ownerTenant, bareName, crossesTenant, veto := resolveLockTarget(req)
	if veto != nil {
		return *veto, nil
	}
	if crossesTenant {
		grant, err := checkCrossTenantLockGrant(ctx, deps, req, ownerTenant, bareName)
		if err != nil {
			return CrossTenantResponse{}, err
		}
		if !grant.Allow {
			return CrossTenantResponse{ReasonCode: grant.ReasonCode, Message: grant.Message}, nil
		}
	}

	cellInstance, err := readTenantCellInstance(ctx, deps.Pool, req.Cluster, req.CallerTenant, req.CallerJobPath)
	if err != nil {
		return CrossTenantResponse{}, err
	}

	lease := req.LeaseSeconds
	if lease <= 0 {
		lease = fleetLockDefaultLeaseSeconds
	}

	outcome, err := acquireFleetLock(ctx, deps.Pool, deps.LockMetrics, deps.LockNotifier, fleetLockAcquireParams{
		Name:                qualifiedLockName(ownerTenant, bareName),
		OwnerTenant:         ownerTenant,
		RunID:               req.RunID,
		Cluster:             req.Cluster,
		Tenant:              req.CallerTenant,
		JobPath:             req.CallerJobPath,
		CellInstance:        cellInstance,
		PlacementGeneration: req.PlacementGeneration,
		Lease:               time.Duration(lease) * time.Second,
	})
	if errors.Is(err, errFleetLockRunOfOtherTenant) {
		return CrossTenantResponse{ReasonCode: ReasonCrossTenant, Message: crossTenantMessage +
			" Run " + req.RunID + " already holds or waits on this lock for another tenant."}, nil
	}
	if err != nil {
		return CrossTenantResponse{}, err
	}

	resp := CrossTenantResponse{
		Tenant:     ownerTenant,
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
// or renew whose owner tenant is not the caller's own.
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

// errFleetLockRunOfOtherTenant reports that the lock already holds a row
// under the caller's run id whose tenant column names another tenant.
// A run id is visible in the lock reads, so the acquire must never touch
// such a row: a refresh would extend a foreign lease.
var errFleetLockRunOfOtherTenant = errors.New("fleet lock row belongs to another tenant")

// fleetLockAcquireParams carries one lock_acquire request's identity,
// already resolved to the qualified lock name and its owner tenant.
type fleetLockAcquireParams struct {
	Name                string
	OwnerTenant         string
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
// metrics counts nothing. notifier publishes the lock notifications a
// committed attempt owes, and a nil notifier publishes nothing.
func acquireFleetLock(ctx context.Context, pool *pgxpool.Pool, metrics *FleetLockMetrics, notifier *RunNotifier, p fleetLockAcquireParams) (fleetLockAcquireOutcome, error) {
	outcome, err := acquireFleetLockOnce(ctx, pool, metrics, notifier, p)
	if err == nil {
		return outcome, nil
	}
	if !isFleetLockOneHolderViolation(err) {
		return fleetLockAcquireOutcome{}, err
	}

	outcome, err = acquireFleetLockOnce(ctx, pool, metrics, notifier, p)
	if err == nil {
		return outcome, nil
	}
	if !isFleetLockOneHolderViolation(err) {
		return fleetLockAcquireOutcome{}, err
	}

	return readFleetLockWaitOutcome(ctx, pool, p.Name, p.RunID, p.Lease)
}

// acquireFleetLockOnce runs one attempt of the five-step acquire
// transaction. It collects each affected row's own identity as it goes,
// and it publishes those rows' notifications after the commit alone, so
// no reader learns of a row a rolled-back attempt touched.
func acquireFleetLockOnce(ctx context.Context, pool *pgxpool.Pool, metrics *FleetLockMetrics, notifier *RunNotifier, p fleetLockAcquireParams) (fleetLockAcquireOutcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fleetLockAcquireOutcome{}, fmt.Errorf("begin fleet lock acquire: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()

	moved, _, err := moveInterruptedHoldersToReleasing(ctx, tx, p.Name, now)
	if err != nil {
		return fleetLockAcquireOutcome{}, err
	}

	expiredRows, err := deleteExpiredFleetLockRows(ctx, tx, p.Name, now)
	if err != nil {
		return fleetLockAcquireOutcome{}, err
	}
	metrics.addExpired(ctx, len(expiredRows))

	expiresAt := now.Add(p.Lease)
	inserted, err := insertWaitingFleetLockRow(ctx, tx, p, now, expiresAt)
	if err != nil {
		return fleetLockAcquireOutcome{}, err
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
		publishAcquireNotifications(notifier, moved, expiredRows, inserted, true, p)
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
	publishAcquireNotifications(notifier, moved, expiredRows, inserted, false, p)
	return fleetLockAcquireOutcome{state: FleetLockStateWaiting, holder: holder, position: position, expiresAt: expiresAt}, nil
}

// deleteExpiredFleetLockRows deletes every expired row of one lock name
// that is not releasing, and answers each deleted row's own identity.
// The caller counts the slice it publishes, so the expiry metric and the
// publish set never disagree.
func deleteExpiredFleetLockRows(ctx context.Context, tx pgx.Tx, name string, now time.Time) ([]movedFleetLockRow, error) {
	rows, err := tx.Query(ctx, `
		DELETE FROM varroa.fleet_locks
		WHERE name = $1 AND expires_at < $2 AND state <> 'releasing'
		RETURNING cluster, tenant, job_path, name, run_id
	`, name, now)
	if err != nil {
		return nil, fmt.Errorf("delete expired fleet lock rows: %w", err)
	}
	return scanMovedFleetLockRows(rows, "delete expired fleet lock rows")
}

// insertWaitingFleetLockRow inserts the caller's own row, or refreshes
// its expiry when the row already exists. It reports whether the
// statement inserted the row: the conflict arm's own xmax names an
// existing tuple version, so xmax = 0 tells a genuine insert from a
// refresh. Only a genuine insert changes the row's state.
//
// The conflict arm refreshes only a row of the caller's own tenant. A
// row of another tenant under the same name and run id answers
// errFleetLockRunOfOtherTenant, and the transaction writes nothing.
func insertWaitingFleetLockRow(ctx context.Context, tx pgx.Tx, p fleetLockAcquireParams, now, expiresAt time.Time) (bool, error) {
	var inserted bool
	if err := tx.QueryRow(ctx, `
		INSERT INTO varroa.fleet_locks
			(name, owner_tenant, run_id, cluster, tenant, job_path, state, waiting_since, expires_at, placement_generation)
		VALUES ($1, $2, $3, $4, $5, $6, 'waiting', $7, $8, $9)
		ON CONFLICT (name, run_id) DO UPDATE SET expires_at = EXCLUDED.expires_at
			WHERE varroa.fleet_locks.tenant = EXCLUDED.tenant
		RETURNING (xmax = 0) AS inserted
	`, p.Name, p.OwnerTenant, p.RunID, p.Cluster, p.Tenant, p.JobPath, now, expiresAt, p.PlacementGeneration).Scan(&inserted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, errFleetLockRunOfOtherTenant
		}
		return false, fmt.Errorf("insert waiting fleet lock row: %w", err)
	}
	return inserted, nil
}

// publishAcquireNotifications publishes the lock notifications one
// committed acquire attempt owes: each interruption move the transaction
// made, each expired row it deleted, then the caller's own row. The
// caller's own row is the row the transaction wrote under the caller's
// own primary key, so the two sets of fields name the same values.
func publishAcquireNotifications(notifier *RunNotifier, moved, expired []movedFleetLockRow, inserted, held bool, p fleetLockAcquireParams) {
	for _, row := range moved {
		notifier.Notify(lockNotification(row.cluster, row.tenant, row.jobPath, row.name, row.runID, FleetLockStateReleasing))
	}
	for _, row := range expired {
		notifier.Notify(lockNotification(row.cluster, row.tenant, row.jobPath, row.name, row.runID, FleetLockStateReleased))
	}
	if inserted {
		notifier.Notify(lockNotification(p.Cluster, p.Tenant, p.JobPath, p.Name, p.RunID, FleetLockStateWaiting))
	}
	if held {
		notifier.Notify(lockNotification(p.Cluster, p.Tenant, p.JobPath, p.Name, p.RunID, FleetLockStateHeld))
	}
}

// fleetLockExecer is the subset of pgx.Tx, *pgxpool.Pool, *pgxpool.Conn,
// and maintDB that moveInterruptedHoldersToReleasing needs. The move
// answers each row's own identity, so the interface declares Query
// beside Exec, and every caller of the derived held-to-releasing move
// satisfies it with no wrapper. The acquire transaction and the
// maintenance round therefore share one implementation of the move,
// never two copies of its SQL.
type fleetLockExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
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
// acquire or the next maintenance round reaches the same state.
//
// It answers every moved row's own identity beside the count, so a
// caller publishes one notification per moved row.
func moveInterruptedHoldersToReleasing(ctx context.Context, db fleetLockExecer, name string, now time.Time) ([]movedFleetLockRow, int, error) {
	rows, err := db.Query(ctx, `
		UPDATE varroa.fleet_locks fl
		SET state = 'releasing', releasing_since = $2
		FROM varroa.runs r
		WHERE fl.state = 'held' AND r.run_id::text = fl.run_id
		  AND r.status = 'COMPLETED' AND r.result = 'INTERRUPTED'
		  AND ($1 = '' OR fl.name = $1)
		RETURNING fl.cluster, fl.tenant, fl.job_path, fl.name, fl.run_id
	`, name, now)
	if err != nil {
		return nil, 0, fmt.Errorf("move interrupted holders to releasing: %w", err)
	}
	moved, err := scanMovedFleetLockRows(rows, "move interrupted holders to releasing")
	if err != nil {
		return nil, 0, err
	}
	return moved, len(moved), nil
}

// movedFleetLockRow is one fleet_locks row a statement changed, carried
// up for that row's own lock notification: the row's own cluster,
// tenant, job path, name, and run id, in that order.
type movedFleetLockRow struct {
	cluster, tenant, jobPath, name, runID string
}

// scanMovedFleetLockRows reads rows shaped by a RETURNING clause of
// cluster, tenant, job_path, name, run_id. It drains and closes the
// rows, which frees the caller's one database connection for its next
// statement.
func scanMovedFleetLockRows(rows pgx.Rows, action string) ([]movedFleetLockRow, error) {
	defer rows.Close()
	var out []movedFleetLockRow
	for rows.Next() {
		var row movedFleetLockRow
		if err := rows.Scan(&row.cluster, &row.tenant, &row.jobPath, &row.name, &row.runID); err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return out, nil
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
