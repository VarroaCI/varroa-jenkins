package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/rehydrate"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// seedRehydrationTeam seeds a placed pipelines Team into client's store.
func seedRehydrationTeam(client *testClient, name string, generation int64) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{}},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: generation},
		},
	}
	crdstore.MustSeed(client.store, team)
}

// deliverRehydration stands for a delivered push of plan's bundle on
// desired, the way handleConnected stamps it after a delivered send.
func deliverRehydration(rec *Reconciler, cr *v1alpha1.Controller, plan *rehydrationSendPlan, desired *mitev1.DesiredStateCommand, now time.Time) {
	rec.markRehydrationSent(cr, plan, desired, now)
}

// answerRehydration stands for the mite's result for commandID, applied
// the way handleConnected's result drain applies it.
func answerRehydration(rec *Reconciler, cr *v1alpha1.Controller, commandID string, success bool, rehydrationErr string, now time.Time) {
	cellKey := cr.Namespace + "/" + cr.Name
	id := commandID
	var match rehydrationResultMatch
	if m, ok := rec.takeRehydrationResult(cellKey, commandID); ok {
		id = m.StampedID
		match = m
	}
	tenants := make([]string, 0, len(cr.Status.Cell.Rehydration))
	for _, e := range cr.Status.Cell.Rehydration {
		tenants = append(tenants, e.Tenant)
	}
	cr.Status.Cell.Rehydration = applyRehydrationResult(cr.Status.Cell.Rehydration, id, success, rehydrationErr,
		map[string][]string{}, partitionJobPathsByTenant(match.CoveredJobPaths, tenants), nil, now)
	rec.commitNumberOnlyProgress(cellKey, match.CellInstance, match.NumberOnlyJobPaths, success, rehydrationErr, nil)
}

// TestAttachCellRehydration_AppliedTenantSendsNoBundle covers task 6.4:
// a tenant that already counts as applied gets no rehydration field on
// the command, and the cell reports no pending tenant.
func TestAttachCellRehydration_AppliedTenantSendsNoBundle(t *testing.T) {
	client := newTestClient()
	seedRehydrationTeam(client, "team-a", 1)
	rec := newTestReconciler(client)
	rec.SetRecordsFunc(func(context.Context, cells.RecordsRequest) (cells.RecordsResult, error) {
		t.Fatal("expected no records fetch for an already-applied tenant")
		return cells.RecordsResult{}, nil
	})

	cr := &v1alpha1.Controller{}
	cr.Name = "cell-a"
	cr.Namespace = "cells-ns"
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{
		Tenants:  []string{"team-a"},
		Instance: "uid-1",
		Rehydration: []v1alpha1.CellRehydrationEntry{
			{Tenant: "team-a", Generation: 1, CellInstance: "uid-1", Applied: true},
		},
	}

	desired := &mitev1.DesiredStateCommand{CommandId: "cmd-1"}
	plan := rec.attachCellRehydration(context.Background(), cr, desired, false, time.Now())

	if plan != nil {
		t.Fatalf("expected no send plan, got %+v", plan)
	}
	if desired.Rehydration != nil {
		t.Fatalf("expected no rehydration field for an applied tenant, got %+v", desired.Rehydration)
	}
}

// TestAttachCellRehydration_PendingTenantSendsBundle covers tasks
// 5.7/5.8/6.1: a tenant with no prior entry gets a fresh bundle, and its
// new entry is written with Applied false ahead of any result.
func TestAttachCellRehydration_PendingTenantSendsBundle(t *testing.T) {
	client := newTestClient()
	seedRehydrationTeam(client, "team-a", 1)
	rec := newTestReconciler(client)
	rec.SetRecordsFunc(func(_ context.Context, req cells.RecordsRequest) (cells.RecordsResult, error) {
		if len(req.JobPaths) == 0 {
			return cells.RecordsResult{Jobs: []cells.JobRecords{{JobPath: "team-a/job-x", NextBuildNumber: 5}}}, nil
		}
		return cells.RecordsResult{Jobs: []cells.JobRecords{{JobPath: "team-a/job-x", NextBuildNumber: 5}}}, nil
	})

	cr := &v1alpha1.Controller{}
	cr.Name = "cell-a"
	cr.Namespace = "cells-ns"
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-a"}, Instance: "uid-1"}

	desired := &mitev1.DesiredStateCommand{CommandId: "cmd-1"}
	plan := rec.attachCellRehydration(context.Background(), cr, desired, false, time.Now())

	if plan == nil || plan.Held {
		t.Fatalf("expected a send plan for a new bundle, got %+v", plan)
	}
	if desired.Rehydration == nil || plan.Bundle != desired.Rehydration {
		t.Fatal("expected the plan's rehydration bundle attached")
	}
	if len(cr.Status.Cell.Rehydration) != 1 {
		t.Fatalf("expected one entry written, got %+v", cr.Status.Cell.Rehydration)
	}
	entry := cr.Status.Cell.Rehydration[0]
	if entry.Applied {
		t.Fatal("expected Applied false before any result arrives")
	}
	if entry.CommandID != "" || entry.BundleID != desired.Rehydration.BundleId {
		t.Fatalf("expected the pre-send entry to name the bundle and no command, got %+v", entry)
	}

	deliverRehydration(rec, cr, plan, desired, time.Now())
	if got := cr.Status.Cell.Rehydration[0].CommandID; got != "cmd-1" {
		t.Fatalf("expected the delivered push to stamp cmd-1, got %q", got)
	}
}

