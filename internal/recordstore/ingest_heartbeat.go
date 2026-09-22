package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// heartbeatOwnerSQL is the owner test both halves of the heartbeat
// write apply. $9 is the envelope's sender and $10 the intent's own
// owning instance. The stored row supplies the other two sources, so
// the test reads the same ownership expression the sweep reads:
// the intent's instance, else the accepted trigger's instance.
//
// The first two rows hold the earlier behaviour where the test has no
// two names to compare: an envelope that names no sender, and a run
// that no source names an owner for. A live install meets neither.
//
// The last row admits the dispatched resume's target. That target's
// first heartbeat is the only proof a resumed build gives, and it
// arrives before any other write moves ownership on.
const heartbeatOwnerSQL = `
	$9::text = ''
	OR COALESCE(NULLIF($10::text, ''), runs.payload->>'accepted_instance') IS NULL
	OR $9::text = COALESCE(NULLIF($10::text, ''), runs.payload->>'accepted_instance')
	OR $9::text = runs.payload->'mesh'->'resume'->>'instance'`

// summaryHeartbeatGuardSQL is the guard the heartbeat summary raise
// reads. $13 is the payload's summary value and $14 is its seq. The
// raise applies only when the payload carries a summary value at all,
// and only when its seq sits at or above the stored summary_seq. The
// floor is zero, so a first summary write whose seq is zero applies.
//
// The guard gates the whole payload expression, never one operand of a
// concatenation. runs.payload is null on a row a first heartbeat
// inserted, and jsonb || and jsonb - are both strict, so a guard inside
// the concatenation would drop the summary_seq key on exactly that row.
const summaryHeartbeatGuardSQL = `$13::text IS NOT NULL
	AND $14::bigint >= COALESCE((runs.payload->>'summary_seq')::bigint, 0)`

// heartbeatSummary is the part of a run.heartbeat payload the summary
// raise reads. Summary stays nil when the payload carries no summary
// key, and that nil is the gate that leaves both the summary column and
// the payload's summary_seq key untouched.
type heartbeatSummary struct {
	Summary *string `json:"summary,omitempty"`
	Seq     int64   `json:"seq"`
}

