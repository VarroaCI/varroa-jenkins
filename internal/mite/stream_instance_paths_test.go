package mite

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// streamPodA and streamPodB name two pods of one controller. Each test
// connects both to one gateway replica.
const (
	streamPodA = "pod-a"
	streamPodB = "pod-b"
)

// openOperatorStream opens a CommandStream on gconn, sends its hello, and
// returns the stream with a channel of every message the gateway sends on
// it.
func openOperatorStream(t *testing.T, gconn *grpc.ClientConn, name, ns string) (mitev1.Mite_CommandStreamClient, <-chan *mitev1.OperatorMessage) {
	t.Helper()
	stream := openHelloStream(t, gconn, name, ns)
	msgs := make(chan *mitev1.OperatorMessage, 64)
	go func() {
		defer close(msgs)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			msgs <- msg
		}
	}()
	return stream, msgs
}

// awaitOperatorMessage waits for the first message on msgs that match
// accepts. It skips every other message.
func awaitOperatorMessage(t *testing.T, msgs <-chan *mitev1.OperatorMessage, what string, match func(*mitev1.OperatorMessage) bool) *mitev1.OperatorMessage {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatalf("stream closed before %s", what)
			}
			if match(msg) {
				return msg
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// pingStream sends a run-state request that the gateway refuses at once,
// and waits for the refusal. The read loop of the stream answers it, so
// the gateway has connected the stream when the answer arrives.
func pingStream(t *testing.T, stream mitev1.Mite_CommandStreamClient, msgs <-chan *mitev1.OperatorMessage) {
	t.Helper()
	pingID := uint64(time.Now().UnixNano())
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_RunStateRequest{RunStateRequest: &mitev1.RunStateRequest{RequestId: &pingID}},
	}); err != nil {
		t.Fatalf("send ping: %v", err)
	}
	awaitOperatorMessage(t, msgs, "the ping answer", func(m *mitev1.OperatorMessage) bool {
		resp := m.GetRunStateResponse()
		return resp != nil && resp.RequestId != nil && *resp.RequestId == pingID
	})
}

// registerPodInstance renews the certificate of name/ns on gconn with
// instance. The gateway writes instance into its per-controller entry.
func registerPodInstance(t *testing.T, gconn *grpc.ClientConn, name, ns, instance string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if _, err := mitev1.NewMiteClient(gconn).Register(context.Background(), &mitev1.RegisterRequest{
		ControllerName: name,
		Namespace:      ns,
		PublicKey:      []byte(pub),
		CellInstance:   instance,
	}); err != nil {
		t.Fatalf("register %s: %v", instance, err)
	}
}

// openStreamUnderOtherEntry opens the stream of one pod on ownConn and
// waits until the gateway connects it. It then registers the other pod on
// otherConn, so the per-controller entry names other while the stream of
// the first pod is current.
func openStreamUnderOtherEntry(t *testing.T, h *BusHandler, ownConn, otherConn *grpc.ClientConn, name, ns, other string) (mitev1.Mite_CommandStreamClient, <-chan *mitev1.OperatorMessage) {
	t.Helper()
	stream, msgs := openOperatorStream(t, ownConn, name, ns)
	pingStream(t, stream, msgs)
	registerPodInstance(t, otherConn, name, ns, other)
	if got := h.lookupCellInstance(ns, name); got != other {
		t.Fatalf("per-controller entry = %q, want %q", got, other)
	}
	return stream, msgs
}