// TestAttachCellRehydration_EmptySelectionAppliedAtOnce covers task
// 6.0a: a tenant whose enumeration returns no job path is applied on
// the same tick, with no rehydration field sent.
func TestAttachCellRehydration_EmptySelectionAppliedAtOnce(t *testing.T) {
	client := newTestClient()
	seedRehydrationTeam(client, "team-a", 1)
	rec := newTestReconciler(client)
	rec.SetRecordsFunc(func(context.Context, cells.RecordsRequest) (cells.RecordsResult, error) {
		return cells.RecordsResult{}, nil
	})

	cr := &v1alpha1.Controller{}
	cr.Name = "cell-a"
	cr.Namespace = "cells-ns"
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-a"}, Instance: "uid-1"}

	desired := &mitev1.DesiredStateCommand{CommandId: "cmd-1"}
	plan := rec.attachCellRehydration(context.Background(), cr, desired, false, time.Now())

	if plan != nil {
		t.Fatalf("expected the empty-selection tenant to count as applied, got plan %+v", plan)
	}
	if desired.Rehydration != nil {
		t.Fatalf("expected no rehydration field sent, got %+v", desired.Rehydration)
	}
	if len(cr.Status.Cell.Rehydration) != 1 || !cr.Status.Cell.Rehydration[0].Applied {
		t.Fatalf("expected an applied empty-selection entry, got %+v", cr.Status.Cell.Rehydration)
	}
}

// TestAttachCellRehydration_NumberOnlyProgressesAcrossThreeTicks
// covers design.md D4's per-tick number-only progression and the
// rehydrationApplied fix: a tenant whose job set needs three
// number-only blocks to seed every job gets three distinct bundles
// over three ticks, one per tick, and counts as applied only once the
// third block's result leaves no job unseeded. Before the fix,
// rehydrationApplied ignored UnseededJobs, so the tenant looked
// applied after the first successful block and ticks two and three
// sent no further bundle.
func TestAttachCellRehydration_NumberOnlyProgressesAcrossThreeTicks(t *testing.T) {
	client := newTestClient()
	seedRehydrationTeam(client, "team-a", 1)
	rec := newTestReconciler(client)

	// A very long job-path suffix inflates each job's own serialized
	// size, so a small, fast-to-test job count already needs three
	// blocks to clear the real 256 KiB budget — the same budget
	// attachCellRehydration uses in production.
	const jobCount = 110
	longSuffix := strings.Repeat("x", 3000)
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

	now := time.Now()
	var lastEntry v1alpha1.CellRehydrationEntry
	bundleIDs := map[string]bool{}

	for tick := 1; tick <= 3; tick++ {
		desired := &mitev1.DesiredStateCommand{CommandId: fmt.Sprintf("cmd-%d", tick)}
		plan := rec.attachCellRehydration(context.Background(), cr, desired, false, now)
		if plan == nil {
			t.Fatalf("tick %d: expected a send plan", tick)
		}
		if desired.Rehydration == nil {
			t.Fatalf("tick %d: expected a bundle sent; a stuck applied predicate would skip it", tick)
		}
		bundleIDs[desired.Rehydration.BundleId] = true
		if len(cr.Status.Cell.Rehydration) != 1 {
			t.Fatalf("tick %d: expected one entry, got %+v", tick, cr.Status.Cell.Rehydration)
		}
		lastEntry = cr.Status.Cell.Rehydration[0]
		if tick < 3 && lastEntry.UnseededJobs == 0 {
			t.Fatalf("tick %d: expected jobs still unseeded before the final block", tick)
		}

		// Simulate a delivered push and the mite applying this tick's
		// bundle successfully, as handleConnected's result drain does.
		deliverRehydration(rec, cr, plan, desired, now)
		answerRehydration(rec, cr, desired.CommandId, true, "", now)
	}

	if len(bundleIDs) != 3 {
		t.Fatalf("expected three distinct bundles across the three ticks, got %d", len(bundleIDs))
	}
	if lastEntry.UnseededJobs != 0 {
		t.Fatalf("expected every job seeded after the third block, UnseededJobs=%d", lastEntry.UnseededJobs)
	}

	// A fourth tick sees the fully-seeded entry and sends no further
	// bundle: the tenant now counts as applied.
	desired4 := &mitev1.DesiredStateCommand{CommandId: "cmd-4"}
	if plan4 := rec.attachCellRehydration(context.Background(), cr, desired4, false, now); plan4 != nil {
		t.Fatal("tick 4: expected the tenant to finally count as applied")
	}
	if desired4.Rehydration != nil {
		t.Fatal("tick 4: expected no further bundle once every job holds its number")
	}
}

