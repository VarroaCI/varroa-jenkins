package mite

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func startNATS(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := server.NewServer(opts)
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

// startGatewayWithCA starts a gRPC gateway server with BusHandler.
// It returns the CA so tests can issue client certs.
func startGatewayWithCA(t *testing.T, natsURL string) (*bus.Conn, net.Listener, *ca.CA) {
	t.Helper()

	certAuth, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new ca: %v", err)
	}
	tokenSigner := NewTokenSigner([]byte(certAuth.PrivateKey()))

	busConn, err := bus.Connect(natsURL)
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("ensure snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		t.Fatalf("ensure presence kv: %v", err)
	}
	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		t.Fatalf("ensure desired kv: %v", err)
	}
	srv := NewServer(certAuth, tokenSigner, "localhost")
	srv.ServerEndpoint = "127.0.0.1:0"
	srv.SetStreamHandler(NewBusHandler(bus.DefaultCluster, busConn, snapshotKV, presenceKV, desiredKV, nil))
	srv.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.GRPCServer().Serve(lis)
	t.Cleanup(srv.GRPCServer().GracefulStop)

	return busConn, lis, certAuth
}

// dialWithMiteCert creates a gRPC client with an mTLS cert issued by the CA.
func dialWithMiteCert(t *testing.T, addr string, certAuth *ca.CA, name, ns string) *grpc.ClientConn {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cert, err := certAuth.IssueMiteCert(name, ns, pub)
	if err != nil {
		t.Fatalf("issue cert: %v", err)
	}

	clientTLS := &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{cert.Raw},
			PrivateKey:  priv,
			Leaf:        cert,
		}},
		RootCAs:    certAuth.CertPool(),
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	}

	gconn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { gconn.Close() })

	return gconn
}

func TestGatewayIntegration_BusBridge(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	name, ns := "testctl", "testns"
	addr := lis.Addr().String()

	gconn := dialWithMiteCert(t, addr, certAuth, name, ns)
	client := mitev1.NewMiteClient(gconn)

	// Subscribe to bus for inbound mite messages.
	received := make(chan []byte, 20)
	busSub, err := busConn.SubscribeData(bus.MiteInSubject(bus.DefaultCluster, ns, name), func(data []byte) {
		received <- data
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", bus.MiteInSubject(bus.DefaultCluster, ns, name), err)
	}
	defer busSub.Unsubscribe()

	// Open CommandStream and send hello.
	stream, err := client.CommandStream(context.Background())
	if err != nil {
		t.Fatalf("command stream: %v", err)
	}

	err = stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Hello{
			Hello: &mitev1.Hello{
				ControllerName: name,
				Namespace:      ns,
				Version:        "test-v1",
			},
		},
	})
	if err != nil {
		t.Fatalf("send hello: %v", err)
	}

	// Drain initial connected event.
	drainChan(received)

	// --- Test 1: Heartbeat arrives on bus ---
	err = stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Heartbeat{
			Heartbeat: &mitev1.Heartbeat{Version: "test-v1", ActualStateHash: "abc123"},
		},
	})
	if err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}

	select {
	case data := <-received:
		var mm mitev1.MiteMessage
		if err := json.Unmarshal(data, &mm); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if hb := mm.GetHeartbeat(); hb == nil || hb.ActualStateHash != "abc123" {
			t.Fatalf("unexpected heartbeat: %+v", hb)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for heartbeat on bus")
	}

	// --- Test 2: Snapshot arrives on bus and in KV ---
	err = stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_StateSnapshot{
			StateSnapshot: &mitev1.StateSnapshot{
				JenkinsVersion: "2.479",
				JenkinsHealth:  "healthy",
				ConfigHash:     "cfg123",
			},
		},
	})
	if err != nil {
		t.Fatalf("send snapshot: %v", err)
	}

	select {
	case data := <-received:
		var mm mitev1.MiteMessage
		if err := json.Unmarshal(data, &mm); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if snap := mm.GetStateSnapshot(); snap == nil || snap.ConfigHash != "cfg123" {
			t.Fatalf("unexpected snapshot: %+v", snap)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for snapshot on bus")
	}

	// Verify KV has snapshot.
	snapshotKV, _ := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	snapData, err := snapshotKV.Get(bus.SnapshotKey(bus.DefaultCluster, ns, name))
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	if snapData == nil {
		t.Fatal("expected snapshot in KV after state snapshot")
	}

	// --- Test 3: Operator desired state forwarded from KV watch to mite ---
	opMsg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_DesiredState{
			DesiredState: &mitev1.DesiredStateCommand{
				CommandId:        "cmd-1",
				DesiredStateHash: "hash-xyz",
			},
		},
	}
	opData, _ := json.Marshal(opMsg)
	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		t.Fatalf("ensure desired kv: %v", err)
	}
	if err := desiredKV.Put(bus.DesiredKey(bus.DefaultCluster, ns, name), opData); err != nil {
		t.Fatalf("put desired state: %v", err)
	}

	recvMsg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv operator msg: %v", err)
	}
	ds, ok := recvMsg.Message.(*mitev1.OperatorMessage_DesiredState)
	if !ok || ds.DesiredState.CommandId != "cmd-1" {
		t.Fatalf("unexpected desired state: %+v", recvMsg.Message)
	}

	// --- Test 4: Disconnect clears KV ---
	stream.CloseSend()

	// Wait a moment for cleanup.
	time.Sleep(100 * time.Millisecond)

	snapData, err = snapshotKV.Get(bus.SnapshotKey(bus.DefaultCluster, ns, name))
	if err != nil {
		t.Fatalf("kv get after disconnect: %v", err)
	}
	if snapData != nil {
		t.Fatal("expected snapshot to be deleted after disconnect")
	}
}

