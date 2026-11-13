package mite

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// miteImperativeQueueCap mirrors imperativeQueueCap in cmd/mite. The mite
// ends its stream when that queue is full, so the gateway limit must not
// exceed it.
const miteImperativeQueueCap = 16

func TestImperativeInFlightLimitFitsMiteQueue(t *testing.T) {
	if imperativeInFlightLimitDefault < 1 || imperativeInFlightLimitDefault > miteImperativeQueueCap {
		t.Fatalf("imperativeInFlightLimitDefault = %d, want 1..%d",
			imperativeInFlightLimitDefault, miteImperativeQueueCap)
	}
}

// fileImperativesUnderToken files one pending entry per command id under token.
func fileImperativesUnderToken(h *BusHandler, token int64, msgs map[string]*nats.Msg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := triggerFailedNS + "/" + triggerFailedCell
	if h.pendingAcks[key] == nil {
		h.pendingAcks[key] = make(map[string]pendingImperative)
	}
	for id, m := range msgs {
		h.pendingAcks[key][id] = pendingImperative{msg: m, token: token}
	}
}

// removePendingImperative deletes one pending entry, as a result does.
func removePendingImperative(h *BusHandler, ns, name, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pendingAcks[ns+"/"+name], id)
}

func TestWaitImperativeSlotHoldsAtLimit(t *testing.T) {
	h := &BusHandler{
		pendingAcks:           make(map[string]map[string]pendingImperative),
		imperativeInFlightMax: 2,
		imperativeSlotRecheck: 5 * time.Millisecond,
		metrics:               noopGatewayMetrics(),
	}
	fileImperativesUnderToken(h, 7, map[string]*nats.Msg{
		"a": {}, "b": {},
	})

	done := make(chan bool, 1)
	go func() {
		done <- h.waitImperativeSlot(context.Background(), triggerFailedNS, triggerFailedCell, 7)
	}()
	select {
	case <-done:
		t.Fatal("waitImperativeSlot returned while the connection held the limit")
	case <-time.After(200 * time.Millisecond):
	}

	removePendingImperative(h, triggerFailedNS, triggerFailedCell, "a")
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitImperativeSlot returned false with a live context")
		}
	case <-time.After(time.Second):
		t.Fatal("waitImperativeSlot did not return after a result freed a slot")
	}
}

func TestWaitImperativeSlotCountsOwnConnection(t *testing.T) {
	h := &BusHandler{
		pendingAcks:           make(map[string]map[string]pendingImperative),
		imperativeInFlightMax: 2,
		imperativeSlotRecheck: 5 * time.Millisecond,
		metrics:               noopGatewayMetrics(),
	}
	// Two entries under token 6 fill the limit for token 6 only.
	fileImperativesUnderToken(h, 6, map[string]*nats.Msg{
		"a": {}, "b": {},
	})

	done := make(chan bool, 1)
	go func() {
		done <- h.waitImperativeSlot(context.Background(), triggerFailedNS, triggerFailedCell, 7)
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitImperativeSlot returned false for a connection under its limit")
		}
	case <-time.After(time.Second):
		t.Fatal("a superseded connection held back the replacement")
	}
}

func TestWaitImperativeSlotEndsWithContext(t *testing.T) {
	h := &BusHandler{
		pendingAcks:           make(map[string]map[string]pendingImperative),
		imperativeInFlightMax: 1,
		imperativeSlotRecheck: 5 * time.Millisecond,
		metrics:               noopGatewayMetrics(),
	}
	fileImperativesUnderToken(h, 7, map[string]*nats.Msg{"a": {}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- h.waitImperativeSlot(ctx, triggerFailedNS, triggerFailedCell, 7)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("waitImperativeSlot returned true after its context ended")
		}
	case <-time.After(time.Second):
		t.Fatal("waitImperativeSlot did not end with its context")
	}
}

