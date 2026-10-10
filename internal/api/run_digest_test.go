// Tests for the run failure digest (internal/api/run_digest.go): the
// presence rule, the failed-stage choice, the log tail and its two
// fallbacks, the failed-test counts, and the failed case list. The log
// cases use the same file-backed blob store and index helpers the
// log-reader tests use.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
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

// digestAt answers a stage start time minutes after a fixed base.
func digestAt(minutes int) *time.Time {
	v := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)
	return &v
}

// digestStage builds one stage row. An empty parentID gives a top-level
// row. A non-empty branchName gives a row whose stored envelope carries
// that name as parallel_branch_name, the way ingest stores it.
func digestStage(runID, stageID, name, nodeType, status, parentID, branchName string, startedAt *time.Time) recordstore.Stage {
	s := recordstore.Stage{RunID: runID, StageID: stageID, Name: name, NodeType: nodeType, Status: status, StartedAt: startedAt}
	if parentID != "" {
		s.ParentID = &parentID
	}
	if branchName != "" {
		s.Payload = json.RawMessage(fmt.Sprintf(`{"kind":"run.stage","payload":{"stage_id":%q,"parallel_branch_name":%q}}`, stageID, branchName))
	}
	return s
}

// digestLog stores a console log whose lines carry the given stage ids,
// one index entry for each line. Each pair is a stage id and a line.
func digestLog(t *testing.T, store blob.Store, runID string, lines [][2]string) {
	t.Helper()
	var console strings.Builder
	entries := make([]runLogIndexEntry, 0, len(lines))
	for i, ln := range lines {
		text := ln[1] + "\n"
		entries = append(entries, runLogIndexEntry{
			Seq: int64(i), StageID: ln[0], Offset: int64(console.Len()), Len: int64(len(text)), Final: i == len(lines)-1,
		})
		console.WriteString(text)
	}
	putRunLogConsole(t, store, runID, []byte(console.String()))
	putRunLogIndex(t, store, runID, entries)
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
	one := int32(1)
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
			// Each run reports one failed test, so every included case has a part.
			run := recordstore.Run{RunID: "r1", Result: c.result, StartedAt: c.started, TestsFailed: &one}
			got := computeRunDigest(context.Background(), nil, run, nil, runTestCaseRead{})
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
		{RunID: "r1", StageID: "s1", Name: "Checkout", Status: "SUCCESS", StartedAt: digestAt(0)},
		{RunID: "r1", StageID: "s2", Name: "Build", Status: "FAILURE", Error: strPtr("compiler error"), StartedAt: digestAt(1)},
		{RunID: "r1", StageID: "s3", Name: "Test", Status: "FAILURE", Error: strPtr("later error"), StartedAt: digestAt(2)},
	}
	got := computeRunDigest(context.Background(), nil, run, stages, runTestCaseRead{})
	if got.Stage != "Build" || got.Error != "compiler error" {
		t.Fatalf("digest stage/error = %q/%q, want Build/compiler error", got.Stage, got.Error)
	}

	noError := []recordstore.Stage{{RunID: "r1", StageID: "s4", Name: "Deploy", Status: "FAILURE"}}
	got = computeRunDigest(context.Background(), nil, run, noError, runTestCaseRead{})
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
	got := computeRunDigest(context.Background(), store, run, stages, runTestCaseRead{})
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
	got := computeRunDigest(context.Background(), store, run, stages, runTestCaseRead{})

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
		got := computeRunDigest(context.Background(), store, run, stages, runTestCaseRead{})
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
		got := computeRunDigest(context.Background(), store, run, stages, runTestCaseRead{})
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
		got := computeRunDigest(context.Background(), store, run, stages, runTestCaseRead{})
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
		got := computeRunDigest(context.Background(), nil, run, stages, runTestCaseRead{})
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
		got := computeRunDigest(context.Background(), store, run, stage, runTestCaseRead{})
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
		got := computeRunDigest(context.Background(), store, run, stage, runTestCaseRead{})
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

	// A FAILURE stage keeps the digest present, so each case reads the
	// failedTests part alone.
	failedStage := []recordstore.Stage{{RunID: "r1", StageID: "s1", Name: "Build", Status: "FAILURE"}}

	t.Run("absentWhenTheRunRecordsNoFailedTest", func(t *testing.T) {
		run := newDigestRun("r1", "FAILURE")
		run.TestsFailed = &zero
		got := computeRunDigest(context.Background(), nil, run, failedStage, runTestCaseRead{})
		if got.FailedTests != nil {
			t.Fatalf("failedTests = %+v, want absent", got.FailedTests)
		}
	})

	t.Run("absentWhenTheRunRecordsNoTestCount", func(t *testing.T) {
		got := computeRunDigest(context.Background(), nil, newDigestRun("r1", "FAILURE"), failedStage, runTestCaseRead{})
		if got.FailedTests != nil {
			t.Fatalf("failedTests = %+v, want absent", got.FailedTests)
		}
	})

	t.Run("presentWithEveryCount", func(t *testing.T) {
		run := newDigestRun("r1", "UNSTABLE")
		run.TestsFailed, run.TestsSkipped, run.TestsTotal = &failed, &skipped, &total
		got := computeRunDigest(context.Background(), nil, run, nil, runTestCaseRead{})
		want := &digestFailedTestsJSON{Failed: 3, Skipped: 2, Total: 20}
		if got.FailedTests == nil || !reflect.DeepEqual(*got.FailedTests, *want) {
			t.Fatalf("failedTests = %+v, want %+v", got.FailedTests, want)
		}
	})

	t.Run("presentWithOnlyTheFailedCount", func(t *testing.T) {
		run := newDigestRun("r1", "UNSTABLE")
		run.TestsFailed = &failed
		got := computeRunDigest(context.Background(), nil, run, nil, runTestCaseRead{})
		want := &digestFailedTestsJSON{Failed: 3}
		if got.FailedTests == nil || !reflect.DeepEqual(*got.FailedTests, *want) {
			t.Fatalf("failedTests = %+v, want %+v", got.FailedTests, want)
		}
	})
}

