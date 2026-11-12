package activity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

var errNoAck = errors.New("no ack")

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// dropRecorder collects the drop reasons and the subjects of dropped events.
type dropRecorder struct {
	mu       sync.Mutex
	reasons  []string
	subjects []string
}

func (r *dropRecorder) onDrop(m pendingEvent, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
	r.subjects = append(r.subjects, m.subject)
}

func (r *dropRecorder) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reasons...), append([]string(nil), r.subjects...)
}

func testConfig(queue int, budget time.Duration, rec *dropRecorder) ackedConfig {
	return ackedConfig{
		queueSize:      queue,
		attemptTimeout: time.Second,
		budget:         budget,
		backoffStart:   time.Millisecond,
		backoffMax:     5 * time.Millisecond,
		onDrop:         rec.onDrop,
	}
}

// callLog counts publish calls and keeps their message IDs and payloads.
type callLog struct {
	mu       sync.Mutex
	calls    int
	msgIDs   []string
	subjects []string
	payloads []string
}

func (c *callLog) record(subject string, data []byte, msgID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.msgIDs = append(c.msgIDs, msgID)
	c.subjects = append(c.subjects, subject)
	c.payloads = append(c.payloads, string(data))
}

func (c *callLog) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestAckedPublisher_RetriesUntilAck(t *testing.T) {
	rec := &dropRecorder{}
	var log callLog
	var n atomic.Int32
	publish := func(_ context.Context, subject string, data []byte, msgID string) error {
		log.record(subject, data, msgID)
		if n.Add(1) <= 3 {
			return errNoAck
		}
		return nil
	}
	p := newAckedPublisher("c", nil, AckedOptions{Logger: quietLogger()}, testConfig(8, 5*time.Second, rec), publish)
	p.PublishData("activity.c.ns.x", "phase", []byte(`{"a":1}`))
	p.Close(2 * time.Second)

	if got := log.count(); got != 4 {
		t.Fatalf("calls = %d, want 4", got)
	}
	if len(log.msgIDs[0]) != 32 {
		t.Fatalf("msgID = %q, want a 32-character ID", log.msgIDs[0])
	}
	for i, id := range log.msgIDs {
		if id != log.msgIDs[0] {
			t.Fatalf("call %d msgID = %s, want the first call's %s", i, id, log.msgIDs[0])
		}
	}
	if reasons, _ := rec.snapshot(); len(reasons) != 0 {
		t.Fatalf("drops = %v, want none", reasons)
	}
}

func TestAckedPublisher_DropsAfterBudget(t *testing.T) {
	rec := &dropRecorder{}
	var log callLog
	publish := func(_ context.Context, subject string, data []byte, msgID string) error {
		log.record(subject, data, msgID)
		return errNoAck
	}
	p := newAckedPublisher("c", nil, AckedOptions{Logger: quietLogger()}, testConfig(8, 100*time.Millisecond, rec), publish)
	p.PublishData("activity.c.ns.x", "phase", []byte(`{}`))
	p.Close(5 * time.Second)

	reasons, _ := rec.snapshot()
	if len(reasons) != 1 || reasons[0] != dropBudget {
		t.Fatalf("drops = %v, want one %q", reasons, dropBudget)
	}
	if got := log.count(); got < 2 {
		t.Fatalf("calls = %d, want at least 2", got)
	}
}

func TestAckedPublisher_SkipsExpiredEventWhileUnhealthy(t *testing.T) {
	rec := &dropRecorder{}
	var log callLog
	gate := make(chan struct{})
	var n atomic.Int32
	publish := func(_ context.Context, subject string, data []byte, msgID string) error {
		log.record(subject, data, msgID)
		if n.Add(1) == 1 {
			<-gate
		}
		return errNoAck
	}
	p := newAckedPublisher("c", nil, AckedOptions{Logger: quietLogger()}, testConfig(8, 50*time.Millisecond, rec), publish)
	p.PublishData("activity.c.ns.a", "phase", []byte(`A`))
	p.PublishData("activity.c.ns.b", "phase", []byte(`B`))
	time.Sleep(100 * time.Millisecond) // both events pass the budget
	close(gate)
	p.Close(5 * time.Second)

	if got := log.count(); got != 1 {
		t.Fatalf("calls = %d, want 1 (B must drop without a try)", got)
	}
	reasons, subjects := rec.snapshot()
	if len(reasons) != 2 || reasons[0] != dropBudget || reasons[1] != dropBudget {
		t.Fatalf("drops = %v, want two %q", reasons, dropBudget)
	}
	if subjects[1] != "activity.c.ns.b" {
		t.Fatalf("second drop subject = %s, want activity.c.ns.b", subjects[1])
	}
}

