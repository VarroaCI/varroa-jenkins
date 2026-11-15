package recordstore

// Two results say that a build did not finish. Neither is a pass or a fail.
const (
	resultInterrupted = "INTERRUPTED"
	resultNotBuilt    = "NOT_BUILT"
)

// countedResultSQL is the SQL predicate of a counted run. It names the
// column result without a table qualifier. A statement that uses it must
// read exactly one relation, so the column is never ambiguous.
const countedResultSQL = `result IS NOT NULL AND result NOT IN ('` + resultInterrupted + `', '` + resultNotBuilt + `')`

// CountsTowardPassRate reports whether a run with this result counts toward
// a pass rate. A run with no result does not count. An INTERRUPTED or
// NOT_BUILT run does not count, because it says nothing about the job's
// code or tests. Every other result counts. A result other than SUCCESS
// counts against the rate. This is the Go form of countedResultSQL.
func CountsTowardPassRate(result string) bool {
	return result != "" && result != resultInterrupted && result != resultNotBuilt
}

// countedResultRunSQL is countedResultSQL qualified by the alias "r". A
// statement that joins two relations uses this form.
const countedResultRunSQL = `r.result IS NOT NULL AND r.result NOT IN ('` + resultInterrupted + `', '` + resultNotBuilt + `')`

// newestCountedRunSQL is the lateral body that finds the newest counted
// build run of one job. It reads the job from the outer alias "j", which
// names the cluster, tenant, and job_path columns. The failing-jobs read
// and the held-run rule of the run groups read both use it, so the two
// reads agree on which run a job holds.
const newestCountedRunSQL = `SELECT r.run_id, r.created_at, r.result FROM varroa.runs r
	WHERE r.cluster = j.cluster AND r.tenant = j.tenant AND r.job_path = j.job_path
		AND ` + countedResultRunSQL + ` AND ` + recordsBuildRunSQL + `
	ORDER BY r.created_at DESC, r.run_id DESC LIMIT 1`

// newestCountedRunBeforeSQL is newestCountedRunSQL limited to the runs created
// before the bound expression, which names a parameter. The run groups read
// uses it, so the held run is the newest counted run known at its window end.
// A run created at or after the window end is not held in that window.
const newestCountedRunBeforeSQL = `SELECT r.run_id, r.created_at, r.result FROM varroa.runs r
	WHERE r.cluster = j.cluster AND r.tenant = j.tenant AND r.job_path = j.job_path
		AND r.created_at < $3 AND ` + countedResultRunSQL + ` AND ` + recordsBuildRunSQL + `
	ORDER BY r.created_at DESC, r.run_id DESC LIMIT 1`
