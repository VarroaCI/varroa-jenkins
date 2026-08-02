package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func startNATS(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

func newBusTransport(t *testing.T, s *server.Server) *BusTransport {
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
	des, err := conn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		t.Fatalf("desired kv: %v", err)
	}
	return NewBusTransport(bus.DefaultCluster, conn, snap, pres, des)
}

// Send is last-value: a second Send overwrites the first; only the latest
// desired state remains in KV.
func TestBusTransport_SendIsLastValue(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	ctx := context.Background()

	mk := func(hash string) *mitev1.OperatorMessage {
		return &mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_DesiredState{
			DesiredState: &mitev1.DesiredStateCommand{DesiredStateHash: hash},
		}}
	}
	if err := tr.Send(ctx, "ns", "c1", mk("v1")); err != nil {
		t.Fatalf("send v1: %v", err)
	}
	if err := tr.Send(ctx, "ns", "c1", mk("v2")); err != nil {
		t.Fatalf("send v2: %v", err)
	}

	data, err := tr.desiredKV.Get(bus.DesiredKey(bus.DefaultCluster, "ns", "c1"))
	if err != nil || data == nil {
		t.Fatalf("get desired: data=%v err=%v", data, err)
	}
	var op mitev1.OperatorMessage
	if err := json.Unmarshal(data, &op); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ds, ok := op.Message.(*mitev1.OperatorMessage_DesiredState)
	if !ok || ds.DesiredState == nil {
		t.Fatalf("expected DesiredState message, got %T", op.Message)
	}
	if got := ds.DesiredState.DesiredStateHash; got != "v2" {
		t.Fatalf("want last value v2, got %q", got)
	}

	// ClearDesired removes it.
	if err := tr.ClearDesired(ctx, "ns", "c1"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if data, _ := tr.desiredKV.Get(bus.DesiredKey(bus.DefaultCluster, "ns", "c1")); data != nil {
		t.Fatalf("expected nil after clear, got %v", data)
	}
}

// Connected/Info read the real presence record (not time.Now()).
func TestBusTransport_PresenceInfo(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))

	if tr.Connected("ns", "c1") {
		t.Fatal("should not be connected before presence write")
	}
	hb := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	ce := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	p, err := json.Marshal(bus.Presence{Version: "2.4", LastHeartbeat: hb, CertExpiry: ce})
	if err != nil {
		t.Fatalf("marshal presence: %v", err)
	}
	if err := tr.presenceKV.Put(bus.PresenceKey(bus.DefaultCluster, "ns", "c1"), p); err != nil {
		t.Fatalf("put presence: %v", err)
	}

	if !tr.Connected("ns", "c1") {
		t.Fatal("should be connected after presence write")
	}
	ver, lastHB, certExp, ok := tr.Info("ns", "c1")
	if !ok || ver != "2.4" || !lastHB.Equal(hb) || !certExp.Equal(ce) {
		t.Fatalf("Info mismatch: ver=%q hb=%v ce=%v ok=%v", ver, lastHB, certExp, ok)
	}
}

