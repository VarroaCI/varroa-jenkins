package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/rehydrate"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// perTenantRecords answers a record store with one job for each tenant.
func perTenantRecords(_ context.Context, req cells.RecordsRequest) (cells.RecordsResult, error) {
	return cells.RecordsResult{Jobs: []cells.JobRecords{{JobPath: req.Tenant + "/build", NextBuildNumber: 5}}}, nil
}

// rehydrationResult answers the mite's result for commandID. The config
// and items sections apply, and the rehydration section reports success,
// rehydrationErr, and the deferred job paths.
func rehydrationResult(cr *v1alpha1.Controller, commandID string, success bool, rehydrationErr string, deferred ...string) *mitev1.CommandResult {
	return &mitev1.CommandResult{
		CommandId: commandID,
		Result: &mitev1.CommandResult_DesiredState{DesiredState: &mitev1.DesiredStateResult{
			AppliedHash:             cr.Status.DesiredStateHash,
			ConfigSuccess:           true,
			ItemsSuccess:            true,
			RehydrationSuccess:      success,
			RehydrationError:        rehydrationErr,
			RehydrationDeferredJobs: deferred,
		}},
	}
}

// tick runs one Connected tick for cr.
func tick(t *testing.T, rec *Reconciler, cr *v1alpha1.Controller) {
	t.Helper()
	_ = rec.handleConnected(context.Background(), cr, runners.RunnerContribution{})
}

// entryFor answers tenant's rehydration entry on cr.
func entryFor(t *testing.T, cr *v1alpha1.Controller, tenant string) v1alpha1.CellRehydrationEntry {
	t.Helper()
	if cr.Status.Cell == nil {
		t.Fatalf("no cell status, want an entry for %s", tenant)
	}
	for _, e := range cr.Status.Cell.Rehydration {
		if e.Tenant == tenant {
			return e
		}
	}
	t.Fatalf("no entry for %s in %+v", tenant, cr.Status.Cell.Rehydration)
	return v1alpha1.CellRehydrationEntry{}
}

// TestReconcileConnected_AutomaticCellSendsNothingForAPendingTenantWithNoBundle
// pins that a converged automatic-mode cell sends nothing on a tick that
// attaches no rehydration bundle. Such a push only sends the delivered
// state again. The drift window still bounds the wait.
func TestReconcileConnected_AutomaticCellSendsNothingForAPendingTenantWithNoBundle(t *testing.T) {
	failingRecords := func(context.Context, cells.RecordsRequest) (cells.RecordsResult, error) {
		return cells.RecordsResult{}, errors.New("record store unavailable")
	}
	failingSeeds := func(r *Reconciler) {
		r.SetBuildJobSeedFunc(func(jr rehydrate.JobRecords, _ int, _ string, _ rehydrate.Options) (rehydrate.JobSeed, error) {
			return rehydrate.JobSeed{}, errors.New("simulated render failure for " + jr.JobPath)
		})
	}
	for _, tc := range []struct {
		name      string
		records   cells.RecordsFunc
		configure func(*Reconciler)
		// prepare runs after the first tick.
		prepare func(*Reconciler, *scriptedSendTransport, *v1alpha1.Controller)
	}{
		{name: "records fetch fails", records: failingRecords},
		{name: "every seed fails to render", records: oneJobRecords, configure: failingSeeds},
		{
			name:    "past the deadline",
			records: oneJobRecords,
			prepare: func(rec *Reconciler, tr *scriptedSendTransport, cr *v1alpha1.Controller) {
				// The first push's result arrives with a deferred job, and
				// the tenant's deadline then passes.
				tr.results = []*mitev1.CommandResult{rehydrationResult(cr, tr.last.CommandId, true, "", "team-a/build")}
				key := cr.Namespace + "/" + cr.Name + "/team-a"
				mark := rec.rehydrationPendingSince[key]
				mark.Since = time.Now().Add(-time.Hour)
				rec.rehydrationPendingSince[key] = mark
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, tr, cr := newRehydrationCell(t, "cell-nobundle", tc.records, tc.configure)
			if tc.prepare != nil {
				tc.prepare(rec, tr, cr)
			}
			tick(t, rec, cr)
			tick(t, rec, cr)
			if tr.sends != 1 {
				t.Fatalf("sends = %d, want 1: a tick with no bundle sends nothing", tr.sends)
			}
			if tc.name == "past the deadline" {
				e := entryFor(t, cr, "team-a")
				if !strings.Contains(e.Error, "deadline") {
					t.Fatalf("entry = %+v, want the deadline error", e)
				}
				if !e.Applied || !slices.Equal(e.Deferred, []string{"team-a/build"}) {
					t.Fatalf("entry = %+v, want the released give-up applied with its deferred job", e)
				}
				if got := seedAnswerOf(&e); got.instance != cr.Status.Cell.Instance || len(got.deferred) != 0 {
					t.Fatalf("seed answer = (%q, %v), want (%q, none)", got.instance, got.deferred, cr.Status.Cell.Instance)
				}
			}
		})
	}
}

