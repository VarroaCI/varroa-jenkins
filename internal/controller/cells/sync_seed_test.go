package cells

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestPlace_Reannounce_AnnouncesTheSeedTheCellReportsThisTick asserts the
// announcement carries the seed the bound cell reports on this tick, and
// not the one the Rehydrated condition holds from the tick before.
//
// The rehydration pass runs after this pass and writes that condition, so
// a read of the condition is always one tick behind the cell. The tenant
// would then be announced as unseeded on the tick its seed applies, and
// every claim would answer NOT_READY until the next announcement.
func TestPlace_Reannounce_AnnouncesTheSeedTheCellReportsThisTick(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	in.Cells = []CellCandidate{cell}
	tenant := testTenant()
	status := &v1alpha1.TenantStatus{}

	out1, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	// The cell now reports the tenant applied on uid-1. The Rehydrated
	// condition still reads False, exactly as it does before the
	// rehydration pass of this same tick runs.
	status.Conditions = append(status.Conditions, v1alpha1.TenantCondition{
		Type: v1alpha1.TenantConditionRehydrated, Status: metav1.ConditionFalse,
		Reason: v1alpha1.TenantReasonRehydrationPending,
	})
	in.MemberSeed = func(string, string) (string, []string) { return "uid-1", nil }
	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute))

	var calls int
	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		got = req
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one reannounce for the applied seed, got %d", calls)
	}
	if got.RehydratedInstance != "uid-1" {
		t.Errorf("RehydratedInstance = %q, want uid-1", got.RehydratedInstance)
	}
	if len(got.PendingJobs) != 0 {
		t.Errorf("expected no pending jobs once applied, got %v", got.PendingJobs)
	}
}

// TestPlace_Reannounce_ReportsNoSeedWhileTheCellReportsNone asserts the
// announcement carries no rehydrated instance while the bound cell has
// not applied the seed, even when the Rehydrated condition still reads
// True from the cell instance before the roll.
func TestPlace_Reannounce_ReportsNoSeedWhileTheCellReportsNone(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	in.Cells = []CellCandidate{cell}
	tenant := testTenant()
	status := &v1alpha1.TenantStatus{}

	out1, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	// The condition still reads True for the instance before the roll.
	status.Conditions = append(status.Conditions, v1alpha1.TenantCondition{
		Type: v1alpha1.TenantConditionRehydrated, Status: metav1.ConditionTrue,
		Reason: v1alpha1.TenantReasonTenantRehydrated,
	})
	status.Rehydration = &v1alpha1.TenantRehydrationStatus{CellInstance: "uid-1"}

	rolled := cell
	rolled.Instance = "uid-2"
	in.Cells = []CellCandidate{rolled}
	in.MemberSeed = func(string, string) (string, []string) { return "", []string{"acme/app"} }
	in.PendingJobs = []string{"acme/app"}
	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute))

	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		got = req
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got.RehydratedInstance != "" {
		t.Errorf("RehydratedInstance = %q, want empty while the cell reports no seed", got.RehydratedInstance)
	}
	if len(got.PendingJobs) != 1 {
		t.Errorf("PendingJobs = %v, want the deferred job while not applied", got.PendingJobs)
	}
}

// TestPlace_Reannounce_CarriesTheSeedAndTheDeferredJobs asserts an applied
// seed with a deferred job is announced with both: the instance the cell
// applied, and the one job that still waits.
func TestPlace_Reannounce_CarriesTheSeedAndTheDeferredJobs(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	in.Cells = []CellCandidate{cell}
	tenant := testTenant()
	status := &v1alpha1.TenantStatus{}

	out1, err := Place(context.Background(), in, tenant, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	in.MemberSeed = func(string, string) (string, []string) { return "uid-1", []string{"acme/app"} }
	in.PendingJobs = []string{"acme/app"}
	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute))

	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		got = req
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, tenant, status); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got.RehydratedInstance != "uid-1" {
		t.Errorf("RehydratedInstance = %q, want uid-1", got.RehydratedInstance)
	}
	if len(got.PendingJobs) != 1 || got.PendingJobs[0] != "acme/app" {
		t.Errorf("PendingJobs = %v, want [acme/app]", got.PendingJobs)
	}
}
