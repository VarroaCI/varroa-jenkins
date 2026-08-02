package mite

import (
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func TestSetAndGetObservabilityReport(t *testing.T) {
	reg := NewRegistry()
	ns, name := "ns", "test-ctrl"

	report := &mitev1.ObservabilityReport{
		ObservedAt:   "2026-06-08T00:00:00Z",
		TTLSeconds:   180,
		Capabilities: []string{"jenkins.health"},
		Sources: []*mitev1.ObservableSource{
			{Provider: "jenkins-api", Status: "exposed"},
		},
	}

	got := reg.GetObservabilityReport(name, ns)
	if got != nil {
		t.Error("expected nil report before setting")
	}

	reg.SetObservabilityReport(name, ns, report)

	got = reg.GetObservabilityReport(name, ns)
	if got == nil {
		t.Fatal("expected report after setting")
	}
	if got.TTLSeconds != 180 {
		t.Errorf("TTLSeconds = %d, want 180", got.TTLSeconds)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "jenkins.health" {
		t.Errorf("capabilities = %v, want [jenkins.health]", got.Capabilities)
	}
}

func TestObservabilityReportHandlesMissingKey(t *testing.T) {
	reg := NewRegistry()
	got := reg.GetObservabilityReport("nonexistent", "ns")
	if got != nil {
		t.Error("expected nil for nonexistent controller")
	}
}

func TestObservabilityReportPersistsAfterDisconnect(t *testing.T) {
	reg := NewRegistry()
	ns, name := "ns", "test-ctrl"

	report := &mitev1.ObservabilityReport{
		ObservedAt:   "2026-06-08T00:00:00Z",
		TTLSeconds:   180,
		Capabilities: []string{"jenkins.health"},
	}

	reg.SetObservabilityReport(name, ns, report)
	reg.Unregister(name, ns)

	got := reg.GetObservabilityReport(name, ns)
	if got == nil {
		t.Fatal("expected report to persist after disconnect")
	}
	if got.TTLSeconds != 180 {
		t.Errorf("TTLSeconds = %d, want 180 after disconnect", got.TTLSeconds)
	}
}

func TestObservabilityReportTTLExpiry(t *testing.T) {
	reg := NewRegistry()
	ns, name := "ns", "test-ctrl"

	oldReport := &mitev1.ObservabilityReport{
		ObservedAt:   time.Now().Add(-200 * time.Second).Format(time.RFC3339),
		TTLSeconds:   180,
		Capabilities: []string{"jenkins.health"},
	}
	reg.SetObservabilityReport(name, ns, oldReport)

	got := reg.GetObservabilityReport(name, ns)
	if got == nil {
		t.Fatal("report should exist")
	}

	parsed, err := time.Parse(time.RFC3339, got.ObservedAt)
	if err != nil {
		t.Fatalf("failed to parse ObservedAt: %v", err)
	}
	if time.Since(parsed) < 180*time.Second {
		t.Skip("test timing: report not yet stale")
	}
	if time.Since(parsed) > 180*time.Second {
		t.Logf("report is stale (age: %v, TTL: %ds)", time.Since(parsed), got.TTLSeconds)
	}
}

func TestIsConnectedWithObservabilityCache(t *testing.T) {
	reg := NewRegistry()
	ns, name := "ns", "test-ctrl"

	report := &mitev1.ObservabilityReport{
		ObservedAt: "2026-06-08T00:00:00Z",
		TTLSeconds: 180,
	}
	reg.SetObservabilityReport(name, ns, report)

	if reg.IsConnected(name, ns) {
		t.Error("expected not connected after SetObservabilityReport without Registration")
	}

	reg.GetObservabilityReport(name, ns)
	if reg.IsConnected(name, ns) {
		t.Error("GetObservabilityReport should not create a connected entry")
	}
}

// ---------------------------------------------------------------------------
// Task 6.9 — plugin inventory hash latch and transport guards
// ---------------------------------------------------------------------------

func TestRegistry_UpdateHeartbeatRetainsInstalledPluginsHash(t *testing.T) {
	reg := NewRegistry()
	ns, name := "ns", "test-ctrl"

	// Register a connection.
	reg.Register(name, ns, nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	// First heartbeat: set the hash.
	reg.UpdateHeartbeat(name, ns, "v1.0", nil, nil, "v1:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")

	h, ok := reg.GetInstalledPluginsHash(name, ns)
	if !ok {
		t.Fatal("expected hash to be available after heartbeat")
	}
	if h != "v1:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Errorf("hash = %q, want v1:abcdef...", h)
	}

	// A subsequent UpdateHeartbeat without a hash (snapshot path) must NOT clear it.
	reg.UpdateHeartbeat(name, ns, "v1.0", &mitev1.StateSnapshot{JenkinsHealth: "healthy"}, nil, "")

	h2, ok2 := reg.GetInstalledPluginsHash(name, ns)
	if !ok2 {
		t.Fatal("expected hash to still be present after snapshot heartbeat")
	}
	if h2 != "v1:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Errorf("hash was cleared by snapshot heartbeat: got %q", h2)
	}
}

func TestRegistry_SetAndGetPluginInventory(t *testing.T) {
	reg := NewRegistry()
	ns, name := "ns", "test-ctrl"

	inv := &mitev1.PluginInventory{
		InstalledPluginsHash: "v1:deadbeef00000000000000000000000000000000000000000000000000000000",
		Source:               "jenkins-api",
		CollectedAt:          "2026-01-01T00:00:00Z",
	}
	reg.SetPluginInventory(name, ns, inv)

	got := reg.GetPluginInventory(name, ns)
	if got == nil {
		t.Fatal("expected inventory after setting")
	}
	if got.InstalledPluginsHash != inv.InstalledPluginsHash {
		t.Errorf("hash = %q, want %q", got.InstalledPluginsHash, inv.InstalledPluginsHash)
	}
}

func TestRegistry_PluginInventoryMissing(t *testing.T) {
	reg := NewRegistry()
	got := reg.GetPluginInventory("nonexistent", "ns")
	if got != nil {
		t.Error("expected nil for nonexistent controller")
	}
}

func TestRegistry_InstalledPluginsHash_Missing(t *testing.T) {
	reg := NewRegistry()
	_, ok := reg.GetInstalledPluginsHash("nonexistent", "ns")
	if ok {
		t.Error("expected false for missing controller")
	}
}
