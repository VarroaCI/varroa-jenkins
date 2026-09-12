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

// runsStatusMergeSQL is the shared CASE expression that keeps
// runs.status monotonic in the order QUEUED < RUNNING < COMPLETED, and
// keeps DUPLICATE terminal so no later event changes it. Every runs
// upsert names its own intended status as the inserted "status" column
// (literally, or through COALESCE($n, 'QUEUED') when the kind does not
// intend to change status), so EXCLUDED.status always holds that value
// here.
const runsStatusMergeSQL = `CASE
	WHEN runs.status = 'DUPLICATE' THEN runs.status
	WHEN (CASE EXCLUDED.status WHEN 'QUEUED' THEN 0 WHEN 'RUNNING' THEN 1 WHEN 'COMPLETED' THEN 2 ELSE -1 END)
	   > (CASE runs.status WHEN 'QUEUED' THEN 0 WHEN 'RUNNING' THEN 1 WHEN 'COMPLETED' THEN 2 ELSE -1 END)
	THEN EXCLUDED.status
	ELSE runs.status
END`

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