// TestComputeRunDigest_FailedCases proves the failed case list: at most
// ten entries, SKIPPED rows left out, the reader's own order kept, and the
// list absent when the run holds no case row or the read fails.
func TestComputeRunDigest_FailedCases(t *testing.T) {
	failed := int32(40)
	newRun := func() recordstore.Run {
		run := newDigestRun("r1", "UNSTABLE")
		run.TestsFailed = &failed
		return run
	}

	t.Run("boundedAtTen", func(t *testing.T) {
		var rows []recordstore.TestCase
		for i := 0; i < 40; i++ {
			rows = append(rows, recordstore.TestCase{Suite: "unit", ClassName: "C", Name: fmt.Sprintf("t%02d", i), Status: "FAILED"})
		}
		rows = append(rows, recordstore.TestCase{Suite: "unit", ClassName: "C", Name: "skipped", Status: "SKIPPED"})
		got := computeRunDigest(context.Background(), nil, newRun(), nil, runTestCaseRead{cases: rows})
		if got.FailedTests == nil || len(got.FailedTests.Cases) != 10 {
			t.Fatalf("failedTests = %+v, want ten cases", got.FailedTests)
		}
		if got.FailedTests.Failed != 40 {
			t.Fatalf("failed = %d, want the aggregate count 40", got.FailedTests.Failed)
		}
		first := got.FailedTests.Cases[0]
		want := digestFailedCaseJSON{Suite: "unit", ClassName: "C", Name: "t00", CaseKey: TestCaseKey("C", "t00")}
		if first != want || got.FailedTests.Cases[9].Name != "t09" {
			t.Fatalf("cases = %+v, want the first ten in the reader's order", got.FailedTests.Cases)
		}
	})

	t.Run("skippedRowsLeftOut", func(t *testing.T) {
		rows := []recordstore.TestCase{
			{Suite: "unit", ClassName: "C", Name: "regressed", Status: "REGRESSION"},
			{Suite: "unit", ClassName: "C", Name: "skipped", Status: "SKIPPED"},
		}
		got := computeRunDigest(context.Background(), nil, newRun(), nil, runTestCaseRead{cases: rows})
		if got.FailedTests == nil || len(got.FailedTests.Cases) != 1 || got.FailedTests.Cases[0].Name != "regressed" {
			t.Fatalf("cases = %+v, want the one case that is not SKIPPED", got.FailedTests)
		}
	})

	t.Run("absentWithNoCaseRow", func(t *testing.T) {
		got := computeRunDigest(context.Background(), nil, newRun(), nil, runTestCaseRead{})
		if got.FailedTests == nil || got.FailedTests.Cases != nil {
			t.Fatalf("failedTests = %+v, want the counts and no case list", got.FailedTests)
		}
	})

	t.Run("absentWhenTheReadFails", func(t *testing.T) {
		rows := []recordstore.TestCase{{Suite: "unit", ClassName: "C", Name: "t", Status: "FAILED"}}
		got := computeRunDigest(context.Background(), nil, newRun(), nil, runTestCaseRead{cases: rows, err: fmt.Errorf("read timeout")})
		if got.FailedTests == nil || got.FailedTests.Failed != 40 || got.FailedTests.Cases != nil {
			t.Fatalf("failedTests = %+v, want the counts and no case list", got.FailedTests)
		}
	})
}