func TestAckedPublisher_ExpiredEventGetsOneTryWhileHealthy(t *testing.T) {
	rec := &dropRecorder{}
	var log callLog
	gate := make(chan struct{})
	var n atomic.Int32
	publish := func(_ context.Context, subject string, data []byte, msgID string) error {
		log.record(subject, data, msgID)
		if n.Add(1) == 1 {
			<-gate
		}
		return nil
	}
	p := newAckedPublisher("c", nil, AckedOptions{Logger: quietLogger()}, testConfig(8, 50*time.Millisecond, rec), publish)
	p.PublishData("activity.c.ns.a", "phase", []byte(`A`))
	p.PublishData("activity.c.ns.b", "phase", []byte(`B`))
	time.Sleep(100 * time.Millisecond)
	close(gate)
	p.Close(5 * time.Second)

	if got := log.count(); got != 2 {
		t.Fatalf("calls = %d, want 2 (B gets one try)", got)
	}
	if reasons, _ := rec.snapshot(); len(reasons) != 0 {
		t.Fatalf("drops = %v, want none", reasons)
	}
}

func TestAckedPublisher_QueueFullDropsNewest(t *testing.T) {
	rec := &dropRecorder{}
	var log callLog
	inflight := make(chan struct{})
	gate := make(chan struct{})
	var n atomic.Int32
	publish := func(_ context.Context, subject string, data []byte, msgID string) error {
		log.record(subject, data, msgID)
		if n.Add(1) == 1 {
			close(inflight)
			<-gate
		}
		return nil
	}
	p := newAckedPublisher("c", nil, AckedOptions{Logger: quietLogger()}, testConfig(1, time.Minute, rec), publish)
	p.PublishData("activity.c.ns.a", "phase", []byte(`A`))
	<-inflight
	p.PublishData("activity.c.ns.b", "phase", []byte(`B`)) // fills the queue
	p.PublishData("activity.c.ns.c", "phase", []byte(`C`)) // queue full
	close(gate)
	p.Close(5 * time.Second)

	reasons, subjects := rec.snapshot()
	if len(reasons) != 1 || reasons[0] != dropQueueFull || subjects[0] != "activity.c.ns.c" {
		t.Fatalf("drops = %v on %v, want one %q on activity.c.ns.c", reasons, subjects, dropQueueFull)
	}
	if got := log.count(); got != 2 {
		t.Fatalf("calls = %d, want 2 (A and B delivered)", got)
	}
}

func TestAckedPublisher_CloseDrainsQueue(t *testing.T) {
	rec := &dropRecorder{}
	var log callLog
	publish := func(_ context.Context, subject string, data []byte, msgID string) error {
		log.record(subject, data, msgID)
		return nil
	}
	p := newAckedPublisher("c", nil, AckedOptions{Logger: quietLogger()}, testConfig(64, time.Minute, rec), publish)
	for i := range 20 {
		p.PublishData("activity.c.ns.x", "phase", []byte{byte('a' + i)})
	}
	p.Close(2 * time.Second)

	if got := log.count(); got != 20 {
		t.Fatalf("calls = %d, want 20", got)
	}
	for i, payload := range log.payloads {
		if payload != string([]byte{byte('a' + i)}) {
			t.Fatalf("payload %d = %q, out of order", i, payload)
		}
	}
	if reasons, _ := rec.snapshot(); len(reasons) != 0 {
		t.Fatalf("drops = %v, want none", reasons)
	}
}

