package recordstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// stateGranted and stateConsumed are the two values run_intents.state
// holds.
const (
	stateGranted  = "GRANTED"
	stateConsumed = "CONSUMED"
)

// DBTX is the subset of *pgxpool.Pool and pgx.Tx the ledger's SQL helpers
// need. A run.started handler passes its own transaction so a watermark
// raise commits with the run write.
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Ledger is the build ledger and claim responder. It holds the single
// writer pool for the run_intents and job_numbers tables.
type Ledger struct {
	pool  *pgxpool.Pool
	fence Fence

	// concurrencyConn, concurrencyMetrics, and concurrencyActionMetrics
	// wire design D10's concurrency cancel, the fire-and-forget side
	// effect of a granted claim. A nil concurrencyConn (NewLedger's
	// default, until SetConcurrencyCancel is called) starts no
	// concurrency cancel at all.
	concurrencyConn          *bus.Conn
	concurrencyMetrics       *ConcurrencyMetrics
	concurrencyActionMetrics *ActionMetrics
	concurrencyLogger        *slog.Logger

	// vetoMarking and vetoNotifier wire the veto mark. A false
	// vetoMarking (NewLedger's default, until SetVetoMarking is called)
	// marks no vetoed run.
	vetoMarking  bool
	vetoNotifier *RunNotifier
}

// NewLedger returns a Ledger over pool. It uses fence to gate a claim, or
// the allow-all default when fence is nil.
func NewLedger(pool *pgxpool.Pool, fence Fence) *Ledger {
	if fence == nil {
		fence = allowAllFence{}
	}
	return &Ledger{pool: pool, fence: fence}
}

// SetConcurrencyCancel wires design D10's concurrency cancel: conn
// publishes the cancel_run action, metrics counts it under
// recorder_concurrency_cancel_total/_skipped_total, and
// actionMetrics counts it under recorder_actions_published_total.
// cmd/recorder calls this once, after NewLedger. A Ledger this method
// never touches runs no concurrency cancel, which every test that only
// exercises Claim relies on.
func (l *Ledger) SetConcurrencyCancel(conn *bus.Conn, metrics *ConcurrencyMetrics, actionMetrics *ActionMetrics, logger *slog.Logger) {
	l.concurrencyConn = conn
	l.concurrencyMetrics = metrics
	l.concurrencyActionMetrics = actionMetrics
	l.concurrencyLogger = logger
}

// startConcurrencyCancel starts design D10's concurrency cancel in its
// own goroutine, with its own background context, so Claim answers the
// caller before the cancel runs. A Ledger with no wired conn
// (SetConcurrencyCancel never called) starts nothing.
func (l *Ledger) startConcurrencyCancel(req ClaimRequest, createdAt time.Time) {
	if l.concurrencyConn == nil {
		return
	}
	go runConcurrencyCancel(context.Background(), l.pool, l.concurrencyConn, l.concurrencyMetrics,
		l.concurrencyActionMetrics, req, createdAt, l.concurrencyLogger)
}

// Claim answers one varroa.ledger.claim request. It parses the intent
// key and the run id, opens the claim's transaction, checks the Fence
// inside it, then runs the grant in the same transaction. Checking the
// fence inside the transaction, not before it opens, closes the race
// the epic review found: a Fence.Check reading the placement row before
// Claim's transaction begins could observe a generation an announcement
// then bumps before the insert commits, granting a run against a
// generation the placement row had already left behind.
func (l *Ledger) Claim(ctx context.Context, req ClaimRequest) ClaimResponse {
	if _, err := ParseIntentKey(req.IntentKey); err != nil {
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: ReasonInvalidKey, Message: err.Error()}
	}

	runID, createdAt, err := parseRunUUIDv7(req.RunID)
	if err != nil {
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: ReasonInvalidKey, Message: err.Error()}
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	result, err := l.checkFence(ctx, tx, req)
	if err != nil {
		return ClaimResponse{Code: CodeUnavailable, Error: err.Error()}
	}
	if !result.Allow {
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: result.ReasonCode, Message: result.Message,
			HomeSlot: result.HomeSlot}
	}

	resp, err := claimTx(ctx, tx, req, runID, createdAt)
	if err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}
	}

	if err := tx.Commit(ctx); err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}
	}
	if resp.Decision == DecisionGranted {
		l.startConcurrencyCancel(req, createdAt)
	}
	return resp
}

