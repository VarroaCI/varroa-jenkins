package mite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/bus/bustest"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// ackWaitStream and ackWaitConsumer name the gateway's own imperative stream
// and the durable consumer of the redelivery cell.
const (
	ackWaitStream   = "varroa"
	ackWaitConsumer = "imp-" + bus.DefaultCluster + "-" + redeliveryNS + "-" + redeliveryCell
)

// connectResultMite opens one CommandStream, as connectRedeliveryMite does,
// and also returns the stream so a test can send a CommandResult.
func connectResultMite(t *testing.T, addr string, certAuth *ca.CA) (mitev1.Mite_CommandStreamClient, chan *mitev1.ImperativeCommand) {
	t.Helper()
	gconn := dialWithMiteCert(t, addr, certAuth, redeliveryCell, redeliveryNS)
	t.Cleanup(func() { _ = gconn.Close() })
	stream, err := mitev1.NewMiteClient(gconn).CommandStream(context.Background())
	if err != nil {
		t.Fatalf("command stream: %v", err)
	}
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Hello{
			Hello: &mitev1.Hello{ControllerName: redeliveryCell, Namespace: redeliveryNS, Version: "test-v1"},
		},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	imps := make(chan *mitev1.ImperativeCommand, 8)
	go func() {
		defer close(imps)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if imp := msg.GetImperative(); imp != nil {
				imps <- imp
			}
		}
	}()
	return stream, imps
}

// heartbeatAckWait sets a two-second imperative ack wait and a 100ms
// in-progress interval. The margin between the two must absorb a scheduler
// stall and a forward step of the wall clock. The server measures the ack
// wait on the wall clock.
func heartbeatAckWait(h *BusHandler) {
	h.imperativeAckWait = 2 * time.Second
	h.imperativeProgressEvery = 100 * time.Millisecond
}

// heartbeatHold is three ack waits of the heartbeat test.
const heartbeatHold = 3 * 2 * time.Second

// consumerConfig waits until the gateway's imperative consumer exists and
// reports its config.
func consumerConfig(t *testing.T, conn *bus.Conn) nats.ConsumerConfig {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := conn.ConsumerInfo(ackWaitStream, ackWaitConsumer)
		if err == nil {
			return info.Config
		}
		if time.Now().After(deadline) {
			t.Fatalf("consumer info: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestImperativeConsumerDeclaresAckWait checks that the per-mite consumer
// carries the explicit ack wait and an unbounded delivery count.
func TestImperativeConsumerDeclaresAckWait(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())
	_, imps := connectResultMite(t, lis.Addr().String(), certAuth)
	publishTriggerBuild(t, busConn)
	awaitImperative(t, imps, 15*time.Second, "mite")

	cfg := consumerConfig(t, busConn)
	if cfg.AckWait != imperativeAckWaitDefault {
		t.Fatalf("ack wait = %s, want %s", cfg.AckWait, imperativeAckWaitDefault)
	}
	if cfg.MaxDeliver != -1 {
		t.Fatalf("max deliver = %d, want -1", cfg.MaxDeliver)
	}
}

// TestImperativeConsumerUpdatesExistingAckWait checks that a durable consumer
// made with the old implicit ack wait takes the new value in place when a mite
// connects.
func TestImperativeConsumerUpdatesExistingAckWait(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())
	if err := busConn.EnsureStream(bus.StreamConfig(ackWaitStream)); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if err := busConn.EnsureConsumer(ackWaitStream, ackWaitConsumer, &nats.ConsumerConfig{
		Durable:       ackWaitConsumer,
		FilterSubject: bus.MiteOutSubject(bus.DefaultCluster, redeliveryNS, redeliveryCell),
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       30 * time.Second,
	}); err != nil {
		t.Fatalf("ensure old consumer: %v", err)
	}

	_, imps := connectResultMite(t, lis.Addr().String(), certAuth)
	publishTriggerBuild(t, busConn)
	awaitImperative(t, imps, 15*time.Second, "mite")

	if got := consumerConfig(t, busConn).AckWait; got != imperativeAckWaitDefault {
		t.Fatalf("ack wait = %s, want %s", got, imperativeAckWaitDefault)
	}
}

// TestPendingImperativeHeartbeatPreventsRedelivery holds a command without a
// result for three ack waits. The in-progress heartbeat must keep the stream
// from a repeat, and the result must then acknowledge the command.
func TestPendingImperativeHeartbeatPreventsRedelivery(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL(), heartbeatAckWait)
	stream, imps := connectResultMite(t, lis.Addr().String(), certAuth)
	publishTriggerBuild(t, busConn)
	awaitImperative(t, imps, 15*time.Second, "mite")

	select {
	case imp, ok := <-imps:
		if ok {
			t.Fatalf("imperative %s delivered again while it ran", imp.CommandId)
		}
		t.Fatal("stream closed while the command ran")
	case <-time.After(heartbeatHold):
	}

	if err := stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_CommandResult{
		CommandResult: &mitev1.CommandResult{CommandId: redeliveryCommandID, Success: true},
	}}); err != nil {
		t.Fatalf("send result: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for ackPending(t, busConn, ackWaitStream, ackWaitConsumer) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the result did not acknowledge the command")
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertNoSecondCopy(t, imps)
}

