package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain_CellPoolReconcilerRegisteredLeaderElected guards "one
// leader-elected ticker owns every CellPool": the pool reconciler is the only
// creator of a cell and the only writer of CellPool.status, so its runnable
// must not set everyReplica. tickerRunnable.NeedLeaderElection() returns true
// only while everyReplica stays unset.
func TestMain_CellPoolReconcilerRegisteredLeaderElected(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, "cells.WarmPoolReconciler{") {
		t.Fatal("main.go does not construct a cells.WarmPoolReconciler")
	}

	const marker = `name:     "warm-pool",`
	start := strings.Index(text, marker)
	if start == -1 {
		t.Fatal(`main.go does not register a tickerRunnable named "warm-pool"`)
	}
	end := strings.Index(text[start:], "}); err != nil {")
	if end == -1 {
		t.Fatal("could not find the end of the warm-pool tickerRunnable block")
	}
	block := text[start : start+end]
	if strings.Contains(block, "everyReplica") {
		t.Fatalf("the warm-pool block sets everyReplica, which would make NeedLeaderElection() false:\n%s", block)
	}
	if !strings.Contains(block, "wake:") {
		t.Fatalf("the warm-pool block declares no wake channel, so a scale edit waits for the backstop:\n%s", block)
	}
}

// TestMain_WarmPoolReconcilerCarriesGateWiring guards task 2.9: the
// cells.WarmPoolReconciler literal must wire the fill-gate fields. The
// bound is the literal itself, up to the next mgr.Add(, because the runner
// reconciler literal later in the file also carries a Secrets: field, and a
// whole-file search would pass on that literal instead of this one.
func TestMain_WarmPoolReconcilerCarriesGateWiring(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	const startMarker = "cells.WarmPoolReconciler{"
	start := strings.Index(text, startMarker)
	if start == -1 {
		t.Fatal("main.go does not construct a cells.WarmPoolReconciler")
	}
	end := strings.Index(text[start:], "mgr.Add(")
	if end == -1 {
		t.Fatal("could not find the mgr.Add( that follows the cells.WarmPoolReconciler literal")
	}
	block := text[start : start+end]
	for _, field := range []string{"ClaimGate:", "BlobTierS3:", "ArtifactSecretBase:", "Secrets:", "Plugins:"} {
		if !strings.Contains(block, field) {
			t.Errorf("the cells.WarmPoolReconciler literal is missing %s:\n%s", field, block)
		}
	}
}

// TestTickerRunnable_WakeChannelRunsATick asserts the wake case: a token on
// the channel runs one tick without waiting for the interval.
func TestTickerRunnable_WakeChannelRunsATick(t *testing.T) {
	wake := make(chan struct{}, 1)
	ticked := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &tickerRunnable{
		name:     "test",
		interval: time.Hour,
		wake:     wake,
		tick:     func(context.Context) { ticked <- struct{}{} },
		logger:   testLogger(),
	}
	go func() { _ = r.Start(ctx) }()

	wake <- struct{}{}
	select {
	case <-ticked:
	case <-time.After(5 * time.Second):
		t.Fatal("a wake token did not run a tick")
	}
}

// TestNonBlockingWake asserts the send never blocks: a full channel drops the
// token, because the pending wake already covers it.
func TestNonBlockingWake(t *testing.T) {
	wake := make(chan struct{}, 1)
	for i := 0; i < 5; i++ {
		notifyWake(wake)
	}
	if len(wake) != 1 {
		t.Fatalf("wake channel holds %d tokens, want 1", len(wake))
	}
}