// TestGatewayIntegration_DesiredStateLastValue proves that a mite connecting
// after two desired-state Puts receives only the latest value (last-value
// semantics), and that a Put while connected is delivered via the watch.
func TestGatewayIntegration_DesiredStateLastValue(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		t.Fatalf("desired kv: %v", err)
	}

	ns, name := "ns", "c1"

	put := func(hash string) {
		op := &mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_DesiredState{
			DesiredState: &mitev1.DesiredStateCommand{DesiredStateHash: hash}}}
		b, _ := json.Marshal(op)
		if err := desiredKV.Put(bus.DesiredKey(bus.DefaultCluster, ns, name), b); err != nil {
			t.Fatal(err)
		}
	}

	// Two Puts BEFORE the mite connects — only the second must be delivered.
	put("stale")
	put("latest")

	// Connect a fake mite.
	gconn := dialWithMiteCert(t, lis.Addr().String(), certAuth, name, ns)
	miteClient := mitev1.NewMiteClient(gconn)

	stream, err := miteClient.CommandStream(context.Background())
	if err != nil {
		t.Fatalf("command stream: %v", err)
	}

	// Send Hello to trigger OnConnect.
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Hello{
			Hello: &mitev1.Hello{ControllerName: name, Namespace: ns, Version: "test-v1"},
		},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	// The first OperatorMessage received must have hash "latest", not "stale".
	recvCh := make(chan *mitev1.OperatorMessage, 4)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if _, ok := msg.Message.(*mitev1.OperatorMessage_DesiredState); ok {
				recvCh <- msg
			}
		}
	}()

	select {
	case msg := <-recvCh:
		ds := msg.Message.(*mitev1.OperatorMessage_DesiredState)
		if got := ds.DesiredState.DesiredStateHash; got != "latest" {
			t.Fatalf("first delivery: want \"latest\", got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial desired state delivery")
	}

	// Put a new value while connected — it must be delivered via the watch.
	put("newer")

	select {
	case msg := <-recvCh:
		ds := msg.Message.(*mitev1.OperatorMessage_DesiredState)
		if got := ds.DesiredState.DesiredStateHash; got != "newer" {
			t.Fatalf("watch update: want \"newer\", got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch update delivery")
	}
}

// TestGatewayIntegration_TokenGrantForwarded proves that a TokenGrant published
// by the operator to the mite's out subject (the same JetStream subject used for
// imperative commands) is forwarded to the mite's gRPC stream: watchImperative
// must not ack-and-drop a non-imperative message, which would silently
// swallow token grants and cause "token refresh grant timeout".
func TestGatewayIntegration_TokenGrantForwarded(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	ns, name := "tgns", "tgctl"
	gconn := dialWithMiteCert(t, lis.Addr().String(), certAuth, name, ns)
	client := mitev1.NewMiteClient(gconn)

	stream, err := client.CommandStream(context.Background())
	if err != nil {
		t.Fatalf("command stream: %v", err)
	}

	// Hello triggers OnConnect, which starts watchImperative on the out subject.
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Hello{
			Hello: &mitev1.Hello{ControllerName: name, Namespace: ns, Version: "test-v1"},
		},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	// Collect operator messages from the stream.
	grants := make(chan *mitev1.TokenGrant, 4)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if g := msg.GetTokenGrant(); g != nil {
				grants <- g
			}
		}
	}()

	// Mirror the operator's handleTokenRefresh publish path.
	grantData, _ := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_TokenGrant{
			TokenGrant: &mitev1.TokenGrant{
				MiteJenkinsToken:    "fresh-token",
				MiteJenkinsTokenExp: 1234567890,
			},
		},
	})
	// Retry the publish until the gateway's durable consumer exists (it is
	// created asynchronously in watchImperative after OnConnect).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := busConn.PublishJetStream(bus.MiteOutSubject(bus.DefaultCluster, ns, name), grantData); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("publish token grant: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case g := <-grants:
		if g.MiteJenkinsToken != "fresh-token" || g.MiteJenkinsTokenExp != 1234567890 {
			t.Fatalf("unexpected token grant: %+v", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for token grant on mite stream")
	}
}

