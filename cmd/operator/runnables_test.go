package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
