package transport

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/mite"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestLocalRegistryIdleGauges_NoGauges verifies that IdleGauges returns false
// when no heartbeat has delivered idle data.
func TestLocalRegistryIdleGauges_NoGauges(t *testing.T) {
	reg := mite.NewRegistry()
	lr := NewLocalRegistry(reg)

	// Register a mite but never send idle gauges.
	reg.Register("test-ctrl", "ns", nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	gauges, receivedAt, ok := lr.IdleGauges("ns", "test-ctrl")
	if ok {
		t.Error("expected ok=false when no idle gauges have been received")
	}
	if gauges != nil {
		t.Errorf("expected nil gauges, got %+v", gauges)
	}
	if !receivedAt.IsZero() {
		t.Errorf("expected zero receivedAt, got %v", receivedAt)
	}
}

// TestLocalRegistryIdleGauges_WithGauges verifies that IdleGauges returns the
// gauges stored by UpdateHeartbeat.
func TestLocalRegistryIdleGauges_WithGauges(t *testing.T) {
	reg := mite.NewRegistry()
	lr := NewLocalRegistry(reg)

	reg.Register("test-ctrl", "ns", nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	expected := &mitev1.IdleGauges{
		LastHttpActivityUnix: 1719000000,
		LastEventUnix:        1719000100,
		RunningBuilds:        2,
		QueueLength:          5,
		TimerTriggerJobs:     1,
	}
	reg.UpdateHeartbeat("test-ctrl", "ns", "v1.0", nil, expected, "")

	gauges, receivedAt, ok := lr.IdleGauges("ns", "test-ctrl")
	if !ok {
		t.Fatal("expected ok=true after UpdateHeartbeat with gauges")
	}
	if gauges == nil {
		t.Fatal("expected non-nil gauges")
	}
	if gauges.LastHttpActivityUnix != expected.LastHttpActivityUnix {
		t.Errorf("LastHttpActivityUnix = %d, want %d", gauges.LastHttpActivityUnix, expected.LastHttpActivityUnix)
	}
	if gauges.LastEventUnix != expected.LastEventUnix {
		t.Errorf("LastEventUnix = %d, want %d", gauges.LastEventUnix, expected.LastEventUnix)
	}
	if gauges.RunningBuilds != expected.RunningBuilds {
		t.Errorf("RunningBuilds = %d, want %d", gauges.RunningBuilds, expected.RunningBuilds)
	}
	if gauges.QueueLength != expected.QueueLength {
		t.Errorf("QueueLength = %d, want %d", gauges.QueueLength, expected.QueueLength)
	}
	if gauges.TimerTriggerJobs != expected.TimerTriggerJobs {
		t.Errorf("TimerTriggerJobs = %d, want %d", gauges.TimerTriggerJobs, expected.TimerTriggerJobs)
	}
	if receivedAt.IsZero() {
		t.Error("expected non-zero receivedAt")
	}
}

// TestLocalRegistryIdleGauges_UpdateOverwrites verifies that a subsequent
// UpdateHeartbeat overwrites the previous gauges.
func TestLocalRegistryIdleGauges_UpdateOverwrites(t *testing.T) {
	reg := mite.NewRegistry()
	lr := NewLocalRegistry(reg)

	reg.Register("test-ctrl", "ns", nil, nil, "v1.0", time.Now().Add(24*time.Hour))

	reg.UpdateHeartbeat("test-ctrl", "ns", "v1.0", nil, &mitev1.IdleGauges{
		RunningBuilds: 1,
	}, "")
	reg.UpdateHeartbeat("test-ctrl", "ns", "v1.0", nil, &mitev1.IdleGauges{
		RunningBuilds: 0,
		QueueLength:   0,
	}, "")

	gauges, _, ok := lr.IdleGauges("ns", "test-ctrl")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if gauges.RunningBuilds != 0 {
		t.Errorf("expected RunningBuilds=0 (overwritten), got %d", gauges.RunningBuilds)
	}
}

// TestLocalRegistryIdleGauges_NotConnected verifies that IdleGauges returns
// false when no mite is registered at all.
func TestLocalRegistryIdleGauges_NotConnected(t *testing.T) {
	reg := mite.NewRegistry()
	lr := NewLocalRegistry(reg)

	// No mite registered.
	gauges, _, ok := lr.IdleGauges("ns", "nonexistent")
	if ok {
		t.Error("expected ok=false when no mite is connected")
	}
	if gauges != nil {
		t.Errorf("expected nil gauges, got %+v", gauges)
	}
}

// TestLocalRegistry_PluginInventoryRoundTrip verifies that Set/GetPluginInventory
// round-trips through the LocalRegistry transport.
func TestLocalRegistry_PluginInventoryRoundTrip(t *testing.T) {
	reg := mite.NewRegistry()
	lr := NewLocalRegistry(reg)

	inv := &mitev1.PluginInventory{
		InstalledPluginsHash: "v1:deadbeef00000000000000000000000000000000000000000000000000000000",
		Source:               "jenkins-api",
		CollectedAt:          "2026-01-01T00:00:00Z",
		Plugins: []*mitev1.InstalledPlugin{
			{Name: "test-plugin", Version: "1.0"},
		},
	}
	lr.reg.SetPluginInventory("test-ctrl", "ns", inv)

	got := lr.PluginInventory("ns", "test-ctrl")
	if got == nil {
		t.Fatal("expected inventory")
	}
	if got.InstalledPluginsHash != inv.InstalledPluginsHash {
		t.Errorf("hash = %q, want %q", got.InstalledPluginsHash, inv.InstalledPluginsHash)
	}
}

// TestLocalRegistry_InstalledPluginsHash verifies the installed plugins hash
// is available through the transport.
func TestLocalRegistry_InstalledPluginsHash(t *testing.T) {
	reg := mite.NewRegistry()
	lr := NewLocalRegistry(reg)

	reg.Register("test-ctrl", "ns", nil, nil, "v1.0", time.Now().Add(24*time.Hour))
	reg.UpdateHeartbeat("test-ctrl", "ns", "v1.0", nil, nil, "v1:abc123")

	h, ok := lr.InstalledPluginsHash("ns", "test-ctrl")
	if !ok {
		t.Fatal("expected hash to be available")
	}
	if h != "v1:abc123" {
		t.Errorf("hash = %q, want v1:abc123", h)
	}

	_, ok = lr.InstalledPluginsHash("ns", "nonexistent")
	if ok {
		t.Error("expected false for missing controller")
	}
}

// TestLocalRegistry_PluginClassificationRoundTrip verifies classification storage.
func TestLocalRegistry_PluginClassificationRoundTrip(t *testing.T) {
	reg := mite.NewRegistry()
	lr := NewLocalRegistry(reg)

	c := &ClassifiedInventory{
		Envelope: ClassifiedEnvelope{
			Hash:  "v1:test",
			Total: 5,
			Counts: map[string]int{
				"declared":   3,
				"bootstrap":  1,
				"dependency": 1,
			},
		},
		Plugins: []ClassifiedPlugin{
			{Name: "p1", Version: "1.0", Class: "declared"},
		},
	}

	// Before put, should not be available.
	_, ok := lr.PluginClassification("ns", "test-ctrl")
	if ok {
		t.Error("expected false before put")
	}

	err := lr.PutPluginClassification(context.TODO(), "ns", "test-ctrl", c)
	if err != nil {
		t.Fatalf("PutPluginClassification: %v", err)
	}

	got, ok := lr.PluginClassification("ns", "test-ctrl")
	if !ok {
		t.Fatal("expected classification after put")
	}
	if got.Envelope.Hash != "v1:test" {
		t.Errorf("hash = %q, want v1:test", got.Envelope.Hash)
	}
	if got.Envelope.Total != 5 {
		t.Errorf("total = %d, want 5", got.Envelope.Total)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].Name != "p1" {
		t.Errorf("plugins = %+v", got.Plugins)
	}
}

// TestBusReadModelTransport_PutPluginClassificationRejected verifies that the
// read-only BFF transport rejects PutPluginClassification with the same error as Send.
func TestBusReadModelTransport_PutPluginClassificationRejected(t *testing.T) {
	model := NewBusReadModel("test", nil, nil, nil)
	tr := NewBusReadModelTransport(model)

	err := tr.PutPluginClassification(context.TODO(), "", "", nil)
	if err == nil {
		t.Fatal("expected error from PutPluginClassification on read-only transport")
	}
	sendErr := tr.Send(context.TODO(), "", "", nil)
	if sendErr == nil {
		t.Fatal("expected error from Send on read-only transport")
	}
	// Both should be rejected — the exact error text is not prescribed, but
	// both must be non-nil.
}
