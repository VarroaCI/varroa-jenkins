// Tests for the run failure digest (internal/api/run_digest.go): the
// presence rule, the failed-stage choice, the log tail and its two
// fallbacks, and the failed-test counts. The log cases use the same
// file-backed blob store and index helpers the log-reader tests use.
package api

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// newDigestRun is one started, finished run: the base every digest test
// varies one field of.
func newDigestRun(runID, result string) recordstore.Run {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return recordstore.Run{RunID: runID, Result: strPtr(result), StartedAt: &started}
}

// putRawRunLogObject writes data as one whole object at key, which a
// fixture needs to store a log index the index helpers cannot express.
func putRawRunLogObject(t *testing.T, store blob.Store, key string, data []byte) {
	t.Helper()
	opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "application/x-ndjson", CreatedAt: time.Now()}
	if err := store.Put(context.Background(), key, bytes.NewReader(data), opts); err != nil {
		t.Fatalf("put object %s: %v", key, err)
	}
}

// TestComputeRunDigest_PresenceRule proves each excluded case answers no
// digest and each included case answers one. A nil result stands for a
// run whose result is not yet set, which is every queued or running run.
func TestComputeRunDigest_PresenceRule(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name    string
		result  *string
		started *time.Time
		want    bool
	}{
		{"resultNotYetSet", nil, &started, false},
		{"triggerFailed", strPtr(recordstore.ResultTriggerFailed), &started, false},
		{"interruptedNeverStarted", strPtr("INTERRUPTED"), nil, false},
		{"success", strPtr("SUCCESS"), &started, false},
		{"unstable", strPtr("UNSTABLE"), &started, true},
		{"interruptedStarted", strPtr("INTERRUPTED"), &started, true},
		{"failure", strPtr("FAILURE"), &started, true},
		{"aborted", strPtr("ABORTED"), &started, true},
		{"notBuilt", strPtr("NOT_BUILT"), &started, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := recordstore.Run{RunID: "r1", Result: c.result, StartedAt: c.started}
			got := computeRunDigest(context.Background(), nil, run, nil)
			if (got != nil) != c.want {
				t.Fatalf("digest present = %v, want %v", got != nil, c.want)
			}
		})
	}
}

// TestComputeRunDigest_FirstFailedStageWins proves the digest names the
// earliest failed stage in the order the read answers it, never a later
// one, and that a failed stage with no error text leaves error absent.
func TestComputeRunDigest_FirstFailedStageWins(t *testing.T) {
	run := newDigestRun("r1", "FAILURE")
	stages := []recordstore.Stage{
		{RunID: "r1", StageID: "s1", Name: "Checkout", Status: "SUCCESS"},
		{RunID: "r1", StageID: "s2", Name: "Build", Status: "FAILURE", Error: strPtr("compiler error")},
		{RunID: "r1", StageID: "s3", Name: "Test", Status: "FAILURE", Error: strPtr("later error")},
	}
	got := computeRunDigest(context.Background(), nil, run, stages)
	if got.Stage != "Build" || got.Error != "compiler error" {
		t.Fatalf("digest stage/error = %q/%q, want Build/compiler error", got.Stage, got.Error)
	}

	noError := []recordstore.Stage{{RunID: "r1", StageID: "s4", Name: "Deploy", Status: "FAILURE"}}
	got = computeRunDigest(context.Background(), nil, run, noError)
	if got.Stage != "Deploy" || got.Error != "" {
		t.Fatalf("digest stage/error = %q/%q, want Deploy and no error", got.Stage, got.Error)
	}
}

// TestComputeRunDigest_LogTailNamesTheFailedStage proves the digest keeps
// the lines the log index attributes to the failed stage, and drops the
// lines of every other stage.
func TestComputeRunDigest_LogTailNamesTheFailedStage(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-digest-attributed"
	console := []byte("prepare\nbuild ok\ndeploy refused\n")
	entries := []runLogIndexEntry{
		{Seq: 0, StageID: "s1", Offset: 0, Len: 8},
		{Seq: 1, StageID: "s1", Offset: 8, Len: 9},
		{Seq: 2, StageID: "s2", Offset: 17, Len: 15, Final: true},
	}
	putRunLogConsole(t, store, runID, console)
	putRunLogIndex(t, store, runID, entries)

	run := newDigestRun(runID, "FAILURE")
	stages := []recordstore.Stage{
		{RunID: runID, StageID: "s1", Name: "Build", Status: "SUCCESS"},
		{RunID: runID, StageID: "s2", Name: "Deploy", Status: "FAILURE", Error: strPtr("deploy refused")},
	}
	got := computeRunDigest(context.Background(), store, run, stages)
	if got.Stage != "Deploy" || got.Error != "deploy refused" {
		t.Fatalf("digest stage/error = %q/%q, want Deploy/deploy refused", got.Stage, got.Error)
	}
	if got.LogTail != "deploy refused" {
		t.Fatalf("logTail = %q, want %q", got.LogTail, "deploy refused")
	}
}