// checkFence runs the fence check for one claim. A Fence that also
// implements TxFence is checked through CheckTx, inside tx, so its
// placement read locks the row for the rest of the claim transaction.
// A plain Fence (allowAllFence, and every test fake) is checked through
// Check, against ctx alone, because it holds no row lock to coordinate.
func (l *Ledger) checkFence(ctx context.Context, tx pgx.Tx, req ClaimRequest) (FenceResult, error) {
	if txFence, ok := l.fence.(TxFence); ok {
		return txFence.CheckTx(ctx, tx, req)
	}
	return l.fence.Check(ctx, req)
}

// claimTx runs the fast-path insert, then falls back to the existing-key
// decision table when the key already exists.
func claimTx(ctx context.Context, tx pgx.Tx, req ClaimRequest, runID uuid.UUID, createdAt time.Time) (ClaimResponse, error) {
	var insertedRunID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO varroa.run_intents
			(intent_key, run_id, created_at, cluster, namespace, tenant, job_path,
			 cell, cell_instance, placement_generation, state, actor, cause, params_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'GRANTED', $11, $12, $13)
		ON CONFLICT (intent_key) DO NOTHING
		RETURNING run_id
	`,
		req.IntentKey, runID, createdAt, req.Cluster, req.Namespace, req.Tenant, req.JobPath,
		nullableString(req.Cell), nullableString(req.CellInstance), req.PlacementGeneration,
		nullableString(req.Actor), nullableString(req.Cause), nullableString(req.ParamsDigest),
	).Scan(&insertedRunID)

	switch {
	case err == nil:
		return grantNewIntent(ctx, tx, req, runID)
	case errors.Is(err, pgx.ErrNoRows):
		return existingKeyDecision(ctx, tx, req, runID, createdAt)
	default:
		return ClaimResponse{}, fmt.Errorf("insert intent: %w", err)
	}
}

// grantNewIntent reserves a build number for a freshly inserted intent
// and writes it back into the row.
func grantNewIntent(ctx context.Context, tx pgx.Tx, req ClaimRequest, runID uuid.UUID) (ClaimResponse, error) {
	buildNumber, err := reserveBuildNumber(ctx, tx, req.Cluster, req.Tenant, req.JobPath, req.JenkinsNextBuildNumber)
	if err != nil {
		return ClaimResponse{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.run_intents SET build_number = $1 WHERE intent_key = $2
	`, buildNumber, req.IntentKey); err != nil {
		return ClaimResponse{}, fmt.Errorf("write reserved build number: %w", err)
	}
	return ClaimResponse{Decision: DecisionGranted, RunID: runID.String(), BuildNumber: buildNumber}, nil
}

// storedIntent is the row existingKeyDecision reads under FOR UPDATE.
type storedIntent struct {
	runID               uuid.UUID
	createdAt           time.Time
	cellInstance        *string
	placementGeneration int64
	state               string
	buildNumber         *int64
}

