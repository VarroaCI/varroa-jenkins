package recordstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunLog merges a run.log envelope's payload into runs.payload
// under the key log_summary, key by key, and changes no other column
// beyond removing the accepted keys when the stored row still holds
// them. The merge never lowers a stored finalized true back to false:
// the maintenance round's log-finalization sweep (task 6.6) sets that
// marker, and a delayed writer heartbeat carrying finalized false must
// not reopen a log the sweep already closed. The merge also keeps a
// stored incomplete_reason that ranks at least as high as the incoming
// one in logReasonRanks, so a later generic marker never replaces a
// precise cause. A run.log that reports incomplete false removes a
// stored generic reason, because a complete log has no reason. It keeps a
// stored reason of logReasonLossRank or higher, and the incomplete flag
// with it: a second completion of one run can report a complete log after
// the first one recorded a loss, and the loss stays true.
func applyRunLog(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			'QUEUED', jsonb_build_object('log_summary', $9::jsonb)
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			payload = (COALESCE(runs.payload, '{}'::jsonb) `+acceptedPayloadKeysSQL+`) || jsonb_build_object(
				'log_summary',
				(COALESCE(runs.payload->'log_summary', '{}'::jsonb) || (EXCLUDED.payload->'log_summary')
					|| CASE WHEN runs.payload->'log_summary'->>'finalized' = 'true'
							THEN '{"finalized": true}'::jsonb ELSE '{}'::jsonb END
					|| `+logReasonKeepSQL(`runs.payload->'log_summary'->>'incomplete_reason'`, `EXCLUDED.payload->'log_summary'->>'incomplete_reason'`)+`
					|| (CASE WHEN `+logReasonLossKeptSQL+`
							THEN '{"incomplete": true}'::jsonb ELSE '{}'::jsonb END))
					- (CASE WHEN EXCLUDED.payload->'log_summary'->>'incomplete' = 'false' AND NOT `+logReasonLossKeptSQL+`
							THEN ARRAY['incomplete_reason'] ELSE ARRAY[]::text[] END))
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath,
		jsonArg(env.Payload),
	); err != nil {
		return fmt.Errorf("merge run.log payload: %w", err)
	}
	return nil
}
