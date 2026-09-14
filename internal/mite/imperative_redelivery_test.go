package mite

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// redeliveryNS and redeliveryCell name the cell these tests roll.
const (
	redeliveryNS        = "ns1"
	redeliveryCell      = "cell1"
	redeliveryCommandID = "trigger-build-run-1"
)

// connectRedeliveryMite opens one CommandStream, sends Hello, and returns the
// client connection plus a channel of every imperative the gateway pushes.
func connectRedeliveryMite(t *testing.T, addr string, certAuth *ca.CA) (*grpc.ClientConn, chan *mitev1.ImperativeCommand) {
	t.Helper()
	gconn := dialWithMiteCert(t, addr, certAuth, redeliveryCell, redeliveryNS)
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
	return gconn, imps
}

// publishTriggerBuild publishes one TRIGGER_BUILD imperative on the cell's out
// subject. It retries until the gateway's own stream exists.
func publishTriggerBuild(t *testing.T, conn *bus.Conn) {
	t.Helper()
	data, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{Imperative: &mitev1.ImperativeCommand{
			CommandId: redeliveryCommandID,
			Type:      mitev1.CommandTypeTriggerBuild,
			TriggerBuild: &mitev1.TriggerBuildPayload{
				RunId:     "run-1",
				IntentKey: "intent-1",
				JobPath:   "team-a/build",
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal imperative: %v", err)
	}
	subject := bus.MiteOutSubject(bus.DefaultCluster, redeliveryNS, redeliveryCell)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := conn.PublishJetStream(subject, data)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("publish imperative: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// awaitImperative waits for one imperative with the wanted command id.
func awaitImperative(t *testing.T, imps chan *mitev1.ImperativeCommand, wait time.Duration, what string) {
	t.Helper()
	select {
	case imp, ok := <-imps:
		if !ok {
			t.Fatalf("%s: stream closed before the imperative arrived", what)
		}
		if imp.CommandId != redeliveryCommandID {
			t.Fatalf("%s: got command %q, want %q", what, imp.CommandId, redeliveryCommandID)
		}
	case <-time.After(wait):
		t.Fatalf("%s: no imperative after %s", what, wait)
	}
}

// TestImperativeRedeliveredAfterMiteDisconnect rolls the cell pod after the
// gateway sends a TRIGGER_BUILD and before any CommandResult. The replacement
// mite must receive the same command again, exactly once.
func TestImperativeRedeliveredAfterMiteDisconnect(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())
	addr := lis.Addr().String()
	gconn1, imps1 := connectRedeliveryMite(t, addr, certAuth)
	publishTriggerBuild(t, busConn)
	awaitImperative(t, imps1, 15*time.Second, "first mite")

	// The pod dies before it runs the command, so it returns no result.
	gconn1.Close()
	time.Sleep(500 * time.Millisecond)

	_, imps2 := connectRedeliveryMite(t, addr, certAuth)
	awaitImperative(t, imps2, 15*time.Second, "replacement mite")
	assertNoSecondCopy(t, imps2)
}

// TestImperativeRedeliveredWhenReplacementConnectsFirst rolls the cell pod the
// way the gateway sees it when the old socket dies without a close: the
// replacement mite connects first, and the superseded stream tears down after.
// The gateway must still release the command, and it must not wait for the
// JetStream ack timeout to do so.
func TestImperativeRedeliveredWhenReplacementConnectsFirst(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())
	addr := lis.Addr().String()
	gconn1, imps1 := connectRedeliveryMite(t, addr, certAuth)
	publishTriggerBuild(t, busConn)
	awaitImperative(t, imps1, 15*time.Second, "first mite")

	_, imps2 := connectRedeliveryMite(t, addr, certAuth)
	time.Sleep(500 * time.Millisecond)
	gconn1.Close()

	// 10 seconds is well inside the consumer's 30-second ack timeout, so a pass
	// proves the disconnect released the command.
	awaitImperative(t, imps2, 10*time.Second, "replacement mite")
	assertNoSecondCopy(t, imps2)
}

// assertNoSecondCopy fails if a second copy of the imperative arrives. The mite
// runs the command once per delivery, so a duplicate delivery is a duplicate
// trigger the run claim must not have to absorb.
func assertNoSecondCopy(t *testing.T, imps chan *mitev1.ImperativeCommand) {
	t.Helper()
	select {
	case imp, ok := <-imps:
		if ok {
			t.Fatalf("imperative %s delivered twice", imp.CommandId)
		}
	case <-time.After(2 * time.Second):
	}
}

// TestForwardImperativeNaksAfterCancel drives the window the deferred nak
// opens: the gateway cancels this pump, naks the pending command, and the
// pump's own outstanding fetch receives it back. The pump must nak that
// delivery and file nothing, so the replacement connection gets the command
// instead of waiting out the consumer's ack timeout.
func TestForwardImperativeNaksAfterCancel(t *testing.T) {
	h, conn := newTriggerFailedHandler(t, &fakePublisher{}, triggerFailedResolver())
	stream, consumer, msg := seedPendingImperative(t, h, conn, triggerBuildCommand())

	// The disconnect released the command, so drop the filed entry the helper
	// seeded and hand the same delivery back to a cancelled pump.
	key := triggerFailedNS + "/" + triggerFailedCell
	h.mu.Lock()
	delete(h.pendingAcks, key)
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sends := 0
	h.forwardImperative(ctx, msg, triggerFailedNS, triggerFailedCell, 7, func(*mitev1.OperatorMessage) error {
		sends++
		return nil
	})

	if sends != 0 {
		t.Fatalf("sends = %d, want 0: a cancelled pump must not reach its dead stream", sends)
	}
	h.mu.Lock()
	filed := len(h.pendingAcks[key])
	h.mu.Unlock()
	if filed != 0 {
		t.Fatalf("filed pending imperatives = %d, want 0", filed)
	}

	requireRedelivered(t, conn, h.cluster, stream, consumer)
}

// requireRedelivered asserts the nak made the command available again, so a
// fresh subscription on the same durable consumer receives it.
func requireRedelivered(t *testing.T, conn *bus.Conn, cluster, stream, consumer string) {
	t.Helper()
	sub, err := conn.PullSubscribe(bus.MiteOutSubject(cluster, triggerFailedNS, triggerFailedCell), stream, consumer)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	msgs, err := conn.DrainConsumer(sub, 1)
	if err != nil {
		t.Fatalf("fetch redelivery: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(msgs))
	}
	var op mitev1.OperatorMessage
	if err := json.Unmarshal(msgs[0].Data, &op); err != nil {
		t.Fatalf("unmarshal redelivery: %v", err)
	}
	if imp := op.GetImperative(); imp == nil || imp.CommandId != "cmd-1" {
		t.Fatalf("redelivered %+v, want command cmd-1", imp)
	}
}

// cancelOnFirstErrCtx cancels itself the first time a caller reads Err(), and
// reports that first read as live. It places a supersede exactly in the gap
// between forwardImperative's fast-path check and its h.mu.Lock(), which is
// the window the re-check under that lock closes.
type cancelOnFirstErrCtx struct {
	context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnFirstErrCtx) Err() error {
	err := c.Context.Err()
	c.once.Do(c.cancel)
	return err
}

// TestForwardImperativeNaksOnCancelUnderLock asserts the pump reads the
// cancellation under h.mu. OnConnect and OnDisconnect both cancel a pump while
// they hold that lock, so a pump that only reads the context before taking it
// files the command under a dead token and sends it on a stream the mite
// replaced.
func TestForwardImperativeNaksOnCancelUnderLock(t *testing.T) {
	h, conn := newTriggerFailedHandler(t, &fakePublisher{}, triggerFailedResolver())
	stream, consumer, msg := seedPendingImperative(t, h, conn, triggerBuildCommand())

	key := triggerFailedNS + "/" + triggerFailedCell
	h.mu.Lock()
	delete(h.pendingAcks, key)
	h.mu.Unlock()

	inner, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnFirstErrCtx{Context: inner, cancel: cancel}

	sends := 0
	h.forwardImperative(ctx, msg, triggerFailedNS, triggerFailedCell, 7, func(*mitev1.OperatorMessage) error {
		sends++
		return nil
	})

	if sends != 0 {
		t.Fatalf("sends = %d, want 0: a supersede under h.mu must stop the send", sends)
	}
	h.mu.Lock()
	filed := len(h.pendingAcks[key])
	h.mu.Unlock()
	if filed != 0 {
		t.Fatalf("filed pending imperatives = %d, want 0", filed)
	}
	requireRedelivered(t, conn, h.cluster, stream, consumer)
}