// subscribeBuildEnvelopes ensures the builds stream and returns a channel
// of every envelope published on it.
func subscribeBuildEnvelopes(t *testing.T, busConn *bus.Conn) <-chan []byte {
	t.Helper()
	if err := busConn.EnsureStream(bus.BuildsStreamConfig(bus.BuildsStreamName)); err != nil {
		t.Fatalf("ensure builds stream: %v", err)
	}
	envCh := make(chan []byte, 32)
	sub, err := busConn.NATSConn().Subscribe(bus.BuildsWildcard, func(msg *nats.Msg) {
		envCh <- msg.Data
	})
	if err != nil {
		t.Fatalf("subscribe builds: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return envCh
}

// awaitRunStateAck waits for the run-state ack of runID on msgs.
func awaitRunStateAck(t *testing.T, msgs <-chan *mitev1.OperatorMessage, runID string) *mitev1.RunStateAck {
	t.Helper()
	msg := awaitOperatorMessage(t, msgs, "the run-state ack of "+runID, func(m *mitev1.OperatorMessage) bool {
		ack := m.GetRunStateAck()
		return ack != nil && ack.RunId == runID
	})
	return msg.GetRunStateAck()
}

// TestGatewayIntegration_RunStateChunkCarriesItsOwnStreamInstance proves
// that a run-state chunk decides its owner from the instance of the
// stream that sent it, and that its run.state envelope carries that
// instance. Pod B owns the run. The entry names pod A while the stream
// of pod B is current, and the chunk of pod B must still be stored. The
// entry then names pod B while the stream of the stale pod A is current,
// and the chunk of pod A must be refused.
func TestGatewayIntegration_RunStateChunkCarriesItsOwnStreamInstance(t *testing.T) {
	runStateStreamInstanceTest(t, func(runID string) *mitev1.RunStateChunk {
		return oneChunk(runID, "jobs/pipeline/builds/1/program.dat", 1, []byte("state"))
	}, "")
}

// TestGatewayIntegration_RestartCopyCarriesItsOwnStreamInstance proves the
// same rule for a restart copy, which arrives after the run is terminal.
func TestGatewayIntegration_RestartCopyCarriesItsOwnStreamInstance(t *testing.T) {
	body := []byte("<build/>")
	runStateStreamInstanceTest(t, func(runID string) *mitev1.RunStateChunk {
		return restartOneChunk(runID, restartTestBuildPath+"build.xml", 100, 1, int64(len(body)), body)
	}, "COMPLETED")
}

// runStateStreamInstanceTest runs the two-stream case of one kind of
// run-state chunk. chunkOf builds the chunk of a run, and status is the
// run status that the ledger answers.
func runStateStreamInstanceTest(t *testing.T, chunkOf func(runID string) *mitev1.RunStateChunk, status string) {
	t.Helper()
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	owner := newFakeOwnerRequester()
	var h *BusHandler
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL(), func(bh *BusHandler) {
		h = bh
		bh.logStore = store
		bh.ledger = owner
	})
	envCh := subscribeBuildEnvelopes(t, busConn)

	addr := lis.Addr().String()
	name, ns := "statectl", "statens"
	ownerRun, staleRun := newTestRunID(), newTestRunID()
	for _, runID := range []string{ownerRun, staleRun} {
		owner.setAnswer(runID, ownerLookupResponse{
			Found: true, Cluster: bus.DefaultCluster, Namespace: ns, Cell: name, Instance: streamPodB, Status: status,
		})
	}
	connA := dialWithMiteCertCellInstance(t, addr, certAuth, name, ns, streamPodA)
	connB := dialWithMiteCertCellInstance(t, addr, certAuth, name, ns, streamPodB)

	streamB, msgsB := openStreamUnderOtherEntry(t, h, connB, connA, name, ns, streamPodA)
	if err := streamB.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_RunStateChunk{RunStateChunk: chunkOf(ownerRun)},
	}); err != nil {
		t.Fatalf("send chunk of pod B: %v", err)
	}
	if ack := awaitRunStateAck(t, msgsB, ownerRun); ack.Status != stateAckStatusStored {
		t.Fatalf("ack of the owner's chunk = %s %s, want %s", ack.Status, ack.Reason, stateAckStatusStored)
	}
	if got := awaitEnvelopeInstance(t, envCh, ownerRun); got != streamPodB {
		t.Fatalf("run.state envelope of pod B carries %q, want %q", got, streamPodB)
	}

	streamA, msgsA := openStreamUnderOtherEntry(t, h, connA, connB, name, ns, streamPodB)
	if err := streamA.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_RunStateChunk{RunStateChunk: chunkOf(staleRun)},
	}); err != nil {
		t.Fatalf("send chunk of pod A: %v", err)
	}
	if ack := awaitRunStateAck(t, msgsA, staleRun); ack.Status != stateAckStatusRejected || ack.Reason != stateReasonForeignRun {
		t.Fatalf("ack of the stale pod's chunk = %s %s, want %s %s",
			ack.Status, ack.Reason, stateAckStatusRejected, stateReasonForeignRun)
	}
}