// TestAttachCellRehydration_FailedSeedStaysUnseeded covers design.md
// D4: every enumerated job gets a seed or a pending/unseeded
// representation. A job whose BuildJobSeed call fails must count as
// unseeded rather than vanishing from tenantJobPaths, the unseeded
// set, and the pending accounting — a rendering job and a failing job
// together must keep the tenant unrehydrated and retry the failing
// job on the next tick.
func TestAttachCellRehydration_FailedSeedStaysUnseeded(t *testing.T) {
	client := newTestClient()
	seedRehydrationTeam(client, "team-fail", 2)
	rec := newTestReconciler(client)
	rec.SetRecordsFunc(func(context.Context, cells.RecordsRequest) (cells.RecordsResult, error) {
		return cells.RecordsResult{Jobs: []cells.JobRecords{
			{JobPath: "team-fail/job-ok", NextBuildNumber: 5},
			{JobPath: "team-fail/job-bad", NextBuildNumber: 3},
		}}, nil
	})
	rec.SetBuildJobSeedFunc(func(jr rehydrate.JobRecords, n int, jenkinsHome string, opts rehydrate.Options) (rehydrate.JobSeed, error) {
		if jr.JobPath == "team-fail/job-bad" {
			return rehydrate.JobSeed{}, fmt.Errorf("simulated render failure for %s", jr.JobPath)
		}
		return rehydrate.BuildJobSeed(jr, n, jenkinsHome, opts)
	})

	cr := &v1alpha1.Controller{}
	cr.Name = "cell-a"
	cr.Namespace = "cells-ns"
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"team-fail"}, Instance: "uid-1"}

	now := time.Now()
	desired := &mitev1.DesiredStateCommand{CommandId: "cmd-1"}
	plan := rec.attachCellRehydration(context.Background(), cr, desired, false, now)
	if plan == nil {
		t.Fatal("expected a send plan: one job rendered")
	}
	if desired.Rehydration == nil {
		t.Fatal("expected a bundle for the successfully rendered job")
	}
	if len(cr.Status.Cell.Rehydration) != 1 {
		t.Fatalf("expected one entry, got %+v", cr.Status.Cell.Rehydration)
	}
	entry := cr.Status.Cell.Rehydration[0]
	if entry.UnseededJobs != 1 {
		t.Fatalf("expected the failed job counted as unseeded, UnseededJobs=%d", entry.UnseededJobs)
	}
	if rehydrationApplied(&entry, 2, "uid-1") {
		t.Fatal("expected the tenant not rehydrated while a job failed to render")
	}

	// The mite applies the bundle for the job that did render; the
	// failed job's path was never inside it, so success alone must
	// not mark the tenant applied.
	deliverRehydration(rec, cr, plan, desired, now)
	answerRehydration(rec, cr, desired.CommandId, true, "", now)
	if rehydrationApplied(&cr.Status.Cell.Rehydration[0], 2, "uid-1") {
		t.Fatal("expected UnseededJobs to keep the tenant unrehydrated after the mite applies the partial bundle")
	}

	// Tick 2: the render still fails, so the tenant stays pending and
	// the failed job is retried, not dropped.
	desired2 := &mitev1.DesiredStateCommand{CommandId: "cmd-2"}
	if plan2 := rec.attachCellRehydration(context.Background(), cr, desired2, false, now); plan2 == nil {
		t.Fatal("tick 2: expected the tenant still pending")
	}
	if len(cr.Status.Cell.Rehydration) != 1 || cr.Status.Cell.Rehydration[0].UnseededJobs != 1 {
		t.Fatalf("tick 2: expected the failed job retried and still counted as unseeded, got %+v", cr.Status.Cell.Rehydration)
	}
}

