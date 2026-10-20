package mite

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// dialWithMiteCertCellInstance mirrors dialWithMiteCert but issues a client
// certificate carrying the given cell instance in its URI SAN, so a test can
// exercise a gateway replica that reads the value from the peer certificate
// instead of its in-process cache.
func dialWithMiteCertCellInstance(t *testing.T, addr string, certAuth *ca.CA, name, ns, cellInstance string) *grpc.ClientConn {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cert, err := certAuth.IssueMiteCert(name, ns, pub, cellInstance)
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

// TestGatewayIntegration_CommandStreamReadsCellInstanceFromCert exercises
// ruling (n): the gateway holds no cell_instance for this controller from
// any prior Register call (the BusHandler's in-process map starts empty),
// so the value the first mapped envelope carries must come from the peer
// certificate's URI SAN, read at CommandStream open.
func TestGatewayIntegration_CommandStreamReadsCellInstanceFromCert(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	if err := busConn.EnsureStream(bus.BuildsStreamConfig(bus.BuildsStreamName)); err != nil {
		t.Fatalf("ensure builds stream: %v", err)
	}

	name, ns := "certctl", "certns"
	const wantCellInstance = "pod-uid-from-cert"

	var received []byte
	envCh := make(chan []byte, 4)
	sub, err := busConn.NATSConn().Subscribe(bus.BuildsWildcard, func(msg *nats.Msg) {
		envCh <- msg.Data
	})
	if err != nil {
		t.Fatalf("subscribe builds: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Note: no Register call happens on this connection, so the gateway's
	// same-replica cellInstances cache never learns wantCellInstance any
	// other way than reading it off this stream's peer certificate.
	gconn := dialWithMiteCertCellInstance(t, lis.Addr().String(), certAuth, name, ns, wantCellInstance)
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

	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_BuildEvent{
			BuildEvent: &mitev1.BuildEvent{
				Phase:           phaseStarted,
				RunId:           "run-cert-1",
				IntentKey:       "scm:certns/certctl:abc",
				JobPath:         "certns/certctl",
				BuildNumber:     1,
				Seq:             1,
				EmittedAtMillis: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send build event: %v", err)
	}

	select {
	case received = <-envCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the mapped envelope on the builds stream")
	}

	var env ingestEnvelope
	if err := json.Unmarshal(received, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.CellInstance != wantCellInstance {
		t.Fatalf("envelope.CellInstance = %q, want %q (from the peer certificate's URI SAN)",
			env.CellInstance, wantCellInstance)
	}
	if env.RunID != "run-cert-1" {
		t.Fatalf("envelope.RunID = %q, want run-cert-1", env.RunID)
	}
}

// TestGatewayIntegration_RegisterRenewalKeepsSameCellInstanceSAN exercises
// the ruling's renewal clause: a renewal that presents the same cell
// instance re-issues a certificate carrying the same URI SAN.
func TestGatewayIntegration_RegisterRenewalKeepsSameCellInstanceSAN(t *testing.T) {
	natsSrv := startNATS(t)
	_, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	addr := lis.Addr().String()
	name, ns := "renewctl", "renewns"
	const cellInstance = "pod-uid-renewal"

	// First registration: bootstrap token, no client cert yet.
	bootstrapTLS := &tls.Config{
		RootCAs:    certAuth.CertPool(),
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	}
	bootstrapConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(bootstrapTLS)))
	if err != nil {
		t.Fatalf("dial (bootstrap): %v", err)
	}
	t.Cleanup(func() { bootstrapConn.Close() })
	bootstrapClient := mitev1.NewMiteClient(bootstrapConn)

	tokenSigner := NewTokenSigner([]byte(certAuth.PrivateKey()))
	token, err := tokenSigner.GenerateToken(name, ns, 5*time.Minute)
	if err != nil {
		t.Fatalf("generate bootstrap token: %v", err)
	}

	pub1, priv1, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key 1: %v", err)
	}

	resp1, err := bootstrapClient.Register(context.Background(), &mitev1.RegisterRequest{
		ControllerName: name,
		Namespace:      ns,
		BootstrapToken: token,
		PublicKey:      []byte(pub1),
		CellInstance:   cellInstance,
	})
	if err != nil {
		t.Fatalf("register (bootstrap): %v", err)
	}
	cert1 := parseCertPEM(t, resp1.CertificatePem)
	if got := ca.CellInstanceFromCert(cert1); got != cellInstance {
		t.Fatalf("initial cert CellInstanceFromCert = %q, want %q", got, cellInstance)
	}

	// Second registration: renewal over mTLS using the just-issued cert.
	renewalTLS := &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{cert1.Raw},
			PrivateKey:  priv1,
			Leaf:        cert1,
		}},
		RootCAs:    certAuth.CertPool(),
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	}
	renewalConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(renewalTLS)))
	if err != nil {
		t.Fatalf("dial (renewal): %v", err)
	}
	t.Cleanup(func() { renewalConn.Close() })
	renewalClient := mitev1.NewMiteClient(renewalConn)

	pub2, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key 2: %v", err)
	}

	resp2, err := renewalClient.Register(context.Background(), &mitev1.RegisterRequest{
		ControllerName: name,
		Namespace:      ns,
		PublicKey:      []byte(pub2),
		CellInstance:   cellInstance,
	})
	if err != nil {
		t.Fatalf("register (renewal): %v", err)
	}
	cert2 := parseCertPEM(t, resp2.CertificatePem)
	if got := ca.CellInstanceFromCert(cert2); got != cellInstance {
		t.Fatalf("renewed cert CellInstanceFromCert = %q, want %q (same value as the initial cert)", got, cellInstance)
	}
}

// TestGatewayIntegration_DesiredStateResultCarriesCertCellInstance pins the
// instance stamp of a desired-state result: the gateway writes the cell
// instance from the stream certificate, and a value that the mite sent
// is replaced.
func TestGatewayIntegration_DesiredStateResultCarriesCertCellInstance(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())

	name, ns := "resultctl", "resultns"
	const wantCellInstance = "pod-uid-a"

	results := make(chan []byte, 8)
	sub, err := busConn.SubscribeData(bus.MiteInSubject(bus.DefaultCluster, ns, name), func(data []byte) {
		results <- data
	})
	if err != nil {
		t.Fatalf("subscribe mite inbound: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	gconn := dialWithMiteCertCellInstance(t, lis.Addr().String(), certAuth, name, ns, wantCellInstance)
	stream, err := mitev1.NewMiteClient(gconn).CommandStream(context.Background())
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
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_CommandResult{
			CommandResult: &mitev1.CommandResult{
				CommandId: "cmd-cert-1",
				Result: &mitev1.CommandResult_DesiredState{
					DesiredState: &mitev1.DesiredStateResult{
						AppliedHash:  "hash-cert",
						ItemsSuccess: true,
						CellInstance: "spoofed",
					},
				},
			},
		},
	}); err != nil {
		t.Fatalf("send command result: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case data := <-results:
			var mm mitev1.MiteMessage
			if err := json.Unmarshal(data, &mm); err != nil {
				t.Fatalf("unmarshal inbound message: %v", err)
			}
			ds := mm.GetCommandResult().GetDesiredState()
			if ds == nil {
				continue
			}
			if got := ds.CellInstance; got != wantCellInstance {
				t.Fatalf("DesiredStateResult.CellInstance = %q, want %q (from the stream certificate)", got, wantCellInstance)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the desired-state result on the mite inbound subject")
		}
	}
}

func parseCertPEM(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("pem.Decode: no block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate: %v", err)
	}
	return cert
}
