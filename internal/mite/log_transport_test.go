package mite

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// startRecordingGateway starts a gRPC gateway server backed by a
// recordingHandler, with no NATS or Postgres dependency. It exercises the
// real CommandStream read loop in server.go, not a hand-simulated switch.
func startRecordingGateway(t *testing.T) (*recordingHandler, net.Listener, *ca.CA) {
	t.Helper()

	certAuth, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new ca: %v", err)
	}
	tokenSigner := NewTokenSigner([]byte(certAuth.PrivateKey()))

	rec := &recordingHandler{
		logChunkCh: make(chan recordedLogChunk, 4),
		tailAckCh:  make(chan recordedTailAck, 4),
	}

	srv := NewServer(certAuth, tokenSigner, "localhost")
	srv.ServerEndpoint = "127.0.0.1:0"
	srv.SetStreamHandler(rec)
	srv.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.GRPCServer().Serve(lis)
	t.Cleanup(srv.GRPCServer().GracefulStop)

	return rec, lis, certAuth
}

// TestGatewayIntegration_LogChunkAndTailAckDispatch asserts that
// server.go's CommandStream read loop routes MiteMessage_LogChunk to
// StreamHandler.OnLogChunk and MiteMessage_TailAck to
// StreamHandler.OnTailAck, each as its own oneof case, with the sender's
// mTLS controller/namespace attached exactly as the other message cases do.
func TestGatewayIntegration_LogChunkAndTailAckDispatch(t *testing.T) {
	rec, lis, certAuth := startRecordingGateway(t)

	name, ns := "logctl", "logns"
	gconn := dialWithMiteCert(t, lis.Addr().String(), certAuth, name, ns)
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

	wantChunk := &mitev1.LogChunk{
		RunId:        "run-log-1",
		JobPath:      "logns/logctl",
		BuildNumber:  1,
		Seq:          0,
		Data:         []byte("hello console\n"),
		CellInstance: "pod-uid-1",
	}
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_LogChunk{LogChunk: wantChunk},
	}); err != nil {
		t.Fatalf("send log chunk: %v", err)
	}

	wantAck := &mitev1.TailAck{
		RunId:     "run-log-1",
		ControlId: "control-1",
		Active:    true,
	}
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_TailAck{TailAck: wantAck},
	}); err != nil {
		t.Fatalf("send tail ack: %v", err)
	}

	select {
	case got := <-rec.logChunkCh:
		if got.name != name || got.ns != ns {
			t.Fatalf("OnLogChunk identity = (%q, %q), want (%q, %q)", got.name, got.ns, name, ns)
		}
		if got.chunk.RunId != wantChunk.RunId || got.chunk.Seq != wantChunk.Seq {
			t.Fatalf("OnLogChunk chunk = %+v, want %+v", got.chunk, wantChunk)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnLogChunk")
	}

	select {
	case got := <-rec.tailAckCh:
		if got.name != name || got.ns != ns {
			t.Fatalf("OnTailAck identity = (%q, %q), want (%q, %q)", got.name, got.ns, name, ns)
		}
		if got.ack.RunId != wantAck.RunId || got.ack.ControlId != wantAck.ControlId {
			t.Fatalf("OnTailAck ack = %+v, want %+v", got.ack, wantAck)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnTailAck")
	}
}
