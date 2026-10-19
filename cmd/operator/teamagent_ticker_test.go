package main

import (
	"os"
	"strings"
	"testing"
)

// TestMain_TeamAgentTickerRegisteredLeaderElected guards the Team agent
// binding loop. The operator must construct the reconciler, drive it from a
// tickerRunnable named teamagent, and that block must not set everyReplica,
// so one leader writes the bindings. The block must sit behind --team-agents.
func TestMain_TeamAgentTickerRegisteredLeaderElected(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, "teamagents.Reconciler{") {
		t.Fatal("main.go does not construct a teamagents.Reconciler")
	}
	if !strings.Contains(text, "teamAgentReconciler.Tick(ctx)") {
		t.Fatal("main.go constructs a teamagents.Reconciler but never drives it from a registered runnable")
	}
	if !strings.Contains(text, `flag.Bool("team-agents"`) {
		t.Fatal("main.go does not define the --team-agents flag")
	}

	const marker = `name:      "teamagent",`
	start := strings.Index(text, marker)
	if start == -1 {
		t.Fatal(`main.go does not register a tickerRunnable named "teamagent"`)
	}
	end := strings.Index(text[start:], "}); err != nil {")
	if end == -1 {
		t.Fatal("could not find the end of the teamagent tickerRunnable block")
	}
	block := text[start : start+end]
	if strings.Contains(block, "everyReplica") {
		t.Fatalf("teamagent tickerRunnable block sets everyReplica, which would make NeedLeaderElection() false:\n%s", block)
	}
	gate := strings.LastIndex(text[:start], "if *teamAgents {")
	if gate == -1 || start-gate > 1200 {
		t.Fatal("the teamagent tickerRunnable is not gated by the --team-agents flag")
	}
}
