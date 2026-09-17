package recordstore

import (
	"context"
	"fmt"
	"time"
)

// heartbeatMsText is the JSON text of a run's stored writer heartbeat,
// in milliseconds since the epoch.
const heartbeatMsText = `(payload->'log_summary'->>'writer_heartbeat_unix_ms')`

// safeHeartbeatMsExpr casts heartbeatMsText to bigint only when its text
// form is one to fifteen ASCII digits. A JSON number can hold a
// fraction, a negative sign, or more digits than bigint accepts; each of
// those fails a raw cast, so this expression yields NULL for them
// instead of aborting the whole statement.
const safeHeartbeatMsExpr = `CASE WHEN ` + heartbeatMsText + ` ~ '^[0-9]{1,15}$' THEN ` + heartbeatMsText + `::bigint END`

// anchorExpr is the SQL expression for a run's finalize-wait anchor: the
// writer's own safe heartbeat when present, else ended_at, then
// heartbeat_received_at, then started_at, then queued_at. The candidate
// selection query and the finalize update both use this exact
// expression, so the two statements can never disagree about which
// moment gates the wait, even when the anchor is a fallback column that
// a concurrent write can change between the two statements.
const anchorExpr = `COALESCE(to_timestamp((` + safeHeartbeatMsExpr + `) / 1000.0), ended_at, heartbeat_received_at, started_at, queued_at)`

// finalizeCandidate is the subset of a runs row the log-finalization
// sweep needs to decide, and to act on, one run.
type finalizeCandidate struct {
	runID                 string
	createdAt             time.Time
	status                string
	cluster               string
	namespace             string
	cell                  string
	tenant                string
	cellInstance          *string
	heartbeatReceivedAt   *time.Time
	startedAt             *time.Time
	queuedAt              *time.Time
	endedAt               *time.Time
	writerHeartbeatRaw    *string
	writerHeartbeatUnixMs *int64
}

// finalizeStaleLogs is step 10 of the maintenance round. It selects a run
// whose status is terminal, or whose owning cell liveness reports gone,
// whose log summary is absent or reports finalized false, and whose wait
// anchor is older than wait. It creates the summary when absent, then
// sets finalized true and incomplete true with reason "not-finalized". It
// never lowers a finalized that is already true, and it skips a run
// whose Liveness call errors. It returns how many runs the guarded
// update actually changed, so a concurrent write that makes the update
// skip a row is never miscounted as finalized.
//
// It reads candidates in keyset batches of retentionBatchSize, ordered by
// (created_at, run_id), so one hourly round never holds an unbounded
// result set for an installation with years of run history.
//
// It skips a TRIGGER_FAILED row. Such a run holds no log at all, and a
// summary marked incomplete would survive a later supersede and mislabel
// the real run's log.
func finalizeStaleLogs(ctx context.Context, db maintDB, liveness Liveness, wait time.Duration, now time.Time) (int, error) {
	cutoff := now.Add(-wait)
	finalized := 0
	var after *finalizeCursor
	for {
		candidates, err := selectFinalizeCandidates(ctx, db, cutoff, after)
		if err != nil {
			return finalized, err
		}
		if len(candidates) == 0 {
			return finalized, nil
		}

		for _, c := range candidates {
			if anchor := waitAnchor(c); anchor == nil || !anchor.Before(cutoff) {
				continue
			}
			ok, err := isTerminalOrCellGone(ctx, liveness, c)
			if err != nil || !ok {
				continue
			}
			changed, err := finalizeLogSummary(ctx, db, c.runID, c.writerHeartbeatRaw, cutoff)
			if err != nil {
				return finalized, fmt.Errorf("finalize log summary for %s: %w", c.runID, err)
			}
			if changed {
				finalized++
			}
		}

		last := candidates[len(candidates)-1]
		after = &finalizeCursor{createdAt: last.createdAt, runID: last.runID}
		if len(candidates) < retentionBatchSize {
			return finalized, nil
		}
	}
}

// finalizeCursor is finalizeStaleLogs' keyset position between batches.
type finalizeCursor struct {
	createdAt time.Time
	runID     string
}

