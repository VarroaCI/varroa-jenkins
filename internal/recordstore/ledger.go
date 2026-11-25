package recordstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
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

	// concurrencyWG counts each running concurrency cancel. A test waits
	// on it before it reads the metrics, which the cancel adds after its
	// publish. concurrencyGate, when a test sets it, holds each cancel
	// until the test closes it. Production code leaves it nil.
	concurrencyWG   sync.WaitGroup
	concurrencyGate chan struct{}

	// vetoMarking and vetoNotifier wire the veto mark. A false
	// vetoMarking (NewLedger's default, until SetVetoMarking is called)
	// marks no vetoed run. vetoActivityPub publishes the
	// build.claim_vetoed row after the commit. A nil value publishes
	// nothing.
	vetoMarking     bool
	vetoNotifier    *RunNotifier
	vetoActivityPub *activity.Publisher

	// notifier publishes a wait notification after each claim that
	// writes or deletes a wait row, and a run notification after each
	// claim that marks a runs row DUPLICATE. A nil notifier publishes
	// nothing.
	notifier *RunNotifier

	// hold wires the grant hold. A nil hold (NewLedger's default, until
	// SetGrantHold is called) holds no repeat claim.
	hold *grantHold

	// moveConn publishes the owner move signal after each claim that
	// moves the owner of an existing intent. A nil moveConn (NewLedger's
	// default, until SetOwnerMoves is called) publishes none.
	moveConn   *bus.Conn
	moveLogger *slog.Logger
}