// equalStringSlices reports whether a and b hold the same strings in
// the same order.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAttachCellRehydration_NumberOnlyBlockNotCommittedUntilConfirmed
// covers the review round 2 fix: a number-only block's job paths join
// the seeded cache only once commitNumberOnlyProgress sees a matching,
// successful DesiredStateResult for it — never merely because a tick
// selected and sent them. An unconfirmed or rejected block must be
// resent unchanged, and the tenant must never count as rehydrated
// until every block in the progression has its own matched success.
func TestAttachCellRehydration_NumberOnlyBlockNotCommittedUntilConfirmed(t *testing.T) {
	client := newTestClient()
	seedRehydrationTeam(client, "team-a", 1)
	rec := newTestReconciler(client)

	// Same sizing as the three-block success-path test: a small job
	// count that still needs several number-only blocks to clear the
	// real 256 KiB budget.
	const jobCount = 110
	longSuffix := strings.Repeat("x", 3000)
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

	now := time.Now()

	var lastPlan *rehydrationSendPlan
	send := func(cmdID string) (*mitev1.DesiredStateCommand, v1alpha1.CellRehydrationEntry) {
		desired := &mitev1.DesiredStateCommand{CommandId: cmdID}
		lastPlan = rec.attachCellRehydration(context.Background(), cr, desired, false, now)
		if lastPlan == nil {
			t.Fatalf("%s: expected a send plan", cmdID)
		}
		if desired.Rehydration == nil {
			t.Fatalf("%s: expected a bundle sent", cmdID)
		}
		if len(cr.Status.Cell.Rehydration) != 1 {
			t.Fatalf("%s: expected one entry, got %+v", cmdID, cr.Status.Cell.Rehydration)
		}
		return desired, cr.Status.Cell.Rehydration[0]
	}

	// Tick 1: block A is selected and sent. Its delivery fails outright
	// — no DesiredStateResult ever arrives for cmd-1.
	desired1, entry1 := send("cmd-1")
	blockA := append([]string(nil), desired1.Rehydration.ReloadItems...)
	if len(blockA) == 0 {
		t.Fatal("tick 1: expected the bundle to name the job paths it covers")
	}
	if entry1.UnseededJobs == 0 {
		t.Fatal("tick 1: expected jobs still unseeded")
	}
	if rehydrationApplied(&entry1, 1, "uid-1") {
		t.Fatal("tick 1: expected the tenant not yet rehydrated")
	}

	// Tick 2: nothing confirmed block A, so the very same block must be
	// resent — not the next block in job-path order.
	desired2, _ := send("cmd-2")
	blockA2 := append([]string(nil), desired2.Rehydration.ReloadItems...)
	if !equalStringSlices(blockA, blockA2) {
		t.Fatalf("tick 2: expected the unconfirmed block resent unchanged, got %v want %v", blockA2, blockA)
	}

	// The mite rejects cmd-2's block outright.
	deliverRehydration(rec, cr, lastPlan, desired2, now)
	answerRehydration(rec, cr, desired2.CommandId, false, "simulated rejection", now)

	// Tick 3: a rejection must not advance progress either — the same
	// block is sent again.
	desired3, entry3 := send("cmd-3")
	blockA3 := append([]string(nil), desired3.Rehydration.ReloadItems...)
	if !equalStringSlices(blockA, blockA3) {
		t.Fatalf("tick 3: expected the still-unconfirmed block resent unchanged, got %v want %v", blockA3, blockA)
	}
	if rehydrationApplied(&entry3, 1, "uid-1") {
		t.Fatal("tick 3: expected the tenant still not rehydrated")
	}

	// The mite now confirms cmd-3's block applied.
	deliverRehydration(rec, cr, lastPlan, desired3, now)
	answerRehydration(rec, cr, desired3.CommandId, true, "", now)
	if rehydrationApplied(&cr.Status.Cell.Rehydration[0], 1, "uid-1") {
		t.Fatal("tick 3: expected the tenant still not rehydrated — later blocks remain unseeded")
	}

	// Confirm every remaining block in turn. Each must be distinct from
	// every block already confirmed, and the tenant must not count as
	// rehydrated until the very last one lands.
	seen := map[string]bool{}
	for _, p := range blockA {
		seen[p] = true
	}
	var lastEntry v1alpha1.CellRehydrationEntry
	tick := 4
	for ; tick <= jobCount+5; tick++ {
		cmdID := fmt.Sprintf("cmd-%d", tick)
		desired := &mitev1.DesiredStateCommand{CommandId: cmdID}
		plan := rec.attachCellRehydration(context.Background(), cr, desired, false, now)
		if plan == nil {
			// No further bundle: the tenant is fully seeded.
			break
		}
		if desired.Rehydration == nil {
			t.Fatalf("tick %d: expected a bundle while pending", tick)
		}
		for _, p := range desired.Rehydration.ReloadItems {
			if seen[p] {
				t.Fatalf("tick %d: expected no already-confirmed job re-covered, got %s again", tick, p)
			}
			seen[p] = true
		}
		deliverRehydration(rec, cr, plan, desired, now)
		answerRehydration(rec, cr, cmdID, true, "", now)
		if len(cr.Status.Cell.Rehydration) != 1 {
			t.Fatalf("tick %d: expected one entry, got %+v", tick, cr.Status.Cell.Rehydration)
		}
		lastEntry = cr.Status.Cell.Rehydration[0]
		if rehydrationApplied(&lastEntry, 1, "uid-1") && len(seen) != jobCount {
			t.Fatalf("tick %d: tenant counted as rehydrated with only %d of %d jobs seeded", tick, len(seen), jobCount)
		}
	}
	if tick > jobCount+5 {
		t.Fatal("numbering never converged")
	}

	if len(seen) != jobCount {
		t.Fatalf("expected every job seeded exactly once across the progression, got %d of %d", len(seen), jobCount)
	}
	if lastEntry.UnseededJobs != 0 {
		t.Fatalf("expected every job seeded, UnseededJobs=%d", lastEntry.UnseededJobs)
	}
	if !rehydrationApplied(&lastEntry, 1, "uid-1") {
		t.Fatal("expected the tenant rehydrated once every block's own success matched")
	}
}

