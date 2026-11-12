package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

const laneTestWait = 5 * time.Second

func laneMsg(data string) *nats.Msg {
	return &nats.Msg{Data: []byte(data)}
}

// msgRecorder keeps the data of each message it sees, in call order.
type msgRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (m *msgRecorder) record(msg *nats.Msg) {
	m.mu.Lock()
	m.seen = append(m.seen, string(msg.Data))
	m.mu.Unlock()
}

func (m *msgRecorder) list() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.seen)
}

func TestReplayLanes_WaitingLaneDoesNotHoldAnotherLane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	unblock := make(chan struct{})
	handledB := make(chan struct{})
	lanes := newReplayLanes(ctx, func(hctx context.Context, msg *nats.Msg) {
		if string(msg.Data) == "a" {
			select {
			case <-unblock:
			case <-hctx.Done():
			}
			return
		}
		close(handledB)
	})
	lanes.release = func(*nats.Msg) {}
	lanes.progress = func(*nats.Msg) {}

	lanes.add("ns1/cell-a", 1, laneMsg("a"))
	lanes.add("ns1/cell-b", 2, laneMsg("b"))

	select {
	case <-handledB:
	case <-time.After(laneTestWait):
		t.Fatal("lane for cell-b did not run while cell-a waited")
	}
	close(unblock)
	cancel()
	lanes.wait()
}

func TestReplayLanes_RunsOneLaneInOrderOneAtATime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var active, highest atomic.Int32
	rec := &msgRecorder{}
	done := make(chan struct{}, 5)
	lanes := newReplayLanes(ctx, func(_ context.Context, msg *nats.Msg) {
		n := active.Add(1)
		for {
			h := highest.Load()
			if n <= h || highest.CompareAndSwap(h, n) {
				break
			}
		}
		rec.record(msg)
		time.Sleep(2 * time.Millisecond)
		active.Add(-1)
		done <- struct{}{}
	})
	lanes.release = func(*nats.Msg) {}
	lanes.progress = func(*nats.Msg) {}

	for i := 1; i <= 5; i++ {
		lanes.add("ns1/cell-a", uint64(i), laneMsg(string(rune('0'+i))))
	}
	for i := 0; i < 5; i++ {
		select {
		case <-done:
		case <-time.After(laneTestWait):
			t.Fatal("lane did not finish its messages")
		}
	}
	cancel()
	lanes.wait()

	want := []string{"1", "2", "3", "4", "5"}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if highest.Load() != 1 {
		t.Fatalf("highest concurrent handlers = %d, want 1", highest.Load())
	}
}

func TestReplayLanes_SkipsRedeliveryOfHeldSequence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	rec := &msgRecorder{}
	lanes := newReplayLanes(ctx, func(hctx context.Context, msg *nats.Msg) {
		rec.record(msg)
		started <- struct{}{}
		select {
		case <-unblock:
		case <-hctx.Done():
		}
	})
	lanes.release = func(*nats.Msg) {}
	lanes.progress = func(*nats.Msg) {}

	if !lanes.add("ns1/cell-a", 7, laneMsg("first")) {
		t.Fatal("first add was refused")
	}
	select {
	case <-started:
	case <-time.After(laneTestWait):
		t.Fatal("handler did not start")
	}
	if lanes.add("ns1/cell-b", 7, laneMsg("redelivery")) {
		t.Fatal("redelivery of a held sequence was queued")
	}
	close(unblock)
	cancel()
	lanes.wait()

	if got, want := rec.list(), []string{"first"}; !slices.Equal(got, want) {
		t.Fatalf("handled = %v, want %v", got, want)
	}
}

func TestReplayLanes_RefreshSendsProgressForEveryHeldMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	lanes := newReplayLanes(ctx, func(hctx context.Context, _ *nats.Msg) {
		started <- struct{}{}
		select {
		case <-unblock:
		case <-hctx.Done():
		}
	})
	lanes.release = func(*nats.Msg) {}
	progress := &msgRecorder{}
	lanes.progress = progress.record

	lanes.add("ns1/cell-a", 1, laneMsg("current"))
	lanes.add("ns1/cell-a", 2, laneMsg("queued-1"))
	lanes.add("ns1/cell-a", 3, laneMsg("queued-2"))
	select {
	case <-started:
	case <-time.After(laneTestWait):
		t.Fatal("handler did not start")
	}
	lanes.refresh()

	want := []string{"current", "queued-1", "queued-2"}
	if got := progress.list(); !slices.Equal(got, want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
	close(unblock)
	cancel()
	lanes.wait()
}