// claimEffects is what one claim transaction leaves for its caller to
// publish after the commit.
type claimEffects struct {
	// marked is the run notification of a runs row that the claim marked
	// DUPLICATE, or nil.
	marked *RunNotification
	// moved names the run whose stored owner the claim replaced, and
	// movedCluster the cluster that the run's ledger row names. Both are
	// empty when the claim moved no owner.
	moved, movedCluster string
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

// SetRunNotifier wires the claim notifications on notifier. After a claim
// commits a change to its run_waits row, the ledger publishes a kind wait
// notification. After a claim or a start mark commits a DUPLICATE mark of
// a runs row, the ledger publishes that row's run notification.
// cmd/recorder calls this once, after NewLedger. A Ledger this method
// never touches publishes neither.
func (l *Ledger) SetRunNotifier(notifier *RunNotifier) {
	l.notifier = notifier
}

// notifyWait publishes the wait notification of one committed claim. A
// claim that changed no displayed value publishes nothing.
func (l *Ledger) notifyWait(req ClaimRequest, runID uuid.UUID, change waitChange) {
	switch change {
	case waitWritten:
		l.notifier.Notify(waitNotification(req, runID.String(), WaitStateWaiting))
	case waitCleared:
		l.notifier.Notify(waitNotification(req, runID.String(), WaitStateCleared))
	}
}

// notifyDuplicate publishes the run notification of a DUPLICATE mark that
// one committed claim or start mark wrote. A nil mark publishes nothing.
func (l *Ledger) notifyDuplicate(marked *RunNotification) {
	if marked != nil {
		l.notifier.Notify(*marked)
	}
}

// SetOwnerMoves wires the owner move signal on conn. After a claim
// commits a move of an existing intent's owner, the ledger publishes one
// signal for that run. Only the gateway replica that relays the claim sees
// its grant, so every other replica learns the move from the signal.
// cmd/recorder calls this once, after NewLedger. A Ledger this method
// never touches publishes no signal.
func (l *Ledger) SetOwnerMoves(conn *bus.Conn, logger *slog.Logger) {
	l.moveConn = conn
	l.moveLogger = logger
}

// publishOwnerMove publishes the owner move signal of one committed
// claim. A claim that moved no owner publishes nothing, and a failed
// publish is logged only: the gateway owner cache lifetimes still bound a
// replica that misses the signal.
func (l *Ledger) publishOwnerMove(effects claimEffects) {
	if effects.moved == "" || l.moveConn == nil {
		return
	}
	if err := PublishOwnerMove(l.moveConn, effects.movedCluster, effects.moved); err != nil {
		logger := l.moveLogger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("publish owner move failed", "run_id", effects.moved, "error", err)
	}
}

// startConcurrencyCancel starts design D10's concurrency cancel in its
// own goroutine, with its own background context, so Claim answers the
// caller before the cancel runs. A Ledger with no wired conn
// (SetConcurrencyCancel never called) starts nothing.
func (l *Ledger) startConcurrencyCancel(req ClaimRequest, createdAt time.Time) {
	if l.concurrencyConn == nil {
		return
	}
	gate := l.concurrencyGate
	l.concurrencyWG.Add(1)
	go func() {
		defer l.concurrencyWG.Done()
		if gate != nil {
			<-gate
		}
		runConcurrencyCancel(context.Background(), l.pool, l.concurrencyConn, l.concurrencyMetrics,
			l.concurrencyActionMetrics, req, createdAt, l.concurrencyLogger)
	}()
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
	if req.StartMark {
		return l.MarkStart(ctx, req, runID)
	}

	resp, needRead := l.claimAttempt(ctx, req, runID, createdAt, nil)
	if needRead {
		// The grant hold reads the ingest consumer only here, between two
		// transactions, so a slow NATS read holds no row lock.
		reading := l.hold.readConsumer(ctx)
		resp, _ = l.claimAttempt(ctx, req, runID, createdAt, &reading)
	}
	if resp.Decision == DecisionGranted {
		l.startConcurrencyCancel(req, createdAt)
	}
	return resp
}

// claimAttempt runs one claim transaction. reading is the grant hold's
// consumer reading, or nil before Claim read it. It answers true, and
// commits nothing, when the hold needs that reading to decide.
func (l *Ledger) claimAttempt(ctx context.Context, req ClaimRequest, runID uuid.UUID, createdAt time.Time,
	reading *consumerReading) (ClaimResponse, bool) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}, false
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	result, err := l.checkFence(ctx, tx, req)
	if err != nil {
		return ClaimResponse{Code: CodeUnavailable, Error: err.Error()}, false
	}
	if !result.Allow {
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: result.ReasonCode, Message: result.Message,
			HomeSlot: result.HomeSlot}, false
	}

	verdict, err := checkCaps(ctx, tx, req, runID)
	if err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}, false
	}
	if verdict.Blocked {
		// A capped veto commits the wait row and no intent. The row is
		// the only record the wait leaves, so the run stays claimable.
		if err := tx.Commit(ctx); err != nil {
			return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}, false
		}
		l.notifyWait(req, runID, verdict.Change)
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: ReasonCapped, Message: verdict.Message,
			RunID: runID.String()}, false
	}

	resp, effects, err := claimTx(ctx, tx, l.hold, reading, req, runID, createdAt, result.Incarnation)
	if errors.Is(err, errConsumerReadNeeded) {
		return ClaimResponse{}, true
	}
	if err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}, false
	}

	if err := tx.Commit(ctx); err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}, false
	}
	l.notifyWait(req, runID, verdict.Change)
	l.notifyDuplicate(effects.marked)
	l.publishOwnerMove(effects)
	return resp, false
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
// decision table when the key already exists. A nil hold holds no repeat
// claim. reading is the hold's consumer reading, or nil when Claim has not
// read it yet. The second answer holds what the caller publishes after
// the commit: the run notification of a runs row that the claim marked
// DUPLICATE, and the run whose stored owner the claim moved.
func claimTx(ctx context.Context, tx pgx.Tx, hold *grantHold, reading *consumerReading, req ClaimRequest,
	runID uuid.UUID, createdAt time.Time, incarnation string) (ClaimResponse, claimEffects, error) {
	var insertedRunID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO varroa.run_intents
			(intent_key, run_id, created_at, cluster, namespace, tenant, job_path,
			 cell, cell_instance, placement_generation, state, actor, cause, params_digest, granted_at,
			 incarnation)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'GRANTED', $11, $12, $13, now(), $14)
		ON CONFLICT (intent_key) DO NOTHING
		RETURNING run_id
	`,
		req.IntentKey, runID, createdAt, req.Cluster, req.Namespace, req.Tenant, req.JobPath,
		nullableString(req.Cell), nullableString(req.CellInstance), req.PlacementGeneration,
		nullableString(req.Actor), nullableString(req.Cause), nullableString(req.ParamsDigest), incarnation,
	).Scan(&insertedRunID)

	switch {
	case err == nil:
		resp, err := grantNewIntent(ctx, tx, req, runID)
		return resp, claimEffects{}, err
	case errors.Is(err, pgx.ErrNoRows):
		return existingKeyDecision(ctx, tx, hold, reading, req, runID, createdAt, incarnation)
	default:
		return ClaimResponse{}, claimEffects{}, fmt.Errorf("insert intent: %w", err)
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
	// grantedAt is the time of the last grant to cellInstance. A row
	// written before the column existed holds nil.
	grantedAt *time.Time
	// startMarkedAt is the time cellInstance marked the start of the
	// build. A nil value proves that the build did not start.
	startMarkedAt *time.Time
	// dbNow is the database clock at the read. The grant hold compares
	// grantedAt against it, so both values come from one clock.
	dbNow time.Time
}

// existingKeyDecision applies D2's decision table, in order, to the row
// a repeated intent_key already holds. The second answer holds the run
// notification of the requesting run's row when Row 4 marked it
// DUPLICATE, and the stored run when Row 2 or Row 3 moved its owner.
func existingKeyDecision(ctx context.Context, tx pgx.Tx, hold *grantHold, reading *consumerReading, req ClaimRequest,
	reqRunID uuid.UUID, reqCreatedAt time.Time, incarnation string) (ClaimResponse, claimEffects, error) {
	var row storedIntent
	err := tx.QueryRow(ctx, `
		SELECT run_id, created_at, cell_instance, placement_generation, state, build_number, granted_at,
		       start_marked_at, now()
		FROM varroa.run_intents
		WHERE intent_key = $1
		FOR UPDATE
	`, req.IntentKey).Scan(&row.runID, &row.createdAt, &row.cellInstance, &row.placementGeneration, &row.state,
		&row.buildNumber, &row.grantedAt, &row.startMarkedAt, &row.dbNow)
	if err != nil {
		return ClaimResponse{}, claimEffects{}, fmt.Errorf("read existing intent: %w", err)
	}

	// Row 1: a lower generation is fenced, no matter which run_id the
	// request carries.
	if req.PlacementGeneration < row.placementGeneration {
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: ReasonFenced, RunID: row.runID.String()}, claimEffects{}, nil
	}

	// Row 2: the stored run_id equals the request's run_id. A started run
	// answers duplicate and writes nothing: only a queue item claims, and a
	// queue item for a run that already started is a redelivered command.
	// A repeat from another instance waits while the stored grant can
	// still be building. A repeat from another instance for a marked
	// intent answers duplicate at once: the owner can have started the
	// build, and its start event can be lost with its pod. The replacement
	// pod of a controller with no cell role is the one exception, when its
	// job counter proves that the build did not start (adoptMarkedIntent).
	// Any other repeat answers granted; an unstarted intent moves to a new
	// instance, and that move can refresh a stale build number.
	if row.runID == reqRunID {
		started, err := intentStarted(ctx, tx, row)
		if err != nil {
			return ClaimResponse{}, claimEffects{}, err
		}
		if started {
			return ClaimResponse{Decision: DecisionDuplicate, RunID: row.runID.String()}, claimEffects{}, nil
		}
		if row.startMarkedAt != nil && storedOwner(row) != req.CellInstance {
			return adoptMarkedIntent(ctx, tx, req, row, incarnation)
		}
		held, message, err := hold.holds(ctx, tx, req, row, reading)
		if err != nil {
			return ClaimResponse{}, claimEffects{}, err
		}
		if held {
			return ClaimResponse{Decision: DecisionVetoed, ReasonCode: ReasonNotReady, Message: message,
				RunID: row.runID.String()}, claimEffects{}, nil
		}
		return reanswerSameRun(ctx, tx, req, row, incarnation)
	}

	// Row 3: a higher generation on an unstarted GRANTED intent transfers
	// ownership. A marked intent never transfers, because its owner can
	// have started the build.
	if req.PlacementGeneration > row.placementGeneration && row.state == stateGranted && row.startMarkedAt == nil {
		exists, err := runsRowExists(ctx, tx, row.runID)
		if err != nil {
			return ClaimResponse{}, claimEffects{}, err
		}
		if !exists {
			resp, cluster, err := transferIntent(ctx, tx, req, reqRunID, reqCreatedAt, incarnation)
			// The stored run loses its intent row, so its old owner no
			// longer holds it.
			return resp, claimEffects{moved: row.runID.String(), movedCluster: cluster}, err
		}
	}

	// Row 4: every other case is a duplicate.
	marked, err := markDuplicateClaim(ctx, tx, reqRunID, reqCreatedAt)
	if err != nil {
		return ClaimResponse{}, claimEffects{}, err
	}
	return ClaimResponse{Decision: DecisionDuplicate, RunID: row.runID.String()}, claimEffects{marked: marked}, nil
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
//
// It answers the run notification of the row it marked, built from that
// row's own identity, so the caller publishes it after the commit. It
// answers nil when the guard matched no row.
func markDuplicateClaim(ctx context.Context, tx pgx.Tx, runID uuid.UUID, createdAt time.Time) (*RunNotification, error) {
	var cluster, tenant, jobPath string
	err := tx.QueryRow(ctx, `
		UPDATE varroa.runs
		SET status = 'DUPLICATE', ended_at = COALESCE(ended_at, now())
		WHERE created_at = $1 AND run_id = $2 AND status = 'QUEUED' AND `+noBuildRecordedSQL+`
		RETURNING cluster, tenant, job_path
	`, createdAt, runID).Scan(&cluster, &tenant, &jobPath)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("mark duplicate claim for run %s: %w", runID, err)
	}
	marked := duplicateNotification(cluster, tenant, jobPath, runID.String())
	return &marked, nil
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
// refreshed intent must always name one. A plain repeat from the same
// instance changes nothing: a pod keeps its Jenkins home across a
// container restart, so its counter passes the stored number only by
// running the build, and a refresh there would start that build a
// second time.
//
// Two refreshes apply from any instance, the same one included. A null
// stored number means a run.started took the grant, so the run holds no
// usable number. A number_conflict that names the stored number means
// the cell refused to apply it; the cell holds no build under it, and
// the number is below the watermark, so the fresh number is above it.
//
// A move names the run in the second answer. Only the gateway replica that
// relays this claim sees the grant, so the caller signals the move to the
// other replicas after the commit.
func reanswerSameRun(ctx context.Context, tx pgx.Tx, req ClaimRequest, row storedIntent, incarnation string) (ClaimResponse, claimEffects, error) {
	buildNumber := derefInt64(row.buildNumber)
	if row.state != stateGranted {
		return ClaimResponse{Decision: DecisionGranted, RunID: row.runID.String(), BuildNumber: buildNumber}, claimEffects{}, nil
	}
	var effects claimEffects
	sameInstance := row.cellInstance != nil && *row.cellInstance == req.CellInstance
	refresh := false
	if !sameInstance {
		// The move stamps the claiming incarnation, because the claiming
		// instance runs the build. The run then belongs to that incarnation.
		var cluster string
		if err := tx.QueryRow(ctx, `
			UPDATE varroa.run_intents
			SET cell = $1, cell_instance = $2, placement_generation = $3, granted_at = now(), incarnation = $5
			WHERE run_id = $4
			RETURNING cluster
		`, nullableString(req.Cell), nullableString(req.CellInstance), req.PlacementGeneration, row.runID, incarnation).
			Scan(&cluster); err != nil {
			return ClaimResponse{}, claimEffects{}, fmt.Errorf("move ownership: %w", err)
		}
		effects = claimEffects{moved: row.runID.String(), movedCluster: cluster}
		refresh = req.CellInstance != "" && req.JenkinsNextBuildNumber > 0 && buildNumber < req.JenkinsNextBuildNumber
	}
	if !refresh {
		var err error
		if refresh, err = namedOrNullRefresh(ctx, tx, req, row); err != nil {
			return ClaimResponse{}, claimEffects{}, err
		}
	}
	if refresh {
		fresh, err := reserveBuildNumber(ctx, tx, req.Cluster, req.Tenant, req.JobPath, req.JenkinsNextBuildNumber)
		if err != nil {
			return ClaimResponse{}, claimEffects{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE varroa.run_intents SET build_number = $1 WHERE run_id = $2
		`, fresh, row.runID); err != nil {
			return ClaimResponse{}, claimEffects{}, fmt.Errorf("refresh stale build number: %w", err)
		}
		buildNumber = fresh
	}
	return ClaimResponse{Decision: DecisionGranted, RunID: row.runID.String(), BuildNumber: buildNumber}, effects, nil
}