// applyRunHeartbeat raises the run's two heartbeat columns. It raises
// last_heartbeat_at to the envelope's occurred_at, which a Jenkins pod
// stamps, and heartbeat_received_at to the recorder's own receive time.
// Each raise applies only when the new value is later than the stored
// one, so a redelivered envelope lowers neither column. It changes no
// other column and no status, except that it removes the accepted keys
// from a stored row that still holds them, so a heartbeat for a run
// with no runs row yet still creates only the base row a later event
// needs.
//
// The two columns serve two readers. Every liveness window reads
// heartbeat_received_at, so both sides of that comparison come from the
// recorder. The reader answers last_heartbeat_at, which reports what
// the build itself sent.
//
// Only the run's own owner raises the columns, or the target a
// dispatched resume names. A heartbeat from any other instance changes
// nothing at all. Two cases hold the unconditional raise, because the
// owner test needs a name on each side: an envelope that names no
// sender, and a run that no source names an owner for.
//
// The dead owner of a resumed run can still deliver a
// heartbeat that was in flight, and that heartbeat proves no liveness:
// it would raise heartbeat_received_at above the resume's own request
// time, and nothing ever lowers the column again. The resume guard and
// the marking write both read that comparison, so the run would stay
// RUNNING for the rest of its life.
//
// heartbeat_received_at belongs in the insert column list, not only in
// the update clause. Postgres resolves EXCLUDED from the insert's own
// column list, so a column the insert omits reads as null in EXCLUDED
// and the CASE would keep the stored value forever.
//
// A heartbeat whose payload carries a summary value also raises the
// summary column and the payload's summary_seq key, under the guard
// summaryHeartbeatGuardSQL states. A heartbeat whose payload carries no
// summary value changes neither the column nor the key.
func applyRunHeartbeat(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope,
	logger *slog.Logger) error {
	receivedAt := time.Now().UTC()
	var summary heartbeatSummary
	if err := json.Unmarshal(env.Payload, &summary); err != nil {
		return fmt.Errorf("%w: decode run.heartbeat payload: %v", ErrMalformedEnvelope, err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, last_heartbeat_at, heartbeat_received_at
		)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, 'QUEUED', $11, $12
		 WHERE $9::text = '' OR $10::text = '' OR $9::text = $10::text
		    OR EXISTS (SELECT 1 FROM varroa.runs r WHERE r.run_id = $1 AND r.created_at = $2)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			last_heartbeat_at = CASE
				WHEN runs.last_heartbeat_at IS NULL OR EXCLUDED.last_heartbeat_at > runs.last_heartbeat_at
				THEN EXCLUDED.last_heartbeat_at
				ELSE runs.last_heartbeat_at
			END,
			heartbeat_received_at = CASE
				WHEN runs.heartbeat_received_at IS NULL
					OR EXCLUDED.heartbeat_received_at > runs.heartbeat_received_at
				THEN EXCLUDED.heartbeat_received_at
				ELSE runs.heartbeat_received_at
			END,
			summary = CASE WHEN `+summaryHeartbeatGuardSQL+`
				THEN $13::text ELSE runs.summary END,
			payload = CASE WHEN `+summaryHeartbeatGuardSQL+`
				THEN (COALESCE(runs.payload, '{}'::jsonb) `+acceptedPayloadKeysSQL+`)
					|| jsonb_build_object('summary_seq', $14::bigint)
				ELSE runs.payload `+acceptedPayloadKeysSQL+` END
		 WHERE `+heartbeatOwnerSQL+`
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
		env.CellInstance, intent.cellInstanceValue(), env.OccurredAt, receivedAt,
		summary.Summary, summary.Seq,
	)
	if err != nil {
		return fmt.Errorf("raise the heartbeat columns: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Debug("drop a heartbeat from an instance that does not own the run",
			"run_id", intent.runID, "cell_instance", env.CellInstance,
			"intent_owner", intent.cellInstanceValue())
		return nil
	}
	return clearProvedResume(ctx, tx, intent, env, receivedAt)
}

// clearProvedResume removes runs.payload->'mesh'.resume once the target
// the recorder named sends its own heartbeat. A resumed build emits no
// run.started, so the heartbeat is the only proof the target took the
// run over.
//
// The clear matters because the resume record shields the run from
// every interruption test. A record left in place would hold a resumed
// run RUNNING, with its state and its fleet lock, through a later death
// of the target itself.
//
// Only the named target's own heartbeat clears it, and only a heartbeat
// that reaches the recorder after the dispatch. The target itself can
// deliver a heartbeat it emitted before it took the run over. That
// heartbeat proves no resume, and clearing on it would drop the run's
// protection before the target restored it.
//
// The comparison runs on the recorder's own clock, never the pod's.
// requested_at is stamped by the recorder, and occurred_at by the
// Jenkins pod, so comparing the two would answer on the skew between
// two machines. receivedAt is the recorder's receive time, and a
// heartbeat always reaches the recorder after it was emitted, so the
// test stays conservative: it clears no earlier than the pod clock
// would.
//
// The caller passes receivedAt, the same value it stamped on
// heartbeat_received_at. One heartbeat therefore carries one receive
// time into every rule that reads it.
func clearProvedResume(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope,
	receivedAt time.Time) error {
	if env.CellInstance == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb) - 'resume')
		 WHERE run_id = $1 AND created_at = $2
		   AND payload->'mesh'->'resume'->>'instance' = $3
		   AND $4::timestamptz > (payload->'mesh'->'resume'->>'requested_at')::timestamptz
	`, intent.runID, intent.createdAt, env.CellInstance, receivedAt); err != nil {
		return fmt.Errorf("clear a proved resume: %w", err)
	}
	return nil
}