func TestReplayLanes_ReleasesQueuedMessagesAfterContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	handled := &msgRecorder{}
	released := &msgRecorder{}
	lanes := newReplayLanes(ctx, func(hctx context.Context, msg *nats.Msg) {
		handled.record(msg)
		started <- struct{}{}
		<-hctx.Done()
	})
	lanes.release = released.record
	lanes.progress = func(*nats.Msg) {}

	lanes.add("ns1/cell-a", 1, laneMsg("in-flight"))
	lanes.add("ns1/cell-a", 2, laneMsg("queued"))
	select {
	case <-started:
	case <-time.After(laneTestWait):
		t.Fatal("handler did not start")
	}
	cancel()
	lanes.wait()

	if got, want := handled.list(), []string{"in-flight"}; !slices.Equal(got, want) {
		t.Fatalf("handled = %v, want %v", got, want)
	}
	if got, want := released.list(), []string{"queued"}; !slices.Equal(got, want) {
		t.Fatalf("released = %v, want %v", got, want)
	}
}

func TestReplayLaneKey(t *testing.T) {
	rec, store, _ := newTenantWebhookTestReconciler(t)
	crdstore.MustSeed(store,
		&v1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "payments"},
			Status: v1alpha1.TenantStatus{Placement: &v1alpha1.TenantPlacementStatus{
				Cell: "cell-a", CellNamespace: "ns1",
			}},
		},
		&v1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "billing"}},
	)

	cases := []struct {
		name string
		msg  *nats.Msg
		want string
	}{
		{"controller", tenantEnvelopeMsg(t, webhookEnvelope{Cluster: "core", Namespace: "ns1", Controller: "cell-a"}), "ns1/cell-a"},
		{"placed tenant", tenantEnvelopeMsg(t, webhookEnvelope{Cluster: "core", Tenant: "payments"}), "ns1/cell-a"},
		{"unplaced tenant", tenantEnvelopeMsg(t, webhookEnvelope{Cluster: "core", Tenant: "billing"}), "tenant:billing"},
		{"missing tenant", tenantEnvelopeMsg(t, webhookEnvelope{Cluster: "core", Tenant: "gone"}), "tenant:gone"},
		{"undecodable", laneMsg("{"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rec.replayLaneKey(context.Background(), tc.msg); got != tc.want {
				t.Fatalf("lane key = %q, want %q", got, tc.want)
			}
		})
	}
}

// answeringTransport records each imperative command. It answers the commands
// for the targets in answer, and it never answers the others.
type answeringTransport struct {
	transport.Transport
	conn    *bus.Conn
	cluster string
	answer  map[string]bool

	mu   sync.Mutex
	sent []string
}

func (a *answeringTransport) SendImperative(_ context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	target := ns + "/" + name
	a.mu.Lock()
	a.sent = append(a.sent, target)
	a.mu.Unlock()
	if a.answer[target] {
		data, err := json.Marshal(&mitev1.CommandResult{CommandId: cmd.CommandId, Success: true, HttpStatus: 200})
		if err != nil {
			return err
		}
		return a.conn.Publish(bus.WebhookResultSubject(a.cluster, ns, name), data)
	}
	return nil
}

func (a *answeringTransport) sentTargets() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sent)
}

