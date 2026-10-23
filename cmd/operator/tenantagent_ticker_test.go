package main

import (
	"os"
	"strings"
	"testing"
)

// TestMain_TenantAgentTickerRegisteredLeaderElected guards the Tenant agent
// binding loop. The operator must construct the reconciler, drive it from a
// tickerRunnable named tenantagent, and that block must not set everyReplica,
// so one leader writes the bindings. The block must sit behind --tenant-agents.
func TestMain_TenantAgentTickerRegisteredLeaderElected(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, "tenantagents.Reconciler{") {
		t.Fatal("main.go does not construct a tenantagents.Reconciler")
	}
	if !strings.Contains(text, "tenantAgentReconciler.Tick(ctx)") {
		t.Fatal("main.go constructs a tenantagents.Reconciler but never drives it from a registered runnable")
	}
	if !strings.Contains(text, `flag.Bool("tenant-agents"`) {
		t.Fatal("main.go does not define the --tenant-agents flag")
	}

	const marker = `name:      "tenantagent",`
	start := strings.Index(text, marker)
	if start == -1 {
		t.Fatal(`main.go does not register a tickerRunnable named "tenantagent"`)
	}
	end := strings.Index(text[start:], "}); err != nil {")
	if end == -1 {
		t.Fatal("could not find the end of the tenantagent tickerRunnable block")
	}
	block := text[start : start+end]
	if strings.Contains(block, "everyReplica") {
		t.Fatalf("tenantagent tickerRunnable block sets everyReplica, which would make NeedLeaderElection() false:\n%s", block)
	}
	gate := strings.LastIndex(text[:start], "if *tenantAgents {")
	if gate == -1 || start-gate > 1200 {
		t.Fatal("the tenantagent tickerRunnable is not gated by the --tenant-agents flag")
	}
}
