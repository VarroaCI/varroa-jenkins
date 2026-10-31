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
