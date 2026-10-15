package mite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
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
// in-progress interval. The margin between the two must absorb a
// scheduler stall on a loaded race-detector run.
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
	msgs, err := conn.DrainConsumer(sub, 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch imperative: %d messages, %v", len(msgs), err)
	}
	return msgs[0]
}

// redeliveredIDs fetches for wait and answers the command ids the stream
// delivered again.
func redeliveredIDs(t *testing.T, conn *bus.Conn, sub *nats.Subscription, wait time.Duration) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		msgs, err := conn.DrainConsumer(sub, 4)
		if err != nil && !errors.Is(err, nats.ErrTimeout) {
			t.Fatalf("fetch: %v", err)
		}
		for _, m := range msgs {
			if imp := pendingCommand(m); imp != nil {
				ids[imp.CommandId] = true
			}
		}
	}
	return ids
}

// TestImperativeProgressOnlyTouchesOwnToken files two pending commands under
// two connection tokens. The heartbeat of one token must keep only its own
// command from a repeat, and a stopped heartbeat must leave its command to the
// ack wait.
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
	key := triggerFailedNS + "/" + triggerFailedCell
	h.mu.Lock()
	h.pendingAcks[key] = map[string]pendingImperative{
		"cmd-own":   {msg: own, token: 7},
		"cmd-other": {msg: other, token: 8},
	}
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.markImperativesInProgress(ctx, triggerFailedNS, triggerFailedCell, 7)
	}()

	got := redeliveredIDs(t, conn, sub, 3*time.Second)
	if got["cmd-own"] {
		t.Fatal("the heartbeat did not keep its own command from a repeat")
	}
	if !got["cmd-other"] {
		t.Fatal("another token's command did not repeat after its ack wait")
	}

	cancel()
	<-done
	if got := redeliveredIDs(t, conn, sub, 3*time.Second); !got["cmd-own"] {
		t.Fatal("a stopped heartbeat did not leave its command to the ack wait")
	}
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
	msgs, err := conn.DrainConsumer(sub, 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch: %d messages, %v", len(msgs), err)
	}

	sends := 0
	h.forwardImperative(context.Background(), msgs[0], triggerFailedNS, triggerFailedCell, 7, func(*mitev1.OperatorMessage) error {
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
