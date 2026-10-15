package transport

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestBusReadModelTransport_SendImperativeToCluster asserts a command for a
// member cluster's controller lands on that cluster's mite-out subject, not
// on the read model's own cluster.
func TestBusReadModelTransport_SendImperativeToCluster(t *testing.T) {
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

	tr := NewBusReadModelTransport(NewBusReadModel(bus.DefaultCluster, busConn, snapshotKV, presenceKV))

	ns, name := "ns", "cell-0"
	sub, err := busConn.JetStream().SubscribeSync(bus.MiteOutSubject("member", ns, name))
	if err != nil {
		t.Fatalf("subscribe member mite-out: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	if err := tr.SendImperativeToCluster(context.Background(), "member", ns, name, &mitev1.ImperativeCommand{
		CommandId: "trigger-member-1",
		Type:      mitev1.CommandTypeTriggerBuild,
	}); err != nil {
		t.Fatalf("SendImperativeToCluster: %v", err)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no command on the member cluster's mite-out subject: %v", err)
	}
	if !strings.Contains(string(msg.Data), "trigger-member-1") {
		t.Errorf("message = %s, want the command id", msg.Data)
	}
	_ = msg.Ack()
}