// TestGatewayIntegration_CommandResultCarriesItsOwnStreamInstance proves
// that the run record of a command result carries the instance of the
// stream that sent the result. The record store keeps an accepted
// trigger's instance as the fallback owner of the run. Each pod answers
// a trigger while the entry names the other pod.
func TestGatewayIntegration_CommandResultCarriesItsOwnStreamInstance(t *testing.T) {
	var h *BusHandler
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL(), func(bh *BusHandler) {
		h = bh
		bh.SetRecordStoreEnabled(true)
	})
	envCh := subscribeBuildEnvelopes(t, busConn)

	addr := lis.Addr().String()
	name, ns := redeliveryCell, redeliveryNS
	connA := dialWithMiteCertCellInstance(t, addr, certAuth, name, ns, streamPodA)
	connB := dialWithMiteCertCellInstance(t, addr, certAuth, name, ns, streamPodB)

	answerTrigger := func(stream mitev1.Mite_CommandStreamClient, msgs <-chan *mitev1.OperatorMessage, id string) {
		t.Helper()
		publishTriggerBuildID(t, busConn, id)
		awaitOperatorMessage(t, msgs, "the trigger "+id, func(m *mitev1.OperatorMessage) bool {
			imp := m.GetImperative()
			return imp != nil && imp.CommandId == id
		})
		if err := stream.Send(&mitev1.MiteMessage{
			Message: &mitev1.MiteMessage_CommandResult{CommandResult: &mitev1.CommandResult{
				CommandId: id, Success: true, HttpStatus: 202,
			}},
		}); err != nil {
			t.Fatalf("send result of %s: %v", id, err)
		}
	}

	streamB, msgsB := openStreamUnderOtherEntry(t, h, connB, connA, name, ns, streamPodA)
	answerTrigger(streamB, msgsB, "trigger-b")
	if got := awaitEnvelopeInstance(t, envCh, "run-trigger-b"); got != streamPodB {
		t.Fatalf("run.trigger_accepted of pod B carries %q, want %q", got, streamPodB)
	}

	streamA, msgsA := openStreamUnderOtherEntry(t, h, connA, connB, name, ns, streamPodB)
	answerTrigger(streamA, msgsA, "trigger-a")
	if got := awaitEnvelopeInstance(t, envCh, "run-trigger-a"); got != streamPodA {
		t.Fatalf("run.trigger_accepted of pod A carries %q, want %q", got, streamPodA)
	}
}

