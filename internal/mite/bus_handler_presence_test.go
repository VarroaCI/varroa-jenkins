package mite

import (
	"context"
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

// TestPresenceVersionIsTheMiteVersion pins the one meaning of the presence
// Version field: the mite version. Every writer carries it: the connect,
// a snapshot, the timer, a watch change, and a heartbeat. A heartbeat with a
// new version replaces it. The Jenkins version of a snapshot never enters
// the record.
func TestPresenceVersionIsTheMiteVersion(t *testing.T) {
	h, kv := presenceTestHandler(t)
	token := h.OnConnect("ctl", "ns", "mite-1.4.0", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", token) })

	version := func() string {
		t.Helper()
		p, _, found := readPresenceRecord(t, kv, "ctl")
		if !found {
			t.Fatal("no presence record")
		}
		return p.Version
	}
	steps := []struct {
		name  string
		write func()
		want  string
	}{
		{"connect", func() {}, "mite-1.4.0"},
		{"snapshot", func() {
			h.OnSnapshot("ctl", "ns", &mitev1.StateSnapshot{JenkinsVersion: "2.504.3", JenkinsHealth: "healthy"})
		}, "mite-1.4.0"},
		{"timer", func() {
			if _, ok := h.refreshPresence("ctl", "ns", token, 0, time.Hour, recvLoop); !ok {
				t.Fatal("refreshPresence answered not current for the live token")
			}
		}, "mite-1.4.0"},
		{"watch change", func() {
			h.markWatchDegraded(context.Background(), "ns", "ctl", "desired", "desired watch setup failing")
		}, "mite-1.4.0"},
		{"heartbeat with a new version", func() {
			h.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "mite-1.4.1"})
		}, "mite-1.4.1"},
		{"heartbeat with no version", func() {
			h.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{})
		}, "mite-1.4.1"},
		{"snapshot after the new version", func() {
			h.OnSnapshot("ctl", "ns", &mitev1.StateSnapshot{JenkinsVersion: "2.504.3"})
		}, "mite-1.4.1"},
	}
	for _, step := range steps {
		step.write()
		if got := version(); got != step.want {
			t.Errorf("%s: record Version = %q, want the mite version %q", step.name, got, step.want)
		}
	}
}
