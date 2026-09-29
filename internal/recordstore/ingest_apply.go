package recordstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// ErrUnknownKind means an envelope's kind is outside the kinds
// ingestKinds lists. The consumer acknowledges and drops such an event;
// it can never become readable.
var ErrUnknownKind = errors.New("unknown ingest kind")

// ErrMalformedEnvelope means an envelope's run_id is not a valid UUID,
// or its typed payload does not decode. The consumer acknowledges and
// drops such an event; a permanent input error can never become
// readable, so it must not retry forever.
var ErrMalformedEnvelope = errors.New("malformed ingest envelope")

// ApplyResult reports a non-error outcome of Ingester.Apply the caller
// still needs to act on.
type ApplyResult struct {
	// Conflict is true when a run.started write hit the run_intents
	// build-number UNIQUE constraint. The recorder stored the run with a
	// null build number and moved the intent to CONSUMED with a null
	// build number. The caller acknowledges the message and counts
	// recorder_ingest_conflict_total.
	Conflict bool

	// ConflictRunID and ConflictNumber name the run that already holds
	// the reported number, for the caller's log line.
	ConflictRunID  string
	ConflictNumber int64

	// NumberTakenFrom names the unstarted run whose grant a run.started
	// took, because Jenkins built the arriving run under that number.
	NumberTakenFrom string

	// NumberMismatch is true when a run.started reported a build number
	// other than the grant it applied. The caller counts
	// recorder_ingest_number_mismatch_total and logs both numbers.
	NumberMismatch   bool
	MismatchGranted  int64
	MismatchReported int64

	// Truncated is true when a job.schedules envelope reported
	// truncation. The recorder upserted every carried row and deleted
	// none. The caller acknowledges the message and counts
	// recorder_schedules_truncated_total.
	Truncated bool

	// ScheduleStale is true when a job.schedules envelope's time was not
	// newer than its cell's stored watermark. The write changed no row.
	// The caller acknowledges the message and counts it.
	ScheduleStale bool

	// TriggerFailedReason holds the reason code of a run.trigger_failed
	// envelope the recorder stored, or skipped because a row already
	// exists. The caller acknowledges the message and counts
	// recorder_ingest_trigger_failed_total with that reason code.
	TriggerFailedReason string

	// TriggerFailedStored reports whether that envelope's insert wrote a
	// row. It is false when the conflict clause skipped the insert,
	// because a redelivery or a real run already holds the run id.
	TriggerFailedStored bool

	// TriggerAcceptedStored reports whether a run.trigger_accepted
	// envelope's insert wrote a row or moved an accepted-only row to a new
	// cell. It is false when the conflict clause skipped the write, because
	// a redelivery or a real run already holds the run id. The caller acknowledges the message and counts
	// recorder_ingest_trigger_accepted_total with a stored attribute.
	TriggerAcceptedStored bool

	// ResumeFailedCleared reports whether a run.resume_failed envelope
	// returned a run to the armed state. It is false for a reason code
	// that proves the target reached the build load, and false when the
	// guard matched no row. The caller acknowledges the message either
	// way and logs the outcome.
	ResumeFailedCleared bool

	// ResumeFailedReason holds that envelope's reason code, for the
	// caller's log line.
	ResumeFailedReason string

	// ResumeFailedInstance holds the instance the failed command named.
	// The envelope's own CellInstance is the cell's current registered
	// instance, which an INSTANCE_MISMATCH failure proves is a different
	// pod, so a log line that read it would name the wrong target.
	ResumeFailedInstance string

	// SupersededStart is true when a run.started reported a build number
	// below the intent's stored number from an instance other than the
	// intent's current owner: the ledger moved and refreshed the intent
	// after that build was authorized. The recorder wrote nothing. The
	// caller acknowledges the message and counts
	// recorder_ingest_superseded_start_total. SupersededReported,
	// SupersededStored, and SupersededOwner carry the two numbers and
	// the owning instance for the caller's log line.
	SupersededStart    bool
	SupersededReported int64
	SupersededStored   int64
	SupersededOwner    string

	// RefusedSource is true when a run.source write carried a seq below
	// the stored source_seq. The merge then left every stored value in
	// place, so the caller acknowledges the message and publishes
	// nothing.
	RefusedSource bool

	// RefusedInput is true when a run.input write carried a seq below
	// the stored inputs_seq, and when the run already holds a terminal
	// status. The write then left every stored value in place, so the
	// caller acknowledges the message and publishes nothing.
	RefusedInput bool

	// ReleasedFleetLocks names every fleet lock a terminal run.completed
	// write released, one entry per deleted row, so notify publishes one
	// lock notification per row. Every other kind releases none.
	ReleasedFleetLocks []ReleasedFleetLock
}

