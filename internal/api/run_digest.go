// Package api: the failure digest the run detail carries. A digest
// names why a run that did not succeed failed: the innermost failed stage,
// its error, the tail of its log, the failed test counts, and the first
// failed test cases. It is a
// read composition, computed fresh on every read from stored rows and
// the stored log, never a second stored copy of those facts.
//
// A hard read failure — a nil blob store, or an undecodable log-index
// entry — degrades logTail to absent and never fails the run read, the
// same way an unreadable source column never fails it. A failed case read
// degrades the failed case list alone, by the same rule. This is a
// deliberate, named exception to internal/api/AGENTS.md's rule that a
// handler which reads console log or artifact bytes answers 503 when
// deps.BlobStore is nil: GET /runs/{run} serves a complete response
// with or without digest.logTail.
//
// The digest is absent when no part has a value. An INTERRUPTED run carries
// no logTail, because its banner already names the reason.
package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// runDigestTailLines bounds how many lines of the log tail a digest
// carries, counted back from the last line of the attributed set.
const runDigestTailLines = 50

// runDigestMaxCases bounds how many failed cases a digest names.
const runDigestMaxCases = 10

type runDigestJSON struct {
	Stage       string                 `json:"stage,omitempty"`
	Error       string                 `json:"error,omitempty"`
	LogTail     string                 `json:"logTail,omitempty"`
	FailedTests *digestFailedTestsJSON `json:"failedTests,omitempty"`
}

// digestFailedTestsJSON is the failed-test part of a digest. Failed is
// the one count that makes the part present, so it carries no omitempty.
// The three counts come from the run's aggregate columns, never from the
// case rows: the rows stop at 500, so a count read from them would
// understate a larger run. Cases is absent when the run holds no failed
// case row.
type digestFailedTestsJSON struct {
	Failed  int32                  `json:"failed"`
	Skipped int32                  `json:"skipped,omitempty"`
	Total   int32                  `json:"total,omitempty"`
	Cases   []digestFailedCaseJSON `json:"cases,omitempty"`
}

// digestFailedCaseJSON names one failed case of a digest. CaseKey is the
// opaque key the case history read takes back unchanged.
type digestFailedCaseJSON struct {
	Suite     string `json:"suite"`
	ClassName string `json:"className"`
	Name      string `json:"name"`
	CaseKey   string `json:"caseKey"`
}

// runTestCaseRead is the result of one ListTestCases call. The run read
// performs the call once and shares it between the run detail and the
// digest. A non-nil err means the read failed.
type runTestCaseRead struct {
	cases []recordstore.TestCase
	err   error
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

// digestFailedStage returns the stage the digest names, and nil when no
// stage failed. A failed stage also fails every stage and parallel branch
// that holds it, so a FAILURE row that holds another FAILURE row only
// repeats that failure. The function returns the first FAILURE row that
// holds no other FAILURE row. The reader answers the stages in
// stage-start order, so the first such row is the earliest one, and the
// function does not sort again. A failed parallel branch that holds no
// stage of its own is a valid answer.
func digestFailedStage(stages []recordstore.Stage) *recordstore.Stage {
	byID := make(map[string]*recordstore.Stage, len(stages))
	for i := range stages {
		byID[stages[i].StageID] = &stages[i]
	}
	holdsFailure := make(map[string]bool)
	for i := range stages {
		if stages[i].Status != "FAILURE" {
			continue
		}
		// The seen set stops the walk on a parent chain that loops.
		seen := map[string]bool{stages[i].StageID: true}
		for p := stages[i].ParentID; p != nil && !seen[*p]; {
			seen[*p] = true
			holdsFailure[*p] = true
			parent, ok := byID[*p]
			if !ok {
				break
			}
			p = parent.ParentID
		}
	}
	for i := range stages {
		if stages[i].Status == "FAILURE" && !holdsFailure[stages[i].StageID] {
			return &stages[i]
		}
	}
	return nil
}

// digestStageName returns the name the digest shows for a stage. Jenkins
// names a parallel branch row "Branch: <name>", so a parallel row with a
// stored parallel_branch_name shows that name. Every other row shows its
// own name.
func digestStageName(s *recordstore.Stage) string {
	if s.NodeType != "parallel" || len(s.Payload) == 0 {
		return s.Name
	}
	var env struct {
		Payload struct {
			ParallelBranchName string `json:"parallel_branch_name"`
		} `json:"payload"`
	}
	if json.Unmarshal(s.Payload, &env) != nil || env.Payload.ParallelBranchName == "" {
		return s.Name
	}
	return env.Payload.ParallelBranchName
}

// stageSubtree returns rootID and the id of every stage nested under it,
// at any depth. An empty rootID returns nil.
func stageSubtree(stages []recordstore.Stage, rootID string) map[string]bool {
	if rootID == "" {
		return nil
	}
	children := make(map[string][]string)
	for _, s := range stages {
		if s.ParentID != nil {
			children[*s.ParentID] = append(children[*s.ParentID], s.StageID)
		}
	}
	out := map[string]bool{rootID: true}
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, c := range children[id] {
			if !out[c] {
				out[c] = true
				queue = append(queue, c)
			}
		}
	}
	return out
}

