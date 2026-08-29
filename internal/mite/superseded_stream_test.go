package mite

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestGatewayIntegration_SupersededStreamStopsDispatching asserts that a
// stream the mite has already replaced must stop feeding the handler: the
// connection-token check must guard the read loop, not just teardown, or a
// late snapshot from a dead connection lands in the snapshot KV the live
// connection owns and the controller reports a stale Jenkins version with
// nothing marking it as stale.
func TestGatewayIntegration_SupersededStreamStopsDispatching(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	ns, name := "ns", "c1"
	addr := lis.Addr().String()

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 0)
	if err != nil {
		t.Fatalf("presence kv: %v", err)
	}

	// A deadline on the stream context so a regression fails the test instead
	// of blocking forever in Recv: without the gate the server keeps the
	// superseded stream open and never returns.
	streamCtx, cancelStreams := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStreams()

	openStream := func(version string) mitev1.Mite_CommandStreamClient {
		t.Helper()
		gconn := dialWithMiteCert(t, addr, certAuth, name, ns)
		stream, err := mitev1.NewMiteClient(gconn).CommandStream(streamCtx)
		if err != nil {
			t.Fatalf("command stream: %v", err)
		}
		if err := stream.Send(&mitev1.MiteMessage{
			Message: &mitev1.MiteMessage_Hello{
				Hello: &mitev1.Hello{ControllerName: name, Namespace: ns, Version: version},
			},
		}); err != nil {
			t.Fatalf("send hello: %v", err)
		}
		return stream
	}

	// Reading the epoch out of presence is what tells us OnConnect has run for
	// a given stream; a bare sleep would make the supersede ordering a guess.
	epochAfter := func(prev int64) int64 {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			data, err := presenceKV.Get(bus.PresenceKey(bus.DefaultCluster, ns, name))
			if err == nil && data != nil {
				var p bus.Presence
				if json.Unmarshal(data, &p) == nil && p.Epoch > prev {
					return p.Epoch
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no presence record with epoch > %d", prev)
		return 0
	}

	old := openStream("old")
	firstEpoch := epochAfter(0)

	// The mite reconnects before the first stream tears down. From here the
	// first stream is superseded.
	_ = openStream("new")
	epochAfter(firstEpoch)

	// gRPC's contract: SendMsg reports a terminated stream as io.EOF and the
	// real status comes from RecvMsg. Failing the test on that would make it
	// flaky whenever the server wins the race to close.
	if err := old.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_StateSnapshot{
			StateSnapshot: &mitev1.StateSnapshot{JenkinsVersion: "stale-from-dead-stream"},
		},
	}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("send snapshot on superseded stream: %v", err)
	}

	// Nothing from the superseded stream may reach state the live connection
	// owns. Poll rather than check once: dispatch is asynchronous, so a single
	// read taken immediately could pass even with the gate removed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := snapshotKV.Get(bus.SnapshotKey(bus.DefaultCluster, ns, name))
		if err == nil && data != nil {
			t.Fatalf("superseded stream's snapshot reached the KV: %s", data)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The gateway ends the superseded stream rather than dispatching its
	// message, which also releases the transport instead of leaving it to
	// linger until keepalive notices.
	if _, err := old.Recv(); status.Code(err) != codes.Aborted {
		t.Fatalf("superseded stream Recv error = %v, want code %v", err, codes.Aborted)
	}
}

// TestBusHandler_IsCurrentConnection covers the token comparison the read-loop
// gate depends on.
func TestBusHandler_IsCurrentConnection(t *testing.T) {
	h := &BusHandler{
		cluster:              "core",
		cancels:              make(map[string]context.CancelFunc),
		certExpiry:           make(map[string]time.Time),
		connectEpoch:         make(map[string]int64),
		idleGauges:           make(map[string]string),
		idleGaugesAt:         make(map[string]time.Time),
		installedPluginsHash: make(map[string]string),
		pendingAcks:          make(map[string]map[string]*nats.Msg),
		replayCmds:           make(map[string]map[string]bool),
		pendingContent:       make(map[string]*nats.Msg),
		pendingContentNS:     make(map[string]string),
		pendingContentTime:   make(map[string]time.Time),
		watchDegraded:        make(map[string]map[string]string),
		miteVersion:          make(map[string]string),
		lastHeartbeat:        make(map[string]time.Time),
		presenceLocks:        make(map[string]*sync.Mutex),
	}
	send := func(*mitev1.OperatorMessage) error { return nil }

	// No connection yet: nothing has superseded the caller.
	if !h.IsCurrentConnection("ctl", "varroa", 1234) {
		t.Error("unclaimed key: want current")
	}

	oldToken := h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	if !h.IsCurrentConnection("ctl", "varroa", oldToken) {
		t.Error("only connection: want current")
	}

	newToken := h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	if h.IsCurrentConnection("ctl", "varroa", oldToken) {
		t.Error("superseded token: want not current")
	}
	if !h.IsCurrentConnection("ctl", "varroa", newToken) {
		t.Error("newest token: want current")
	}

	// A handler that issues no identity must not be gated off its own stream.
	if !h.IsCurrentConnection("ctl", "varroa", 0) {
		t.Error("zero token: want current")
	}

	// A different controller is unaffected by this key's supersede.
	if !h.IsCurrentConnection("other", "varroa", oldToken) {
		t.Error("unrelated controller: want current")
	}
}

// TestRegistry_IsCurrentEpoch covers the same gate for the in-process
// RegistryHandler path used by the BFF.
func TestRegistry_IsCurrentEpoch(t *testing.T) {
	reg := NewRegistry()

	if !reg.IsCurrentEpoch("ctl", "varroa", 99) {
		t.Error("unregistered controller: want current")
	}

	oldConn := reg.Register("ctl", "varroa", nil, nil, "v1", time.Time{})
	if !reg.IsCurrentEpoch("ctl", "varroa", oldConn.Epoch) {
		t.Error("only connection: want current")
	}

	newConn := reg.Register("ctl", "varroa", nil, nil, "v1", time.Time{})
	if oldConn.Epoch == newConn.Epoch {
		t.Fatalf("Register did not bump the epoch (%d)", newConn.Epoch)
	}
	if reg.IsCurrentEpoch("ctl", "varroa", oldConn.Epoch) {
		t.Error("superseded epoch: want not current")
	}
	if !reg.IsCurrentEpoch("ctl", "varroa", newConn.Epoch) {
		t.Error("newest epoch: want current")
	}
	if !reg.IsCurrentEpoch("ctl", "varroa", 0) {
		t.Error("zero epoch: want current")
	}
}
