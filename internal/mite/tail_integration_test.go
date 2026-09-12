package mite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// requestWithRetry retries a request-reply call past "no responders"
// until deadline passes. watchTail subscribes asynchronously from
// OnConnect, so a request sent immediately after Hello can otherwise
// race a subscription that has not landed yet.
func requestWithRetry(t *testing.T, conn *bus.Conn, subject string, data []byte, deadline time.Duration) (*nats.Msg, error) {
	t.Helper()
	end := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(end) {
		msg, err := conn.Request(subject, data, 200*time.Millisecond)
		if err == nil {
			return msg, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

// TestGatewayIntegration_LiveTail drives a start, a renewal, and a stop
// on TailSubject against a real gRPC gateway and an embedded NATS server,
// and proves the reply follows the push: by the time the request-reply
// call returns, the gateway has already sent the matching TailControl on
// the mite's stream.
func TestGatewayIntegration_LiveTail(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	name, ns := "testctl", "testns"
	addr := lis.Addr().String()
	gconn := dialWithMiteCert(t, addr, certAuth, name, ns)
	client := mitev1.NewMiteClient(gconn)

	stream, err := client.CommandStream(context.Background())
	if err != nil {
		t.Fatalf("command stream: %v", err)
	}
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Hello{
			Hello: &mitev1.Hello{ControllerName: name, Namespace: ns, Version: "test-v1"},
		},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	runID := newTestRunID()
	subject := bus.TailSubject(bus.DefaultCluster, ns, name)

	// --- start ---
	// watchTail subscribes asynchronously from OnConnect, so the very
	// first request can race a subscription that has not landed yet;
	// retry past "no responders" rather than the request itself.
	startBody, _ := json.Marshal(tailRequest{RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 5})
	startReplyMsg, err := requestWithRetry(t, busConn, subject, startBody, 3*time.Second)
	if err != nil {
		t.Fatalf("start request: %v", err)
	}
	startReply := decodeTailReply(t, startReplyMsg.Data)
	if !startReply.OK || startReply.LeaseSeconds != tailLeaseSeconds {
		t.Fatalf("unexpected start reply: %+v", startReply)
	}

	recvMsg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv tail control (start): %v", err)
	}
	startControl, ok := recvMsg.Message.(*mitev1.OperatorMessage_TailControl)
	if !ok || startControl.TailControl.RunId != runID || startControl.TailControl.Action != "start" {
		t.Fatalf("unexpected first push: %+v", recvMsg.Message)
	}
	if startControl.TailControl.JobPath != "folder/job" || startControl.TailControl.BuildNumber != 5 {
		t.Fatalf("start push missing job identity: %+v", startControl.TailControl)
	}
	firstControlID := startControl.TailControl.ControlId

	// --- renewal ---
	renewReplyMsg, err := busConn.Request(subject, startBody, 3*time.Second)
	if err != nil {
		t.Fatalf("renewal request: %v", err)
	}
	renewReply := decodeTailReply(t, renewReplyMsg.Data)
	if !renewReply.OK || renewReply.LeaseSeconds != tailLeaseSeconds {
		t.Fatalf("unexpected renewal reply: %+v", renewReply)
	}

	recvMsg, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv tail control (renewal): %v", err)
	}
	renewControl, ok := recvMsg.Message.(*mitev1.OperatorMessage_TailControl)
	if !ok || renewControl.TailControl.Action != "start" {
		t.Fatalf("unexpected renewal push: %+v", recvMsg.Message)
	}
	if renewControl.TailControl.ControlId == firstControlID {
		t.Fatal("expected the renewal to mint a fresh control_id")
	}

	// --- stop ---
	stopBody, _ := json.Marshal(tailRequest{RunID: runID, Action: "stop"})
	stopReplyMsg, err := busConn.Request(subject, stopBody, 3*time.Second)
	if err != nil {
		t.Fatalf("stop request: %v", err)
	}
	stopReply := decodeTailReply(t, stopReplyMsg.Data)
	if !stopReply.OK {
		t.Fatalf("unexpected stop reply: %+v", stopReply)
	}

	recvMsg, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv tail control (stop): %v", err)
	}
	stopControl, ok := recvMsg.Message.(*mitev1.OperatorMessage_TailControl)
	if !ok || stopControl.TailControl.Action != "stop" || stopControl.TailControl.RunId != runID {
		t.Fatalf("unexpected stop push: %+v", recvMsg.Message)
	}
	// The gateway carries the job identity forward from the lease's last
	// start; the stop request itself named neither.
	if stopControl.TailControl.JobPath != "folder/job" || stopControl.TailControl.BuildNumber != 5 {
		t.Fatalf("stop push lost the job identity: %+v", stopControl.TailControl)
	}
}
