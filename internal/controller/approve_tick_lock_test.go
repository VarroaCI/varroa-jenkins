package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// approveDuringTick runs approve on its own goroutine right after the tick's
// first read of ns/mine. The hook gives the approve a short window to finish
// before the tick goes on. Without the lock the approve finishes inside that
// window and the tick's end write comes after it. It returns the approve's
// error after the tick and the approve both end.
func approveDuringTick(t *testing.T, client *testClient, rec *Reconciler, approve func() error) error {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("controller gvr: %v", err)
	}
	// An atomic flag, not a sync.Once: the approve's own read runs this
	// hook too, and a sync.Once would block it until the window ends.
	var fired atomic.Bool
	done := make(chan error, 1)
	started := make(chan struct{})
	client.store.SetAfterGet(gvr, func(string, string) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		close(started)
		go func() { done <- approve() }()
		select {
		case err := <-done:
			done <- err
		case <-time.After(200 * time.Millisecond):
		}
	})
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "mine"}}
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("the tick did not read the controller")
	}
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the approve did not finish")
		return nil
	}
}

func getMine(t *testing.T, store *crdstore.Fake) *v1alpha1.Controller {
	t.Helper()
	got, err := crdstore.Get[v1alpha1.Controller](context.Background(), store, "mine", "ns")
	if err != nil {
		t.Fatalf("get controller: %v", err)
	}
	return got
}

// TestReconcile_PluginRollApproveDuringTheTickSurvivesTheEndWrite pins that
// a plugin-roll approval that arrives after the tick's read is not reverted
// by the tick's end-of-tick status write.
func TestReconcile_PluginRollApproveDuringTheTickSurvivesTheEndWrite(t *testing.T) {
	client := newTestClient()
	client.statusToStore = true
	rec := newTestReconciler(client)
	rec.miteTransport = &captureTransport{}
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseFailed)
	cr.ResourceVersion = "1"
	cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{TargetChecksum: "abc"}
	crdstore.MustSeed(client.store, cr)

	err := approveDuringTick(t, client, rec, func() error {
		return rec.ApproveRestart(context.Background(), "", "ns", "mine", "plugin-roll", "")
	})
	if err != nil {
		t.Fatalf("approve plugin roll: %v", err)
	}
	got := getMine(t, client.store)
	if got.Status.ApprovedPluginRollChecksum != "abc" || got.Status.PendingPluginRoll != nil {
		t.Fatalf("approved = %q pending = %v, want abc and none",
			got.Status.ApprovedPluginRollChecksum, got.Status.PendingPluginRoll)
	}
}

// TestReconcile_DeletionApproveDuringTheTickSurvivesTheEndWrite pins that a
// deletion approval that arrives after the tick's read is not reverted by
// the tick's end-of-tick status write.
func TestReconcile_DeletionApproveDuringTheTickSurvivesTheEndWrite(t *testing.T) {
	client := newTestClient()
	client.statusToStore = true
	rec := newTestReconciler(client)
	rec.miteTransport = &captureTransport{}
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseFailed)
	cr.ResourceVersion = "1"
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "team/job"}}
	crdstore.MustSeed(client.store, cr)

	err := approveDuringTick(t, client, rec, func() error {
		return rec.ApproveDeletion(context.Background(), "", "ns", "mine", "team/job")
	})
	if err != nil {
		t.Fatalf("approve deletion: %v", err)
	}
	got := getMine(t, client.store)
	if len(got.Status.PendingItemDeletions) != 0 {
		t.Fatalf("pendingItemDeletions = %v, want none", got.Status.PendingItemDeletions)
	}
}

// TestApproveRestart_WaitsForANormalTick pins that an approve waits for a
// tick that holds the lock for less than approveLockWait, then succeeds.
func TestApproveRestart_WaitsForANormalTick(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.miteTransport = &captureTransport{}
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{TargetChecksum: "abc"}
	crdstore.MustSeed(client.store, cr)

	if err := rec.controllerLocks.Lock(context.Background(), "ns/mine"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		rec.controllerLocks.Unlock("ns/mine")
	}()
	if err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "plugin-roll", ""); err != nil {
		t.Fatalf("approve plugin roll: %v", err)
	}
	if got := getMine(t, client.store); got.Status.ApprovedPluginRollChecksum != "abc" {
		t.Fatalf("approved = %q, want abc", got.Status.ApprovedPluginRollChecksum)
	}
}

// TestApproveRestart_ReturnsARetryableErrorWhenTheTickHoldsTheLock pins
// that an approve gives up after approveLockWait with an error that tells
// the user to retry, and writes nothing.
func TestApproveRestart_ReturnsARetryableErrorWhenTheTickHoldsTheLock(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.miteTransport = &captureTransport{}
	rec.approveLockWait = 20 * time.Millisecond
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{TargetChecksum: "abc"}
	crdstore.MustSeed(client.store, cr)

	if err := rec.controllerLocks.Lock(context.Background(), "ns/mine"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer rec.controllerLocks.Unlock("ns/mine")
	err := rec.ApproveRestart(context.Background(), "", "ns", "mine", "plugin-roll", "")
	if err == nil || !strings.Contains(err.Error(), "retry the approve") {
		t.Fatalf("err = %v, want a retryable error", err)
	}
	got := getMine(t, client.store)
	if got.Status.ApprovedPluginRollChecksum != "" || got.Status.PendingPluginRoll == nil {
		t.Fatalf("approved = %q pending = %v, want no change",
			got.Status.ApprovedPluginRollChecksum, got.Status.PendingPluginRoll)
	}
}

// TestApproveDeletion_ReturnsARetryableErrorWhenTheTickHoldsTheLock pins
// that a deletion approval gives up after approveLockWait with an error that
// tells the user to retry, and sends nothing to the mite.
func TestApproveDeletion_ReturnsARetryableErrorWhenTheTickHoldsTheLock(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	tr := &captureTransport{}
	rec.miteTransport = tr
	rec.approveLockWait = 20 * time.Millisecond
	cr := testController("mine", "ns", v1alpha1.ControllerPhaseConnected)
	cr.ResourceVersion = "1"
	cr.Status.PendingItemDeletions = []v1alpha1.PendingDeletion{{Path: "team/job"}}
	crdstore.MustSeed(client.store, cr)

	if err := rec.controllerLocks.Lock(context.Background(), "ns/mine"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer rec.controllerLocks.Unlock("ns/mine")
	err := rec.ApproveDeletion(context.Background(), "", "ns", "mine", "team/job")
	if err == nil || !strings.Contains(err.Error(), "retry the approve") {
		t.Fatalf("err = %v, want a retryable error", err)
	}
	if len(tr.imperatives) != 0 {
		t.Fatalf("imperatives = %d, want none", len(tr.imperatives))
	}
	if got := getMine(t, client.store); len(got.Status.PendingItemDeletions) != 1 {
		t.Fatalf("pendingItemDeletions = %v, want the entry kept", got.Status.PendingItemDeletions)
	}
}
