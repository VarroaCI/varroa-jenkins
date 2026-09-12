package rehydrate

// AdvanceNumberOnlyRound implements design.md D4 order 4's per-tick
// progression (task 5.7). A caller that just found SelectForBundle's
// result NumberOnly true is in the "very large tenant" state: sending
// the same leading block of number files every tick would never reach
// the tail, because SelectWithinBudget always starts from the same
// sorted seed list. AdvanceNumberOnlyRound excludes every job path
// numbered already names — a prior tick's number-only bundle already
// covered it — selects the next block of the seeds that remain, in
// job-path order, and adds the newly covered paths into numbered.
//
// It answers this tick's BudgetResult (number-only files only) and
// whether every job in seeds now holds its number. Once that is true,
// the caller sends record bundles again (design.md D4): it re-runs
// SelectForBundle on the full, record-including seed set, which no
// longer needs the number-only reduction because numbering is the only
// reason a fresh cell's tenant reaches order 4 in the first place.
//
// numbered is the caller's own persisted state; losing it (an operator
// restart) costs a repeated round trip through jobs that already hold
// their number, which is a harmless no-op on the mite side (design.md
// D5's write rule 3: nextBuildNumber is written only when absent or
// lower).
func AdvanceNumberOnlyRound(seeds []JobSeed, numbered map[string]bool, budgetBytes int) (BudgetResult, bool, error) {
	var remaining []JobSeed
	for _, s := range seeds {
		if !numbered[s.JobPath] {
			remaining = append(remaining, JobSeed{JobPath: s.JobPath, NumberFile: s.NumberFile})
		}
	}
	if len(remaining) == 0 {
		return BudgetResult{}, true, nil
	}

	result, err := SelectWithinBudget(remaining, budgetBytes)
	if err != nil {
		return BudgetResult{}, false, err
	}
	for _, p := range result.JobPaths {
		numbered[p] = true
	}

	allNumbered := true
	for _, s := range seeds {
		if !numbered[s.JobPath] {
			allNumbered = false
			break
		}
	}
	return result, allNumbered, nil
}