func TestGatewayIntegration_Register(t *testing.T) {
	natsSrv := startNATS(t)
	_, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	addr := lis.Addr().String()

	// Use TLS without a client cert (bootstrap registration).
	tlsCfg := &tls.Config{
		RootCAs:    certAuth.CertPool(),
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	}
	gconn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer gconn.Close()

	client := mitev1.NewMiteClient(gconn)

	tokenSigner := NewTokenSigner([]byte(certAuth.PrivateKey()))
	token, err := tokenSigner.GenerateToken("regctl", "regns", 5*time.Minute)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	resp, err := client.Register(context.Background(), &mitev1.RegisterRequest{
		ControllerName: "regctl",
		Namespace:      "regns",
		BootstrapToken: token,
		PublicKey:      []byte(pub),
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.CertificatePem == "" {
		t.Fatal("expected certificate")
	}
	t.Logf("register success: endpoint=%s", resp.ServerEndpoint)
}

func drainChan(ch <-chan []byte) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestGatewayIntegration_FullTokenRefreshGrant(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	if err := busConn.EnsureStream(bus.StreamConfig("varroa")); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	ns, name := "refns", "refctl"
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

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("ensure snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		t.Fatalf("ensure presence kv: %v", err)
	}
	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		t.Fatalf("ensure desired kv: %v", err)
	}
	_ = snapshotKV
	_ = presenceKV
	_ = desiredKV

	mintSeq := 0
	mintTokenFunc := func() (string, int64) {
		mintSeq++
		return fmt.Sprintf("token-%d", mintSeq), time.Now().Add(60 * time.Minute).Unix()
	}

	sub, err := busConn.SubscribeData(bus.MiteInSubject(bus.DefaultCluster, ns, name), func(data []byte) {
		var mm mitev1.MiteMessage
		if err := json.Unmarshal(data, &mm); err != nil {
			return
		}
		if mm.GetTokenRefreshRequest() != nil {
			token, exp := mintTokenFunc()
			grantData, _ := json.Marshal(&mitev1.OperatorMessage{
				Message: &mitev1.OperatorMessage_TokenGrant{
					TokenGrant: &mitev1.TokenGrant{
						MiteJenkinsToken:    token,
						MiteJenkinsTokenExp: exp,
					},
				},
			})
			busConn.PublishJetStream(bus.MiteOutSubject(bus.DefaultCluster, ns, name), grantData)
		}
	})
	if err != nil {
		t.Fatalf("subscribe inbound: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	refreshTime := time.Now()
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_TokenRefreshRequest{
			TokenRefreshRequest: &mitev1.TokenRefreshRequest{},
		},
	}); err != nil {
		t.Fatalf("send refresh request: %v", err)
	}

	grants := make(chan *mitev1.TokenGrant, 4)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if g := msg.GetTokenGrant(); g != nil {
				grants <- g
			}
		}
	}()

	select {
	case g := <-grants:
		if g.MiteJenkinsToken == "" {
			t.Error("expected non-empty token")
		}
		if g.MiteJenkinsTokenExp <= refreshTime.Unix() {
			t.Errorf("token expiry %d should be after refresh time %d", g.MiteJenkinsTokenExp, refreshTime.Unix())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for token grant on mite stream")
	}
}

