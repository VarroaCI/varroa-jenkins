package recordstore

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

// runs.status values. The CHECK constraint in migration 00001 pins this
// exact set.
const (
	StatusQueued    = "QUEUED"
	StatusRunning   = "RUNNING"
	StatusCompleted = "COMPLETED"
	StatusDuplicate = "DUPLICATE"
)

// ResultTriggerFailed is the runs.result value of a build trigger Jenkins
// refused. Such a row records that no build exists. Migration 00008 widens
// runs_result_check to accept it.
const ResultTriggerFailed = "TRIGGER_FAILED"

// notTriggerFailedSQL excludes a synthetic TRIGGER_FAILED row from a query
// that asks about a real run. It says IS DISTINCT FROM, never <>: a queued
// or running row holds a null result, and <> would drop it.
const notTriggerFailedSQL = `result IS DISTINCT FROM '` + ResultTriggerFailed + `'`

// notTriggerFailedRunSQL is notTriggerFailedSQL qualified by "r", the alias
// every query that joins varroa.runs beside another table gives it.
const notTriggerFailedRunSQL = `r.` + notTriggerFailedSQL

// acceptedPayloadKeysSQL removes the two accepted keys. Every real write
// applies it before its own merge, so a row that holds a build event
// never carries them.
const acceptedPayloadKeysSQL = `- 'accepted_at' - 'accepted_instance'`

// noBuildRecordedSQL matches a row that records no build: a refused
// trigger, or an accepted trigger that no real event has yet followed.
// The COALESCE keeps it false on a null payload, which a heartbeat-first
// or tests-first placeholder holds; NOT NULL would otherwise drop a live
// run.
const noBuildRecordedSQL = `(result IS NOT DISTINCT FROM '` + ResultTriggerFailed + `' OR COALESCE(payload ? 'accepted_at', false))`

// recordsBuildSQL is noBuildRecordedSQL's negation, for a query that asks
// about a real run.
const recordsBuildSQL = `NOT ` + noBuildRecordedSQL

// recordsBuildRunSQL is recordsBuildSQL qualified by "r", the alias every
// query that joins varroa.runs beside another table gives it.
const recordsBuildRunSQL = `NOT (r.result IS NOT DISTINCT FROM '` + ResultTriggerFailed + `' OR COALESCE(r.payload ? 'accepted_at', false))`

// acceptedSupersedeKeysSQL removes the accepted keys and the interrupted
// sweep's own marking keys. A superseding write applies it to a row that
// still holds the accepted keys, so a synthetic finalized log summary
// never survives into the real run's log.
const acceptedSupersedeKeysSQL = `- 'accepted_at' - 'accepted_instance' - 'interrupted_reason' - 'log_summary'`

// supersedeSQL is the predicate a real write carries to supersede a
// stored row that records no build: a TRIGGER_FAILED row, or a row that
// still holds the accepted keys. realWrite is the half the caller
// supplies: TRUE in applyRunStarted, which always establishes a real
// run, and the owner flag in applyRunCompleted. A foreign completion
// supersedes nothing, because it changes no terminal state.
//
// A row that records no build is not a run record. It records that
// Jenkins refused the trigger, or that Jenkins accepted it and no real
// event has yet followed, so a real event replaces the row rather than
// merging with it.
func supersedeSQL(realWrite string) string {
	return `(runs.result IS NOT DISTINCT FROM '` + ResultTriggerFailed + `' OR COALESCE(runs.payload ? 'accepted_at', false)) AND ` + realWrite
}

// startLostRunSQL matches a stored row that holds a start-lost mark. The
// mark is provisional, because its owner can still build the run. Only
// applyRunStarted supersedes such a row, and only the owner's
// run.completed clears its reason. supersedeSQL never matches it, because
// the row records a build.
const startLostRunSQL = `(runs.result IS NOT DISTINCT FROM 'INTERRUPTED' AND runs.payload->>'interrupted_reason' IS NOT DISTINCT FROM '` + ReasonStartLost + `')`

// heartbeatLostRunSQL matches a stored row that holds a heartbeat-lost
// mark. The mark is provisional, because its owner can still finish the
// run. Only the owner's run.completed replaces such a row, and it also
// removes the mark's reason.
const heartbeatLostRunSQL = `(runs.result IS NOT DISTINCT FROM 'INTERRUPTED' AND runs.payload->>'interrupted_reason' IS NOT DISTINCT FROM '` + ReasonHeartbeatLost + `')`

