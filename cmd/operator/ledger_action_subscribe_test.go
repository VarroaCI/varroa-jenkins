package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

func startLedgerActionNATS(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{Port: -1})
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

// ledgerActionSubscriber connects one operator of cluster to s and
// subscribes it through subscribeLedgerActions in the shared
// operator-workers queue group. The returned counter holds the number
// of messages that operator received.
func ledgerActionSubscriber(t *testing.T, s *server.Server, cluster string) *atomic.Int64 {
	t.Helper()
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect %s: %v", cluster, err)
	}
	t.Cleanup(conn.Close)
	var got atomic.Int64
	if _, err := subscribeLedgerActions(conn, cluster, "operator-workers", func(*nats.Msg) {
		got.Add(1)
	}); err != nil {
		t.Fatalf("subscribe %s: %v", cluster, err)
	}
	if err := conn.NATSConn().Flush(); err != nil {
		t.Fatalf("flush %s: %v", cluster, err)
	}
	return &got
}

// waitLedgerActionCount waits until got reaches want or the deadline
// passes, then answers the final count.
func waitLedgerActionCount(got *atomic.Int64, want int64) int64 {
	deadline := time.Now().Add(2 * time.Second)
	for got.Load() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return got.Load()
}

// TestSubscribeLedgerActions_EachClusterReceivesOnlyItsOwn puts a core
// operator and a member-c operator on one NATS server in the same
// operator-workers queue group. Every action published on core's
// subject must reach core, and no action may reach member-c. The mirror
// case publishes on member-c's subject.
func TestSubscribeLedgerActions_EachClusterReceivesOnlyItsOwn(t *testing.T) {
	s := startLedgerActionNATS(t)
	coreGot := ledgerActionSubscriber(t, s, "core")
	memberGot := ledgerActionSubscriber(t, s, "member-c")

	pub, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect publisher: %v", err)
	}
	t.Cleanup(pub.Close)

	const coreActions = 8
	for i := 0; i < coreActions; i++ {
		if err := pub.Publish(bus.LedgerActionSubject("core"), []byte("{}")); err != nil {
			t.Fatalf("publish core: %v", err)
		}
	}
	if err := pub.NATSConn().Flush(); err != nil {
		t.Fatalf("flush publisher: %v", err)
	}
	if got := waitLedgerActionCount(coreGot, coreActions); got != coreActions {
		t.Fatalf("core operator received %d of %d core actions", got, coreActions)
	}
	if got := memberGot.Load(); got != 0 {
		t.Fatalf("member-c operator received %d core actions, want 0", got)
	}

	if err := pub.Publish(bus.LedgerActionSubject("member-c"), []byte("{}")); err != nil {
		t.Fatalf("publish member-c: %v", err)
	}
	if err := pub.NATSConn().Flush(); err != nil {
		t.Fatalf("flush publisher: %v", err)
	}
	if got := waitLedgerActionCount(memberGot, 1); got != 1 {
		t.Fatalf("member-c operator received %d of 1 member-c action", got)
	}
	if got := coreGot.Load(); got != coreActions {
		t.Fatalf("core operator received %d actions after the member-c action, want %d", got, coreActions)
	}
}