// TestReconcileConnected_AutomaticCellPushesAnAttachedBundleOnAnUnchangedHash
// pins that an attached bundle still bypasses the short-circuit. The
// bundle stays outside the full hash.
func TestReconcileConnected_AutomaticCellPushesAnAttachedBundleOnAnUnchangedHash(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-retry", oneJobRecords, nil)
	delivered := cr.Status.DesiredStateHash
	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, tr.last.CommandId, true, "", "team-a/build")}

	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, bundle = %v, want a second push with a bundle", tr.sends, tr.last.Rehydration != nil)
	}
	if cr.Status.DesiredStateHash != delivered {
		t.Fatalf("desiredStateHash = %q, want the unchanged hash %q", cr.Status.DesiredStateHash, delivered)
	}
	if got := entryFor(t, cr, "team-a").CommandID; got != tr.last.CommandId {
		t.Fatalf("entry command = %q, want the sent command %q", got, tr.last.CommandId)
	}
}

// TestReconcileConnected_ResendKeepsTheAppliedSeed pins that a deferred job
// does not drop the seed of an applied bundle: the resend that follows the
// result carries the applied seed and its deferred job until its own result.
func TestReconcileConnected_ResendKeepsTheAppliedSeed(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-resend", oneJobRecords, nil)
	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, tr.last.CommandId, true, "", "team-a/build")}

	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, bundle = %v, want a resend with a bundle", tr.sends, tr.last.Rehydration != nil)
	}
	e := entryFor(t, cr, "team-a")
	if !e.Applied || !slices.Equal(e.Deferred, []string{"team-a/build"}) || e.CommandID != tr.last.CommandId {
		t.Fatalf("entry = %+v, want the applied seed with its deferred job on the resend %q", e, tr.last.CommandId)
	}
}

// TestReconcileConnected_SlowRehydrationApplyCountsAfterALaterTick pins
// that a result that arrives after a later tick still counts. The later
// tick holds: it sends nothing and keeps the stamped command ID.
func TestReconcileConnected_SlowRehydrationApplyCountsAfterALaterTick(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-slow", oneJobRecords, nil)
	pushA := tr.last.CommandId
	if tr.last.Rehydration == nil {
		t.Fatal("the first push carries no bundle")
	}

	tick(t, rec, cr)
	if tr.sends != 1 {
		t.Fatalf("sends = %d, want 1: the tick must hold the in-flight push", tr.sends)
	}
	if got := entryFor(t, cr, "team-a").CommandID; got != pushA {
		t.Fatalf("entry command = %q, want the in-flight command %q", got, pushA)
	}

	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, pushA, true, "")}
	tick(t, rec, cr)
	if e := entryFor(t, cr, "team-a"); !rehydrationApplied(&e, 1, "uid-1") {
		t.Fatalf("entry = %+v, want the late result applied", e)
	}
	if tr.sends != 1 {
		t.Fatalf("sends = %d, want 1: an applied tenant needs no push", tr.sends)
	}
}

// TestReconcileConnected_RehydrationHoldEndsAfterTheWindow pins that a
// push with no result inside its window gets a new bundle and a new
// command ID.
func TestReconcileConnected_RehydrationHoldEndsAfterTheWindow(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-window", oneJobRecords, nil)
	pushA := tr.last.CommandId
	key := cr.Namespace + "/" + cr.Name
	rec.rehydrationInFlight[key].Until = time.Now().Add(-time.Second)

	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, want a new push with a bundle after the window", tr.sends)
	}
	if got := entryFor(t, cr, "team-a").CommandID; got == pushA || got != tr.last.CommandId {
		t.Fatalf("entry command = %q, want the new command %q", got, tr.last.CommandId)
	}
}