// TestComputeRunDigest_LogTailKeepsTheLastFiftyLines proves the digest
// bounds the tail at fifty lines, counted back from the last line.
func TestComputeRunDigest_LogTailKeepsTheLastFiftyLines(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-digest-fifty"
	var console strings.Builder
	var entries []runLogIndexEntry
	for i := 1; i <= 60; i++ {
		line := fmt.Sprintf("line%02d\n", i)
		entries = append(entries, runLogIndexEntry{Seq: int64(i - 1), StageID: "s1", Offset: int64(console.Len()), Len: int64(len(line)), Final: i == 60})
		console.WriteString(line)
	}
	putRunLogConsole(t, store, runID, []byte(console.String()))
	putRunLogIndex(t, store, runID, entries)

	run := newDigestRun(runID, "FAILURE")
	stages := []recordstore.Stage{{RunID: runID, StageID: "s1", Name: "Build", Status: "FAILURE"}}
	got := computeRunDigest(context.Background(), store, run, stages)

	var want []string
	for i := 11; i <= 60; i++ {
		want = append(want, fmt.Sprintf("line%02d", i))
	}
	if got.LogTail != strings.Join(want, "\n") {
		t.Fatalf("logTail = %q, want the last fifty lines %q", got.LogTail, strings.Join(want, "\n"))
	}
}

// TestComputeRunDigest_WholeLogFallback proves the digest falls back to
// the whole log when no failed stage names one: a run with no failed
// stage, a failed stage the index does not name, and an existing index
// object that names no entry all answer the same tail.
func TestComputeRunDigest_WholeLogFallback(t *testing.T) {
	console := []byte("prepare\nbuild ok\ndeploy refused\n")
	entries := []runLogIndexEntry{
		{Seq: 0, StageID: "s1", Offset: 0, Len: 8},
		{Seq: 1, StageID: "s1", Offset: 8, Len: 9},
		{Seq: 2, StageID: "s2", Offset: 17, Len: 15, Final: true},
	}
	want := "prepare\nbuild ok\ndeploy refused"

	t.Run("noFailedStage", func(t *testing.T) {
		store := newRunLogTestStore(t)
		runID := "run-digest-fallback-no-failure"
		putRunLogConsole(t, store, runID, console)
		putRunLogIndex(t, store, runID, entries)

		run := newDigestRun(runID, "ABORTED")
		stages := []recordstore.Stage{{RunID: runID, StageID: "s1", Name: "Build", Status: "SUCCESS"}}
		got := computeRunDigest(context.Background(), store, run, stages)
		if got.Stage != "" {
			t.Fatalf("stage = %q, want no stage", got.Stage)
		}
		if got.LogTail != want {
			t.Fatalf("logTail = %q, want the whole log %q", got.LogTail, want)
		}
	})

	t.Run("noIndexEntryNamesTheFailedStage", func(t *testing.T) {
		store := newRunLogTestStore(t)
		runID := "run-digest-fallback-unattributed"
		putRunLogConsole(t, store, runID, console)
		putRunLogIndex(t, store, runID, entries)

		run := newDigestRun(runID, "FAILURE")
		stages := []recordstore.Stage{{RunID: runID, StageID: "s3", Name: "Deploy", Status: "FAILURE"}}
		got := computeRunDigest(context.Background(), store, run, stages)
		if got.Stage != "Deploy" {
			t.Fatalf("stage = %q, want Deploy", got.Stage)
		}
		if got.LogTail != want {
			t.Fatalf("logTail = %q, want the whole log %q", got.LogTail, want)
		}
	})

	t.Run("indexObjectNamesNoEntry", func(t *testing.T) {
		store := newRunLogTestStore(t)
		runID := "run-digest-fallback-empty-index"
		putRunLogConsole(t, store, runID, console)
		// An index object that holds one blank line: the parser skips a
		// blank line, so the object decodes to no entry at all.
		putRawRunLogObject(t, store, blob.LogIndexKey(runID), []byte("\n"))

		run := newDigestRun(runID, "FAILURE")
		stages := []recordstore.Stage{{RunID: runID, StageID: "s1", Name: "Build", Status: "FAILURE"}}
		got := computeRunDigest(context.Background(), store, run, stages)
		if got.LogTail != want {
			t.Fatalf("logTail = %q, want the whole log %q", got.LogTail, want)
		}
	})
}