// selectFinalizeCandidates reads up to retentionBatchSize runs whose log
// summary is not yet finalized and whose wait anchor (anchorExpr) is
// already older than cutoff, ordered by (created_at, run_id) starting
// after after. safeHeartbeatMsExpr gates the bigint cast, so a run whose
// writer_heartbeat_unix_ms holds a fractional, negative, oversized, or
// non-numeric JSON value never reaches the cast: it falls back to
// ended_at, heartbeat_received_at, started_at, or queued_at, matching
// waitAnchor's own precedence.
func selectFinalizeCandidates(ctx context.Context, db maintDB, cutoff time.Time, after *finalizeCursor) ([]finalizeCandidate, error) {
	args := []any{cutoff}
	cursorClause := ""
	if after != nil {
		args = append(args, after.createdAt, after.runID)
		cursorClause = fmt.Sprintf("AND (r.created_at, r.run_id) > ($%d, $%d::uuid)", len(args)-1, len(args))
	}
	args = append(args, retentionBatchSize)

	query := fmt.Sprintf(`
		SELECT r.run_id, r.created_at, r.status, r.cluster, r.namespace, r.cell, r.tenant,
		       r.heartbeat_received_at, r.started_at, r.queued_at, r.ended_at,
		       `+heartbeatMsText+`,
		       `+safeHeartbeatMsExpr+`,
		       ri.cell_instance
		FROM varroa.runs r
		LEFT JOIN varroa.run_intents ri ON ri.run_id = r.run_id
		WHERE (r.payload->'log_summary'->>'finalized') IS DISTINCT FROM 'true'
		  AND `+notTriggerFailedRunSQL+`
		  -- An accepted-only row needs no separate exclusion here: its
		  -- anchor is always null, so anchorExpr never selects it, and a
		  -- row the sweep already marked is already finalized.
		  %s
		  AND `+anchorExpr+` < $1
		ORDER BY r.created_at, r.run_id
		LIMIT $%d
	`, cursorClause, len(args))

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select log-finalization candidates: %w", err)
	}
	defer rows.Close()

	var candidates []finalizeCandidate
	for rows.Next() {
		var c finalizeCandidate
		if err := rows.Scan(&c.runID, &c.createdAt, &c.status, &c.cluster, &c.namespace, &c.cell, &c.tenant,
			&c.heartbeatReceivedAt, &c.startedAt, &c.queuedAt, &c.endedAt,
			&c.writerHeartbeatRaw, &c.writerHeartbeatUnixMs, &c.cellInstance); err != nil {
			return nil, fmt.Errorf("scan log-finalization candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select log-finalization candidates: %w", err)
	}
	return candidates, nil
}

// isTerminalOrCellGone reports whether c counts as terminal for the
// finalize sweep: its own status is terminal, or a non-terminal run's
// owning cell liveness reports gone. A Liveness error means the sweep
// cannot judge c this round.
func isTerminalOrCellGone(ctx context.Context, liveness Liveness, c finalizeCandidate) (bool, error) {
	if c.status == StatusCompleted || c.status == StatusDuplicate {
		return true, nil
	}
	if liveness == nil {
		return false, nil
	}
	return liveness.CellGone(ctx, RunLiveness{
		RunID:               c.runID,
		Cluster:             c.cluster,
		Namespace:           c.namespace,
		Cell:                c.cell,
		Tenant:              c.tenant,
		CellInstance:        derefString(c.cellInstance),
		HeartbeatReceivedAt: c.heartbeatReceivedAt,
		StartedAt:           c.startedAt,
	})
}

// waitAnchor returns the moment the finalize wait counts from: the
// writer's own reported heartbeat when present, else the first of
// ended_at, heartbeat_received_at, started_at, or queued_at that is
// set. The heartbeat anchor is the recorder's own receive time, so the
// wait never counts a pod clock against a recorder cutoff.
func waitAnchor(c finalizeCandidate) *time.Time {
	if c.writerHeartbeatUnixMs != nil {
		t := time.UnixMilli(*c.writerHeartbeatUnixMs).UTC()
		return &t
	}
	for _, t := range []*time.Time{c.endedAt, c.heartbeatReceivedAt, c.startedAt, c.queuedAt} {
		if t != nil {
			return t
		}
	}
	return nil
}

// finalizeLogSummary creates runs.payload.log_summary when absent, then
// sets finalized true and incomplete true with reason "not-finalized".
// It reports whether the guarded update actually changed the row.
//
// The WHERE guard repeats the monotonic finalized check at write time,
// so a concurrent writer that finalized the run between the select and
// this update leaves the row untouched. The writerHeartbeatRaw guard
// does the same for a concurrent run.log that stamped a fresher writer
// heartbeat in that same window: it must equal the exact text the
// select observed, or the update skips the row. anchorExpr repeats the
// same check for a fallback anchor: a concurrent run.heartbeat that
// moves heartbeat_received_at (or any other fallback column) into the wait
// window between the select and this update also makes the update skip
// the row, even though the writer heartbeat guard alone would not catch
// that change.
func finalizeLogSummary(ctx context.Context, db maintDB, runID string, writerHeartbeatRaw *string, cutoff time.Time) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE varroa.runs
		SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object(
			'log_summary',
			COALESCE(payload->'log_summary', '{}'::jsonb) || '{"finalized": true, "incomplete": true, "incomplete_reason": "not-finalized"}'::jsonb
		)
		WHERE run_id = $1
		  AND (payload->'log_summary'->>'finalized') IS DISTINCT FROM 'true'
		  AND (payload->'log_summary'->>'writer_heartbeat_unix_ms') IS NOT DISTINCT FROM $2::text
		  AND `+anchorExpr+` < $3
	`, runID, writerHeartbeatRaw, cutoff)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
