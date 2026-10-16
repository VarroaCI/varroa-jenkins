package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// unseededFixture builds one cell with one tenant whose jobCount jobs need
// several number-only blocks. It returns the reconciler, the cell, and the
// cell.
func unseededFixture(t *testing.T, jobCount, suffixLen int) (*Reconciler, *v1alpha1.Controller) {
	t.Helper()
	client := newTestClient()
	seedRehydrationTeam(client, "team-a", 1)
	rec := newTestReconciler(client)

	longSuffix := strings.Repeat("x", suffixLen)
	jobs := make([]cells.JobRecords, jobCount)
	byPath := make(map[string]cells.JobRecords, jobCount)
	for i := 0; i < jobCount; i++ {
		job := fmt.Sprintf("team-a/job-%s-%04d", longSuffix, i)
		jobs[i] = cells.JobRecords{JobPath: job, NextBuildNumber: 1}
		byPath[job] = jobs[i]
	}
	rec.SetRecordsFunc(func(_ context.Context, req cells.RecordsRequest) (cells.RecordsResult, error) {
		if len(req.JobPaths) == 0 {
			return cells.RecordsResult{Jobs: jobs}, nil
		}
		page := make([]cells.JobRecords, 0, len(req.JobPaths))
		for _, p := range req.JobPaths {
			if j, ok := byPath[p]; ok {
				page = append(page, j)
			}
		}
		return cells.RecordsResult{Jobs: page}, nil
	})

	cr := &v1alpha1.Controller{}
	cr.Name = "cell-a"
	cr.Namespace = "cells-ns"
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-a"}, Instance: "uid-1"}
	return rec, cr
}

// pathSet returns the set of paths.
func pathSet(paths []string) map[string]bool {
	out := make(map[string]bool, len(paths))
	for _, p := range paths {
		out[p] = true
	}
	return out
}

// TestAttachCellRehydration_NumberOnlyRoundHoldsOnlyUncoveredJobs pins
// that, while a number-only block is outstanding, its jobs wait for
// their seed, the jobs that the applied blocks covered build, and the
// applied instance stays answered. An uncovered job never builds before
// its number seed lands.
func TestAttachCellRehydration_NumberOnlyRoundHoldsOnlyUncoveredJobs(t *testing.T) {
	// 120 jobs of about 2 KB keep the pending list under the cell byte budget,
	// so the cap does not drop it. The block still needs a second round.
	const jobCount = 120
	rec, cr := unseededFixture(t, jobCount, 2000)
	now := time.Now()

	send := func(cmdID string) *mitev1.DesiredStateCommand {
		t.Helper()
		desired := &mitev1.DesiredStateCommand{CommandId: cmdID}
		plan := rec.attachCellRehydration(context.Background(), cr, desired, false, now)
		if plan == nil || desired.Rehydration == nil {
			t.Fatalf("%s: expected a bundle sent", cmdID)
		}
		if len(cr.Status.Cell.Rehydration) != 1 {
			t.Fatalf("%s: expected one entry, got %+v", cmdID, cr.Status.Cell.Rehydration)
		}
		deliverRehydration(rec, cr, plan, desired, now)
		return desired
	}

	// Tick 1: the first block is in flight and has no result yet. The
	// tenant holds whole, because no seed has applied.
	desired1 := send("cmd-1")
	blockA := append([]string(nil), desired1.Rehydration.ReloadItems...)
	if ans := seedAnswerOf(&cr.Status.Cell.Rehydration[0]); ans.instance != "" {
		t.Fatalf("tick 1: unapplied entry answered instance %q, want none", ans.instance)
	}
	answerRehydration(rec, cr, "cmd-1", true, "", now)

	// After the block applies, only the uncovered jobs wait.
	coveredA := pathSet(blockA)
	ansA := seedAnswerOf(&cr.Status.Cell.Rehydration[0])
	if ansA.instance != "uid-1" {
		t.Fatalf("after block A: answered instance %q, want uid-1", ansA.instance)
	}
	pendingA := pathSet(ansA.pending)
	for p := range coveredA {
		if pendingA[p] {
			t.Fatalf("after block A: covered job %s still pending", p)
		}
	}
	if want := jobCount - len(blockA); len(pendingA) != want {
		t.Fatalf("after block A: %d jobs pending, want the %d uncovered jobs", len(pendingA), want)
	}

	// Tick 2: block B is in flight. The applied instance stays answered,
	// and block B's jobs wait for their seed, while block A's do not.
	desired2 := send("cmd-2")
	blockB := append([]string(nil), desired2.Rehydration.ReloadItems...)
	ansB := seedAnswerOf(&cr.Status.Cell.Rehydration[0])
	if ansB.instance != "uid-1" {
		t.Fatalf("block B in flight: answered instance %q, want uid-1", ansB.instance)
	}
	pendingB := pathSet(ansB.pending)
	if !pendingB[blockB[0]] {
		t.Fatalf("block B in flight: job %s builds before its seed lands", blockB[0])
	}
	for p := range coveredA {
		if pendingB[p] {
			t.Fatalf("block B in flight: applied job %s is held", p)
		}
	}
	answerRehydration(rec, cr, "cmd-2", true, "", now)

	// Every remaining block follows the same rule, and the tenant holds
	// nothing once every job has its seed.
	seen := pathSet(append(append([]string(nil), blockA...), blockB...))
	tick := 3
	for ; tick <= jobCount+5; tick++ {
		cmdID := fmt.Sprintf("cmd-%d", tick)
		desired := &mitev1.DesiredStateCommand{CommandId: cmdID}
		plan := rec.attachCellRehydration(context.Background(), cr, desired, false, now)
		if plan == nil {
			break
		}
		if desired.Rehydration == nil {
			t.Fatalf("tick %d: expected a bundle while pending", tick)
		}
		deliverRehydration(rec, cr, plan, desired, now)
		if ans := seedAnswerOf(&cr.Status.Cell.Rehydration[0]); !pathSet(ans.pending)[desired.Rehydration.ReloadItems[0]] {
			t.Fatalf("tick %d: block job %s builds before its seed lands", tick, desired.Rehydration.ReloadItems[0])
		}
		for _, p := range desired.Rehydration.ReloadItems {
			seen[p] = true
		}
		answerRehydration(rec, cr, cmdID, true, "", now)
	}
	if tick > jobCount+5 {
		t.Fatal("numbering never converged")
	}
	if len(seen) != jobCount {
		t.Fatalf("expected every job seeded, got %d of %d", len(seen), jobCount)
	}
	final := &cr.Status.Cell.Rehydration[0]
	if len(final.Unseeded) != 0 || final.UnseededJobs != 0 {
		t.Fatalf("expected no unseeded job left, got %+v", final)
	}
	if !rehydrationApplied(final, 1, "uid-1") {
		t.Fatal("expected the tenant rehydrated once every job held its seed")
	}
}

