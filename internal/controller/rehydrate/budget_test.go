package rehydrate

import (
	"fmt"
	"math/rand"
	"testing"
)

// noisyBytes answers n deterministic, poorly-compressible bytes, so a
// test's gzip-based size math is predictable (repeated content would
// let gzip erase most of it).
func noisyBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

func numberFile(jobPath string, n int64) CandidateFile {
	return CandidateFile{Path: NextBuildNumberPath(jobPath), Content: []byte(formatNextBuildNumber(n))}
}

func gzLen(t *testing.T, content []byte) int {
	t.Helper()
	gz, err := gzipCompress(content)
	if err != nil {
		t.Fatalf("gzipCompress: %v", err)
	}
	return len(gz)
}

// TestSelectWithinBudget_Fits covers the "under budget" case (order
// 1): everything is included and nothing is reported truncated.
func TestSelectWithinBudget_Fits(t *testing.T) {
	seed := JobSeed{
		JobPath:    "team/api",
		NumberFile: numberFile("team/api", 5),
		records: []jobSeedRecord{
			{buildNumber: 4, protected: true, buildXML: CandidateFile{Path: BuildXMLPath("team/api", 4), Content: noisyBytes(100, 1)}},
			{buildNumber: 3, buildXML: CandidateFile{Path: BuildXMLPath("team/api", 3), Content: noisyBytes(100, 2)}},
		},
	}

	result, err := SelectWithinBudget([]JobSeed{seed}, BundleBudgetBytes)
	if err != nil {
		t.Fatalf("SelectWithinBudget: %v", err)
	}
	if result.Truncated {
		t.Error("Truncated = true, want false: the bundle fits at the default budget")
	}
	if result.NumberOnly {
		t.Error("NumberOnly = true, want false")
	}
	if result.RecordCount != 2 {
		t.Errorf("RecordCount = %d, want 2", result.RecordCount)
	}
	if len(result.Files) != 3 { // number file + 2 build.xml
		t.Errorf("got %d files, want 3", len(result.Files))
	}
	if len(result.UnseededJobPaths) != 0 {
		t.Errorf("UnseededJobPaths = %v, want none", result.UnseededJobPaths)
	}
}

// TestSelectWithinBudget_DropsChangelogsFirst covers order 2: every
// changelog file is dropped before any record.
func TestSelectWithinBudget_DropsChangelogsFirst(t *testing.T) {
	buildXML := CandidateFile{Path: BuildXMLPath("team/api", 1), Content: noisyBytes(200, 3)}
	changelog := CandidateFile{Path: ChangelogFilePath("team/api", 1), Content: noisyBytes(4000, 4)}
	seed := JobSeed{
		JobPath:    "team/api",
		NumberFile: numberFile("team/api", 2),
		records: []jobSeedRecord{
			{buildNumber: 1, protected: true, buildXML: buildXML, changelog: &changelog},
		},
	}

	withChangelog, err := SerializedSize(finishResult([]JobSeed{seed}, false, false, nil))
	if err != nil {
		t.Fatalf("SerializedSize: %v", err)
	}
	stripped := dropChangelogs([]JobSeed{seed})
	withoutChangelog, err := SerializedSize(finishResult(stripped, true, false, nil))
	if err != nil {
		t.Fatalf("SerializedSize: %v", err)
	}
	budget := withoutChangelog
	if budget >= withChangelog {
		t.Fatalf("test setup: budget %d must be under the full size %d", budget, withChangelog)
	}

	result, err := SelectWithinBudget([]JobSeed{seed}, budget)
	if err != nil {
		t.Fatalf("SelectWithinBudget: %v", err)
	}
	if !result.Truncated {
		t.Error("Truncated = false, want true")
	}
	if result.NumberOnly {
		t.Error("NumberOnly = true, want false: dropping the changelog alone must fit")
	}
	if result.RecordCount != 1 {
		t.Errorf("RecordCount = %d, want 1: the build.xml itself is kept", result.RecordCount)
	}
	for _, f := range result.Files {
		if f.Path == changelog.Path {
			t.Error("changelog file present after order 2 should have dropped it")
		}
	}
}

