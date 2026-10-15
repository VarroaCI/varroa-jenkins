package rehydrate

import "sort"

// BundleBudgetBytes is the serialized RehydrationBundle's byte cap
// (design.md D3/D4): one NATS message carries the whole desired-state
// command, which also carries the JCasC, RBAC, and items documents.
const BundleBudgetBytes = 256 * 1024

// BudgetResult is what SelectWithinBudget decided for a tenant's job
// seeds: the files that fit, whether anything was dropped, and which
// jobs no bundle covers.
type BudgetResult struct {
	// Files are the raw (uncompressed) files the bundle builder
	// gzips. Never gzipped here: SelectWithinBudget gzips only to
	// measure size.
	Files []CandidateFile
	// JobPaths is every job path this selection covers, sorted. It
	// feeds the bundle's reload_items.
	JobPaths []string
	// Truncated is true once anything was dropped to fit the budget.
	Truncated bool
	// RecordCount is the number of build.xml entries included. It is
	// 0 for a number-only bundle.
	RecordCount int
	// NumberOnly is true once even the number-only seeds needed
	// trimming (design.md D4, order 4).
	NumberOnly bool
	// UnseededJobPaths lists the job paths no bundle (records or
	// number-only) covers this round, sorted.
	UnseededJobPaths []string
}

// SelectForBundle runs SelectWithinBudget at the shipped 256 KiB
// budget.
func SelectForBundle(seeds []JobSeed) (BudgetResult, error) {
	return SelectWithinBudget(seeds, BundleBudgetBytes)
}

// SelectWithinBudget applies design.md D4's selection order to seeds,
// so the caller-supplied budgetBytes bounds the resulting bundle:
//
//  1. Everything fits: send it.
//  2. Over the cap: drop every changelog file, across every seed.
//  3. Still over the cap: drop the oldest unprotected record of each
//     seed in turn, round-robin, until it fits.
//  4. The number-only seeds alone are still over the cap: send a
//     number-only bundle covering the seeds that fit, in job-path
//     order, and report the rest as unseeded.
//
// seeds need not be sorted; SelectWithinBudget orders by job path
// itself, since order 4 requires it.
func SelectWithinBudget(seeds []JobSeed, budgetBytes int) (BudgetResult, error) {
	ordered := append([]JobSeed(nil), seeds...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].JobPath < ordered[j].JobPath })

	fits, err := fitsSerializedBudget(ordered, false, false, budgetBytes)
	if err != nil {
		return BudgetResult{}, err
	}
	if fits {
		return finishResult(ordered, false, false, nil), nil
	}

	stripped := dropChangelogs(ordered)
	fits, err = fitsSerializedBudget(stripped, true, false, budgetBytes)
	if err != nil {
		return BudgetResult{}, err
	}
	if fits {
		return finishResult(stripped, true, false, nil), nil
	}

	trimmed := stripped
	for {
		dropped := false
		for i := range trimmed {
			if !dropOldest(&trimmed[i]) {
				continue
			}
			dropped = true
			fits, err = fitsSerializedBudget(trimmed, true, false, budgetBytes)
			if err != nil {
				return BudgetResult{}, err
			}
			if fits {
				return finishResult(trimmed, true, false, nil), nil
			}
		}
		if !dropped {
			break
		}
	}

	kept, unseeded, err := selectNumberOnly(trimmed, budgetBytes)
	if err != nil {
		return BudgetResult{}, err
	}
	return finishResult(kept, true, true, unseeded), nil
}

// dropOldest removes seed's oldest unprotected record, and its
// changelog if it still carries one. It answers whether it removed
// anything.
func dropOldest(seed *JobSeed) bool {
	for i := len(seed.records) - 1; i >= 0; i-- {
		if seed.records[i].protected {
			continue
		}
		seed.records = append(seed.records[:i], seed.records[i+1:]...)
		return true
	}
	return false
}

// dropChangelogs answers a copy of seeds with every record's
// changelog file removed. The owning build.xml keeps its <checkouts>
// reference to the now-absent path: a missing changelog file loses
// that run's change set on load and nothing else (design.md D1a).
func dropChangelogs(seeds []JobSeed) []JobSeed {
	out := make([]JobSeed, len(seeds))
	for i, s := range seeds {
		records := make([]jobSeedRecord, len(s.records))
		for j, r := range s.records {
			r.changelog = nil
			records[j] = r
		}
		out[i] = JobSeed{JobPath: s.JobPath, NumberFile: s.NumberFile, records: records}
	}
	return out
}

// selectNumberOnly answers the seeds' NumberFile-only reduction: the
// leading run (in seeds' existing job-path order) that fits
// budgetBytes, and the job paths from the first miss onward. The
// serialized bundle only grows as more number-only entries join it, so
// the largest fitting prefix is unique: a binary search over the
// prefix length lands on the same "stop at the first miss, in
// job-path order" cutoff a linear scan would, without re-measuring
// every shorter prefix's serialized size on every probe — a tenant
// with thousands of jobs (design.md D4) still resolves in a bounded
// number of measurements.
func selectNumberOnly(seeds []JobSeed, budgetBytes int) (kept []JobSeed, unseeded []string, err error) {
	numberSeeds := make([]JobSeed, len(seeds))
	for i, s := range seeds {
		numberSeeds[i] = JobSeed{JobPath: s.JobPath, NumberFile: s.NumberFile}
	}

	lo, hi := 0, len(numberSeeds)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		fits, ferr := fitsSerializedBudget(numberSeeds[:mid], true, true, budgetBytes)
		if ferr != nil {
			return nil, nil, ferr
		}
		if fits {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	kept = numberSeeds[:lo]
	for _, s := range numberSeeds[lo:] {
		unseeded = append(unseeded, s.JobPath)
	}
	return kept, unseeded, nil
}

// fitsSerializedBudget answers whether seeds' assembled
// RehydrationBundle — truncated and numberOnly set as the caller's
// candidate round would set them — serializes within budgetBytes.
// Design.md D3/D4: "The operator builds every seed, then measures the
// serialized bundle." A sum of the files' own gzipped lengths misses
// the message's field tags, paths, reload items, and metadata, so
// SelectWithinBudget SHALL measure the assembled bundle, not the raw
// file sum.
func fitsSerializedBudget(seeds []JobSeed, truncated, numberOnly bool, budgetBytes int) (bool, error) {
	size, err := SerializedSize(finishResult(seeds, truncated, numberOnly, nil))
	if err != nil {
		return false, err
	}
	return size <= budgetBytes, nil
}

// finishResult builds a BudgetResult from seeds after a selection
// round. numberOnly seeds carry only their NumberFile.
func finishResult(seeds []JobSeed, truncated, numberOnly bool, unseeded []string) BudgetResult {
	result := BudgetResult{
		Truncated:        truncated,
		NumberOnly:       numberOnly,
		UnseededJobPaths: unseeded,
	}
	for _, s := range seeds {
		result.JobPaths = append(result.JobPaths, s.JobPath)
		result.Files = append(result.Files, s.NumberFile)
		if numberOnly {
			continue
		}
		for _, r := range s.records {
			result.Files = append(result.Files, r.buildXML)
			result.RecordCount++
			if r.changelog != nil {
				result.Files = append(result.Files, *r.changelog)
			}
		}
	}
	return result
}
