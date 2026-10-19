package triggers

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// startNATS runs an embedded server with JetStream for one test.
func startNATS(t *testing.T) string {
	t.Helper()
	srv, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server did not start")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// fetchCount pulls up to max messages and reports how many arrived before the
// wait ended.
func fetchCount(t *testing.T, conn *bus.Conn) int {
	t.Helper()
	sub, err := conn.PullSubscribe("activity.>", StreamName, ConsumerName)
	if err != nil {
		t.Fatalf("PullSubscribe: %v", err)
	}
	msgs, err := sub.Fetch(10, nats.MaxWait(500*time.Millisecond))
	if err != nil && !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("Fetch: %v", err)
	}
	for _, m := range msgs {
		_ = m.Ack()
	}
	return len(msgs)
}

// TestConsumer_NewConsumerReplaysNothing proves that a consumer created after
// rows are in the stream starts at new rows only, and that a row published after
// creation is delivered.
func TestConsumer_NewConsumerReplaysNothing(t *testing.T) {
	conn, err := bus.Connect(startNATS(t), bus.Config{})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	if err := conn.EnsureStream(bus.ActivityStreamConfig(StreamName)); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := conn.PublishJetStream("activity.build.failed", []byte(`{"type":"build.failed"}`)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if err := Ensure(conn); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := fetchCount(t, conn); got != 0 {
		t.Fatalf("a new consumer replayed %d rows, want 0", got)
	}
	if _, err := conn.PublishJetStream("activity.build.failed", []byte(`{"type":"build.failed"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := fetchCount(t, conn); got != 1 {
		t.Fatalf("a row published after the consumer = %d, want 1", got)
	}
}

// TestEnsure_AbsentStream proves that Ensure names the absent stream instead of
// creating a consumer on nothing.
func TestEnsure_AbsentStream(t *testing.T) {
	conn, err := bus.Connect(startNATS(t), bus.Config{})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	if err := Ensure(conn); !errors.Is(err, ErrStreamAbsent) {
		t.Fatalf("Ensure on no stream = %v, want ErrStreamAbsent", err)
	}
}