// TestHandleGetRun_FailedCaseReadDegradesTheListAlone proves a failed case
// read leaves the run read and the digest standing, with no case list on
// either.
func TestHandleGetRun_FailedCaseReadDegradesTheListAlone(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	failed := int32(1)
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result: strPtr("FAILURE"), StartedAt: &started, TestsFailed: &failed,
	}
	reader.stages["r1"] = []recordstore.Stage{
		{RunID: "r1", StageID: "s1", Name: "Test", Status: "FAILURE", Error: strPtr("tests failed")},
	}
	reader.testCases["r1"] = []recordstore.TestCase{{Suite: "unit", ClassName: "C", Name: "t", Status: "FAILED"}}
	reader.testCasesErr = fmt.Errorf("read timeout")
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200: a failed case read must not fail the run read", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Digest == nil || detail.Digest.Stage != "Test" || detail.Digest.FailedTests == nil || detail.Digest.FailedTests.Failed != 1 {
		t.Fatalf("digest = %+v, want the stage and the counts", detail.Digest)
	}
	if detail.Digest.FailedTests.Cases != nil || detail.Cases != nil {
		t.Fatalf("case lists = %+v / %+v, want both absent", detail.Digest.FailedTests.Cases, detail.Cases)
	}

	reader.testCasesErr = nil
	rec = doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(detail.Digest.FailedTests.Cases) != 1 || detail.Digest.FailedTests.Cases[0].CaseKey != "C/t" {
		t.Fatalf("digest cases = %+v, want the one failed case", detail.Digest.FailedTests.Cases)
	}
}

// TestComputeRunDigest_ParallelFailureNamesTheInnerStage proves the live
// shape: a failed stage inside a parallel branch fails the branch row and
// the outer stage row too. The digest names the inner stage and its
// error, and the tail keeps the lines of the inner stage alone.
func TestComputeRunDigest_ParallelFailureNamesTheInnerStage(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-digest-parallel"
	digestLog(t, store, runID, [][2]string{
		{"3", "starting CI"},
		{"8", "lint ok"},
		{"7", "go build ./..."},
		{"8", "lint done"},
		{"7", "main.go:3: undefined: x"},
	})
	stages := []recordstore.Stage{
		digestStage(runID, "3", "CI", "stage", "FAILURE", "", "", digestAt(0)),
		digestStage(runID, "5", "Branch: Test", "parallel", "FAILURE", "3", "Test", digestAt(1)),
		digestStage(runID, "6", "Branch: Lint", "parallel", "SUCCESS", "3", "Lint", digestAt(1)),
		digestStage(runID, "7", "Test", "stage", "FAILURE", "5", "", digestAt(2)),
		digestStage(runID, "8", "Lint", "stage", "SUCCESS", "6", "", digestAt(2)),
	}
	stages[0].Error = strPtr("outer error")
	stages[1].Error = strPtr("branch error")
	stages[3].Error = strPtr("script returned exit code 1")

	got := computeRunDigest(context.Background(), store, newDigestRun(runID, "FAILURE"), stages, runTestCaseRead{})
	if got.Stage != "Test" || got.Error != "script returned exit code 1" {
		t.Fatalf("digest stage/error = %q/%q, want Test/script returned exit code 1", got.Stage, got.Error)
	}
	if want := "go build ./...\nmain.go:3: undefined: x"; got.LogTail != want {
		t.Fatalf("logTail = %q, want %q", got.LogTail, want)
	}
}