func TestGatewayIntegration_StaleGrantDoesNotAdvanceCachedToken(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	if err := busConn.EnsureStream(bus.StreamConfig("varroa")); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	ns, name := "stns", "stctl"
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

	// Wait for the gateway's pull consumer to be created.
	time.Sleep(200 * time.Millisecond)

	fixedExp := time.Now().Add(60 * time.Minute).Unix()
	fresherExp := time.Now().Add(90 * time.Minute).Unix()

	grantData, _ := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_TokenGrant{
			TokenGrant: &mitev1.TokenGrant{
				MiteJenkinsToken:    "initial-token",
				MiteJenkinsTokenExp: fixedExp,
			},
		},
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := busConn.PublishJetStream(bus.MiteOutSubject(bus.DefaultCluster, ns, name), grantData); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("publish initial grant: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	grants := make(chan *mitev1.TokenGrant, 8)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if g := msg.GetTokenGrant(); g != nil {
				grants <- g
			}
		}
	}()

	var firstGrant *mitev1.TokenGrant
	select {
	case g := <-grants:
		firstGrant = g
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first token grant")
	}
	if firstGrant.MiteJenkinsToken != "initial-token" || firstGrant.MiteJenkinsTokenExp != fixedExp {
		t.Fatalf("unexpected first grant: tok=%q exp=%d", firstGrant.MiteJenkinsToken, firstGrant.MiteJenkinsTokenExp)
	}

	grantData2, _ := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_TokenGrant{
			TokenGrant: &mitev1.TokenGrant{
				MiteJenkinsToken:    "same-expiry-token",
				MiteJenkinsTokenExp: fixedExp,
			},
		},
	})
	if _, err := busConn.PublishJetStream(bus.MiteOutSubject(bus.DefaultCluster, ns, name), grantData2); err != nil {
		t.Fatalf("publish stale grant: %v", err)
	}

	grantData3, _ := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_TokenGrant{
			TokenGrant: &mitev1.TokenGrant{
				MiteJenkinsToken:    "fresher-token",
				MiteJenkinsTokenExp: fresherExp,
			},
		},
	})
	if _, err := busConn.PublishJetStream(bus.MiteOutSubject(bus.DefaultCluster, ns, name), grantData3); err != nil {
		t.Fatalf("publish fresher grant: %v", err)
	}

	hasStale := false
	hasFresher := false
	deadline2 := time.Now().Add(5 * time.Second)
	for !hasStale || !hasFresher {
		if time.Now().After(deadline2) {
			t.Fatal("timed out waiting for all token grants")
		}
		select {
		case g := <-grants:
			if g.MiteJenkinsToken == "same-expiry-token" && g.MiteJenkinsTokenExp == fixedExp {
				hasStale = true
			}
			if g.MiteJenkinsToken == "fresher-token" && g.MiteJenkinsTokenExp > fixedExp {
				hasFresher = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for token grant")
		}
	}

	if !hasStale {
		t.Error("stale grant (equal expiry) was not forwarded to mite stream")
	}
	if !hasFresher {
		t.Error("fresher grant was not forwarded to mite stream")
	}
}