// TestRehydrationPushWindow_FollowsTheCommandDeadline pins the window: the
// mite's command deadline plus the result's travel time.
func TestRehydrationPushWindow_FollowsTheCommandDeadline(t *testing.T) {
	if got := rehydrationPushWindow(&mitev1.DesiredStateCommand{}); got != 21*time.Minute {
		t.Fatalf("default window = %v, want 21m", got)
	}
	if got := rehydrationPushWindow(&mitev1.DesiredStateCommand{CommandDeadlineSec: 300}); got != 6*time.Minute {
		t.Fatalf("window = %v, want 6m for a 300 s deadline", got)
	}
}

// TestReconcileConnected_RehydrationHoldEndsOnReconnect pins that a new
// mite process ends the hold. The process holds no command.
func TestReconcileConnected_RehydrationHoldEndsOnReconnect(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-reconnect", oneJobRecords, nil)
	pushA := tr.last.CommandId
	tr.epoch = 2

	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, want the reconnect push with a bundle", tr.sends)
	}
	if got := entryFor(t, cr, "team-a").CommandID; got == pushA {
		t.Fatalf("entry command = %q, want the reconnect push's command", got)
	}
}

// placeTeamB places a second tenant, team-b, on cr's cell.
func placeTeamB(rec *Reconciler, cr *v1alpha1.Controller) {
	crdstore.MustSeed(rec.client.(*testClient).store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec:       v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{}},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: cr.Name, CellNamespace: "default", Generation: 1},
		},
	})
	cr.Status.Cell.Tenants = []string{"team-a", "team-b"}
}

// TestReconcileConnected_RehydrationHoldKeepsWhileAnotherTenantHasNoSeed
// pins that a tenant outside the held push with no seed to send does not
// end the hold. Its records fetch fails on each tick, and ending the hold
// for it would replace the stamped command ID on each tick. The result of
// the first push still counts.
func TestReconcileConnected_RehydrationHoldKeepsWhileAnotherTenantHasNoSeed(t *testing.T) {
	records := func(ctx context.Context, req cells.RecordsRequest) (cells.RecordsResult, error) {
		if req.Tenant == "team-b" {
			return cells.RecordsResult{}, errors.New("record store unavailable")
		}
		return perTenantRecords(ctx, req)
	}
	rec, tr, cr := newRehydrationCell(t, "cell-otherfails", records, nil)
	pushA := tr.last.CommandId
	heldBundle := tr.last.Rehydration.BundleId
	placeTeamB(rec, cr)

	// The new tenant's homes change the full hash, so this tick pushes.
	// The push carries the held bundle.
	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil || tr.last.Rehydration.BundleId != heldBundle {
		t.Fatalf("sends = %d, want the hash-change push with the held bundle %q", tr.sends, heldBundle)
	}
	tick(t, rec, cr)
	if tr.sends != 2 {
		t.Fatalf("sends = %d, want 2: the hold must survive a tenant with no seed", tr.sends)
	}
	if got := entryFor(t, cr, "team-a").CommandID; got != pushA {
		t.Fatalf("entry command = %q, want the in-flight command %q", got, pushA)
	}

	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, pushA, true, "")}
	tick(t, rec, cr)
	if e := entryFor(t, cr, "team-a"); !rehydrationApplied(&e, 1, "uid-1") {
		t.Fatalf("entry = %+v, want the late result applied", e)
	}
}

// TestReconcileConnected_RehydrationHoldEndsWhenAnotherTenantHasASeed pins
// that a tenant placed during the hold gets a bundle at once. The new
// bundle covers every pending tenant.
func TestReconcileConnected_RehydrationHoldEndsWhenAnotherTenantHasASeed(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-newtenant", perTenantRecords, nil)
	placeTeamB(rec, cr)

	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, want a new push with a bundle for the new tenant", tr.sends)
	}
	for _, tenant := range []string{"team-a", "team-b"} {
		if got := entryFor(t, cr, tenant).CommandID; got != tr.last.CommandId {
			t.Fatalf("%s entry command = %q, want the new command %q", tenant, got, tr.last.CommandId)
		}
	}
}