// TestComputeRunDigest_EarliestStartedFailedBranchWins proves that of two
// failed branches the digest names the failure that started first. The
// stages arrive in the stage-start order of the reader.
func TestComputeRunDigest_EarliestStartedFailedBranchWins(t *testing.T) {
	runID := "r1"
	stages := []recordstore.Stage{
		digestStage(runID, "3", "CI", "stage", "FAILURE", "", "", digestAt(0)),
		digestStage(runID, "5", "Branch: Bravo", "parallel", "FAILURE", "3", "Bravo", digestAt(1)),
		digestStage(runID, "9", "Bravo build", "stage", "FAILURE", "5", "", digestAt(2)),
		digestStage(runID, "4", "Branch: Alpha", "parallel", "FAILURE", "3", "Alpha", digestAt(3)),
		digestStage(runID, "11", "Alpha build", "stage", "FAILURE", "4", "", digestAt(4)),
	}
	got := computeRunDigest(context.Background(), nil, newDigestRun(runID, "FAILURE"), stages, runTestCaseRead{})
	if got.Stage != "Bravo build" {
		t.Fatalf("digest stage = %q, want Bravo build", got.Stage)
	}
}

// TestComputeRunDigest_ScriptedBranchWithNoInnerStage proves a failed
// scripted parallel branch that holds no stage of its own is the failure
// the digest names, by its branch name. A parallel row with no stored
// branch name keeps its row name.
func TestComputeRunDigest_ScriptedBranchWithNoInnerStage(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-digest-scripted"
	digestLog(t, store, runID, [][2]string{
		{"5", "alpha-1"},
		{"6", "bravo-1"},
		{"6", "red"},
		{"7", "charlie-1"},
	})
	stages := []recordstore.Stage{
		digestStage(runID, "3", "Fan out", "stage", "FAILURE", "", "", digestAt(0)),
		digestStage(runID, "5", "Branch: Alpha", "parallel", "SUCCESS", "3", "Alpha", digestAt(1)),
		digestStage(runID, "6", "Branch: Bravo", "parallel", "FAILURE", "3", "Bravo", digestAt(1)),
		digestStage(runID, "7", "Branch: Charlie", "parallel", "SUCCESS", "3", "Charlie", digestAt(1)),
	}
	stages[2].Error = strPtr("script returned exit code 3")

	got := computeRunDigest(context.Background(), store, newDigestRun(runID, "FAILURE"), stages, runTestCaseRead{})
	if got.Stage != "Bravo" || got.Error != "script returned exit code 3" {
		t.Fatalf("digest stage/error = %q/%q, want Bravo/script returned exit code 3", got.Stage, got.Error)
	}
	if want := "bravo-1\nred"; got.LogTail != want {
		t.Fatalf("logTail = %q, want %q", got.LogTail, want)
	}

	stages[2].Payload = nil
	got = computeRunDigest(context.Background(), nil, newDigestRun(runID, "FAILURE"), stages, runTestCaseRead{})
	if got.Stage != "Branch: Bravo" {
		t.Fatalf("digest stage = %q, want the row name Branch: Bravo", got.Stage)
	}
}

// TestComputeRunDigest_LogTailKeepsTheNestedStages proves the tail of a
// failed stage keeps the lines of its nested stages too, and drops the
// lines of every stage outside it.
func TestComputeRunDigest_LogTailKeepsTheNestedStages(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-digest-subtree"
	digestLog(t, store, runID, [][2]string{
		{"2", "checkout"},
		{"4", "prepare deploy"},
		{"5", "render manifests"},
		{"9", "notify"},
		{"4", "deploy refused"},
	})
	stages := []recordstore.Stage{
		digestStage(runID, "2", "Checkout", "stage", "SUCCESS", "", "", digestAt(0)),
		digestStage(runID, "4", "Deploy", "stage", "FAILURE", "", "", digestAt(1)),
		digestStage(runID, "5", "Render", "stage", "SUCCESS", "4", "", digestAt(2)),
		digestStage(runID, "9", "Notify", "stage", "SUCCESS", "", "", digestAt(3)),
	}
	got := computeRunDigest(context.Background(), store, newDigestRun(runID, "FAILURE"), stages, runTestCaseRead{})
	if got.Stage != "Deploy" {
		t.Fatalf("digest stage = %q, want Deploy", got.Stage)
	}
	if want := "prepare deploy\nrender manifests\ndeploy refused"; got.LogTail != want {
		t.Fatalf("logTail = %q, want %q", got.LogTail, want)
	}
}

