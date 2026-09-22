// Package api: the failure digest the run detail carries. A digest
// names why a run that did not succeed failed: the first failed stage,
// its error, the tail of its log, and the failed test counts. It is a
// read composition, computed fresh on every read from stored rows and
// the stored log, never a second stored copy of those facts.
//
// A hard read failure — a nil blob store, or an undecodable log-index
// entry — degrades logTail to absent and never fails the run read, the
// same way an unreadable source column never fails it. This is a
// deliberate, named exception to internal/api/AGENTS.md's rule that a
// handler which reads console log or artifact bytes answers 503 when
// deps.BlobStore is nil: GET /runs/{run} serves a complete response
// with or without digest.logTail.
package api

import (
	"context"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// runDigestTailLines bounds how many lines of the log tail a digest
// carries, counted back from the last line of the attributed set.
const runDigestTailLines = 50

type runDigestJSON struct {
	Stage       string                 `json:"stage,omitempty"`
	Error       string                 `json:"error,omitempty"`
	LogTail     string                 `json:"logTail,omitempty"`
	FailedTests *digestFailedTestsJSON `json:"failedTests,omitempty"`
}

// digestFailedTestsJSON is the failed-test part of a digest. Failed is
// the one count that makes the part present, so it carries no omitempty.
type digestFailedTestsJSON struct {
	Failed  int32 `json:"failed"`
	Skipped int32 `json:"skipped,omitempty"`
	Total   int32 `json:"total,omitempty"`
}

// digestApplies reports whether a run carries a digest at all. Each
// condition excludes one case: a nil result excludes a run whose result
// is not yet set, ResultTriggerFailed excludes a refused trigger, which
// never reached Jenkins and already carries its own card, and a nil
// StartedAt excludes a run that never started.
func digestApplies(run recordstore.Run) bool {
	return run.Result != nil &&
		*run.Result != "SUCCESS" &&
		*run.Result != recordstore.ResultTriggerFailed &&
		run.StartedAt != nil
}

// firstFailedStage returns the first stage whose status is FAILURE, in
// the order the read answers them, and nil when no stage failed. Stages
// arrive in stage-start order, so the first failure is the earliest one
// and no re-sort is needed.
func firstFailedStage(stages []recordstore.Stage) *recordstore.Stage {
	for i := range stages {
		if stages[i].Status == "FAILURE" {
			return &stages[i]
		}
	}
	return nil
}

// computeFailedTests returns the failed-test counts, and nil when the
// run records no failed test. A count the run omits stays zero, which
// omitempty renders as an absent key.
func computeFailedTests(run recordstore.Run) *digestFailedTestsJSON {
	if run.TestsFailed == nil || *run.TestsFailed == 0 {
		return nil
	}
	out := &digestFailedTestsJSON{Failed: *run.TestsFailed}
	if run.TestsSkipped != nil {
		out.Skipped = *run.TestsSkipped
	}
	if run.TestsTotal != nil {
		out.Total = *run.TestsTotal
	}
	return out
}

// computeLogTail returns the last runDigestTailLines lines of a run's
// console log, and a flag that reports a hard read failure. It keeps
// only the lines the log index attributes to stageID when stageID is
// not empty, and falls back to every line when that filter names none:
// a run with no failed stage and a run whose index names no failed
// stage share that one path. A nil store and an undecodable index entry
// are hard failures, so the caller drops the log tail and keeps the
// rest of the digest. An empty log is not a failure: it answers no tail
// with the flag clear.
func computeLogTail(ctx context.Context, store blob.Store, runID, stageID string) (tail *string, hardFailure bool) {
	if store == nil {
		return nil, true
	}
	consoleData, err := readRunLogObject(ctx, store, blob.LogKey(runID))
	if err != nil {
		return nil, true
	}
	entries, _, err := readRunLogIndex(ctx, store, blob.LogIndexKey(runID))
	if err != nil {
		return nil, true
	}
	lines := splitRunLogLines(consoleData, entries)
	var attributed []runLogLine
	if stageID != "" {
		for _, ln := range lines {
			if ln.StageID == stageID {
				attributed = append(attributed, ln)
			}
		}
	}
	tailLines := attributed
	if len(tailLines) == 0 {
		// No failed stage, or no index entry names one: fall through to
		// the whole log. A missing index object decodes to an empty
		// entry map, not an error, so this branch also covers a run with
		// no index yet.
		tailLines = lines
	}
	if len(tailLines) == 0 {
		return nil, false
	}
	start := len(tailLines) - runDigestTailLines
	if start < 0 {
		start = 0
	}
	texts := make([]string, 0, len(tailLines)-start)
	for _, ln := range tailLines[start:] {
		texts = append(texts, ln.Text)
	}
	joined := strings.Join(texts, "\n")
	return &joined, false
}

// computeRunDigest composes the digest a run detail carries, and nil
// for a run that carries none. Every part is independently optional: a
// run with no failed stage, no failed test, and an empty log still
// answers a digest with every key absent.
func computeRunDigest(ctx context.Context, store blob.Store, run recordstore.Run, stages []recordstore.Stage) *runDigestJSON {
	if !digestApplies(run) {
		return nil
	}
	failed := firstFailedStage(stages)
	d := &runDigestJSON{FailedTests: computeFailedTests(run)}
	stageID := ""
	if failed != nil {
		d.Stage = failed.Name
		if failed.Error != nil {
			d.Error = *failed.Error
		}
		stageID = failed.StageID
	}
	// The hard-failure flag is read and discarded: it only degrades
	// logTail to absent, and the digest itself still stands.
	tail, _ := computeLogTail(ctx, store, run.RunID, stageID)
	if tail != nil {
		d.LogTail = *tail
	}
	return d
}