// TestReconcileConnected_FailedRehydrationResultEndsTheHold pins that a
// failed result ends the hold, and the next tick sends a new bundle.
func TestReconcileConnected_FailedRehydrationResultEndsTheHold(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-failed", oneJobRecords, nil)
	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, tr.last.CommandId, false, "disk full")}

	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, want a new push with a bundle after the failure", tr.sends)
	}
}

// TestReconcileConnected_UndeliveredRehydrationPushStampsNoCommandID pins
// that a push no gateway forwarded stamps no command ID and starts no
// hold. The next tick sends the bundle again.
func TestReconcileConnected_UndeliveredRehydrationPushStampsNoCommandID(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-undelivered", oneJobRecords, nil)
	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, tr.last.CommandId, true, "", "team-a/build")}
	tr.sendErr = transport.ErrMiteUnreachable

	tick(t, rec, cr)
	if tr.sends != 2 {
		t.Fatalf("sends = %d, want an attempted push", tr.sends)
	}
	if got := entryFor(t, cr, "team-a").CommandID; got != "" {
		t.Fatalf("entry command = %q, want none for an undelivered push", got)
	}
	if rec.rehydrationInFlightFor(cr.Namespace+"/"+cr.Name) != nil {
		t.Fatal("an undelivered push started a hold")
	}

	tr.sendErr = nil
	tick(t, rec, cr)
	if tr.sends != 3 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, want the bundle sent again", tr.sends)
	}
	if got := entryFor(t, cr, "team-a").CommandID; got != tr.last.CommandId {
		t.Fatalf("entry command = %q, want the delivered command %q", got, tr.last.CommandId)
	}
}

// TestReconcileConnected_PushWithoutBundleDoesNotMarkATenantApplied pins
// that a successful result of a push that carried no bundle leaves a
// pending tenant pending. The mite reports success for an absent
// rehydration section, so an entry that names that push's command ID
// would count as applied with no seed written.
func TestReconcileConnected_PushWithoutBundleDoesNotMarkATenantApplied(t *testing.T) {
	failingBuild := func(r *Reconciler) {
		r.buildBundle = func(rehydrate.BudgetResult) (*mitev1.RehydrationBundle, error) {
			return nil, errors.New("simulated bundle build failure")
		}
	}
	rec, tr, cr := newRehydrationCell(t, "cell-nobuild", oneJobRecords, failingBuild)
	if tr.last.Rehydration != nil {
		t.Fatal("the first push carries a bundle, want none")
	}
	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, tr.last.CommandId, true, "")}
	tr.epoch = 2

	tick(t, rec, cr)
	if tr.sends != 2 {
		t.Fatalf("sends = %d, want the reconnect push", tr.sends)
	}
	e := entryFor(t, cr, "team-a")
	if e.Applied || e.CommandID != "" {
		t.Fatalf("entry = %+v, want a pending entry with no command ID", e)
	}
}

// TestReconcileConnected_HeldBundleRidesAnotherPushAndEitherResultCounts
// pins that a push that goes out during the hold carries the held bundle.
// The mite keeps only the newest desired state, so the result of either
// push counts.
func TestReconcileConnected_HeldBundleRidesAnotherPushAndEitherResultCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		which int
	}{
		{"result of the first push", 0},
		{"result of the drift push", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, tr, cr := newRehydrationCell(t, "cell-ride", oneJobRecords, nil)
			first := tr.last
			// The drift window passes, so the tick pushes the delivered
			// state again.
			old := metav1.NewTime(time.Now().Add(-6 * time.Minute))
			cr.Status.LastDesiredPushAt = &old

			tick(t, rec, cr)
			if tr.sends != 2 || tr.last.Rehydration == nil {
				t.Fatalf("sends = %d, want the drift push with the held bundle", tr.sends)
			}
			if tr.last.Rehydration.BundleId != first.Rehydration.BundleId {
				t.Fatalf("bundle = %q, want the held bundle %q", tr.last.Rehydration.BundleId, first.Rehydration.BundleId)
			}
			if got := entryFor(t, cr, "team-a").CommandID; got != first.CommandId {
				t.Fatalf("entry command = %q, want the first command %q", got, first.CommandId)
			}

			ids := []string{first.CommandId, tr.last.CommandId}
			tr.results = []*mitev1.CommandResult{rehydrationResult(cr, ids[tc.which], true, "")}
			tick(t, rec, cr)
			if e := entryFor(t, cr, "team-a"); !rehydrationApplied(&e, 1, "uid-1") {
				t.Fatalf("entry = %+v, want the result applied", e)
			}
		})
	}
}

