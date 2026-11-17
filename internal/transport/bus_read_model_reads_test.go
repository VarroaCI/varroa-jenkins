package transport

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// newBusReadModel returns a read model for the default cluster over new
// snapshot and presence buckets on s.
func newBusReadModel(t *testing.T, s *server.Server) *BusReadModel {
	t.Helper()
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)
	snap, err := conn.EnsureKV(bus.KVSnapshotBucket, 5*time.Minute)
	if err != nil {
		t.Fatalf("snapshot kv: %v", err)
	}
	pres, err := conn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		t.Fatalf("presence kv: %v", err)
	}
	return NewBusReadModel(bus.DefaultCluster, conn, snap, pres)
}

// presenceJSON returns p as the JSON that the gateway writes.
func presenceJSON(t *testing.T, p bus.Presence) []byte {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal presence: %v", err)
	}
	return data
}

// putReadModelPresence writes raw as the presence record of ns/name on
// cluster.
func putReadModelPresence(t *testing.T, m *BusReadModel, cluster, name string, raw []byte) {
	t.Helper()
	if err := m.presenceKV.Put(bus.PresenceKey(cluster, "ns", name), raw); err != nil {
		t.Fatalf("put presence: %v", err)
	}
}

// TestBusReadModel_IdleGauges asserts that the read model answers the gauges
// and their receive time from the presence record of its own cluster. A
// missing record, a record with no gauges, and a record that does not decode
// answer nil, a zero time, and false.
func TestBusReadModel_IdleGauges(t *testing.T) {
	m := newBusReadModel(t, startNATS(t))
	gauges, err := json.Marshal(&mitev1.IdleGauges{RunningBuilds: 2, QueueLength: 5, TimerTriggerJobs: 1})
	if err != nil {
		t.Fatalf("marshal gauges: %v", err)
	}
	at := time.Now().Truncate(time.Second)
	full := bus.Presence{Version: "v1", IdleGaugesJSON: string(gauges), IdleGaugesReceivedAt: at}
	putReadModelPresence(t, m, bus.DefaultCluster, "c1", presenceJSON(t, full))
	putReadModelPresence(t, m, "member", "other-cluster", presenceJSON(t, full))
	putReadModelPresence(t, m, bus.DefaultCluster, "bare", presenceJSON(t, bus.Presence{Version: "v1"}))
	putReadModelPresence(t, m, bus.DefaultCluster, "bad-record", []byte("{"))
	putReadModelPresence(t, m, bus.DefaultCluster, "bad-gauges", presenceJSON(t, bus.Presence{Version: "v1", IdleGaugesJSON: "{", IdleGaugesReceivedAt: at}))

	for _, tc := range []struct{ desc, mite string }{
		{"missing record", "absent"},
		{"record of another cluster", "other-cluster"},
		{"record with no gauges", "bare"},
		{"record that does not decode", "bad-record"},
		{"gauges that do not decode", "bad-gauges"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got, receivedAt, ok := m.IdleGauges("ns", tc.mite)
			if ok || got != nil || !receivedAt.IsZero() {
				t.Errorf("IdleGauges = %v, %v, %v; want nil, zero time, false", got, receivedAt, ok)
			}
		})
	}

	t.Run("record with gauges", func(t *testing.T) {
		got, receivedAt, ok := m.IdleGauges("ns", "c1")
		if !ok || got == nil {
			t.Fatalf("IdleGauges ok = %v, gauges = %v; want true and gauges", ok, got)
		}
		if got.RunningBuilds != 2 || got.QueueLength != 5 || got.TimerTriggerJobs != 1 {
			t.Errorf("gauges = running %d, queue %d, timers %d; want 2, 5, 1", got.RunningBuilds, got.QueueLength, got.TimerTriggerJobs)
		}
		if !receivedAt.Equal(at) {
			t.Errorf("receivedAt = %v, want %v", receivedAt, at)
		}
	})

	t.Run("transport answers the read model", func(t *testing.T) {
		got, receivedAt, ok := NewBusReadModelTransport(m).IdleGauges("ns", "c1")
		if !ok || got == nil || got.RunningBuilds != 2 || !receivedAt.Equal(at) {
			t.Errorf("transport IdleGauges = %v, %v, %v; want running 2, %v, true", got, receivedAt, ok, at)
		}
	})
}

// TestBusReadModel_InstalledPluginsHash asserts that the read model answers
// the hash from the presence record of its own cluster. A missing record, an
// empty hash, and a record that does not decode answer an empty hash and
// false.
func TestBusReadModel_InstalledPluginsHash(t *testing.T) {
	m := newBusReadModel(t, startNATS(t))
	full := bus.Presence{Version: "v1", InstalledPluginsHash: "v1:abc123"}
	putReadModelPresence(t, m, bus.DefaultCluster, "c1", presenceJSON(t, full))
	putReadModelPresence(t, m, "member", "other-cluster", presenceJSON(t, full))
	putReadModelPresence(t, m, bus.DefaultCluster, "bare", presenceJSON(t, bus.Presence{Version: "v1"}))
	putReadModelPresence(t, m, bus.DefaultCluster, "bad-record", []byte("{"))

	for _, tc := range []struct{ desc, mite string }{
		{"missing record", "absent"},
		{"record of another cluster", "other-cluster"},
		{"record with an empty hash", "bare"},
		{"record that does not decode", "bad-record"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if h, ok := m.InstalledPluginsHash("ns", tc.mite); ok || h != "" {
				t.Errorf("InstalledPluginsHash = %q, %v; want empty, false", h, ok)
			}
		})
	}

	t.Run("record with a hash", func(t *testing.T) {
		if h, ok := m.InstalledPluginsHash("ns", "c1"); !ok || h != "v1:abc123" {
			t.Errorf("InstalledPluginsHash = %q, %v; want v1:abc123, true", h, ok)
		}
	})

	t.Run("transport answers the read model", func(t *testing.T) {
		if h, ok := NewBusReadModelTransport(m).InstalledPluginsHash("ns", "c1"); !ok || h != "v1:abc123" {
			t.Errorf("transport InstalledPluginsHash = %q, %v; want v1:abc123, true", h, ok)
		}
	})
}