func TestAckedPublisher_CloseCancelsAtTimeout(t *testing.T) {
	rec := &dropRecorder{}
	publish := func(ctx context.Context, _ string, _ []byte, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	cfg := testConfig(8, time.Minute, rec)
	cfg.attemptTimeout = time.Minute
	p := newAckedPublisher("c", nil, AckedOptions{Logger: quietLogger()}, cfg, publish)
	p.PublishData("activity.c.ns.a", "phase", []byte(`A`))
	p.PublishData("activity.c.ns.b", "phase", []byte(`B`))
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	p.Close(100 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v, want about 100ms", elapsed)
	}
	reasons, _ := rec.snapshot()
	if len(reasons) != 2 || reasons[0] != dropShutdown || reasons[1] != dropShutdown {
		t.Fatalf("drops = %v, want two %q", reasons, dropShutdown)
	}
}

func TestAckedPublisher_PublishAfterCloseDrops(t *testing.T) {
	rec := &dropRecorder{}
	publish := func(context.Context, string, []byte, string) error { return nil }
	p := newAckedPublisher("c", nil, AckedOptions{Logger: quietLogger()}, testConfig(8, time.Minute, rec), publish)
	p.Close(time.Second)
	p.PublishData("activity.c.ns.x", "phase", []byte(`{}`))

	reasons, _ := rec.snapshot()
	if len(reasons) != 1 || reasons[0] != dropClosed {
		t.Fatalf("drops = %v, want one %q", reasons, dropClosed)
	}
}

func TestAckedPublisher_StoresEventAfterStreamAppears(t *testing.T) {
	s := startActivityNATS(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	cfg := testConfig(8, 10*time.Second, &dropRecorder{})
	cfg.attemptTimeout = 100 * time.Millisecond
	cfg.backoffStart = 10 * time.Millisecond
	cfg.backoffMax = 50 * time.Millisecond
	p := newAckedPublisher("member-b", conn, AckedOptions{Logger: quietLogger()}, cfg, jetStreamPublish(conn))

	p.Publish(Event{Cluster: "member-b", Namespace: "varroa-system", Controller: "cell-0", Type: "phase"})
	time.Sleep(300 * time.Millisecond) // no stream yet, so the sender retries
	if err := conn.EnsureStream(&nats.StreamConfig{
		Name:     bus.ActivityStreamName,
		Subjects: []string{"activity.>"},
		Storage:  nats.MemoryStorage,
	}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := conn.StreamInfo(bus.ActivityStreamName)
		if err == nil && info.State.Msgs == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream did not store the event (info=%v, err=%v)", info, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.Close(2 * time.Second)

	info, err := conn.StreamInfo(bus.ActivityStreamName)
	if err != nil || info.State.Msgs != 1 {
		t.Fatalf("after close: msgs=%v err=%v, want 1", info, err)
	}
	msg, err := conn.JetStream().GetLastMsg(bus.ActivityStreamName, "activity.member-b.varroa-system.cell-0")
	if err != nil {
		t.Fatalf("last message on the controller subject: %v", err)
	}
	if msg.Subject != "activity.member-b.varroa-system.cell-0" {
		t.Fatalf("stored subject = %s", msg.Subject)
	}
}

func TestNewPublisherFromEnv_SelectsMode(t *testing.T) {
	cases := []struct {
		value string
		acked bool
	}{
		{"1", true},
		{"", false},
		{"0", false},
	}
	for _, tc := range cases {
		getenv := func(key string) string {
			if key != AckEnv {
				t.Fatalf("read env %q, want %q", key, AckEnv)
			}
			return tc.value
		}
		p := NewPublisherFromEnv(getenv, "c", nil, AckedOptions{Logger: quietLogger()})
		if got := p.acked != nil; got != tc.acked {
			t.Fatalf("value %q: ack mode = %v, want %v", tc.value, got, tc.acked)
		}
		p.Close(time.Second)
	}
}

// startActivityNATS starts an embedded NATS server with JetStream enabled.
func startActivityNATS(t *testing.T) *server.Server {
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