// TestReconcileConnected_HoldEndedByAnotherTenantKeepsTheOldResult pins
// that a hold that a new tenant's seed ends keeps the in-flight push
// until a delivered push replaces it. The replacement push is not
// delivered here, so the result of the first push must still count.
func TestReconcileConnected_HoldEndedByAnotherTenantKeepsTheOldResult(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-keep", perTenantRecords, nil)
	pushA := tr.last.CommandId
	firstBundle := tr.last.Rehydration.BundleId
	placeTeamB(rec, cr)
	tr.sendErr = transport.ErrMiteUnreachable

	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, want an attempted push with a new bundle", tr.sends)
	}
	if e := entryFor(t, cr, "team-a"); e.CommandID != pushA || e.BundleID != firstBundle {
		t.Fatalf("team-a entry = %+v, want the whole entry of the in-flight push %q", e, pushA)
	}
	if got := entryFor(t, cr, "team-b").CommandID; got != "" {
		t.Fatalf("team-b entry command = %q, want none before a delivered push", got)
	}

	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, pushA, true, "")}
	tick(t, rec, cr)
	if e := entryFor(t, cr, "team-a"); !rehydrationApplied(&e, 1, "uid-1") {
		t.Fatalf("team-a entry = %+v, want the first push's result applied", e)
	}
}

// TestReconcileConnected_DeliveredPushReplacesAKeptEntry pins that a
// delivered push writes the new entry of a tenant whose entry still
// described the kept push.
func TestReconcileConnected_DeliveredPushReplacesAKeptEntry(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-replace", perTenantRecords, nil)
	pushA := tr.last.CommandId
	placeTeamB(rec, cr)

	tick(t, rec, cr)
	if tr.sends != 2 || tr.last.Rehydration == nil {
		t.Fatalf("sends = %d, want a delivered push with a new bundle", tr.sends)
	}
	for _, tenant := range []string{"team-a", "team-b"} {
		e := entryFor(t, cr, tenant)
		if e.CommandID != tr.last.CommandId || e.BundleID != tr.last.Rehydration.BundleId {
			t.Fatalf("%s entry = %+v, want the new push %q and bundle %q", tenant, e, tr.last.CommandId, tr.last.Rehydration.BundleId)
		}
	}
	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, pushA, true, "")}
	tick(t, rec, cr)
	if e := entryFor(t, cr, "team-a"); e.Applied {
		t.Fatalf("team-a entry = %+v, want the replaced push's result ignored", e)
	}
}

// TestReconcileConnected_FailedBuildKeepsTheKeptStamp pins that a bundle
// build failure after a new tenant ended the hold keeps the stamp of the
// kept push. Its result must still count.
func TestReconcileConnected_FailedBuildKeepsTheKeptStamp(t *testing.T) {
	rec, tr, cr := newRehydrationCell(t, "cell-keepfail", perTenantRecords, nil)
	pushA := tr.last.CommandId
	placeTeamB(rec, cr)
	rec.buildBundle = func(rehydrate.BudgetResult) (*mitev1.RehydrationBundle, error) {
		return nil, errors.New("simulated bundle build failure")
	}

	tick(t, rec, cr)
	if got := entryFor(t, cr, "team-a").CommandID; got != pushA {
		t.Fatalf("team-a entry command = %q, want the kept command %q", got, pushA)
	}

	tr.results = []*mitev1.CommandResult{rehydrationResult(cr, pushA, true, "")}
	tick(t, rec, cr)
	if e := entryFor(t, cr, "team-a"); !rehydrationApplied(&e, 1, "uid-1") {
		t.Fatalf("team-a entry = %+v, want the kept push's result applied", e)
	}
}
