package controller

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestAnnounceSingleTenantPlacement_RequestFields asserts task 4.4b's
// request shape: a controller that sets no cell role announces a
// generation-zero row naming itself as both tenant and cell, with an
// empty policy and the rehydrated instance set to its own pod.
func TestAnnounceSingleTenantPlacement_RequestFields(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	rec.SetClusterName("core")
	lastSeen := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	var calls int
	var got cells.AnnounceRequest
	rec.SetSyncAnnouncer(func(_ context.Context, req cells.AnnounceRequest) (cells.AnnounceResult, error) {
		calls++
		got = req
		return cells.AnnounceResult{OK: true}, nil
	})

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "team-ns"},
		Status: v1alpha1.ControllerStatus{
			Cell: &v1alpha1.CellStatus{Instance: "pod-uid-1"},
			MiteStatus: &v1alpha1.MiteStatus{
				Connected: true,
				LastSeen:  &lastSeen,
			},
		},
	}

	rec.announceSingleTenantPlacement(context.Background(), cr)

	if calls != 1 {
		t.Fatalf("expected exactly one announce call, got %d", calls)
	}
	if got.Cluster != "core" {
		t.Errorf("Cluster = %q, want core", got.Cluster)
	}
	if got.Tenant != "team-ns/solo" {
		t.Errorf("Tenant = %q, want team-ns/solo (the gateway resolver's own stamp)", got.Tenant)
	}
	if got.Namespace != "team-ns" || got.Cell != "solo" {
		t.Errorf("Namespace/Cell = %q/%q, want team-ns/solo", got.Namespace, got.Cell)
	}
	if got.Generation != 0 {
		t.Errorf("Generation = %d, want 0", got.Generation)
	}
	if got.CellInstance != "pod-uid-1" || got.RehydratedInstance != "pod-uid-1" {
		t.Errorf("CellInstance/RehydratedInstance = %q/%q, want pod-uid-1/pod-uid-1", got.CellInstance, got.RehydratedInstance)
	}
	if !got.Policy.Equal(cells.SyncPolicy{}) {
		t.Errorf("Policy = %+v, want empty", got.Policy)
	}
	if !got.CellAlive {
		t.Error("CellAlive = false, want true")
	}
	if got.CellAliveAt == nil {
		t.Error("CellAliveAt is nil")
	}
	if got.CellLastSeenAt == nil || !got.CellLastSeenAt.Equal(lastSeen.Time) {
		t.Errorf("CellLastSeenAt = %v, want %v", got.CellLastSeenAt, lastSeen.Time)
	}
}

// TestAnnounceSingleTenantPlacement_SkipsCellController asserts that a
// controller that sets a cell role is never announced by this path: C5's
// placement pass (section 4's cells.Place) owns that tenant's row instead.
func TestAnnounceSingleTenantPlacement_SkipsCellController(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	rec.SetClusterName("core")

	var calls int
	rec.SetSyncAnnouncer(func(context.Context, cells.AnnounceRequest) (cells.AnnounceResult, error) {
		calls++
		return cells.AnnounceResult{OK: true}, nil
	})

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "cells-ns"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
	}
	rec.announceSingleTenantPlacement(context.Background(), cr)
	if calls != 0 {
		t.Fatalf("expected no announce for a cell controller, got %d calls", calls)
	}
}

// TestReconcileController_AnnouncesBeforeFirstDesiredState asserts task
// 4.4b's ordering requirement: the generation-zero announcement is sent on
// the reconcile tick that provisions the controller, before
// buildDesiredStateCommand's first call — which, for a controller that has
// only just reached the Pending phase, has not happened at all yet this
// tick. It also proves the announce runs unconditionally (independent of
// phase-specific desired-state logic), which is what "before the first
// desired state" requires: the row must exist by the time any later tick
// reaches handleConnected and builds one.
func TestReconcileController_AnnouncesBeforeFirstDesiredState(t *testing.T) {
	rec := newTestReconciler(newTestClientWithBundle())
	rec.SetClusterName("core")

	var calls int
	rec.SetSyncAnnouncer(func(_ context.Context, req cells.AnnounceRequest) (cells.AnnounceResult, error) {
		calls++
		if req.Generation != 0 || req.Tenant != "ns/solo" {
			t.Errorf("unexpected announce request: %+v", req)
		}
		return cells.AnnounceResult{OK: true}, nil
	})

	cr := testController("solo", "ns", "")
	cr.Spec.Cell = nil // no cell role: this is what task 4.4b covers.

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one announce on the tick that provisions the controller, got %d", calls)
	}
	// A brand new controller's first tick moves Pending -> Provisioning and
	// stops there (handlePending never calls buildDesiredStateCommand):
	// the announce is the only tenant.sync-shaped work this tick performs,
	// which is what "before the first desired state" means for the tick
	// that provisions a controller.
	if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
		t.Fatalf("expected the controller to reach Provisioning on this tick, got %q", cr.Status.Phase)
	}
}