// TestAttachCellRehydration_LostInFlightRecordSelfHeals pins that an
// applied entry with unseeded jobs keeps the tenant pending, so a lost
// in-flight record sends the next block on the next tick.
func TestAttachCellRehydration_LostInFlightRecordSelfHeals(t *testing.T) {
	rec, cr := unseededFixture(t, 110, 3000)
	now := time.Now()

	desired1 := &mitev1.DesiredStateCommand{CommandId: "cmd-1"}
	plan1 := rec.attachCellRehydration(context.Background(), cr, desired1, false, now)
	if plan1 == nil {
		t.Fatal("tick 1: expected a send plan")
	}
	deliverRehydration(rec, cr, plan1, desired1, now)
	answerRehydration(rec, cr, "cmd-1", true, "", now)

	// The operator loses the in-flight record, as after a restart.
	rec.dropRehydrationInFlight(cr.Namespace + "/" + cr.Name)

	desired2 := &mitev1.DesiredStateCommand{CommandId: "cmd-2"}
	plan2 := rec.attachCellRehydration(context.Background(), cr, desired2, false, now)
	if plan2 == nil || desired2.Rehydration == nil {
		t.Fatal("tick 2: expected the pending tenant to send its next block")
	}
	if rehydrationApplied(&cr.Status.Cell.Rehydration[0], 1, "uid-1") {
		t.Fatal("tick 2: expected the tenant still pending")
	}
}

// TestAttachCellRehydration_RolledInstanceRenumbersEveryJob pins that a
// rolled pod starts the number-only progression again: the seeded set of
// the old instance does not carry over.
func TestAttachCellRehydration_RolledInstanceRenumbersEveryJob(t *testing.T) {
	rec, cr := unseededFixture(t, 110, 3000)
	now := time.Now()

	desired1 := &mitev1.DesiredStateCommand{CommandId: "cmd-1"}
	plan1 := rec.attachCellRehydration(context.Background(), cr, desired1, false, now)
	if plan1 == nil {
		t.Fatal("tick 1: expected a send plan")
	}
	blockA := append([]string(nil), desired1.Rehydration.ReloadItems...)
	deliverRehydration(rec, cr, plan1, desired1, now)
	answerRehydration(rec, cr, "cmd-1", true, "", now)

	cr.Status.Cell.Instance = "uid-2"
	desired2 := &mitev1.DesiredStateCommand{CommandId: "cmd-2"}
	plan2 := rec.attachCellRehydration(context.Background(), cr, desired2, false, now)
	if plan2 == nil || desired2.Rehydration == nil {
		t.Fatal("rolled instance: expected a bundle")
	}
	if !equalStringSlices(desired2.Rehydration.ReloadItems, blockA) {
		t.Fatalf("rolled instance: expected the first block renumbered, got %v want %v",
			desired2.Rehydration.ReloadItems, blockA)
	}
}

// TestNumberSeededJobs_ResetsOnCellInstanceChange pins that the number-seeded
// set belongs to one cell instance.
func TestNumberSeededJobs_ResetsOnCellInstanceChange(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	const key = "cells-ns/cell-a"

	rec.commitNumberOnlyProgress(key, "uid-1", []string{"team-a/x"}, true, "", nil)
	if got := rec.numberSeededJobsCopy(key, "uid-1"); !got["team-a/x"] {
		t.Fatalf("same instance: want team-a/x seeded, got %v", got)
	}
	if got := rec.numberSeededJobsCopy(key, "uid-2"); len(got) != 0 {
		t.Fatalf("new instance: want an empty seeded set, got %v", got)
	}
	rec.commitNumberOnlyProgress(key, "uid-2", []string{"team-a/y"}, true, "", nil)
	if got := rec.numberSeededJobsCopy(key, "uid-2"); got["team-a/x"] || !got["team-a/y"] {
		t.Fatalf("after commit on uid-2: want only team-a/y, got %v", got)
	}
}

// TestNumberSeededJobs_ReloadFailedJobIsNotConfirmed pins that a job whose
// reload failed stays unconfirmed in the number cache, so a later block
// cannot advance past it.
func TestNumberSeededJobs_ReloadFailedJobIsNotConfirmed(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	const key = "cells-ns/cell-a"

	rec.commitNumberOnlyProgress(key, "uid-1", []string{"team-a/x", "team-a/y"}, true, "", []string{"team-a/x"})
	got := rec.numberSeededJobsCopy(key, "uid-1")
	if got["team-a/x"] || !got["team-a/y"] {
		t.Fatalf("want only team-a/y confirmed, got %v", got)
	}
}
