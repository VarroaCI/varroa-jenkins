package rehydrate

import "testing"

// numberOnlySeed builds a seed carrying only a nextBuildNumber file, no
// records, for the number-only progression tests below.
func numberOnlySeed(t *testing.T, jobPath string, n int64) JobSeed {
	t.Helper()
	seed, err := BuildJobSeed(JobRecords{JobPath: jobPath, NextBuildNumber: n}, 0, "/var/jenkins_home", Options{})
	if err != nil {
		t.Fatalf("BuildJobSeed(%s): %v", jobPath, err)
	}
	return seed
}

// oneSeedBundleSize answers the serialized size of a number-only
// bundle carrying exactly one seed, so the test can pick a budget that
// admits exactly one seed per tick.
func oneSeedBundleSize(t *testing.T, seed JobSeed) int {
	t.Helper()
	size, err := SerializedSize(finishResult([]JobSeed{seed}, true, true, nil))
	if err != nil {
		t.Fatalf("SerializedSize: %v", err)
	}
	return size
}

// TestAdvanceNumberOnlyRound_ThreeBlocksThenRecordBundle covers task
// 5.7's acceptance: a tenant with three blocks of jobs needs three
// number-only bundles, one per tick, in job-path order, before every
// job holds its number — after which the caller sends a record bundle
// again.
func TestAdvanceNumberOnlyRound_ThreeBlocksThenRecordBundle(t *testing.T) {
	seedA := numberOnlySeed(t, "team/job-a", 1)
	seedB := numberOnlySeed(t, "team/job-b", 1)
	seedC := numberOnlySeed(t, "team/job-c", 1)
	seeds := []JobSeed{seedA, seedB, seedC}

	// A budget that admits exactly one seed's NumberFile at a time.
	budget := oneSeedBundleSize(t, seedA)

	numbered := map[string]bool{}

	result1, done1, err := AdvanceNumberOnlyRound(seeds, numbered, budget)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if done1 {
		t.Fatal("tick 1: expected numbering not yet complete")
	}
	if len(result1.JobPaths) != 1 || result1.RecordCount != 0 {
		t.Fatalf("tick 1: expected exactly one number-only file, got %+v", result1)
	}

	result2, done2, err := AdvanceNumberOnlyRound(seeds, numbered, budget)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if done2 {
		t.Fatal("tick 2: expected numbering not yet complete")
	}
	if len(result2.JobPaths) != 1 || result2.JobPaths[0] == result1.JobPaths[0] {
		t.Fatalf("tick 2: expected the next distinct job path, got %+v (tick 1 was %+v)", result2.JobPaths, result1.JobPaths)
	}

	result3, done3, err := AdvanceNumberOnlyRound(seeds, numbered, budget)
	if err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if !done3 {
		t.Fatal("tick 3: expected numbering complete after the third block")
	}
	if len(result3.JobPaths) != 1 {
		t.Fatalf("tick 3: expected the final job path, got %+v", result3.JobPaths)
	}

	covered := map[string]bool{result1.JobPaths[0]: true, result2.JobPaths[0]: true, result3.JobPaths[0]: true}
	for _, jp := range []string{"team/job-a", "team/job-b", "team/job-c"} {
		if !covered[jp] {
			t.Fatalf("expected %s covered across the three ticks, covered=%v", jp, covered)
		}
	}

	// Tick 4: every job now holds its number, so the caller re-selects
	// the full seed set and gets a record bundle again — the ordinary,
	// well-within-budget path (order 1), needing no more number-only
	// reduction.
	full, err := SelectForBundle(seeds)
	if err != nil {
		t.Fatalf("tick 4 (full selection): %v", err)
	}
	if full.NumberOnly {
		t.Fatal("tick 4: expected a record bundle, not another number-only reduction")
	}
}