// computeFailedTests returns the failed-test counts, and nil when the
// run records no failed test. A count the run omits stays zero, which
// omitempty renders as an absent key. It names at most runDigestMaxCases
// cases whose status is not SKIPPED, in the reader's own order. The case
// table holds failed and skipped rows alone, so that one test is the whole
// filter. A failed case read leaves the list absent and keeps the counts.
func computeFailedTests(run recordstore.Run, read runTestCaseRead) *digestFailedTestsJSON {
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
	if read.err != nil {
		return out
	}
	for _, c := range read.cases {
		if len(out.Cases) == runDigestMaxCases {
			break
		}
		if c.Status == "SKIPPED" {
			continue
		}
		out.Cases = append(out.Cases, digestFailedCaseJSON{
			Suite:     c.Suite,
			ClassName: c.ClassName,
			Name:      c.Name,
			CaseKey:   TestCaseKey(c.ClassName, c.Name),
		})
	}
	return out
}

// computeLogTail returns the last runDigestTailLines lines of a run's
// console log, and a flag that reports a hard read failure. The plugin
// tags each line with the innermost stage that wrote it. The function
// keeps only the lines the log index attributes to a stage in stageIDs
// when stageIDs is not empty. It falls back to every line when that
// filter names none: a run with no failed stage and a run whose index
// names no failed stage share that one path. A nil store and an
// undecodable index entry are hard failures, so the caller drops the log
// tail and keeps the rest of the digest. An empty log is not a failure:
// it answers no tail with the flag clear. The tail carries no ANSI escape
// sequence, because a client shows it as plain text.
func computeLogTail(ctx context.Context, store blob.Store, runID string, stageIDs map[string]bool) (tail *string, hardFailure bool) {
	if store == nil {
		return nil, true
	}
	consoleData, err := readRunLogObject(ctx, store, runID)
	if err != nil {
		return nil, true
	}
	entries, _, err := readRunLogIndex(ctx, store, blob.LogIndexKey(runID))
	if err != nil {
		return nil, true
	}
	lines := splitRunLogLines(consoleData, entries)
	var attributed []runLogLine
	if len(stageIDs) > 0 {
		for _, ln := range lines {
			if stageIDs[ln.StageID] {
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
		texts = append(texts, stripANSIEscapes(ln.Text))
	}
	joined := strings.Join(texts, "\n")
	return &joined, false
}

// computeRunDigest composes the digest a run detail carries, and nil
// for a run that carries none. Every part is independently optional. The
// digest is nil when no part has a value, so a client never draws an
// empty card. An INTERRUPTED run carries no log tail: its banner names
// the reason, and the tail of a lost or rolled run names no failure.
func computeRunDigest(ctx context.Context, store blob.Store, run recordstore.Run, stages []recordstore.Stage, cases runTestCaseRead) *runDigestJSON {
	if !digestApplies(run) {
		return nil
	}
	failed := digestFailedStage(stages)
	d := &runDigestJSON{FailedTests: computeFailedTests(run, cases)}
	var stageIDs map[string]bool
	if failed != nil {
		d.Stage = digestStageName(failed)
		if failed.Error != nil {
			d.Error = *failed.Error
		}
		stageIDs = stageSubtree(stages, failed.StageID)
	}
	// digestApplies guarantees a non-nil result, so the dereference is safe.
	// The hard-failure flag is read and discarded: it only degrades
	// logTail to absent, and the digest itself still stands.
	if *run.Result != "INTERRUPTED" {
		tail, _ := computeLogTail(ctx, store, run.RunID, stageIDs)
		if tail != nil {
			d.LogTail = *tail
		}
	}
	if d.Stage == "" && d.Error == "" && d.LogTail == "" && d.FailedTests == nil {
		return nil
	}
	return d
}