// existingKeyDecision applies D2's decision table, in order, to the row
// a repeated intent_key already holds.
func existingKeyDecision(ctx context.Context, tx pgx.Tx, req ClaimRequest, reqRunID uuid.UUID, reqCreatedAt time.Time) (ClaimResponse, error) {
	var row storedIntent
	err := tx.QueryRow(ctx, `
		SELECT run_id, created_at, cell_instance, placement_generation, state, build_number
		FROM varroa.run_intents
		WHERE intent_key = $1
		FOR UPDATE
	`, req.IntentKey).Scan(&row.runID, &row.createdAt, &row.cellInstance, &row.placementGeneration, &row.state, &row.buildNumber)
	if err != nil {
		return ClaimResponse{}, fmt.Errorf("read existing intent: %w", err)
	}

	// Row 1: a lower generation is fenced, no matter which run_id the
	// request carries.
	if req.PlacementGeneration < row.placementGeneration {
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: ReasonFenced, RunID: row.runID.String()}, nil
	}

	// Row 2: the stored run_id equals the request's run_id. A started run
	// answers duplicate and writes nothing: only a queue item claims, and a
	// queue item for a run that already started is a redelivered command.
	// Any other repeat answers granted; an unstarted intent moves to a new
	// instance, and that move can refresh a stale build number.
	if row.runID == reqRunID {
		started, err := intentStarted(ctx, tx, row)
		if err != nil {
			return ClaimResponse{}, err
		}
		if started {
			return ClaimResponse{Decision: DecisionDuplicate, RunID: row.runID.String()}, nil
		}
		return reanswerSameRun(ctx, tx, req, row)
	}

	// Row 3: a higher generation on an unstarted GRANTED intent transfers
	// ownership.
	if req.PlacementGeneration > row.placementGeneration && row.state == stateGranted {
		exists, err := runsRowExists(ctx, tx, row.runID)
		if err != nil {
			return ClaimResponse{}, err
		}
		if !exists {
			return transferIntent(ctx, tx, req, reqRunID, reqCreatedAt)
		}
	}

	// Row 4: every other case is a duplicate.
	if err := markDuplicateClaim(ctx, tx, reqRunID, reqCreatedAt); err != nil {
		return ClaimResponse{}, err
	}
	return ClaimResponse{Decision: DecisionDuplicate, RunID: row.runID.String()}, nil
}

// markDuplicateClaim marks the requesting run's accepted-trigger row
// terminal, in the same transaction as the decision that made it
// terminal.
//
// The requesting run never builds: the plugin cancels the queue item for
// a duplicate decision. Its accepted-trigger row is QUEUED and records
// no build, and the interrupted sweep leaves such a row alone while its
// accepting instance is alive. Without this write a healthy cell holds
// the row at QUEUED for good, and it reads as a build still waiting.
//
// The guard is the row's own state: only a QUEUED row that records no
// build is marked, so a real run of that identifier is never touched.
func markDuplicateClaim(ctx context.Context, tx pgx.Tx, runID uuid.UUID, createdAt time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.runs
		SET status = 'DUPLICATE', ended_at = COALESCE(ended_at, now())
		WHERE created_at = $1 AND run_id = $2 AND status = 'QUEUED' AND `+noBuildRecordedSQL+`
	`, createdAt, runID); err != nil {
		return fmt.Errorf("mark duplicate claim for run %s: %w", runID, err)
	}
	return nil
}

// intentStarted reports whether the run a stored intent names has
// started: run.started consumed the intent, or a runs row records the
// build. A repeat claim for such a run answers duplicate, and the caller
// must not call markDuplicateClaim, because the row it would test is the
// real run's own row.
func intentStarted(ctx context.Context, tx pgx.Tx, row storedIntent) (bool, error) {
	if row.state == stateConsumed {
		return true, nil
	}
	return runsRowExists(ctx, tx, row.runID)
}

// reanswerSameRun grants a repeat claim for the run_id the row already
// holds, for a run that has not started. It moves the ownership columns
// only when the caller's cell instance differs and the intent is still
// GRANTED. Inside that move it also refreshes the build
// number when the new instance's counter already passed the stored
// number, because a cell refuses a number below its own counter; the
// old number stays burned, and a zero counter never refreshes. A request
// that names no cell instance never refreshes either: the ingest path
// tells a superseded start from a real one by the intent's owner, so a
// refreshed intent must always name one. A repeat from the same instance
// changes nothing: a pod keeps its Jenkins home across a container
// restart, so its counter passes the stored number only by running the
// build, and a refresh there would start that build a second time.
func reanswerSameRun(ctx context.Context, tx pgx.Tx, req ClaimRequest, row storedIntent) (ClaimResponse, error) {
	buildNumber := derefInt64(row.buildNumber)
	sameInstance := row.cellInstance != nil && *row.cellInstance == req.CellInstance
	if !sameInstance && row.state == stateGranted {
		if _, err := tx.Exec(ctx, `
			UPDATE varroa.run_intents
			SET cell = $1, cell_instance = $2, placement_generation = $3
			WHERE run_id = $4
		`, nullableString(req.Cell), nullableString(req.CellInstance), req.PlacementGeneration, row.runID); err != nil {
			return ClaimResponse{}, fmt.Errorf("move ownership: %w", err)
		}
		if req.CellInstance != "" && req.JenkinsNextBuildNumber > 0 && buildNumber < req.JenkinsNextBuildNumber {
			fresh, err := reserveBuildNumber(ctx, tx, req.Cluster, req.Tenant, req.JobPath, req.JenkinsNextBuildNumber)
			if err != nil {
				return ClaimResponse{}, err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE varroa.run_intents SET build_number = $1 WHERE run_id = $2
			`, fresh, row.runID); err != nil {
				return ClaimResponse{}, fmt.Errorf("refresh stale build number: %w", err)
			}
			buildNumber = fresh
		}
	}
	return ClaimResponse{Decision: DecisionGranted, RunID: row.runID.String(), BuildNumber: buildNumber}, nil
}