// seedProgressMessage publishes one imperative, fetches it through the seeded
// consumer, and returns the fetched message.
func seedProgressMessage(t *testing.T, conn *bus.Conn, sub *nats.Subscription, subject, commandID string) *nats.Msg {
	t.Helper()
	cmd := triggerBuildCommand()
	cmd.CommandId = commandID
	data, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{Imperative: cmd},
	})
	if err != nil {
		t.Fatalf("marshal imperative: %v", err)
	}
	if _, err := conn.PublishJetStream(subject, data); err != nil {
		t.Fatalf("publish imperative: %v", err)
	}
	return bustest.FetchOne(t, sub, bustest.PresenceWait)
}

// progressWait bounds each wait of the progress test on an event that must
// come. The wait ends when the event comes, so a pass costs no more time.
const progressWait = 10 * time.Second

// progressMarks subscribes to the acknowledgement subject of msg. Each
// in-progress acknowledgement that the gateway sends for msg arrives on the
// subscription. The subscription and the gateway share one connection, so
// the server sees the subscription before any acknowledgement.
func progressMarks(t *testing.T, conn *bus.Conn, msg *nats.Msg) *nats.Subscription {
	t.Helper()
	marks, err := conn.NATSConn().SubscribeSync(msg.Reply)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", msg.Reply, err)
	}
	t.Cleanup(func() { _ = marks.Unsubscribe() })
	return marks
}

// awaitProgressMarks waits for n in-progress acknowledgements on marks.
func awaitProgressMarks(t *testing.T, marks *nats.Subscription, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(progressWait)
	for got := 0; got < n; got++ {
		m, err := marks.NextMsg(time.Until(deadline))
		if err != nil {
			t.Fatalf("%s: %d of %d in-progress acknowledgements: %v", what, got, n, err)
		}
		if string(m.Data) != "+WPI" {
			t.Fatalf("%s: acknowledgement %q, want +WPI", what, m.Data)
		}
	}
}

// awaitRedelivery fetches until the stream delivers commandID again.
func awaitRedelivery(t *testing.T, conn *bus.Conn, sub *nats.Subscription, commandID, what string) {
	t.Helper()
	deadline := time.Now().Add(progressWait)
	for time.Now().Before(deadline) {
		msgs, err := conn.DrainConsumer(sub, 4)
		if err != nil && !errors.Is(err, nats.ErrTimeout) {
			t.Fatalf("fetch: %v", err)
		}
		for _, m := range msgs {
			if imp := pendingCommand(m); imp != nil && imp.CommandId == commandID {
				return
			}
		}
	}
	t.Fatal(what)
}

