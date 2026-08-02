package bus

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startTestServer starts an embedded NATS server with JetStream enabled.
// It is automatically shut down when the test completes.
func startTestServer(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{
		Port:      -1, // random port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// connectTest connects to an embedded NATS server for testing.
func connectTest(t *testing.T, s *server.Server) *Conn {
	t.Helper()
	c, err := Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestConnect(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	if c.NATSConn() == nil {
		t.Fatal("expected non-nil NATS conn")
	}
	if !c.NATSConn().IsConnected() {
		t.Fatal("expected connected")
	}
}

func TestPublishSubscribe(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	received := make(chan []byte, 1)
	_, err := c.Subscribe("test.foo", func(msg *nats.Msg) {
		received <- msg.Data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := c.Publish("test.foo", []byte("hello")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case data := <-received:
		if string(data) != "hello" {
			t.Fatalf("expected 'hello', got %q", string(data))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestQueueSubscribe(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	received := make(chan string, 5)
	handler := func(msg *nats.Msg) {
		received <- string(msg.Data)
	}

	if _, err := c.QueueSubscribe("test.q", "workers", handler); err != nil {
		t.Fatalf("sub 1: %v", err)
	}
	if _, err := c.QueueSubscribe("test.q", "workers", handler); err != nil {
		t.Fatalf("sub 2: %v", err)
	}

	// Publish 3 messages; they should be distributed across the 2 queue subscribers.
	for i := 0; i < 3; i++ {
		if err := c.Publish("test.q", []byte("x")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	count := 0
	timeout := time.After(2 * time.Second)
	for count < 3 {
		select {
		case <-received:
			count++
		case <-timeout:
			t.Fatalf("expected 3 messages, got %d", count)
		}
	}
}

func TestRequestReply(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	// Set up a responder.
	_, err := c.Subscribe("req.echo", func(msg *nats.Msg) {
		msg.Respond([]byte("echo: " + string(msg.Data)))
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	resp, err := c.Request("req.echo", []byte("ping"), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if string(resp.Data) != "echo: ping" {
		t.Fatalf("expected 'echo: ping', got %q", string(resp.Data))
	}
}

// --- JetStream tests ---

func TestEnsureStream(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	cfg := &nats.StreamConfig{
		Name:     "teststream",
		Subjects: []string{"test.*.out"},
		MaxAge:   time.Hour,
		Storage:  nats.FileStorage,
	}
	if err := c.EnsureStream(cfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	// Idempotent: should succeed on a second call.
	if err := c.EnsureStream(cfg); err != nil {
		t.Fatalf("ensure stream (2nd): %v", err)
	}
}

func TestJetStreamPublishAndConsume(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	// Create stream and consumer.
	stream := "cmds"
	cfg := &nats.StreamConfig{
		Name:     stream,
		Subjects: []string{"mite.*.*.*.out"},
		Storage:  nats.FileStorage,
	}
	if err := c.EnsureStream(cfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	consumerCfg := &nats.ConsumerConfig{
		Durable:       "worker",
		AckPolicy:     nats.AckExplicitPolicy,
		FilterSubject: "mite.core.ns.name.out",
	}
	if err := c.EnsureConsumer(stream, "worker", consumerCfg); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}

	// Publish a message.
	ack, err := c.PublishJetStream("mite.core.ns.name.out", []byte("hello-js"))
	if err != nil {
		t.Fatalf("publish js: %v", err)
	}
	if ack == nil {
		t.Fatal("expected non-nil pub ack")
	}
	t.Logf("published seq=%d stream=%s", ack.Sequence, ack.Stream)

	// Pull consume.
	sub, err := c.PullSubscribe("mite.core.ns.name.out", stream, "worker")
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}

	msgs, err := c.DrainConsumer(sub, 10)
	if err != nil {
		t.Fatalf("drain consumer: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if string(msgs[0].Data) != "hello-js" {
		t.Fatalf("expected 'hello-js', got %q", string(msgs[0].Data))
	}
	msgs[0].Ack()
}

func TestJetStreamAsyncPublish(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	stream := "asyncstream"
	cfg := &nats.StreamConfig{
		Name:     stream,
		Subjects: []string{"async.>"},
		Storage:  nats.MemoryStorage,
	}
	if err := c.EnsureStream(cfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	future, err := c.PublishJetStreamAsync("async.test", []byte("fire-and-forget"))
	if err != nil {
		t.Fatalf("publish async: %v", err)
	}
	select {
	case <-future.Ok():
		// Good: ack received.
	case err := <-future.Err():
		t.Fatalf("async pub failed: %v", err)
	}
}

// --- KV tests ---

func TestKVPutGetDelete(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("test_bucket", 0)
	if err != nil {
		t.Fatalf("ensure kv: %v", err)
	}

	// Get non-existent key.
	val, err := kv.Get("missing")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if val != nil {
		t.Fatalf("expected nil for missing key, got %q", string(val))
	}

	// Put and get.
	if err := kv.PutString("key1", "value1"); err != nil {
		t.Fatalf("put: %v", err)
	}
	val, err = kv.Get("key1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(val) != "value1" {
		t.Fatalf("expected 'value1', got %q", string(val))
	}

	// Delete and verify gone.
	if err := kv.Delete("key1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	val, err = kv.Get("key1")
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if val != nil {
		t.Fatalf("expected nil after delete, got %q", string(val))
	}
}

func TestKVRoundTrip(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV(KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("ensure kv: %v", err)
	}

	key := SnapshotKey(DefaultCluster, "myns", "mycontroller")
	if err := kv.PutString(key, "snapshot-data"); err != nil {
		t.Fatalf("put: %v", err)
	}

	val, err := kv.Get(key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(val) != "snapshot-data" {
		t.Fatalf("expected 'snapshot-data', got %q", string(val))
	}
}

// --- Subject helpers ---

func TestMiteSubjects(t *testing.T) {
	if s := MiteInSubject(DefaultCluster, "ns", "name"); s != "mite.core.ns.name.in" {
		t.Fatalf("expected 'mite.core.ns.name.in', got %q", s)
	}
	if s := MiteOutSubject(DefaultCluster, "ns", "name"); s != "mite.core.ns.name.out" {
		t.Fatalf("expected 'mite.core.ns.name.out', got %q", s)
	}
}

func TestSnapshotKey(t *testing.T) {
	if k := SnapshotKey(DefaultCluster, "ns", "name"); k != "core/ns/name" {
		t.Fatalf("expected 'core/ns/name', got %q", k)
	}
}

// Test default stream configs compile and are usable.
func TestStreamConfigDefaults(t *testing.T) {
	cfg := StreamConfig("varroa")
	if cfg.Name != "varroa" {
		t.Fatalf("expected name 'varroa', got %q", cfg.Name)
	}
	if len(cfg.Subjects) != 1 || cfg.Subjects[0] != "mite.*.*.*.out" {
		t.Fatal("expected subjects [mite.*.*.*.out]")
	}

	actCfg := ActivityStreamConfig("varroa_activity", 168*time.Hour, 100000, 1<<30)
	if actCfg.Name != "varroa_activity" {
		t.Fatalf("expected name 'varroa_activity', got %q", actCfg.Name)
	}
	if len(actCfg.Subjects) != 1 || actCfg.Subjects[0] != "activity.>" {
		t.Fatalf("expected subjects [activity.>], got %v", actCfg.Subjects)
	}
	if actCfg.Retention != nats.LimitsPolicy {
		t.Fatalf("expected LimitsPolicy retention, got %v", actCfg.Retention)
	}
	if actCfg.MaxAge != 168*time.Hour {
		t.Fatalf("expected MaxAge 168h, got %v", actCfg.MaxAge)
	}
	if actCfg.MaxMsgs != 100000 {
		t.Fatalf("expected MaxMsgs 100000, got %d", actCfg.MaxMsgs)
	}
	if actCfg.MaxBytes != 1<<30 {
		t.Fatalf("expected MaxBytes 1GiB, got %d", actCfg.MaxBytes)
	}
	if actCfg.Discard != nats.DiscardOld {
		t.Fatal("expected DiscardOld policy")
	}
	if actCfg.Storage != nats.FileStorage {
		t.Fatal("expected FileStorage")
	}
	if actCfg.Replicas != 1 {
		t.Fatalf("expected Replicas 1, got %d", actCfg.Replicas)
	}
}
