package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
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
	// indexReadFailedUnixMs is the run's first failed index read, from
	// indexReadFailedKey. It is nil when the run holds none.
	indexReadFailedUnixMs *int64
	// generationZero is true when the claim granted the run's intent under
	// placement generation 0. A run with no intent answers false.
	generationZero bool
}

// Bounds of the stored log index reads of one log-finalization round.
const (
	// finalizeIndexReadBudget bounds the stored log indexes that one round
	// reads for runs that hold no failed-read time. A read that finds no
	// index does not count: it costs one listing on an object-store tier
	// and one query on the Postgres tier. A failed read counts once, and
	// the run then holds a failed-read time. A run past the budget waits
	// for the next round.
	finalizeIndexReadBudget = 1000

	// finalizeIndexDeferredReadBudget bounds the reads of one round for
	// runs that hold a failed-read time. These reads never spend
	// finalizeIndexReadBudget, so unreadable indexes cannot hold back the
	// readable runs behind them. A run past this budget gets no read in
	// the round.
	finalizeIndexDeferredReadBudget = 50

	// finalizeIndexGiveUpAge is how long the sweep retries a run whose
	// index read fails while the blob tier answers. The time counts from
	// the run's own first failed read, which indexReadFailedKey holds. A
	// read that fails at least this long after that time marks the run
	// not-finalized. A failed read while the tier does not answer never
	// marks a run and starts no clock.
	finalizeIndexGiveUpAge = 24 * time.Hour
)

// indexReadFailedKey is the runs.payload key that holds a run's first
// failed index read, in milliseconds since the epoch on the recorder's
// clock. It sits beside log_summary, not inside it: a log_summary that
// held only this key would read as a summary to every log reader. The
// sweep's own final write removes it, and a supersede that drops the
// log summary drops it too (acceptedSupersedeKeysSQL).
const indexReadFailedKey = "log_index_read_failed_unix_ms"

// indexReadFailedMsExpr casts indexReadFailedKey to bigint only when its
// text is one to fifteen ASCII digits, and yields NULL otherwise.
const indexReadFailedMsExpr = `(CASE WHEN (payload->>'` + indexReadFailedKey + `') ~ '^[0-9]{1,15}$' THEN (payload->>'` + indexReadFailedKey + `')::bigint END)`

// finalizeProbeRunID names a run that no gateway writes, because each
// run id is a UUIDv7. A read of its index key tells an index that fails
// for one run from a blob tier that does not answer. The key holds no
// part, so on an object-store tier an answer proves only that a listing
// works. A tier that lists but fails each GET answers the probe.
const finalizeProbeRunID = "00000000-0000-0000-0000-000000000000"

// finalizeStaleLogs is step 11 of the maintenance round. It selects a run
// whose status is terminal, or whose owning cell liveness reports gone,
// whose log summary is absent or reports finalized false, and whose wait
// anchor is older than wait. It then reads the run's stored log index
// through logs. When the index holds the final entry, it sets finalized
// true with no incomplete mark, as the gateway's own marker does.
// Otherwise it sets finalized true and incomplete true with reason
// "not-finalized" unless the summary already holds a reason. It creates
// the summary when absent. It never lowers a finalized that is already
// true, and it skips a run whose Liveness call errors. It returns how
// many runs the guarded update actually changed, so a concurrent write
// that makes the update skip a row is never miscounted as finalized.
//
// A failed index read is no evidence that a log is short. After each
// failed read the sweep reads the probe key finalizeProbeRunID. When the
// tier does not answer the probe, the round ends and writes nothing for
// the run. When the tier answers, the read failed for that run alone.
// The first such failure stores now under indexReadFailedKey, and the
// run is then deferred: later rounds read it under
// finalizeIndexDeferredReadBudget. A deferred read that fails at least
// finalizeIndexGiveUpAge after the stored time marks the run
// not-finalized. One failed read therefore never marks a run. A gap in
// the reads of a run never moves the stored time, so after a gap of
// finalizeIndexGiveUpAge or more the next failed read marks the run.
//
// It reads candidates in keyset batches of retentionBatchSize, ordered by
// (created_at, run_id), so one hourly round never holds an unbounded
// result set for an installation with years of run history. A run it
// does not read waits for the next round.
//
// It skips a TRIGGER_FAILED row. Such a run holds no log at all, and a
// summary marked incomplete would survive a later supersede and mislabel
// the real run's log.
func finalizeStaleLogs(ctx context.Context, db maintDB, liveness Liveness, logs LogRangeReader, wait time.Duration, now time.Time) (int, error) {
	return finalizeStaleLogsWithin(ctx, db, liveness, logs, wait, now,
		finalizeReadBudget{first: finalizeIndexReadBudget, deferred: finalizeIndexDeferredReadBudget})
}