// TestImperativeProgressOnlyTouchesOwnToken files two pending commands under
// two connection tokens. The heartbeat of one token must mark only its own
// command in progress, and a stopped heartbeat must leave its command to the
// ack wait.
//
// The test reads the acknowledgements on the wire. It does not assert that
// the marked command never repeats. The server measures the ack wait on the
// wall clock. A forward clock step larger than the margin after a mark
// expires a marked command. A clock sync on the test host can step the clock
// by more than this test's 750ms margin.
// TestPendingImperativeHeartbeatPreventsRedelivery covers the repeat end to
// end with a wider margin.
func TestImperativeProgressOnlyTouchesOwnToken(t *testing.T) {
	h, conn := newTriggerFailedHandler(t, &fakePublisher{}, triggerFailedResolver())
	h.imperativeAckWait = time.Second
	subject := bus.MiteOutSubject(h.cluster, triggerFailedNS, triggerFailedCell)
	stream, consumer := "imp_progress", "imp_progress_consumer"
	if err := conn.EnsureStream(&nats.StreamConfig{Name: stream, Subjects: []string{subject}, Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if err := conn.EnsureConsumer(stream, consumer, &nats.ConsumerConfig{
		Durable:       consumer,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       h.imperativeAckWaitOrDefault(),
		MaxDeliver:    -1,
		FilterSubject: subject,
	}); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}
	sub, err := conn.PullSubscribe(subject, stream, consumer)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	own := seedProgressMessage(t, conn, sub, subject, "cmd-own")
	other := seedProgressMessage(t, conn, sub, subject, "cmd-other")
	ownMarks := progressMarks(t, conn, own)
	otherMarks := progressMarks(t, conn, other)
	key := triggerFailedNS + "/" + triggerFailedCell
	h.mu.Lock()
	h.pendingAcks[key] = map[string]pendingImperative{
		"cmd-own":   {msg: own, token: 7},
		"cmd-other": {msg: other, token: 8},
	}
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.markImperativesInProgress(ctx, triggerFailedNS, triggerFailedCell, 7)
	}()

	awaitProgressMarks(t, ownMarks, 2, "the heartbeat did not mark its own command in progress")
	awaitRedelivery(t, conn, sub, "cmd-other", "another token's command did not repeat after its ack wait")

	cancel()
	select {
	case <-done:
	case <-time.After(progressWait):
		t.Fatal("the heartbeat did not stop with its context")
	}
	// The flush round trip puts every acknowledgement that the stopped
	// heartbeat sent into the subscription queue before the count below.
	if err := conn.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n, _, err := otherMarks.Pending(); err != nil || n != 0 {
		t.Fatalf("the heartbeat marked another token's command in progress %d times (err %v)", n, err)
	}
	awaitRedelivery(t, conn, sub, "cmd-own", "a stopped heartbeat did not leave its command to the ack wait")
}

// TestForwardImperativeTerminatesUndecodableMessage checks that a message that
// does not decode is terminated: the stream must not deliver it again.
func TestForwardImperativeTerminatesUndecodableMessage(t *testing.T) {
	h, conn := newTriggerFailedHandler(t, &fakePublisher{}, triggerFailedResolver())
	h.imperativeAckWait = time.Second
	subject := bus.MiteOutSubject(h.cluster, triggerFailedNS, triggerFailedCell)
	stream, consumer := "imp_poison", "imp_poison_consumer"
	if err := conn.EnsureStream(&nats.StreamConfig{Name: stream, Subjects: []string{subject}, Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if err := conn.EnsureConsumer(stream, consumer, &nats.ConsumerConfig{
		Durable:       consumer,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       h.imperativeAckWaitOrDefault(),
		MaxDeliver:    -1,
		FilterSubject: subject,
	}); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}
	if _, err := conn.PublishJetStream(subject, []byte("{not json")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	sub, err := conn.PullSubscribe(subject, stream, consumer)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	msg := bustest.FetchOne(t, sub, bustest.PresenceWait)

	sends := 0
	h.forwardImperative(context.Background(), msg, triggerFailedNS, triggerFailedCell, 7, func(*mitev1.OperatorMessage) error {
		sends++
		return nil
	})
	if sends != 0 {
		t.Fatalf("sent %d messages to the mite, want 0", sends)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		again, err := conn.DrainConsumer(sub, 1)
		if err != nil && !errors.Is(err, nats.ErrTimeout) {
			t.Fatalf("fetch: %v", err)
		}
		if len(again) > 0 {
			t.Fatal("the stream delivered the undecodable message again")
		}
	}
	if n := ackPending(t, conn, stream, consumer); n != 0 {
		t.Fatalf("ack pending = %d, want 0", n)
	}
}