// Ingester applies one IngestEnvelope to the record tables inside a
// single transaction. It is the write path the durable consumer calls
// for every delivered message.
type Ingester struct {
	pool          *pgxpool.Pool
	conn          *bus.Conn
	logger        *slog.Logger
	fanoutEval    *GrantEvaluator
	fanoutMetrics *UpstreamFanoutMetrics
	sweepMetrics  *InterruptedMetrics
	stateDeleter  ObjectDeleter
	notifier      *RunNotifier
}

// NewIngester returns an Ingester over pool. conn publishes a ledger
// action the duplicate backstop raises; a nil conn skips publishing,
// which a test with no bus connection relies on.
func NewIngester(pool *pgxpool.Pool, conn *bus.Conn, logger *slog.Logger) *Ingester {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ingester{pool: pool, conn: conn, logger: logger}
}

// SetUpstreamFanout enables the fan-out round design.md D9 runs after
// every owner run.completed write. An Ingester built with no call to
// this method runs no fan-out, which is what every existing NewIngester
// caller keeps getting.
func (g *Ingester) SetUpstreamFanout(evaluator *GrantEvaluator, metrics *UpstreamFanoutMetrics) {
	g.fanoutEval = evaluator
	g.fanoutMetrics = metrics
}

// SetSweepMetrics gives the ingest path the sweep's own counters. It
// counts the re-fire marker self-heal a rejected trigger performs, and
// the resume outcome a failed resume answers. One object carries both,
// because the takeover already holds it: a second setter and a second
// field would let one missed call drop a counter in silence.
//
// It follows SetUpstreamFanout's own convention. An Ingester built with
// no call to this method still heals the marker and still answers the
// resume, and counts nothing, because every add is a no-op on a nil
// receiver.
func (g *Ingester) SetSweepMetrics(metrics *InterruptedMetrics) {
	g.sweepMetrics = metrics
}

// SetStateDeleter enables the run-state cleanup that follows a run's
// real terminal write and the recorder's own cap refusal. An Ingester
// built with no call to this method deletes nothing, which every
// existing caller keeps getting.
func (g *Ingester) SetStateDeleter(d ObjectDeleter) {
	g.stateDeleter = d
}

// SetRunNotifier enables the live run notifications the BFF streams. An
// Ingester built with no call to this method publishes nothing, which
// every test with no bus relies on.
func (g *Ingester) SetRunNotifier(n *RunNotifier) {
	g.notifier = n
}

// notify publishes one ingest envelope's live notifications: the run
// notification the kind earns, and one lock notification per fleet lock
// the write released. It publishes nothing for a nil notifier or a
// failed write. The two families are independent facts about one
// commit, so a write that earns no run notification still publishes the
// lock notifications it owes.
func (g *Ingester) notify(env IngestEnvelope, res ApplyResult, err error) {
	if err != nil || g.notifier == nil {
		return
	}
	if n, ok := notificationFor(env, res); ok {
		g.notifier.Notify(n)
	}
	for _, lock := range res.ReleasedFleetLocks {
		g.notifier.Notify(lockNotification(lock.Cluster, lock.Tenant, lock.JobPath, lock.LockName, env.RunID, FleetLockStateReleased))
	}
}