// namedOrNullRefresh reports whether a GRANTED intent needs a fresh
// number from any instance: its stored number is null, or the request's
// number_conflict names the stored number and that number is below the
// job's watermark.
func namedOrNullRefresh(ctx context.Context, tx pgx.Tx, req ClaimRequest, row storedIntent) (bool, error) {
	if row.buildNumber == nil {
		return true, nil
	}
	if req.NumberConflict <= 0 || *row.buildNumber != req.NumberConflict {
		return false, nil
	}
	next, err := NextBuildNumber(ctx, tx, req.Cluster, req.Tenant, req.JobPath)
	if err != nil {
		return false, err
	}
	return *row.buildNumber < next, nil
}

// transferIntent moves an intent to a new run_id, reserving a new build
// number for it. It also answers the cluster that the intent row names,
// which the caller signals the old run's move on.
func transferIntent(ctx context.Context, tx pgx.Tx, req ClaimRequest, newRunID uuid.UUID, newCreatedAt time.Time, incarnation string) (ClaimResponse, string, error) {
	buildNumber, err := reserveBuildNumber(ctx, tx, req.Cluster, req.Tenant, req.JobPath, req.JenkinsNextBuildNumber)
	if err != nil {
		return ClaimResponse{}, "", err
	}
	var cluster string
	if err := tx.QueryRow(ctx, `
		UPDATE varroa.run_intents
		SET run_id = $1, created_at = $2, cell = $3, cell_instance = $4,
		    placement_generation = $5, build_number = $6, granted_at = now(), start_marked_at = NULL,
		    incarnation = $8
		WHERE intent_key = $7
		RETURNING cluster
	`, newRunID, newCreatedAt, nullableString(req.Cell), nullableString(req.CellInstance),
		req.PlacementGeneration, buildNumber, req.IntentKey, incarnation).Scan(&cluster); err != nil {
		return ClaimResponse{}, "", fmt.Errorf("transfer intent: %w", err)
	}
	return ClaimResponse{Decision: DecisionGranted, RunID: newRunID.String(), BuildNumber: buildNumber}, cluster, nil
}

// storedOwner answers the cell instance the stored intent names, or an
// empty string for a null one. The start mark compares it with the
// request's instance, so an owner that named no instance at its grant
// still matches its own mark and its own repeat.
func storedOwner(row storedIntent) string {
	if row.cellInstance == nil {
		return ""
	}
	return *row.cellInstance
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
