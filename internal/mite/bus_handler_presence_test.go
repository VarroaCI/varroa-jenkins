package mite

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestBusHandler_HeartbeatKeepsPresenceFields asserts that a heartbeat with
// no installed plugins hash and no idle gauges keeps the values that an
// earlier heartbeat wrote into the presence record. The operator reads both
// values from that record.
func TestBusHandler_HeartbeatKeepsPresenceFields(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)
	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		t.Fatalf("presence kv: %v", err)
	}
	h := mustNewBusHandler(t, busConn, snapshotKV, presenceKV)
	t.Cleanup(h.Close)

	send := func(*mitev1.OperatorMessage) error { return nil }
	token := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), send)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", token) })

	h.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{
		Version:              "v1",
		InstalledPluginsHash: "v1:abc123",
		Idle:                 &mitev1.IdleGauges{RunningBuilds: 2},
	})
	h.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "v1"})

	data, err := presenceKV.Get(bus.PresenceKey(bus.DefaultCluster, "ns", "ctl"))
	if err != nil || data == nil {
		t.Fatalf("presence record: data=%v err=%v", data, err)
	}
	var p bus.Presence
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("decode presence: %v", err)
	}
	if p.InstalledPluginsHash != "v1:abc123" {
		t.Errorf("InstalledPluginsHash = %q, want v1:abc123", p.InstalledPluginsHash)
	}
	var gauges mitev1.IdleGauges
	if err := json.Unmarshal([]byte(p.IdleGaugesJSON), &gauges); err != nil {
		t.Fatalf("decode idle gauges %q: %v", p.IdleGaugesJSON, err)
	}
	if gauges.RunningBuilds != 2 {
		t.Errorf("RunningBuilds = %d, want 2", gauges.RunningBuilds)
	}
}