// TestSelectWithinBudget_DropsOldestRecordsNext covers order 3: the
// oldest unprotected record of a seed is dropped before its newest
// record or its newest successful record.
func TestSelectWithinBudget_DropsOldestRecordsNext(t *testing.T) {
	mk := func(n int64, protected bool, seedByte int64) jobSeedRecord {
		return jobSeedRecord{
			buildNumber: n,
			protected:   protected,
			buildXML:    CandidateFile{Path: BuildXMLPath("team/api", n), Content: noisyBytes(1500, seedByte)},
		}
	}
	seed := JobSeed{
		JobPath:    "team/api",
		NumberFile: numberFile("team/api", 5),
		records: []jobSeedRecord{
			mk(4, true, 10),  // newest, protected
			mk(3, false, 11), // unprotected, oldest-but-one
			mk(2, false, 12), // unprotected, dropped first (closer to the tail)
			mk(1, true, 13),  // newest successful record, protected
		},
	}

	full, err := SerializedSize(finishResult([]JobSeed{seed}, false, false, nil))
	if err != nil {
		t.Fatalf("SerializedSize: %v", err)
	}
	oneDroppedSeed := seed
	oneDroppedSeed.records = []jobSeedRecord{seed.records[0], seed.records[1], seed.records[3]} // record "2" dropped first
	oneDropped, err := SerializedSize(finishResult([]JobSeed{oneDroppedSeed}, true, false, nil))
	if err != nil {
		t.Fatalf("SerializedSize: %v", err)
	}
	protectedOnlySeed := seed
	protectedOnlySeed.records = []jobSeedRecord{seed.records[0], seed.records[3]}
	protectedOnly, err := SerializedSize(finishResult([]JobSeed{protectedOnlySeed}, true, false, nil))
	if err != nil {
		t.Fatalf("SerializedSize: %v", err)
	}
	budget := protectedOnly
	if budget >= oneDropped || oneDropped >= full {
		t.Fatalf("test setup: expected protectedOnly(%d) < oneDropped(%d) < full(%d)", protectedOnly, oneDropped, full)
	}

	result, err := SelectWithinBudget([]JobSeed{seed}, budget)
	if err != nil {
		t.Fatalf("SelectWithinBudget: %v", err)
	}
	if !result.Truncated {
		t.Error("Truncated = false, want true")
	}
	if result.NumberOnly {
		t.Error("NumberOnly = true, want false: the protected records alone must fit")
	}
	if result.RecordCount != 2 {
		t.Fatalf("RecordCount = %d, want 2 (only the protected records)", result.RecordCount)
	}
	kept := map[int64]bool{}
	for _, f := range result.Files {
		if f.Path == BuildXMLPath("team/api", 4) {
			kept[4] = true
		}
		if f.Path == BuildXMLPath("team/api", 1) {
			kept[1] = true
		}
		if f.Path == BuildXMLPath("team/api", 2) || f.Path == BuildXMLPath("team/api", 3) {
			t.Errorf("unprotected record %s survived truncation", f.Path)
		}
	}
	if !kept[4] || !kept[1] {
		t.Error("the newest record and the newest successful record must survive truncation")
	}
}