// transferIntent moves an intent to a new run_id, reserving a new build
// number for it.
func transferIntent(ctx context.Context, tx pgx.Tx, req ClaimRequest, newRunID uuid.UUID, newCreatedAt time.Time) (ClaimResponse, error) {
	buildNumber, err := reserveBuildNumber(ctx, tx, req.Cluster, req.Tenant, req.JobPath, req.JenkinsNextBuildNumber)
	if err != nil {
		return ClaimResponse{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.run_intents
		SET run_id = $1, created_at = $2, cell = $3, cell_instance = $4,
		    placement_generation = $5, build_number = $6
		WHERE intent_key = $7
	`, newRunID, newCreatedAt, nullableString(req.Cell), nullableString(req.CellInstance),
		req.PlacementGeneration, buildNumber, req.IntentKey); err != nil {
		return ClaimResponse{}, fmt.Errorf("transfer intent: %w", err)
	}
	return ClaimResponse{Decision: DecisionGranted, RunID: newRunID.String(), BuildNumber: buildNumber}, nil
}

// runsRowExists reports whether a runs row already records a build for
// runID. Its absence proves no out-of-order run.completed recorded the
// build. A row that records no build — a TRIGGER_FAILED row, or an
// accepted trigger no real event has yet followed — never counts here:
// it would otherwise keep the old owner on a repeat claim, and the real
// completion would then read as foreign.
func runsRowExists(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM varroa.runs WHERE run_id = $1 AND `+recordsBuildSQL+`)
	`, runID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check runs row: %w", err)
	}
	return exists, nil
}

// parseRunUUIDv7 validates s as a UUIDv7 string and derives the
// millisecond timestamp embedded in it. The recorder never uses now()
// for created_at.
func parseRunUUIDv7(s string) (uuid.UUID, time.Time, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, time.Time{}, fmt.Errorf("run_id is not a valid uuid: %w", err)
	}
	if id.Version() != 7 {
		return uuid.UUID{}, time.Time{}, fmt.Errorf("run_id is not a UUIDv7, got version %s", id.Version())
	}
	sec, nsec := id.Time().UnixTime()
	return id, time.Unix(sec, nsec).UTC(), nil
}

// nullableString returns a pointer for a non-empty string, and nil for
// an empty one, so an unset field stores SQL NULL rather than "".
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefInt64 returns 0 for a nil pointer.
func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// classifyDBError maps a database failure to the ClaimResponse code D2
// pins: CodeUnavailable when Postgres does not answer, CodeInternal for
// any other server fault.
func classifyDBError(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return CodeUnavailable
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return CodeUnavailable
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return CodeInternal
	}
	return CodeUnavailable
}
