package rehydrate

import "sort"

// PendingJobPaths answers pending_jobs (design.md D13, task 5.7a): the
// union of a tenant's deferred job paths (the mite's own
// rehydration_deferred_jobs report) and the job paths its selection has
// not covered yet this round (design.md D4 order 4's unseeded job
// paths). The caller hands the result to the tenant.sync announcement
// task 4.4a sends.
//
// The answer is sorted and holds no duplicate: the execution fence reads
// it as a set, and a stable order keeps a test or a log deterministic.
func PendingJobPaths(deferred, uncovered []string) []string {
	seen := make(map[string]bool, len(deferred)+len(uncovered))
	var out []string
	for _, group := range [][]string{deferred, uncovered} {
		for _, path := range group {
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}
