package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestNewMuxReadyzFailsBeforeMigrationsFinish asserts /readyz answers
// non-200 while migrations have not finished, and /healthz still
// answers 200.
func TestNewMuxReadyzFailsBeforeMigrationsFinish(t *testing.T) {
	var migrated atomic.Bool // zero value is false
	mux := newMux(&migrated, nil, func() bool { return true })

	readyRec := httptest.NewRecorder()
	mux.ServeHTTP(readyRec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readyRec.Code == http.StatusOK {
		t.Fatalf("/readyz status = %d, want a non-200 status before migrations finish", readyRec.Code)
	}

	healthRec := httptest.NewRecorder()
	mux.ServeHTTP(healthRec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthRec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want %d", healthRec.Code, http.StatusOK)
	}
}

// TestNewMuxReadyzFailsOnDisconnectedBus asserts /readyz answers
// non-200 once migrations finished but the bus is disconnected.
func TestNewMuxReadyzFailsOnDisconnectedBus(t *testing.T) {
	var migrated atomic.Bool
	migrated.Store(true)
	mux := newMux(&migrated, nil, func() bool { return false })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("/readyz status = %d, want a non-200 status while the bus is disconnected", rec.Code)
	}
}

// TestRunWorkersReturnsNilWhenContextCancelled asserts runWorkers returns
// nil once every one of the four workers returns after the context is
// cancelled: the normal shutdown path, not an early-return failure.
func TestRunWorkersReturnsNilWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blocker := func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- runWorkers(ctx,
			namedWorker{"ingest", blocker},
			namedWorker{"claim responder", blocker},
			namedWorker{"owner responder", blocker},
			namedWorker{"maintenance", blocker},
			namedWorker{"activity ingest", blocker},
		)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWorkers returned %v, want nil after a normal context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runWorkers did not return after the context was cancelled")
	}
}

// TestRunWorkersCancelsSiblingsWhenOneStopsEarly asserts one worker
// returning before the context is done makes runWorkers cancel the other
// three and return an error naming the worker that stopped.
func TestRunWorkersCancelsSiblingsWhenOneStopsEarly(t *testing.T) {
	var ingestCancelled, ownerCancelled, maintenanceCancelled, activityCancelled atomic.Bool
	watcher := func(flag *atomic.Bool) func(context.Context) error {
		return func(ctx context.Context) error {
			<-ctx.Done()
			flag.Store(true)
			return nil
		}
	}
	deadResponder := func(context.Context) error {
		return errors.New("boom")
	}

	done := make(chan error, 1)
	go func() {
		done <- runWorkers(context.Background(),
			namedWorker{"ingest", watcher(&ingestCancelled)},
			namedWorker{"claim responder", deadResponder},
			namedWorker{"owner responder", watcher(&ownerCancelled)},
			namedWorker{"maintenance", watcher(&maintenanceCancelled)},
			namedWorker{"activity ingest", watcher(&activityCancelled)},
		)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runWorkers returned nil, want a named error")
		}
		if !strings.Contains(err.Error(), "claim responder") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("error %v does not name the worker that stopped early and its cause", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runWorkers did not return after a worker stopped early")
	}
	if !ingestCancelled.Load() || !ownerCancelled.Load() || !maintenanceCancelled.Load() || !activityCancelled.Load() {
		t.Fatal("runWorkers did not cancel the surviving workers")
	}
}

// TestAwaitShutdownReturnsHTTPServerError binds the listen address first,
// so a real ListenAndServe on it fails, then asserts awaitShutdown
// returns that failure instead of treating it as a normal shutdown.
func TestAwaitShutdownReturnsHTTPServerError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	occupied := &http.Server{Addr: listener.Addr().String()}
	serverErr := make(chan error, 1)
	go func() {
		if err := occupied.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	workerDone := make(chan error, 1)
	workerReceived, got := awaitShutdown(context.Background(), serverErr, workerDone)
	if got == nil {
		t.Fatal("awaitShutdown returned nil, want the occupied-address error")
	}
	if workerReceived {
		t.Fatal("awaitShutdown reported workerReceived=true for an HTTP server error")
	}
}

// TestAwaitShutdownReturnsNilOnContextDone asserts the normal shutdown
// signal produces no error and no worker read.
func TestAwaitShutdownReturnsNilOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	workerReceived, got := awaitShutdown(ctx, make(chan error, 1), make(chan error, 1))
	if got != nil {
		t.Fatalf("awaitShutdown returned %v, want nil", got)
	}
	if workerReceived {
		t.Fatal("awaitShutdown reported workerReceived=true for a context cancellation")
	}
}

// TestConfigConfirmedGoneGraceFallback asserts a zero field resolves to
// the package default. main applies the environment default, but every
// other Config construction would otherwise run a zero grace on the
// placement-liveness and tenant-sync seams, which counts an instance
// gone the moment the operator clears it.
func TestConfigConfirmedGoneGraceFallback(t *testing.T) {
	cases := []struct {
		name  string
		field time.Duration
		want  time.Duration
	}{
		{name: "zero takes the default", field: 0, want: recordstore.DefaultConfirmedGoneGrace},
		{name: "negative takes the default", field: -5 * time.Second, want: recordstore.DefaultConfirmedGoneGrace},
		{name: "a set value is kept", field: 90 * time.Second, want: 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{ConfirmedGoneGrace: tc.field}
			if got := cfg.confirmedGoneGrace(); got != tc.want {
				t.Fatalf("confirmedGoneGrace() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWireGrantHoldWiresTheLedger asserts run's wiring helper turns the
// ledger's grant hold on. A ledger without the hold grants a repeat claim
// from another instance while the stored grant can still be building.
func TestWireGrantHoldWiresTheLedger(t *testing.T) {
	ledger := recordstore.NewLedger(nil, nil)
	if ledger.GrantHoldWired() {
		t.Fatal("a new ledger holds the grant hold before the wiring")
	}
	wireGrantHold(ledger, nil, Config{})
	if !ledger.GrantHoldWired() {
		t.Fatal("wireGrantHold did not wire the grant hold")
	}
}
