package rehydrate

import (
	"testing"
	"time"
)

func recordAt(buildNumber int64, result string) Record {
	ts := time.UnixMilli(1_700_000_000_000 + buildNumber*1000)
	return Record{BuildNumber: buildNumber, QueuedAt: &ts, Result: result}
}

// records answers n records numbered highest-to-lowest (newest first,
// matching the record store's own ordering), with results taken from
// results in the same order. len(results) must equal n.
func records(results ...string) []Record {
	n := int64(len(results))
	out := make([]Record, len(results))
	for i, r := range results {
		out[i] = recordAt(n-int64(i), r)
	}
	return out
}

// TestBuildJobSeed_NormalN covers a normal N: the N newest records,
// plus the newest successful record found beyond them.
func TestBuildJobSeed_NormalN(t *testing.T) {
	// Newest first: 10 FAILURE, then a SUCCESS beyond N=3.
	jr := JobRecords{
		JobPath:         "team/api",
		NextBuildNumber: 11,
		Records: records(
			"FAILURE", "FAILURE", "FAILURE", // newest 3, all in the N window
			"SUCCESS", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE",
		),
	}
	seed, err := BuildJobSeed(jr, 3, "/var/jenkins_home", Options{})
	if err != nil {
		t.Fatalf("BuildJobSeed: %v", err)
	}
	if len(seed.records) != 4 { // 3 newest + 1 success beyond the window
		t.Fatalf("got %d records, want 4", len(seed.records))
	}
	if !seed.records[0].protected {
		t.Error("the newest record must be protected")
	}
	last := seed.records[len(seed.records)-1]
	if !last.protected {
		t.Error("the newest successful record must be protected")
	}
	if seed.NumberFile.Path != NextBuildNumberPath("team/api") {
		t.Errorf("NumberFile path = %q, want %q", seed.NumberFile.Path, NextBuildNumberPath("team/api"))
	}
}

// TestBuildJobSeed_ZeroN covers N of zero: the seed still carries the
// successful record, and nothing else.
func TestBuildJobSeed_ZeroN(t *testing.T) {
	jr := JobRecords{
		JobPath:         "team/api",
		NextBuildNumber: 5,
		Records:         records("FAILURE", "FAILURE", "SUCCESS", "FAILURE"),
	}
	seed, err := BuildJobSeed(jr, 0, "/var/jenkins_home", Options{})
	if err != nil {
		t.Fatalf("BuildJobSeed: %v", err)
	}
	if len(seed.records) != 1 {
		t.Fatalf("got %d records, want 1 (the successful record only)", len(seed.records))
	}
	if !seed.records[0].protected {
		t.Error("the sole record must be protected")
	}
	// Always seeds the nextBuildNumber file, even for N=0.
	if len(seed.NumberFile.Content) == 0 {
		t.Error("NumberFile carries no content")
	}
}

// TestBuildJobSeed_SuccessAfterManyFailures covers a long failure
// streak: the last successful record is present after twenty
// failures.
func TestBuildJobSeed_SuccessAfterManyFailures(t *testing.T) {
	results := make([]string, 0, 21)
	for i := 0; i < 20; i++ {
		results = append(results, "FAILURE")
	}
	results = append(results, "SUCCESS")
	jr := JobRecords{JobPath: "team/api", NextBuildNumber: 22, Records: records(results...)}

	seed, err := BuildJobSeed(jr, 10, "/var/jenkins_home", Options{})
	if err != nil {
		t.Fatalf("BuildJobSeed: %v", err)
	}
	if len(seed.records) != 11 { // 10 newest failures + the one success
		t.Fatalf("got %d records, want 11", len(seed.records))
	}
	oldest := seed.records[len(seed.records)-1]
	if oldest.buildNumber != 1 { // the success is the oldest (build 1 of 21)
		t.Errorf("oldest record build number = %d, want 1", oldest.buildNumber)
	}
	if !oldest.protected {
		t.Error("the successful record must be protected")
	}
}

// TestBuildJobSeed_NeverSucceeded covers a job whose every run failed:
// no successful record is added, and the seed carries exactly N.
func TestBuildJobSeed_NeverSucceeded(t *testing.T) {
	jr := JobRecords{
		JobPath:         "team/api",
		NextBuildNumber: 6,
		Records:         records("FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE"),
	}
	seed, err := BuildJobSeed(jr, 3, "/var/jenkins_home", Options{})
	if err != nil {
		t.Fatalf("BuildJobSeed: %v", err)
	}
	if len(seed.records) != 3 {
		t.Fatalf("got %d records, want 3", len(seed.records))
	}
}

func TestBuildJobSeed_RendersChangelogForRecordsWithChanges(t *testing.T) {
	rec := recordAt(1, "SUCCESS")
	rec.Changes = []Change{{CommitID: "abc", AuthorID: "ada", Message: "m", Timestamp: time.Now()}}
	jr := JobRecords{JobPath: "team/api", NextBuildNumber: 2, Records: []Record{rec}}

	seed, err := BuildJobSeed(jr, 1, "/var/jenkins_home", Options{})
	if err != nil {
		t.Fatalf("BuildJobSeed: %v", err)
	}
	if len(seed.records) != 1 {
		t.Fatalf("got %d records, want 1", len(seed.records))
	}
	if seed.records[0].changelog == nil {
		t.Fatal("expected a changelog file for a record with a change")
	}
	if seed.records[0].changelog.Path != ChangelogFilePath("team/api", 1) {
		t.Errorf("changelog path = %q, want %q", seed.records[0].changelog.Path, ChangelogFilePath("team/api", 1))
	}
}
