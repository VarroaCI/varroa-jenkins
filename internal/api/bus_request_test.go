package api

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestRequestWithFallback_PrefersContextSeam(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "marker")
	withCtx := func(got context.Context, _ string, _ []byte, _ time.Duration) ([]byte, error) {
		if got != ctx {
			t.Errorf("withCtx ctx = %v, want the caller's ctx", got)
		}
		return []byte("ctx"), nil
	}
	plain := func(string, []byte, time.Duration) ([]byte, error) {
		t.Error("plain seam called although withCtx is set")
		return nil, nil
	}
	data, err := requestWithFallback(ctx, withCtx, plain, "subj", nil, time.Second)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(data) != "ctx" {
		t.Fatalf("data = %q, want ctx", data)
	}
}

func TestRequestWithFallback_PlainSeamReturnsOnCancel(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	plain := func(string, []byte, time.Duration) ([]byte, error) {
		close(started)
		<-release
		return []byte("late"), errors.New("released")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := requestWithFallback(ctx, nil, plain, "subj", nil, time.Second)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("requestWithFallback did not return after cancel")
	}
}

func TestBusSeamConstructorsWireContextRequest(t *testing.T) {
	logger := slog.Default()

	cfg, ok := NewBusConfigBrood("core", nil, nil, nil, logger).(*busConfigBrood)
	if !ok || cfg.requestWithCtx == nil {
		t.Fatal("NewBusConfigBrood did not wire requestWithCtx")
	}

	ops, ok := NewBusBroodOps("core", nil, nil, nil, nil, logger).(*busBroodOps)
	if !ok || ops.requestWithCtx == nil {
		t.Fatal("NewBusBroodOps did not wire requestWithCtx")
	}

	sched, ok := NewBusBroodSchedules("core", nil, nil, logger).(*busBroodSchedules)
	if !ok || sched.requestWithCtx == nil {
		t.Fatal("NewBusBroodSchedules did not wire requestWithCtx")
	}
}