// heartbeatLostLogSummarySQL removes the mark's incomplete_reason from the
// stored log summary when the real run.log reports the log complete. The
// mark writes incomplete true with a reason, and the real run.log, which
// the mite publishes just before run.completed, sets incomplete false but
// sets no reason. It answers an empty object otherwise, so the caller can
// merge it unconditionally.
const heartbeatLostLogSummarySQL = `CASE WHEN runs.payload->'log_summary'->>'incomplete' = 'false'
	THEN jsonb_build_object('log_summary', (runs.payload->'log_summary') - 'incomplete_reason')
	ELSE '{}'::jsonb END`

// logReasonNotFinalized is the incomplete_reason the recorder writes when
// it finalizes a log that never produced its final chunk.
const logReasonNotFinalized = "not-finalized"

// logReasonRanks is the single precedence table for a log summary's
// incomplete_reason. A higher rank names a more precise cause. A plugin
// token outranks a gateway token, and a gateway token outranks a generic
// timeout or backstop token. The gateway's logSummaryPayload applies the
// same order when it merges its own reason into a completion.
var logReasonRanks = []struct {
	token string
	rank  int
}{
	{"plugin-drop", 3},
	{"terminal-response", 3},
	{"jenkins-restart", 3},
	{"rejected", 2},
	{"gateway-gap", 2},
	{"finalize-deadline", 1},
	{logReasonNotFinalized, 1},
	{"no-summary", 1},
}

// logReasonRankSQL returns a SQL expression that answers the rank of the
// reason expr. An absent, empty, or unknown reason answers 0.
func logReasonRankSQL(expr string) string {
	sql := "(CASE " + expr
	for _, r := range logReasonRanks {
		sql += " WHEN '" + r.token + "' THEN " + strconv.Itoa(r.rank)
	}
	return sql + " ELSE 0 END)"
}

// logReasonKeepSQL returns a jsonb expression that restores the stored
// incomplete_reason after a writer merged its own reason over it. It
// answers the stored reason when that reason is not empty and its rank is
// at least the incoming rank, so the first reason wins inside one rank.
// It answers an empty object otherwise, so the caller appends it last to
// the log summary merge.
func logReasonKeepSQL(stored, incoming string) string {
	return `(CASE WHEN COALESCE(` + stored + `, '') <> '' AND ` +
		logReasonRankSQL(stored) + ` >= ` + logReasonRankSQL(incoming) +
		` THEN jsonb_build_object('incomplete_reason', ` + stored + `) ELSE '{}'::jsonb END)`
}

// triggerPayloadKeysSQL removes the four trigger keys from a stored
// payload. A superseding write applies it before its own merge, and keeps
// every other stored key.
const triggerPayloadKeysSQL = `- 'trigger_error' - 'trigger_reason_code' - 'trigger_http_status' - 'trigger_failed_at'`

// runsStatusMergeSQL is the shared CASE expression that keeps
// runs.status monotonic in the order QUEUED < RUNNING < COMPLETED, and
// keeps DUPLICATE terminal so no later event changes it. Every runs
// upsert names its own intended status as the inserted "status" column
// (literally, or through COALESCE($n, 'QUEUED') when the kind does not
// intend to change status), so EXCLUDED.status always holds that value
// here.
//
// realWrite is the second half of the supersede predicate. The supersede
// arm comes first, and the monotonic rule does not apply to a superseded
// row. Neither a TRIGGER_FAILED row nor an accepted-only row is ever
// DUPLICATE, so the order changes no other case.
func runsStatusMergeSQL(realWrite string) string {
	return `CASE
	WHEN ` + supersedeSQL(realWrite) + ` THEN EXCLUDED.status
	WHEN runs.status = 'DUPLICATE' THEN runs.status
	WHEN (CASE EXCLUDED.status WHEN 'QUEUED' THEN 0 WHEN 'RUNNING' THEN 1 WHEN 'COMPLETED' THEN 2 ELSE -1 END)
	   > (CASE runs.status WHEN 'QUEUED' THEN 0 WHEN 'RUNNING' THEN 1 WHEN 'COMPLETED' THEN 2 ELSE -1 END)
	THEN EXCLUDED.status
	ELSE runs.status
END`
}

// runIntentsBuildNumberConstraint is the UNIQUE constraint name migration
// 00001 gives run_intents(cluster, tenant, job_path, build_number). A
// violation of it is the build-number conflict D4 names.
const runIntentsBuildNumberConstraint = "run_intents_build_number_key"

// isBuildNumberConflict reports whether err is the UNIQUE violation on
// run_intents(cluster, tenant, job_path, build_number). Apply catches
// it, retries the write with a null build number, and reports the
// conflict to the caller so it can count recorder_ingest_conflict_total.
func isBuildNumberConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == runIntentsBuildNumberConstraint
}

// jsonArg returns a query argument for a jsonb column: nil for an empty
// or absent RawMessage (stores SQL NULL), or the raw bytes as a string
// for pgx to send as text, which the SQL's "::jsonb" cast accepts.
func jsonArg(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