// Apply writes env to the record tables. It returns ErrIntentMissing
// when no run_intents row exists for env.RunID, ErrUnknownKind when
// env.Kind is outside the kinds ingestKinds lists, and
// ErrMalformedEnvelope when env.RunID is not a valid UUID or a typed
// payload does not decode. Any other error is a retryable database
// failure. A nil error and a false ApplyResult.Conflict mean the write
// committed cleanly.
func (g *Ingester) Apply(ctx context.Context, env IngestEnvelope) (ApplyResult, error) {
	if !IsKnownKind(env.Kind) {
		return ApplyResult{}, fmt.Errorf("%w: %s", ErrUnknownKind, env.Kind)
	}

	// job.schedules carries no run_id and needs no run_intents row: the
	// message describes a cell, not a run. Every other kind's write goes
	// through the run_intents-anchored path below.
	if env.Kind == KindJobSchedules {
		return g.applyJobSchedules(ctx, env)
	}

	// run.trigger_failed and run.trigger_accepted each write the only
	// record of its run when no other event has arrived, so no intent
	// needs to exist before either one. Both take the same early route.
	if env.Kind == KindRunTriggerFailed {
		res, err := g.applyTriggerFailed(ctx, env)
		g.notify(env, res, err)
		return res, err
	}
	if env.Kind == KindRunTriggerAccepted {
		res, err := g.applyTriggerAccepted(ctx, env)
		g.notify(env, res, err)
		return res, err
	}

	// run.resume_failed touches one runs row and nothing else. The run
	// already holds an intent, but this write needs none of it, so the
	// kind takes the same early route the two trigger kinds take.
	if env.Kind == KindRunResumeFailed {
		res, err := g.applyResumeFailed(ctx, env)
		g.notify(env, res, err)
		return res, err
	}

	runID, err := uuid.Parse(env.RunID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: run_id is not a valid uuid: %v", ErrMalformedEnvelope, err)
	}

	dispatched, err := g.applyOnce(ctx, runID, env, false)
	result := ApplyResult{}
	if err != nil {
		if !isBuildNumberConflict(err) {
			return result, err
		}
		dispatched, err = g.applyOnce(ctx, runID, env, true)
		if err != nil {
			return result, err
		}
		// The retry can answer superseded on its own: the first attempt
		// released the intent lock when it rolled back, and a claim can
		// move and refresh the intent before the retry reads it. No
		// null-number run was stored then, so Conflict stays false.
		if !dispatched.supersededStart {
			result.Conflict = true
			result.ConflictRunID = dispatched.conflictRunID
			result.ConflictNumber = dispatched.conflictNumber
			g.logger.Warn("run.started build number conflict; stored run with a null build number",
				"runID", env.RunID, "buildNumber", dispatched.conflictNumber,
				"holderRunID", dispatched.conflictRunID)
		}
	}
	if dispatched.numberTakenFrom != "" {
		result.NumberTakenFrom = dispatched.numberTakenFrom
		g.logger.Warn("run.started took the build number of an unstarted grant",
			"runID", env.RunID, "holderRunID", dispatched.numberTakenFrom)
	}
	if dispatched.numberMismatch {
		result.NumberMismatch = true
		result.MismatchGranted = dispatched.mismatchGranted
		result.MismatchReported = dispatched.mismatchReported
	}
	if dispatched.supersededStart {
		result.SupersededStart = true
		result.SupersededReported = dispatched.supersededReported
		result.SupersededStored = dispatched.supersededStored
		result.SupersededOwner = dispatched.supersededOwner
	}
	if dispatched.refusedSource {
		result.RefusedSource = true
	}
	if dispatched.refusedInput {
		result.RefusedInput = true
	}
	result.ReleasedFleetLocks = dispatched.releasedFleetLocks

	if dispatched.action != nil && g.conn != nil {
		if perr := PublishLedgerAction(g.conn, *dispatched.action); perr != nil {
			g.logger.Error("publish ledger action failed", "kind", dispatched.action.Kind, "error", perr)
		}
	}

	// The upstream fan-out (design.md D9) runs after the transaction that
	// recorded env has committed, in this same handler, never inside it:
	// it reads the grant evaluator over several queries and publishes on
	// the bus, and a lost publish must never roll runs back. It runs only
	// for the run's real terminal write, dispatch's own owner test.
	if dispatched.fanout != nil && g.fanoutEval != nil {
		deps := FanoutDeps{Pool: g.pool, Evaluator: g.fanoutEval, Conn: g.conn, Metrics: g.fanoutMetrics}
		if fanoutErr := RunUpstreamFanout(ctx, deps, *dispatched.fanout, time.Now().UTC(), g.logger); fanoutErr != nil {
			g.logger.Error("upstream fan-out round failed", "runID", env.RunID, "error", fanoutErr)
		}
	}

	// The run-state cleanup runs after the commit, never inside it: a
	// lost object delete must never roll the record back. The hourly
	// backstop repairs a recorder that stops between the two.
	if dispatched.stateCleanup != "" && g.stateDeleter != nil {
		if delErr := deleteRunStateExceptScript(ctx, g.stateDeleter, dispatched.stateCleanup, dispatched.stateCleanupFiles); delErr != nil {
			g.logger.Error("delete run state failed", "runID", dispatched.stateCleanup, "error", delErr)
		}
	}

	// The superseded-object delete runs at the same place and for the
	// same reason. It is best effort: a failed delete leaves one object,
	// which the retention round's prefix delete removes with the run's record.
	if dispatched.stateKeyCleanup != "" && g.stateDeleter != nil {
		if delErr := g.stateDeleter.Delete(ctx, dispatched.stateKeyCleanup); delErr != nil {
			g.logger.Error("delete superseded run-state object failed",
				"runID", env.RunID, "key", dispatched.stateKeyCleanup, "error", delErr)
		}
	}

	// The live notification joins the other post-commit sites: it runs
	// after the commit, never inside it, and a lost publish never rolls a
	// record back.
	g.notify(env, result, nil)

	return result, nil
}