func TestRunWebhookReplayPass_UnansweredControllerHoldsOnlyItsOwnLane(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}
	opts := &server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	client := newTestClient()
	for _, name := range []string{"slow", "fast"} {
		crdstore.MustSeed(client.store, &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"},
			Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
		})
	}
	rec := newTestReconciler(client)
	fake := &answeringTransport{
		Transport: rec.miteTransport,
		conn:      conn,
		cluster:   "core",
		answer:    map[string]bool{"ns1/fast": true},
	}
	rec.miteTransport = fake

	if err := conn.EnsureStream(bus.WebhookStreamConfig(bus.WebhookStreamName, 1)); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	for _, name := range []string{"slow", "fast"} {
		env := webhookEnvelope{
			Method:     "POST",
			Path:       "github-webhook/",
			BodyB64:    base64.StdEncoding.EncodeToString([]byte("{}")),
			ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Cluster:    "core",
			Namespace:  "ns1",
			Controller: name,
		}
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := conn.PublishJetStream(bus.WebhookSubject("core", "ns1", name), data); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- rec.runWebhookReplayPass(ctx, conn, "core", 1, time.Minute)
	}()
	defer func() {
		cancel()
		<-done
	}()

	durable := webhookReplayConsumer + "-core"
	deadline := time.Now().Add(10 * time.Second)
	for {
		sentFast := slices.Contains(fake.sentTargets(), "ns1/fast")
		info, err := conn.ConsumerInfo(bus.WebhookStreamName, durable)
		if sentFast && err == nil && info.NumAckPending == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fast controller was not replayed while slow waited; sent=%v", fake.sentTargets())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReplayLanes_RefreshExpiresWaitingMessagePastBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	handled := &msgRecorder{}
	lanes := newReplayLanes(ctx, func(hctx context.Context, msg *nats.Msg) {
		handled.record(msg)
		if string(msg.Data) == "current" {
			started <- struct{}{}
			select {
			case <-unblock:
			case <-hctx.Done():
			}
		}
	})
	lanes.release = func(*nats.Msg) {}
	progress := &msgRecorder{}
	lanes.progress = progress.record
	expired := &msgRecorder{}
	lanes.expire = expired.record
	lanes.pastBound = func(m *nats.Msg) bool { return string(m.Data) == "old" }

	lanes.add("ns1/cell-a", 1, laneMsg("current"))
	lanes.add("ns1/cell-a", 2, laneMsg("old"))
	lanes.add("ns1/cell-a", 3, laneMsg("young"))
	select {
	case <-started:
	case <-time.After(laneTestWait):
		t.Fatal("handler did not start")
	}
	lanes.refresh()

	if got, want := expired.list(), []string{"old"}; !slices.Equal(got, want) {
		t.Fatalf("expired = %v, want %v", got, want)
	}
	if got, want := progress.list(), []string{"current", "young"}; !slices.Equal(got, want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
	// The expired sequence is free again, so a redelivery is queued.
	if !lanes.add("ns1/cell-a", 2, laneMsg("old")) {
		t.Fatal("add of an expired sequence = false, want true")
	}
	close(unblock)
	deadline := time.Now().Add(laneTestWait)
	for len(handled.list()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	lanes.wait()

	if got, want := handled.list(), []string{"current", "young", "old"}; !slices.Equal(got, want) {
		t.Fatalf("handled = %v, want %v", got, want)
	}
}

// commandRecorder records each imperative command.
type commandRecorder struct {
	transport.Transport
	mu   sync.Mutex
	cmds []*mitev1.ImperativeCommand
}

func (c *commandRecorder) SendImperative(_ context.Context, _, _ string, cmd *mitev1.ImperativeCommand) error {
	c.mu.Lock()
	c.cmds = append(c.cmds, cmd)
	c.mu.Unlock()
	return nil
}

func (c *commandRecorder) list() []*mitev1.ImperativeCommand {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.cmds)
}

func TestRunWebhookReplayPass_HeldLaneSendsNextAfterAnswer(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}
	opts := &server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	client := newTestClient()
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "slow", Namespace: "ns1"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	})
	rec := newTestReconciler(client)
	rec.replayResultWait = 100 * time.Millisecond
	rec.replayProbeEvery = time.Hour
	cmds := &commandRecorder{Transport: rec.miteTransport}
	rec.miteTransport = cmds

	if err := conn.EnsureStream(bus.WebhookStreamConfig(bus.WebhookStreamName, 1)); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	for i := range 2 {
		env := webhookEnvelope{
			Method:     "POST",
			Path:       "github-webhook/",
			BodyB64:    base64.StdEncoding.EncodeToString([]byte("{}")),
			ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Cluster:    "member-b",
			Namespace:  "ns1",
			Controller: "slow",
		}
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := conn.PublishJetStream(bus.WebhookSubject("member-b", "ns1", "slow"), data); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- rec.runWebhookReplayPass(ctx, conn, "member-b", 1, time.Minute)
	}()
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(500 * time.Millisecond)
	if got := len(cmds.list()); got != 1 {
		t.Fatalf("sends while the first replay is held = %d, want 1", got)
	}
	info, err := conn.ConsumerInfoContext(context.Background(), bus.WebhookStreamName, webhookReplayConsumer+"-member-b")
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.NumAckPending != 2 {
		t.Fatalf("NumAckPending = %d, want 2", info.NumAckPending)
	}

	first := cmds.list()[0]
	data, err := json.Marshal(&mitev1.CommandResult{CommandId: first.CommandId, Success: true, HttpStatus: 200})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := conn.Publish(bus.WebhookResultSubject("member-b", "ns1", "slow"), data); err != nil {
		t.Fatalf("publish result: %v", err)
	}

	deadline := time.Now().Add(laneTestWait)
	for len(cmds.list()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(cmds.list()); got != 2 {
		t.Fatalf("sends after the answer = %d, want 2", got)
	}
	if second := cmds.list()[1]; second.CommandId == first.CommandId {
		t.Fatalf("second command id = %q, want a new id", second.CommandId)
	}
}