// TestComputeRunDigest_LogTailHasNoANSIEscapes proves the tail carries
// no ANSI escape sequence and no escape byte at all.
func TestComputeRunDigest_LogTailHasNoANSIEscapes(t *testing.T) {
	store := newRunLogTestStore(t)
	runID := "run-digest-ansi"
	digestLog(t, store, runID, [][2]string{
		{"4", "\x1b[31mred\x1b[0m"},
		{"4", "\x1b[1;32mok\x1b[m done"},
		{"4", "\x1b]0;title\x07plain\x1b[2K"},
		{"4", "lone escape\x1b"},
		{"4", "\x1b(Bcharset\x1b7 saved\x1b8"},
		{"4", "cut\x1b]0;build title"},
	})
	stages := []recordstore.Stage{digestStage(runID, "4", "Build", "stage", "FAILURE", "", "", digestAt(0))}
	got := computeRunDigest(context.Background(), store, newDigestRun(runID, "FAILURE"), stages, runTestCaseRead{})
	if want := "red\nok done\nplain\nlone escape\ncharset saved\ncut"; got.LogTail != want {
		t.Fatalf("logTail = %q, want %q", got.LogTail, want)
	}
	if strings.ContainsRune(got.LogTail, 0x1b) {
		t.Fatalf("logTail = %q, want no escape byte", got.LogTail)
	}
}

// TestComputeRunDigest_EmptyDigestIsAbsent proves a run with no part that
// has a value answers no digest, so the build page draws no empty card.
func TestComputeRunDigest_EmptyDigestIsAbsent(t *testing.T) {
	ctx := context.Background()
	if got := computeRunDigest(ctx, nil, newDigestRun("r1", "FAILURE"), nil, runTestCaseRead{}); got != nil {
		t.Fatalf("digest without a store = %+v, want nil", got)
	}
	store := newRunLogTestStore(t)
	if got := computeRunDigest(ctx, store, newDigestRun("r1", "FAILURE"), nil, runTestCaseRead{}); got != nil {
		t.Fatalf("digest with an empty log = %+v, want nil", got)
	}
}

// TestComputeRunDigest_InterruptedRunCarriesNoLogTail proves an interrupted
// run keeps its failed stage and failed tests, and never a log tail.
func TestComputeRunDigest_InterruptedRunCarriesNoLogTail(t *testing.T) {
	ctx := context.Background()
	const runID = "r1"
	store := newRunLogTestStore(t)
	putRunLogConsole(t, store, runID, []byte("apiVersion: v1\nkind: Pod\n"))

	t.Run("noPartIsNil", func(t *testing.T) {
		if got := computeRunDigest(ctx, store, newDigestRun(runID, "INTERRUPTED"), nil, runTestCaseRead{}); got != nil {
			t.Fatalf("digest = %+v, want nil", got)
		}
	})

	t.Run("failedStageKeepsTheStageAndDropsTheTail", func(t *testing.T) {
		stages := []recordstore.Stage{digestStage(runID, "s1", "Deploy", "", "FAILURE", "", "", nil)}
		got := computeRunDigest(ctx, store, newDigestRun(runID, "INTERRUPTED"), stages, runTestCaseRead{})
		if got == nil || got.Stage != "Deploy" {
			t.Fatalf("digest = %+v, want stage Deploy", got)
		}
		if got.LogTail != "" {
			t.Fatalf("logTail = %q, want absent", got.LogTail)
		}
	})

	t.Run("failedTestKeepsTheCountAndDropsTheTail", func(t *testing.T) {
		run := newDigestRun(runID, "INTERRUPTED")
		failed := int32(1)
		run.TestsFailed = &failed
		got := computeRunDigest(ctx, store, run, nil, runTestCaseRead{})
		if got == nil || got.FailedTests == nil {
			t.Fatalf("digest = %+v, want failedTests", got)
		}
		if got.LogTail != "" {
			t.Fatalf("logTail = %q, want absent", got.LogTail)
		}
	})
}
