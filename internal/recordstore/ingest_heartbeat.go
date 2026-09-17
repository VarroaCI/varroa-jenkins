package recordstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

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
// heartbeat_received_at belongs in the insert column list, not only in
// the update clause. Postgres resolves EXCLUDED from the insert's own
// column list, so a column the insert omits reads as null in EXCLUDED
// and the CASE would keep the stored value forever.
func applyRunHeartbeat(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	receivedAt := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, last_heartbeat_at, heartbeat_received_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, 'QUEUED', $9, $10
		)
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
			payload = runs.payload `+acceptedPayloadKeysSQL+`
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath, env.OccurredAt, receivedAt,
	); err != nil {
		return fmt.Errorf("raise the heartbeat columns: %w", err)
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
// that reaches the recorder after the dispatch. The dead instance can
// still deliver a heartbeat that was in flight, and the target itself
// can deliver one it emitted before it took the run over. Neither
// proves the resume, and clearing on either would drop the run's
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
