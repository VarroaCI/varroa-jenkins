package main

import (
	"os"
	"strings"
	"testing"
)

// TestMain_RunnerReconcilerRegisteredLeaderElected guards design D2's "one
// leader-elected ticker owns every Runner": the operator must construct
// runners.Reconciler, drive it from a registered tickerRunnable, and that
// runnable must not set everyReplica — tickerRunnable.NeedLeaderElection()
// returns true only while everyReplica stays unset, and TestTickerRunnable_
// NeedLeaderElection above proves what that method itself returns for such
// a runnable.
func TestMain_RunnerReconcilerRegisteredLeaderElected(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, "runners.Reconciler{") {
		t.Fatal("main.go does not construct a runners.Reconciler")
	}
	if !strings.Contains(text, "runnerReconciler.Tick(ctx)") {
		t.Fatal("main.go constructs a runners.Reconciler but never drives it from a registered runnable")
	}

	const marker = `name:      "runner",`
	start := strings.Index(text, marker)
	if start == -1 {
		t.Fatal(`main.go does not register a tickerRunnable named "runner"`)
	}
	end := strings.Index(text[start:], "}); err != nil {")
	if end == -1 {
		t.Fatal("could not find the end of the runner tickerRunnable block")
	}
	block := text[start : start+end]
	if strings.Contains(block, "everyReplica") {
		t.Fatalf("runner tickerRunnable block sets everyReplica, which would make NeedLeaderElection() false:\n%s", block)
	}
}