// dispatchResult is what dispatch reports for one write, beyond a plain
// error: a pending LedgerAction for run.started's duplicate backstop,
// for a run.completed write that was this run's real terminal write,
// the producer identity Apply feeds the fan-out round, for a
// run.started that described a superseded grant, the superseded fields
// Apply copies onto its ApplyResult, for a run.source write the
// guard refused, the refusal flag Apply copies onto its ApplyResult,
// and for a terminal run.completed write the fleet locks it released,
// which Apply copies onto its ApplyResult too.
type dispatchResult struct {
	action *LedgerAction
	fanout *FanoutProducer

	// stateCleanup names the run whose run-state objects Apply deletes
	// after the transaction commits. It is set on a run's real terminal
	// write, the same owner test the fan-out uses, and on the recorder's
	// own cap refusal.
	stateCleanup string

	// stateCleanupFiles is the run's mesh file list, read inside the same
	// transaction that sets stateCleanup. The cleanup keeps the entry
	// RunScriptObjectKey selects and deletes every other entry by key. A
	// terminal write passes a non-nil list, empty for a NULL or
	// undecodable column. The run-state cap leaves it nil, so the cleanup
	// deletes the whole prefix.
	stateCleanupFiles []runStateFile

	// stateKeyCleanup names one exact run-state object key Apply deletes
	// after the transaction commits. A run-state upsert that supersedes
	// another epoch's object sets it, so the stored bytes of a run stay
	// equal to the bytes its record counts.
	stateKeyCleanup string

	supersededStart    bool
	supersededReported int64
	supersededStored   int64
	supersededOwner    string

	// numberTakenFrom names the unstarted run whose grant a run.started
	// took. conflictRunID and conflictNumber name the started run that
	// holds the reported number on the null-number retry.
	numberTakenFrom string
	conflictRunID   string
	conflictNumber  int64

	// numberMismatch is true when a run.started reported a build number
	// other than its grant.
	numberMismatch   bool
	mismatchGranted  int64
	mismatchReported int64

	// refusedSource is true when a run.source write carried a seq below
	// the stored source_seq, so the merge changed no stored value. The
	// kind raises no ledger action either way.
	refusedSource bool

	// refusedInput is true when a run.input write carried a seq below the
	// stored inputs_seq, and when the run already holds a terminal
	// status, so the write changed no stored set.
	refusedInput bool

	// releasedFleetLocks names every fleet lock a terminal run.completed
	// write released, one entry per deleted row. Apply copies it onto
	// its ApplyResult, the same two-hop path supersededStart and
	// refusedSource already take.
	releasedFleetLocks []ReleasedFleetLock
}

