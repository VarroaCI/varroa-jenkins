package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func TestTickerRunnable_NeedLeaderElection(t *testing.T) {
	tr := &tickerRunnable{}
	if !tr.NeedLeaderElection() {
		t.Error("NeedLeaderElection() should return true")
	}
}

func TestTickerRunnable_ImmediateTick(t *testing.T) {
	var counter atomic.Int32
	tr := &tickerRunnable{
		name:      "test-immediate",
		interval:  10 * time.Millisecond,
		immediate: true,
		tick:      func(ctx context.Context) { counter.Add(1) },
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_ = tr.Start(ctx)

	if n := counter.Load(); n < 1 {
		t.Errorf("immediate tick should have fired at least once, got %d", n)
	}
}

func TestTickerRunnable_NoImmediateTick(t *testing.T) {
	var counter atomic.Int32
	tr := &tickerRunnable{
		name:      "test-no-immediate",
		interval:  10 * time.Millisecond,
		immediate: false,
		tick:      func(ctx context.Context) { counter.Add(1) },
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_ = tr.Start(ctx)

	if n := counter.Load(); n != 0 {
		t.Errorf("immediate=false should not tick before interval, got %d", n)
	}
}

func TestTickerRunnable_CtxCancelReturnsNil(t *testing.T) {
	var counter atomic.Int32
	tr := &tickerRunnable{
		name:      "test-cancel",
		interval:  1 * time.Hour,
		immediate: false,
		tick:      func(ctx context.Context) { counter.Add(1) },
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- tr.Start(ctx)
	}()

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start should return nil on ctx cancel, got: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Start did not return within 100ms of cancel")
	}
}

func TestTickerRunnable_SetupError(t *testing.T) {
	var counter atomic.Int32
	tr := &tickerRunnable{
		name:      "test-setup-error",
		interval:  10 * time.Millisecond,
		immediate: true,
		setup:     func(ctx context.Context) error { return errors.New("boom") },
		tick:      func(ctx context.Context) { counter.Add(1) },
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := tr.Start(ctx)
	if err == nil {
		t.Fatal("Start should return error when setup fails")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should wrap 'boom', got: %v", err)
	}
	if n := counter.Load(); n != 0 {
		t.Errorf("tick should never run after setup error, got %d", n)
	}
}

// TestMain_ProfileCandidateReconcilerRegistered guards against the operator
// constructing a ProfileCandidateReconciler without ever running it (or vice
// versa): with no runnable driving it, no candidate ever leaves Pending and
// promotion can never succeed.
func TestMain_ProfileCandidateReconcilerRegistered(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "controller.NewProfileCandidateReconciler(") {
		t.Fatal("main.go does not construct a ProfileCandidateReconciler")
	}
	if !strings.Contains(text, "reconcileAllProfileCandidates(") {
		t.Fatal("main.go constructs a ProfileCandidateReconciler but never drives it from a registered runnable")
	}
}

func TestTickerRunnable_SetupBeforeImmediateTick(t *testing.T) {
	var setupRan bool
	var tickRan bool
	tr := &tickerRunnable{
		name:      "test-setup-order",
		interval:  10 * time.Millisecond,
		immediate: true,
		setup: func(ctx context.Context) error {
			setupRan = true
			return nil
		},
		tick: func(ctx context.Context) {
			if !setupRan {
				t.Error("tick ran before setup completed")
			}
			tickRan = true
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_ = tr.Start(ctx)

	if !setupRan {
		t.Error("setup should have run")
	}
	if !tickRan {
		t.Error("immediate tick should have run")
	}
}

// stubCandidateReconciler is a reconcile.Reconciler test double that lets a
// test control, per candidate name, the Result and error
// reconcileAllProfileCandidates observes, and records how many times each
// name was actually reconciled.
type stubCandidateReconciler struct {
	results map[string]reconcile.Result
	errs    map[string]error
	calls   map[string]int
}

func newStubCandidateReconciler() *stubCandidateReconciler {
	return &stubCandidateReconciler{
		results: make(map[string]reconcile.Result),
		errs:    make(map[string]error),
		calls:   make(map[string]int),
	}
}

func (s *stubCandidateReconciler) Reconcile(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
	s.calls[req.Name]++
	return s.results[req.Name], s.errs[req.Name]
}

func seedProfileCandidate(t *testing.T, store crdstore.Backend, name string) {
	t.Helper()
	pc := &v1alpha1.ProfileCandidate{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := crdstore.Create[v1alpha1.ProfileCandidate](context.Background(), store, pc); err != nil {
		t.Fatalf("seed ProfileCandidate %s: %v", name, err)
	}
}

func TestReconcileAllProfileCandidates_BackoffSkipsUntilDue(t *testing.T) {
	ctx := context.Background()
	store := crdstore.NewFake()
	seedProfileCandidate(t, store, "cand-a")

	rec := newStubCandidateReconciler()
	rec.results["cand-a"] = reconcile.Result{RequeueAfter: 5 * time.Minute}

	backoff := newProfileCandidateBackoff()
	now := time.Now()
	backoff.now = func() time.Time { return now }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// First tick reconciles and records a 5-minute backoff.
	reconcileAllProfileCandidates(ctx, store, rec, backoff, logger)
	if rec.calls["cand-a"] != 1 {
		t.Fatalf("expected 1 reconcile call after first tick, got %d", rec.calls["cand-a"])
	}

	// A tick that lands inside the backoff window is skipped.
	now = now.Add(1 * time.Minute)
	reconcileAllProfileCandidates(ctx, store, rec, backoff, logger)
	if rec.calls["cand-a"] != 1 {
		t.Fatalf("expected candidate skipped inside backoff window, got %d calls", rec.calls["cand-a"])
	}

	// A tick after the backoff window reconciles again.
	now = now.Add(5 * time.Minute)
	reconcileAllProfileCandidates(ctx, store, rec, backoff, logger)
	if rec.calls["cand-a"] != 2 {
		t.Fatalf("expected candidate reconciled again after backoff window, got %d calls", rec.calls["cand-a"])
	}
}

func TestReconcileAllProfileCandidates_NoRequeueReconciledEveryTick(t *testing.T) {
	ctx := context.Background()
	store := crdstore.NewFake()
	seedProfileCandidate(t, store, "cand-b")

	rec := newStubCandidateReconciler() // zero-value Result: no RequeueAfter
	backoff := newProfileCandidateBackoff()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for i := 1; i <= 3; i++ {
		reconcileAllProfileCandidates(ctx, store, rec, backoff, logger)
		if rec.calls["cand-b"] != i {
			t.Fatalf("tick %d: expected %d reconcile calls, got %d", i, i, rec.calls["cand-b"])
		}
	}
}

func TestReconcileAllProfileCandidates_PrunesDeletedCandidates(t *testing.T) {
	ctx := context.Background()
	store := crdstore.NewFake()
	seedProfileCandidate(t, store, "cand-c")

	rec := newStubCandidateReconciler()
	rec.results["cand-c"] = reconcile.Result{RequeueAfter: 5 * time.Minute}
	backoff := newProfileCandidateBackoff()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	reconcileAllProfileCandidates(ctx, store, rec, backoff, logger)
	if _, tracked := backoff.next["cand-c"]; !tracked {
		t.Fatal("expected cand-c to be tracked in the backoff map after reconciling")
	}

	if err := crdstore.Delete[v1alpha1.ProfileCandidate](ctx, store, "cand-c", ""); err != nil {
		t.Fatalf("delete cand-c: %v", err)
	}

	reconcileAllProfileCandidates(ctx, store, rec, backoff, logger)
	if _, tracked := backoff.next["cand-c"]; tracked {
		t.Fatal("expected backoff entry for deleted candidate to be pruned")
	}
	if len(backoff.next) != 0 {
		t.Fatalf("expected backoff map to be empty after pruning, got %d entries", len(backoff.next))
	}
}