// finalizeReadBudget is the stored log index reads left in one round.
type finalizeReadBudget struct {
	// first is the reads left for runs that hold no failed-read time.
	first int
	// deferred is the reads left for runs that hold a failed-read time.
	deferred int
}

// finalizeStaleLogsWithin is finalizeStaleLogs with a read budget of its
// own, so a test can spend the budget with a few runs.
func finalizeStaleLogsWithin(ctx context.Context, db maintDB, liveness Liveness, logs LogRangeReader, wait time.Duration,
	now time.Time, budget finalizeReadBudget) (int, error) {
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
			changed, stop, err := finalizeCandidateLog(ctx, db, logs, c, now, cutoff, &budget)
			if err != nil {
				return finalized, fmt.Errorf("finalize log of %s: %w", c.runID, err)
			}
			if changed {
				finalized++
			}
			if stop {
				return finalized, nil
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

// finalizeCandidateLog reads the stored log index of c within budget
// and writes the run's log summary. changed reports whether the final
// write changed the row. stop is true when the round must end: the first
// budget is spent, or the blob tier does not answer. A deferred run past
// the deferred budget gets no read and does not stop the round.
func finalizeCandidateLog(ctx context.Context, db maintDB, logs LogRangeReader, c finalizeCandidate, now, cutoff time.Time,
	budget *finalizeReadBudget) (changed, stop bool, err error) {
	deferred := c.indexReadFailedUnixMs != nil
	switch {
	case deferred && budget.deferred <= 0:
		return false, false, nil
	case deferred:
		budget.deferred--
	case budget.first <= 0:
		return false, true, nil
	}

	finalChunks, found, readErr := storedLogFinal(ctx, logs, c.runID)
	switch {
	case readErr == nil:
		if found && !deferred {
			budget.first--
		}
	case !blobTierAnswers(ctx, logs):
		return false, true, nil
	case !deferred:
		budget.first--
		return false, false, markIndexReadFailed(ctx, db, c.runID, now)
	case now.Sub(time.UnixMilli(*c.indexReadFailedUnixMs)) < finalizeIndexGiveUpAge:
		return false, false, nil
	default:
		finalChunks = 0
	}
	changed, err = finalizeLogSummary(ctx, db, c.runID, c.writerHeartbeatRaw, cutoff, finalChunks)
	return changed, false, err
}

// markIndexReadFailed stores now under indexReadFailedKey for a run that
// is not finalized and holds no valid failed-read time, so a later
// failure never moves the give-up clock.
func markIndexReadFailed(ctx context.Context, db maintDB, runID string, now time.Time) error {
	if _, err := db.Exec(ctx, `
		UPDATE varroa.runs
		SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('`+indexReadFailedKey+`', $2::bigint)
		WHERE run_id = $1
		  AND (payload->'log_summary'->>'finalized') IS DISTINCT FROM 'true'
		  AND `+indexReadFailedMsExpr+` IS NULL
	`, runID, now.UnixMilli()); err != nil {
		return fmt.Errorf("store failed index read: %w", err)
	}
	return nil
}

// blobTierAnswers reports whether the blob tier answers a read of the
// probe index key. A missing key is an answer.
func blobTierAnswers(ctx context.Context, logs LogRangeReader) bool {
	r, err := logs.NewRangeReader(ctx, blob.LogIndexKey(finalizeProbeRunID), 0, -1)
	if err != nil {
		return errors.Is(err, blob.ErrObjectNotFound)
	}
	_ = r.Close()
	return true
}

// storedLogFinal reads the stored log index of runID. finalChunks is one
// above the sequence of the highest entry with final true, and 0 when no
// entry carries it. found reports whether an index is stored at all. The
// gateway writes a final entry only after every lower entry, and its own
// restart recovery reads that entry as a finished log. An index that
// does not parse holds no final entry that a reader can use. A nil logs
// holds no index. It reads no dropped count, because only the plugin
// reports plugin-drop.
func storedLogFinal(ctx context.Context, logs LogRangeReader, runID string) (finalChunks int64, found bool, err error) {
	if logs == nil {
		return 0, false, nil
	}
	r, err := logs.NewRangeReader(ctx, blob.LogIndexKey(runID), 0, -1)
	if errors.Is(err, blob.ErrObjectNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read log index: %w", err)
	}
	defer func() { _ = r.Close() }()
	entries, err := parseStoredLogIndex(r)
	if errors.Is(err, errLogIndexMalformed) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	for _, e := range entries {
		if e.Final && e.Seq+1 > finalChunks {
			finalChunks = e.Seq + 1
		}
	}
	return finalChunks, true, nil
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
		       ri.cell_instance,
		       `+indexReadFailedMsExpr+`,
		       COALESCE(ri.placement_generation = 0, false)
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
			&c.writerHeartbeatRaw, &c.writerHeartbeatUnixMs, &c.cellInstance, &c.indexReadFailedUnixMs,
			&c.generationZero); err != nil {
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
		Status:              c.status,
		GenerationZero:      c.generationZero,
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

// logNotFinalizedMarkSQL is the jsonb that a writer merges over a stored
// log summary when the stored log index proves no final entry. It sets
// finalized and incomplete true. It writes the reason not-finalized only
// when the summary holds no reason of the same or a higher rank. The
// log-finalization sweep and the interrupted-run marking both use it.
var logNotFinalizedMarkSQL = `'{"finalized": true, "incomplete": true, "incomplete_reason": "` + logReasonNotFinalized + `"}'::jsonb
				|| ` + logReasonKeepSQL(`payload->'log_summary'->>'incomplete_reason'`, `'`+logReasonNotFinalized+`'`)

// logFinalEntryMarkSQL returns the jsonb that a writer merges over a
// stored log summary when the stored log index holds the final entry. It
// sets finalized true and adds no incomplete mark, as the gateway's own
// marker does. chunkCount names the SQL parameter that holds the chunk
// count of the final entry. The write stores that count only when the
// summary holds no chunk_count. The log-finalization sweep and the
// interrupted-run marking both use it.
func logFinalEntryMarkSQL(chunkCount string) string {
	return `'{"finalized": true}'::jsonb
				|| (CASE WHEN payload->'log_summary'->'chunk_count' IS NULL
				         THEN jsonb_build_object('chunk_count', ` + chunkCount + `::bigint) ELSE '{}'::jsonb END)`
}

// finalizeLogSummary creates runs.payload.log_summary when absent, then
// sets finalized true. When finalChunks is above 0, the stored index
// holds the final entry, so it adds no incomplete mark, as the gateway's
// own marker does. It also writes chunk_count as finalChunks when the
// summary holds no chunk_count, so a log manifest checks each chunk that
// the index names. Otherwise it also sets incomplete true. It writes the
// reason "not-finalized" only when the summary holds no reason, because a
// stored reason names a more precise cause or arrived first in the same
// rank. It removes indexReadFailedKey in the same write. It reports
// whether the guarded update actually changed the row.
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
func finalizeLogSummary(ctx context.Context, db maintDB, runID string, writerHeartbeatRaw *string, cutoff time.Time,
	finalChunks int64) (bool, error) {
	args := []any{runID, writerHeartbeatRaw, cutoff}
	mark := logNotFinalizedMarkSQL
	if finalChunks > 0 {
		args = append(args, finalChunks)
		mark = logFinalEntryMarkSQL("$4")
	}
	tag, err := db.Exec(ctx, `
		UPDATE varroa.runs
		SET payload = (COALESCE(payload, '{}'::jsonb) - '`+indexReadFailedKey+`') || jsonb_build_object(
			'log_summary',
			COALESCE(payload->'log_summary', '{}'::jsonb) || `+mark+`
		)
		WHERE run_id = $1
		  AND (payload->'log_summary'->>'finalized') IS DISTINCT FROM 'true'
		  AND (payload->'log_summary'->>'writer_heartbeat_unix_ms') IS NOT DISTINCT FROM $2::text
		  AND `+anchorExpr+` < $3
	`, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