// TestBusTransport_TokenRefreshGrant verifies that in bus mode a mite's
// TokenRefreshRequest (published on the mite→operator subject) is answered
// with a freshly minted TokenGrant published on the operator→mite subject.
// Without this the dedicated grant path is silently dropped in bus mode.
func TestBusTransport_TokenRefreshGrant(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))

	// The operator→mite stream must exist for PublishJetStream to succeed.
	if err := tr.conn.EnsureStream(bus.StreamConfig("varroa")); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	called := make(chan string, 1)
	tr.TokenGrantFunc = func(ns, name string) (string, int64, error) {
		called <- ns + "/" + name
		return "minted-token", 4242, nil
	}

	ns, name := "ns", "ctrl"

	// Subscribe to the mite-out subject to capture the grant.
	sub, err := tr.conn.JetStream().SubscribeSync(bus.MiteOutSubject(bus.DefaultCluster, ns, name))
	if err != nil {
		t.Fatalf("subscribe mite-out: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Create the inbound subscription, then publish a TokenRefreshRequest.
	tr.DrainResults(ns, name)
	data, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_TokenRefreshRequest{TokenRefreshRequest: &mitev1.TokenRefreshRequest{}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := tr.conn.Publish(bus.MiteInSubject(bus.DefaultCluster, ns, name), data); err != nil {
		t.Fatalf("publish request: %v", err)
	}

	select {
	case got := <-called:
		if got != "ns/ctrl" {
			t.Errorf("TokenGrantFunc called with %q, want ns/ctrl", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TokenGrantFunc was not called for the TokenRefreshRequest")
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no TokenGrant published to mite-out: %v", err)
	}
	var om mitev1.OperatorMessage
	if err := json.Unmarshal(msg.Data, &om); err != nil {
		t.Fatalf("unmarshal grant: %v", err)
	}
	tg := om.GetTokenGrant()
	if tg == nil || tg.MiteJenkinsToken != "minted-token" || tg.MiteJenkinsTokenExp != 4242 {
		t.Errorf("unexpected token grant: %+v", tg)
	}
}

func TestBusReadModel_NoInboundSubscription(t *testing.T) {
	s := startNATS(t)
	busConn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("ensure snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		t.Fatalf("ensure presence kv: %v", err)
	}

	m := NewBusReadModel(bus.DefaultCluster, busConn, snapshotKV, presenceKV)

	ns, name := "ns", "ctrl"

	if m.Connected(ns, name) {
		t.Error("BusReadModel.Connected should not subscribe nor report connected without presence data")
	}
	_, _, _, ok := m.Info(ns, name)
	if ok {
		t.Error("BusReadModel.Info should not subscribe")
	}
	snap := m.Snapshot(ns, name)
	if snap != nil {
		t.Error("BusReadModel.Snapshot should not subscribe")
	}
	report := m.ObservabilityReport(ns, name)
	if report != nil {
		t.Error("BusReadModel.ObservabilityReport should not subscribe")
	}
	health := m.Health(ns, name)
	if health != "unreachable" {
		t.Errorf("BusReadModel.Health should be unreachable without data, got %q", health)
	}

	if err := m.PublishSafeRestart(context.Background(), ns, name, &mitev1.ImperativeCommand{
		CommandId: "read-model-test",
		Type:      mitev1.CommandTypeSafeRestart,
	}); err == nil {
		t.Error("BusReadModel.PublishSafeRestart on non-existent stream should have failed (no JetStream stream)")
	}
}

func TestBusReadModelTransport_PublishSafeRestart(t *testing.T) {
	s := startNATS(t)
	busConn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	if err := busConn.EnsureStream(bus.StreamConfig("varroa")); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("ensure snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		t.Fatalf("ensure presence kv: %v", err)
	}

	model := NewBusReadModel(bus.DefaultCluster, busConn, snapshotKV, presenceKV)
	tr := NewBusReadModelTransport(model)

	ns, name := "ns", "ctrl"
	cmdID := "safe-restart-test-1"
	err = tr.SendImperative(context.Background(), ns, name, &mitev1.ImperativeCommand{
		CommandId: cmdID,
		Type:      mitev1.CommandTypeSafeRestart,
	})
	if err != nil {
		t.Fatalf("BusReadModelTransport.SendImperative: %v", err)
	}

	sub, err := busConn.JetStream().SubscribeSync(bus.MiteOutSubject(bus.DefaultCluster, ns, name))
	if err != nil {
		t.Fatalf("subscribe mite-out: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no safe restart message published to mite-out: %v", err)
	}
	if msg == nil || len(msg.Data) == 0 {
		t.Fatal("empty safe restart message")
	}
	_ = msg.Ack()

	resultCh := tr.DrainResults(ns, name)
	if len(resultCh) != 0 {
		t.Error("BusReadModelTransport should not drain results")
	}

	if err := tr.Send(context.Background(), ns, name, nil); err == nil {
		t.Error("BusReadModelTransport.Send should return error")
	}

	if err := tr.ClearDesired(context.Background(), ns, name); err == nil {
		t.Error("BusReadModelTransport.ClearDesired should return error")
	}
}