// publishTriggerBuildID publishes one TRIGGER_BUILD imperative with the given
// command id on the cell's out subject. It retries until the gateway's own
// stream exists.
func publishTriggerBuildID(t *testing.T, conn *bus.Conn, id string) {
	t.Helper()
	data, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{Imperative: &mitev1.ImperativeCommand{
			CommandId: id,
			Type:      mitev1.CommandTypeTriggerBuild,
			TriggerBuild: &mitev1.TriggerBuildPayload{
				RunId:     "run-" + id,
				IntentKey: "intent-" + id,
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
		_, err := conn.JetStream().Publish(subject, data, nats.MsgId(id))
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("publish imperative %s: %v", id, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// awaitImperativeID waits for the imperative with the wanted command id.
func awaitImperativeID(t *testing.T, imps chan *mitev1.ImperativeCommand, id string) {
	t.Helper()
	select {
	case imp, ok := <-imps:
		if !ok {
			t.Fatalf("stream closed before %s arrived", id)
		}
		if imp.CommandId != id {
			t.Fatalf("got command %q, want %q", imp.CommandId, id)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no imperative %s after %s", id, 15*time.Second)
	}
}

// TestImperativePumpHoldsInFlightLimit publishes more commands than the limit
// to a mite that returns no results. The gateway must send exactly the limit,
// then one more after the first result.
func TestImperativePumpHoldsInFlightLimit(t *testing.T) {
	limit := imperativeInFlightLimitDefault
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())
	stream, imps := connectResultMite(t, lis.Addr().String(), certAuth)

	total := limit + 3
	for i := 1; i <= total; i++ {
		publishTriggerBuildID(t, busConn, fmt.Sprintf("cmd-%02d", i))
	}

	for i := 1; i <= limit; i++ {
		awaitImperativeID(t, imps, fmt.Sprintf("cmd-%02d", i))
	}
	assertNoSecondCopy(t, imps)

	if err := stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_CommandResult{
		CommandResult: &mitev1.CommandResult{CommandId: "cmd-01", Success: true},
	}}); err != nil {
		t.Fatalf("send result: %v", err)
	}
	awaitImperativeID(t, imps, fmt.Sprintf("cmd-%02d", limit+1))
	assertNoSecondCopy(t, imps)
}

// TestReleasePendingImperativesNaksInStreamOrder releases six unanswered
// commands. The redelivery must keep the publish order, which a nak in Go map
// order does not.
func TestReleasePendingImperativesNaksInStreamOrder(t *testing.T) {
	h, conn := newTriggerFailedHandler(t, &fakePublisher{}, triggerFailedResolver())
	subject := bus.MiteOutSubject(h.cluster, triggerFailedNS, triggerFailedCell)
	const streamName = "imp_inflight_order"
	const consumerName = "imp_inflight_order_consumer"
	if err := conn.EnsureStream(&nats.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
		Storage:  nats.MemoryStorage,
	}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if err := conn.EnsureConsumer(streamName, consumerName, &nats.ConsumerConfig{
		Durable:       consumerName,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       h.imperativeAckWaitOrDefault(),
		MaxDeliver:    -1,
		FilterSubject: subject,
	}); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}

	const n = 6
	for i := 1; i <= n; i++ {
		data, err := json.Marshal(&mitev1.OperatorMessage{
			Message: &mitev1.OperatorMessage_Imperative{Imperative: &mitev1.ImperativeCommand{
				CommandId: fmt.Sprintf("cmd-%d", i),
				Type:      mitev1.CommandTypeTriggerBuild,
			}},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := conn.PublishJetStream(subject, data); err != nil {
			t.Fatalf("publish cmd-%d: %v", i, err)
		}
	}

	sub, err := conn.PullSubscribe(subject, streamName, consumerName)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	held := make(map[string]*nats.Msg, n)
	for len(held) < n {
		msgs := drainAll(t, conn, sub, n-len(held))
		for _, m := range msgs {
			held[pendingCommand(m).CommandId] = m
		}
	}
	fileImperativesUnderToken(h, 7, held)

	h.releasePendingImperatives(triggerFailedNS, triggerFailedCell, 7)

	var got []string
	for len(got) < n {
		for _, m := range drainAll(t, conn, sub, n-len(got)) {
			got = append(got, pendingCommand(m).CommandId)
		}
	}
	for i, id := range got {
		if want := fmt.Sprintf("cmd-%d", i+1); id != want {
			t.Fatalf("redelivery order = %v, want publish order", got)
		}
	}
}

// drainAll fetches up to n messages, retrying past idle timeouts until at
// least one arrives.
func drainAll(t *testing.T, conn *bus.Conn, sub *nats.Subscription, n int) []*nats.Msg {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := conn.DrainConsumer(sub, n)
		if err == nil && len(msgs) > 0 {
			return msgs
		}
	}
	t.Fatal("no message arrived")
	return nil
}

// connectGrantMite opens one CommandStream as the redelivery cell and returns
// the imperatives and the token grants that the stream delivers.
func connectGrantMite(t *testing.T, addr string, certAuth *ca.CA) (chan *mitev1.ImperativeCommand, chan *mitev1.TokenGrant) {
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
	imps := make(chan *mitev1.ImperativeCommand, 32)
	grants := make(chan *mitev1.TokenGrant, 8)
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
			if g := msg.GetTokenGrant(); g != nil {
				grants <- g
			}
		}
	}()
	return imps, grants
}

// TestImperativeBacklogDoesNotHoldTokenGrant fills the in-flight limit with
// unanswered imperatives. A token grant must still reach the mite at once,
// because it travels on the desired-state request, not behind the backlog.
func TestImperativeBacklogDoesNotHoldTokenGrant(t *testing.T) {
	limit := imperativeInFlightLimitDefault
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())
	imps, grants := connectGrantMite(t, lis.Addr().String(), certAuth)

	total := limit + 3
	for i := 1; i <= total; i++ {
		publishTriggerBuildID(t, busConn, fmt.Sprintf("cmd-%02d", i))
	}
	for i := 1; i <= limit; i++ {
		awaitImperativeID(t, imps, fmt.Sprintf("cmd-%02d", i))
	}

	requestTokenGrant(t, busConn, redeliveryNS, redeliveryCell, &mitev1.TokenGrant{
		MiteJenkinsToken:    "fresh-token",
		MiteJenkinsTokenExp: 4242,
	})
	select {
	case g := <-grants:
		if g.MiteJenkinsToken != "fresh-token" {
			t.Fatalf("unexpected token grant: %+v", g)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("token grant waited behind the imperative backlog")
	}
	assertNoSecondCopy(t, imps)

	info, err := busConn.JetStream().StreamInfo(bus.DefaultStreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != uint64(total) {
		t.Errorf("stream holds %d messages, want %d imperatives and no grant", info.State.Msgs, total)
	}
}

// TestImperativeSubjectDropsTokenGrant proves that a token grant someone
// publishes on the imperative subject is acked and dropped, never forwarded.
// No grant may ride the imperative consumer.
func TestImperativeSubjectDropsTokenGrant(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())
	imps, grants := connectGrantMite(t, lis.Addr().String(), certAuth)

	grantData, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_TokenGrant{TokenGrant: &mitev1.TokenGrant{
			MiteJenkinsToken:    "stray-token",
			MiteJenkinsTokenExp: 4242,
		}},
	})
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	subject := bus.MiteOutSubject(bus.DefaultCluster, redeliveryNS, redeliveryCell)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := busConn.JetStream().Publish(subject, grantData)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("publish grant on imperative subject: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	publishTriggerBuildID(t, busConn, "after-grant")
	awaitImperativeID(t, imps, "after-grant")

	select {
	case g := <-grants:
		t.Fatalf("grant on the imperative subject reached the mite: %+v", g)
	case <-time.After(500 * time.Millisecond):
	}
	waitFor(t, func() bool {
		info, err := busConn.JetStream().StreamInfo(bus.DefaultStreamName)
		return err == nil && info.State.Msgs == 1
	}, 2*time.Second, "the dropped grant was not acked: only the imperative should remain")
}
