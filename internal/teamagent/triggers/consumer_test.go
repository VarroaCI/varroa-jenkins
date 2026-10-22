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

// absenceWait is the window for a check that proves nothing arrived. An
// absence check proves only that no row arrived in this window.
const absenceWait = 500 * time.Millisecond

// presenceWait is the bound for a check that expects one row. A check with
// batch 1 returns as soon as the row arrives, so the long bound costs nothing
// when the test passes.
const presenceWait = 10 * time.Second

// pullSub binds one pull subscription to the trigger consumer for the whole
// test. A second subscription can lose a row to a pull request that the
// first one left on the server.
func pullSub(t *testing.T, conn *bus.Conn) *nats.Subscription {
	t.Helper()
	sub, err := conn.PullSubscribe("activity.>", StreamName, ConsumerName)
	if err != nil {
		t.Fatalf("PullSubscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return sub
}

// fetchWithin pulls up to limit messages, acks them, and reports how many
// arrived before wait ended.
func fetchWithin(t *testing.T, sub *nats.Subscription, limit int, wait time.Duration) int {
	t.Helper()
	msgs, err := sub.Fetch(limit, nats.MaxWait(wait))
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
	sub := pullSub(t, conn)
	if got := fetchWithin(t, sub, 10, absenceWait); got != 0 {
		t.Fatalf("a new consumer replayed %d rows, want 0", got)
	}
	if _, err := conn.PublishJetStream("activity.build.failed", []byte(`{"type":"build.failed"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := fetchWithin(t, sub, 1, presenceWait); got != 1 {
		t.Fatalf("a row published after the consumer = %d, want 1", got)
	}
	if got := fetchWithin(t, sub, 10, absenceWait); got != 0 {
		t.Fatalf("extra rows after the published row = %d, want 0", got)
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