// TestAnnounceSingleTenantPlacement_ClaimGranted asserts task 4.4b's other
// half against the real recorder responder (already shipped by C6 section
// 2): the announced row satisfies design D7's decision table for a claim
// from this controller's own pod — order 1 (a row exists), orders 2-6 (the
// claim's identity matches the row), and orders 8-9 (the seed is applied,
// because a generation-zero row always seeds itself). Section 3 has not
// landed the real Fence yet, so this test reads the row back directly
// instead of driving a live claim.
func TestAnnounceSingleTenantPlacement_ClaimGranted(t *testing.T) {
	dsn := os.Getenv("VARROA_TEST_DSN")
	if dsn == "" {
		t.Skip("VARROA_TEST_DSN is not set; skipping record-store integration test")
	}
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}
	ctx := context.Background()

	// RunMigrations is safe against a database another test is using
	// concurrently: it only ever creates the schema and applies pending
	// migrations under an advisory lock, and never drops anything.
	if err := recordstore.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	conn := startTestSyncBus(t)
	sub, err := recordstore.RegisterTenantSyncResponder(conn, pool, recordstore.DefaultConfirmedGoneGrace, nil, nil, nil)
	if err != nil {
		t.Fatalf("register responder: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	rec := newTestReconciler(newTestClient())
	rec.SetClusterName("c6-batch4-single-tenant-test")
	rec.SetSyncAnnouncer(NewNATSSyncAnnouncer(conn))

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "solo-claim-test", Namespace: "single-tenant-test-ns"},
		Status: v1alpha1.ControllerStatus{
			Cell:       &v1alpha1.CellStatus{Instance: "pod-uid-claim-test"},
			MiteStatus: &v1alpha1.MiteStatus{Connected: true},
		},
	}
	rec.announceSingleTenantPlacement(ctx, cr)

	var (
		generation         int64
		cellInstance       string
		rehydratedInstance string
		pendingJobsRaw     []byte
	)
	err = pool.QueryRow(ctx, `
		SELECT generation, cell_instance, rehydrated_instance, pending_jobs
		FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2
	`, "c6-batch4-single-tenant-test", "single-tenant-test-ns/solo-claim-test").
		Scan(&generation, &cellInstance, &rehydratedInstance, &pendingJobsRaw)
	if err != nil {
		t.Fatalf("read tenant_placements row: %v", err)
	}

	if generation != 0 {
		t.Errorf("generation = %d, want 0 (orders 2-3 pass trivially for a single-tenant controller)", generation)
	}
	if cellInstance != "pod-uid-claim-test" {
		t.Errorf("cell_instance = %q, want pod-uid-claim-test (order 6 needs this to match the claim)", cellInstance)
	}
	if rehydratedInstance != "pod-uid-claim-test" {
		t.Errorf("rehydrated_instance = %q, want pod-uid-claim-test (order 8 needs this to match the claim's cell instance)", rehydratedInstance)
	}
	if string(pendingJobsRaw) != "[]" {
		t.Errorf("pending_jobs = %s, want [] (order 9 needs no job pending)", pendingJobsRaw)
	}
}

// TestAnnounceSingleTenantPlacement_ReportsPreviousInstanceDead asserts a
// recorded previous pod uid rides the announcement as a dead instance
// with its last-seen time, and that no entry rides without one.
func TestAnnounceSingleTenantPlacement_ReportsPreviousInstanceDead(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	rec.SetClusterName("core")
	var got cells.AnnounceRequest
	rec.SetSyncAnnouncer(func(_ context.Context, req cells.AnnounceRequest) (cells.AnnounceResult, error) {
		got = req
		return cells.AnnounceResult{OK: true}, nil
	})
	seen := metav1.NewTime(time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "team-ns"},
		Status: v1alpha1.ControllerStatus{
			Cell: &v1alpha1.CellStatus{
				Instance:                 "pod-uid-2",
				PreviousInstance:         "pod-uid-1",
				PreviousInstanceLastSeen: &seen,
			},
		},
	}
	rec.announceSingleTenantPlacement(context.Background(), cr)
	if len(got.DeadInstances) != 1 || got.DeadInstances[0].Instance != "pod-uid-1" || !got.DeadInstances[0].LastSeenAt.Equal(seen.Time) {
		t.Fatalf("DeadInstances = %+v, want pod-uid-1 last seen %v", got.DeadInstances, seen.Time)
	}

	cr.Status.Cell.PreviousInstance = ""
	rec.announceSingleTenantPlacement(context.Background(), cr)
	if len(got.DeadInstances) != 0 {
		t.Fatalf("DeadInstances = %+v, want none without a previous instance", got.DeadInstances)
	}
}
