package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// putTestPresence writes p as the presence record of ns/name.
func putTestPresence(t *testing.T, tr *BusTransport, name string, p bus.Presence) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal presence: %v", err)
	}
	if err := tr.presenceKV.Put(bus.PresenceKey(bus.DefaultCluster, "ns", name), data); err != nil {
		t.Fatalf("put presence: %v", err)
	}
}

// TestBusTransport_IdleGauges asserts that IdleGauges answers the gauges and
// their receive time from the presence record. A missing record and a record
// with no gauges answer false.
func TestBusTransport_IdleGauges(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	if _, _, ok := tr.IdleGauges("ns", "absent"); ok {
		t.Error("missing record: want ok=false")
	}
	putTestPresence(t, tr, "bare", bus.Presence{Version: "v1"})
	if _, _, ok := tr.IdleGauges("ns", "bare"); ok {
		t.Error("record with no gauges: want ok=false")
	}
	gauges, err := json.Marshal(&mitev1.IdleGauges{RunningBuilds: 2, QueueLength: 5, TimerTriggerJobs: 1})
	if err != nil {
		t.Fatalf("marshal gauges: %v", err)
	}
	at := time.Now().Truncate(time.Second)
	putTestPresence(t, tr, "c1", bus.Presence{Version: "v1", IdleGaugesJSON: string(gauges), IdleGaugesReceivedAt: at})
	got, receivedAt, ok := tr.IdleGauges("ns", "c1")
	if !ok {
		t.Fatal("record with gauges: want ok=true")
	}
	if got.RunningBuilds != 2 || got.QueueLength != 5 || got.TimerTriggerJobs != 1 {
		t.Errorf("gauges = running %d, queue %d, timers %d; want 2, 5, 1", got.RunningBuilds, got.QueueLength, got.TimerTriggerJobs)
	}
	if !receivedAt.Equal(at) {
		t.Errorf("receivedAt = %v, want %v", receivedAt, at)
	}
}

// TestBusTransport_InstalledPluginsHash asserts that the hash comes from the
// presence record, and that a missing record or an empty hash answers false.
func TestBusTransport_InstalledPluginsHash(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	if _, ok := tr.InstalledPluginsHash("ns", "absent"); ok {
		t.Error("missing record: want ok=false")
	}
	putTestPresence(t, tr, "bare", bus.Presence{Version: "v1"})
	if _, ok := tr.InstalledPluginsHash("ns", "bare"); ok {
		t.Error("record with no hash: want ok=false")
	}
	putTestPresence(t, tr, "c1", bus.Presence{Version: "v1", InstalledPluginsHash: "v1:abc123"})
	if h, ok := tr.InstalledPluginsHash("ns", "c1"); !ok || h != "v1:abc123" {
		t.Errorf("InstalledPluginsHash = %q, %v; want v1:abc123, true", h, ok)
	}
}

// TestBusTransport_PluginInventory asserts that the inventory comes from the
// snapshot bucket, and that a missing entry answers nil.
func TestBusTransport_PluginInventory(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	if inv := tr.PluginInventory("ns", "absent"); inv != nil {
		t.Errorf("missing inventory = %v, want nil", inv)
	}
	want := &mitev1.PluginInventory{
		InstalledPluginsHash: "v1:deadbeef",
		Source:               "jenkins-api",
		Plugins:              []*mitev1.InstalledPlugin{{Name: "test-plugin", Version: "1.0"}},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	if err := tr.snapshotKV.Put(bus.PluginInventoryKey(bus.DefaultCluster, "ns", "c1"), data); err != nil {
		t.Fatalf("put inventory: %v", err)
	}
	got := tr.PluginInventory("ns", "c1")
	if got == nil || got.InstalledPluginsHash != want.InstalledPluginsHash || len(got.Plugins) != 1 || got.Plugins[0].Name != "test-plugin" {
		t.Errorf("PluginInventory = %v, want hash %q and plugin test-plugin", got, want.InstalledPluginsHash)
	}
}

// TestBusTransport_PluginClassificationRoundTrip asserts that a stored
// classification reads back unchanged.
func TestBusTransport_PluginClassificationRoundTrip(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	if _, ok := tr.PluginClassification("ns", "c1"); ok {
		t.Error("before the put: want ok=false")
	}
	c := &ClassifiedInventory{
		Envelope: ClassifiedEnvelope{Hash: "v1:test", Total: 5, Counts: map[string]int{"declared": 3, "bootstrap": 1, "dependency": 1}},
		Plugins:  []ClassifiedPlugin{{Name: "p1", Version: "1.0", Class: "declared"}},
	}
	if err := tr.PutPluginClassification(context.Background(), "ns", "c1", c); err != nil {
		t.Fatalf("PutPluginClassification: %v", err)
	}
	got, ok := tr.PluginClassification("ns", "c1")
	if !ok {
		t.Fatal("after the put: want ok=true")
	}
	if got.Envelope.Hash != "v1:test" || got.Envelope.Total != 5 || got.Envelope.Counts["declared"] != 3 {
		t.Errorf("envelope = %+v", got.Envelope)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].Name != "p1" || got.Plugins[0].Class != "declared" {
		t.Errorf("plugins = %+v", got.Plugins)
	}
}