// TestGatewayIntegration_ClaimCarriesItsOwnStreamInstance proves that a
// claim names the instance of the stream that sent it. Each pod sends a
// claim whose payload names the other pod. The ledger request and the
// run-owner cache must name the sending pod.
func TestGatewayIntegration_ClaimCarriesItsOwnStreamInstance(t *testing.T) {
	var h *BusHandler
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL(), func(bh *BusHandler) { h = bh })

	claims := make(chan ledgerClaimRequest, 4)
	sub, err := busConn.NATSConn().Subscribe(bus.LedgerClaimSubject, func(msg *nats.Msg) {
		var req ledgerClaimRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			return
		}
		claims <- req
		reply, _ := json.Marshal(ledgerClaimResponse{Decision: claimDecisionGranted, RunID: req.RunID, BuildNumber: 1})
		_ = msg.Respond(reply)
	})
	if err != nil {
		t.Fatalf("subscribe ledger claims: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	addr := lis.Addr().String()
	name, ns := "claimctl", "claimns"
	connA := dialWithMiteCertCellInstance(t, addr, certAuth, name, ns, streamPodA)
	connB := dialWithMiteCertCellInstance(t, addr, certAuth, name, ns, streamPodB)

	claimFrom := func(stream mitev1.Mite_CommandStreamClient, msgs <-chan *mitev1.OperatorMessage, sent, want string) {
		t.Helper()
		runID := newTestRunID()
		if err := stream.Send(&mitev1.MiteMessage{
			Message: &mitev1.MiteMessage_ClaimRequest{ClaimRequest: &mitev1.ClaimRequest{
				RequestId: runID, Origin: "VARROA_INTENT", IntentKey: "manual:" + runID, RunId: runID,
				JobPath: "pipeline/build", CellInstance: sent,
			}},
		}); err != nil {
			t.Fatalf("send claim: %v", err)
		}
		select {
		case req := <-claims:
			if req.RunID != runID || req.CellInstance != want {
				t.Fatalf("ledger request names run %s instance %q, want run %s instance %q", req.RunID, req.CellInstance, runID, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the ledger claim request")
		}
		awaitOperatorMessage(t, msgs, "the claim answer", func(m *mitev1.OperatorMessage) bool {
			resp := m.GetClaimResponse()
			return resp != nil && resp.RequestId == runID
		})
		entry, ok := h.ownerCache.get(runID)
		if !ok || entry.tuple.instance != want {
			t.Fatalf("run-owner cache = %+v (found %v), want instance %q", entry.tuple, ok, want)
		}
	}

	streamB, msgsB := openStreamUnderOtherEntry(t, h, connB, connA, name, ns, streamPodA)
	claimFrom(streamB, msgsB, streamPodA, streamPodB)

	streamA, msgsA := openStreamUnderOtherEntry(t, h, connA, connB, name, ns, streamPodB)
	claimFrom(streamA, msgsA, streamPodB, streamPodA)
}

// TestGatewayIntegration_ClaimKeepsItsInstanceWhenTheCertificateHasNone
// covers a stream whose certificate names no cell instance. The claim
// keeps the instance that the mite sent, because the certificate offers
// no value to overwrite it with.
func TestGatewayIntegration_ClaimKeepsItsInstanceWhenTheCertificateHasNone(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	claims := make(chan ledgerClaimRequest, 4)
	sub, err := busConn.NATSConn().Subscribe(bus.LedgerClaimSubject, func(msg *nats.Msg) {
		var req ledgerClaimRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			return
		}
		claims <- req
		reply, _ := json.Marshal(ledgerClaimResponse{Decision: claimDecisionGranted, RunID: req.RunID, BuildNumber: 1})
		_ = msg.Respond(reply)
	})
	if err != nil {
		t.Fatalf("subscribe ledger claims: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	conn := dialWithMiteCertCellInstance(t, lis.Addr().String(), certAuth, "claimctl", "claimns", "")
	stream, msgs := openOperatorStream(t, conn, "claimctl", "claimns")
	pingStream(t, stream, msgs)

	runID := newTestRunID()
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_ClaimRequest{ClaimRequest: &mitev1.ClaimRequest{
			RequestId: runID, Origin: "VARROA_INTENT", IntentKey: "manual:" + runID, RunId: runID,
			JobPath: "pipeline/build", CellInstance: "pod-real",
		}},
	}); err != nil {
		t.Fatalf("send claim: %v", err)
	}
	select {
	case req := <-claims:
		if req.RunID != runID || req.CellInstance != "pod-real" {
			t.Fatalf("ledger request names run %s instance %q, want run %s instance %q", req.RunID, req.CellInstance, runID, "pod-real")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the ledger claim request")
	}
}