// applyOnce runs one attempt of env's write in its own transaction.
func (g *Ingester) applyOnce(ctx context.Context, runID uuid.UUID, env IngestEnvelope, nullBuildNumber bool) (dispatchResult, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return dispatchResult{}, fmt.Errorf("begin ingest transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	intent, err := loadIntent(ctx, tx, runID)
	if err != nil {
		return dispatchResult{}, err
	}

	result, err := g.dispatch(ctx, tx, intent, env, nullBuildNumber)
	if err != nil {
		return dispatchResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return dispatchResult{}, fmt.Errorf("commit ingest transaction: %w", err)
	}
	return result, nil
}

// dispatch runs the write for env.Kind against tx.
func (g *Ingester) dispatch(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope, nullBuildNumber bool) (dispatchResult, error) {
	switch env.Kind {
	case KindRunStarted:
		return applyRunStarted(ctx, tx, intent, env, nullBuildNumber)
	case KindRunStage:
		return dispatchResult{}, applyRunStage(ctx, tx, intent, env)
	case KindRunCompleted:
		owner, payload, releasedLocks, err := applyRunCompleted(ctx, tx, intent, env)
		if err != nil {
			return dispatchResult{}, err
		}
		if !owner {
			return dispatchResult{}, nil
		}
		files, filesErr := readRunStateFiles(ctx, tx, intent)
		if filesErr != nil {
			return dispatchResult{}, filesErr
		}
		res := dispatchResult{
			stateCleanup:       intent.runID.String(),
			stateCleanupFiles:  files,
			releasedFleetLocks: releasedLocks,
		}
		if payload.Result != nil {
			// The fan-out round is the one part of the terminal path
			// that is not idempotent, so it runs behind its own
			// at-most-once claim in this same transaction.
			claimed, claimErr := claimFanoutRound(ctx, tx, intent.createdAt, intent.runID)
			if claimErr != nil {
				return dispatchResult{}, claimErr
			}
			if claimed {
				res.fanout = &FanoutProducer{
					Cluster: intent.cluster,
					Tenant:  intent.tenant,
					JobPath: intent.jobPath,
					RunID:   intent.runID.String(),
					Result:  *payload.Result,
				}
			}
		}
		return res, nil
	case KindRunTests:
		return dispatchResult{}, applyRunTests(ctx, tx, intent, env)
	case KindRunArtifacts:
		return dispatchResult{}, applyRunArtifacts(ctx, tx, intent, env)
	case KindRunChanges:
		return dispatchResult{}, applyRunChanges(ctx, tx, intent, env)
	case KindRunLog:
		return dispatchResult{}, applyRunLog(ctx, tx, intent, env)
	case KindRunHeartbeat:
		return dispatchResult{}, applyRunHeartbeat(ctx, tx, intent, env, g.logger)
	case KindRunSource:
		refused, err := applyRunSource(ctx, tx, intent, env)
		if err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{refusedSource: refused}, nil
	case KindRunInput:
		refused, err := applyRunInput(ctx, tx, intent, env)
		if err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{refusedInput: refused}, nil
	case KindRunState:
		return applyRunState(ctx, tx, intent, env)
	default:
		return dispatchResult{}, fmt.Errorf("%w: %s", ErrUnknownKind, env.Kind)
	}
}

// readRunStateFiles reads the run's mesh file list inside tx for the
// terminal cleanup. A missing row, a NULL column, and an undecodable list
// all answer an empty non-nil slice, so the cleanup deletes the whole
// prefix and never reads the list as the run-state cap's nil.
func readRunStateFiles(ctx context.Context, tx pgx.Tx, intent *intentRow) ([]runStateFile, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `
		SELECT payload->'mesh'->'files' FROM varroa.runs WHERE run_id = $1 AND created_at = $2
	`, intent.runID, intent.createdAt).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return []runStateFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run-state file list: %w", err)
	}
	return runStateFilesOrEmpty(raw), nil
}