// TestComputeRunDigest_HardReadFailure proves a hard read failure leaves
// the rest of the digest standing: a nil blob store and an undecodable
// log-index entry both answer no log tail, no error, and the failed
// stage the row names.
func TestComputeRunDigest_HardReadFailure(t *testing.T) {
	stages := []recordstore.Stage{{RunID: "r1", StageID: "s2", Name: "Deploy", Status: "FAILURE", Error: strPtr("deploy refused")}}

	t.Run("nilBlobStore", func(t *testing.T) {
		run := newDigestRun("r1", "FAILURE")
		got := computeRunDigest(context.Background(), nil, run, stages)
		if got == nil {
			t.Fatal("digest = nil, want a digest with no log tail")
		}
		if got.LogTail != "" {
			t.Fatalf("logTail = %q, want absent", got.LogTail)
		}
		if got.Stage != "Deploy" || got.Error != "deploy refused" {
			t.Fatalf("digest stage/error = %q/%q, want Deploy/deploy refused", got.Stage, got.Error)
		}
	})

	t.Run("undecodableIndexEntry", func(t *testing.T) {
		store := newRunLogTestStore(t)
		runID := "run-digest-bad-index"
		putRunLogConsole(t, store, runID, []byte("deploy refused\n"))
		putRawRunLogObject(t, store, blob.LogIndexKey(runID), []byte("{not-json}\n"))

		run := newDigestRun(runID, "FAILURE")
		stage := []recordstore.Stage{{RunID: runID, StageID: "s2", Name: "Deploy", Status: "FAILURE", Error: strPtr("deploy refused")}}
		got := computeRunDigest(context.Background(), store, run, stage)
		if got == nil {
			t.Fatal("digest = nil, want a digest with no log tail")
		}
		if got.LogTail != "" {
			t.Fatalf("logTail = %q, want absent", got.LogTail)
		}
		if got.Stage != "Deploy" {
			t.Fatalf("stage = %q, want Deploy", got.Stage)
		}
	})

	t.Run("emptyLogIsNotAFailure", func(t *testing.T) {
		store := newRunLogTestStore(t)
		runID := "run-digest-empty-log"
		run := newDigestRun(runID, "FAILURE")
		stage := []recordstore.Stage{{RunID: runID, StageID: "s2", Name: "Deploy", Status: "FAILURE"}}
		got := computeRunDigest(context.Background(), store, run, stage)
		if got == nil {
			t.Fatal("digest = nil, want a digest with no log tail")
		}
		if got.LogTail != "" {
			t.Fatalf("logTail = %q, want absent", got.LogTail)
		}
	})
}

// TestComputeRunDigest_FailedTests proves the failed-test part stays
// absent for a run that records no failed test, and that it carries the
// counts the run records otherwise.
func TestComputeRunDigest_FailedTests(t *testing.T) {
	zero := int32(0)
	failed := int32(3)
	skipped := int32(2)
	total := int32(20)

	t.Run("absentWhenTheRunRecordsNoFailedTest", func(t *testing.T) {
		run := newDigestRun("r1", "FAILURE")
		run.TestsFailed = &zero
		got := computeRunDigest(context.Background(), nil, run, nil)
		if got.FailedTests != nil {
			t.Fatalf("failedTests = %+v, want absent", got.FailedTests)
		}
	})

	t.Run("absentWhenTheRunRecordsNoTestCount", func(t *testing.T) {
		got := computeRunDigest(context.Background(), nil, newDigestRun("r1", "FAILURE"), nil)
		if got.FailedTests != nil {
			t.Fatalf("failedTests = %+v, want absent", got.FailedTests)
		}
	})

	t.Run("presentWithEveryCount", func(t *testing.T) {
		run := newDigestRun("r1", "UNSTABLE")
		run.TestsFailed, run.TestsSkipped, run.TestsTotal = &failed, &skipped, &total
		got := computeRunDigest(context.Background(), nil, run, nil)
		want := &digestFailedTestsJSON{Failed: 3, Skipped: 2, Total: 20}
		if got.FailedTests == nil || *got.FailedTests != *want {
			t.Fatalf("failedTests = %+v, want %+v", got.FailedTests, want)
		}
	})

	t.Run("presentWithOnlyTheFailedCount", func(t *testing.T) {
		run := newDigestRun("r1", "UNSTABLE")
		run.TestsFailed = &failed
		got := computeRunDigest(context.Background(), nil, run, nil)
		want := &digestFailedTestsJSON{Failed: 3}
		if got.FailedTests == nil || *got.FailedTests != *want {
			t.Fatalf("failedTests = %+v, want %+v", got.FailedTests, want)
		}
	})
}
