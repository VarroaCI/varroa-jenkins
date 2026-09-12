package recordstore

import (
	"encoding/json"
	"errors"

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

// supersedeTriggerFailedSQL is the predicate a real write carries to
// supersede a stored TRIGGER_FAILED row in place. realWrite is the half the
// caller supplies: TRUE in applyRunStarted, which always establishes a real
// run, and the owner flag in applyRunCompleted. A foreign completion
// supersedes nothing, because it changes no terminal state.
//
// A TRIGGER_FAILED row is not a run record. It records that no build
// exists, so a real event replaces it rather than merging with it.
func supersedeTriggerFailedSQL(realWrite string) string {
	return `runs.result = 'TRIGGER_FAILED' AND ` + realWrite
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
// row. A TRIGGER_FAILED row is never DUPLICATE, so the order changes no
// other case.
func runsStatusMergeSQL(realWrite string) string {
	return `CASE
	WHEN ` + supersedeTriggerFailedSQL(realWrite) + ` THEN EXCLUDED.status
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