func TestNumberOnlyCaches_ConcurrentCellsDoNotRace(t *testing.T) {
	r := &Reconciler{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cr := &v1alpha1.Controller{}
			cr.Namespace = "ns"
			cr.Name = fmt.Sprintf("cell-%d", i%3)
			key := cr.Namespace + "/" + cr.Name
			for n := 0; n < 50; n++ {
				id := fmt.Sprintf("cmd-%d-%d", i, n)
				bundle := &mitev1.RehydrationBundle{BundleId: id}
				plan := &rehydrationSendPlan{Bundle: bundle, NumberOnlyJobPaths: []string{"a", "b"}}
				r.markRehydrationSent(cr, plan, &mitev1.DesiredStateCommand{CommandId: id, Rehydration: bundle}, time.Now())
				_ = r.rehydrationInFlightFor(key)
				_ = r.numberSeededJobsCopy(key, "uid-1")
				match, _ := r.takeRehydrationResult(key, id)
				r.commitNumberOnlyProgress(key, match.CellInstance, match.NumberOnlyJobPaths, true, "", nil)
				r.dropRehydrationInFlight(key)
			}
		}(i)
	}
	wg.Wait()
}

// TestRehydrationPendingSince_ConcurrentCellsDoNotRace covers up to
// maxConcurrentReconciles cells calling rehydrationPendingSinceFor and
// clearRehydrationPendingSince for distinct tenants at once. Run with
// -race: an unguarded map access fails this test.
func TestRehydrationPendingSince_ConcurrentCellsDoNotRace(t *testing.T) {
	r := &Reconciler{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cellKey := fmt.Sprintf("ns/cell-%d", i%3)
			tenant := fmt.Sprintf("team-%d", i)
			for n := 0; n < 50; n++ {
				r.rehydrationPendingSinceFor(cellKey, tenant, int64(n), "uid-1", time.Now())
				r.clearRehydrationPendingSince(cellKey, tenant)
			}
		}(i)
	}
	wg.Wait()
}