// TestSelectWithinBudget_NumberOnlyWhenNumbersAloneOverflow covers
// order 4: when even the number-only seeds exceed the budget, the
// operator sends a number-only bundle covering the seeds that fit, in
// job-path order, and records the rest as unseeded.
func TestSelectWithinBudget_NumberOnlyWhenNumbersAloneOverflow(t *testing.T) {
	jobs := []string{"e", "c", "a", "d", "b"} // deliberately unsorted
	seeds := make([]JobSeed, 0, len(jobs))
	for _, job := range jobs {
		seeds = append(seeds, JobSeed{
			JobPath:    job,
			NumberFile: numberFile(job, 1),
			records: []jobSeedRecord{
				{buildNumber: 1, protected: true, buildXML: CandidateFile{Path: BuildXMLPath(job, 1), Content: noisyBytes(5000, int64(job[0]))}},
			},
		})
	}

	twoOnly := []JobSeed{
		{JobPath: "a", NumberFile: numberFile("a", 1)},
		{JobPath: "b", NumberFile: numberFile("b", 1)},
	}
	budget, err := SerializedSize(finishResult(twoOnly, true, true, nil))
	if err != nil {
		t.Fatalf("SerializedSize: %v", err)
	}

	result, err := SelectWithinBudget(seeds, budget)
	if err != nil {
		t.Fatalf("SelectWithinBudget: %v", err)
	}
	if !result.NumberOnly {
		t.Fatal("NumberOnly = false, want true")
	}
	if !result.Truncated {
		t.Error("Truncated = false, want true")
	}
	if result.RecordCount != 0 {
		t.Errorf("RecordCount = %d, want 0 for a number-only bundle", result.RecordCount)
	}
	wantCovered := []string{"a", "b"}
	if len(result.JobPaths) != len(wantCovered) {
		t.Fatalf("JobPaths = %v, want %v", result.JobPaths, wantCovered)
	}
	for i, p := range wantCovered {
		if result.JobPaths[i] != p {
			t.Errorf("JobPaths[%d] = %q, want %q (job-path order)", i, result.JobPaths[i], p)
		}
	}
	wantUnseeded := []string{"c", "d", "e"}
	if len(result.UnseededJobPaths) != len(wantUnseeded) {
		t.Fatalf("UnseededJobPaths = %v, want %v", result.UnseededJobPaths, wantUnseeded)
	}
	for i, p := range wantUnseeded {
		if result.UnseededJobPaths[i] != p {
			t.Errorf("UnseededJobPaths[%d] = %q, want %q", i, result.UnseededJobPaths[i], p)
		}
	}
	for _, f := range result.Files {
		if f.Path == BuildXMLPath("a", 1) || f.Path == BuildXMLPath("b", 1) {
			t.Errorf("a number-only bundle must not carry a build.xml, found %s", f.Path)
		}
	}
}

// TestSelectWithinBudget_MeasuresSerializedBundleNotRawPayloads covers
// design.md D3/D4: "The operator builds every seed, then measures the
// serialized bundle." Many small number-only seeds keep the raw sum of
// their gzipped payloads well under the budget, while the assembled
// RehydrationBundle's own field tags, per-file paths, and per-file
// overhead push its serialized size over it. A budget check that
// summed the payloads alone would wrongly report this set as fitting.
func TestSelectWithinBudget_MeasuresSerializedBundleNotRawPayloads(t *testing.T) {
	const jobCount = 2000
	seeds := make([]JobSeed, jobCount)
	rawPayloadSum := 0
	for i := 0; i < jobCount; i++ {
		job := fmt.Sprintf("team/job-%04d", i)
		nf := numberFile(job, 1)
		seeds[i] = JobSeed{JobPath: job, NumberFile: nf}
		rawPayloadSum += gzLen(t, nf.Content)
	}

	if rawPayloadSum >= BundleBudgetBytes {
		t.Fatalf("test setup: raw payload sum %d must be under the budget %d", rawPayloadSum, BundleBudgetBytes)
	}

	serialized, err := SerializedSize(finishResult(seeds, false, true, nil))
	if err != nil {
		t.Fatalf("SerializedSize: %v", err)
	}
	if serialized <= BundleBudgetBytes {
		t.Fatalf("test setup: serialized size %d must exceed the budget %d for this test to prove anything", serialized, BundleBudgetBytes)
	}

	result, err := SelectWithinBudget(seeds, BundleBudgetBytes)
	if err != nil {
		t.Fatalf("SelectWithinBudget: %v", err)
	}
	if !result.NumberOnly || !result.Truncated {
		t.Fatalf("expected the serialized overflow to force a truncated number-only bundle despite the raw payload sum fitting, got NumberOnly=%v Truncated=%v", result.NumberOnly, result.Truncated)
	}
	if len(result.UnseededJobPaths) == 0 {
		t.Fatal("expected some job to go unseeded once the serialized bundle exceeds the budget")
	}
}
