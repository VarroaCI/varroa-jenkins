// Command fakemite is a synthetic load generator for Varroa. It creates N
// fake mite sidecars, each opening a real mTLS CommandStream to the gateway
// and emitting heartbeats + state snapshots on the production cadence (15 s).
// Use it to:
//
//   - Measure per-tier CPU/memory under load
//   - Profile etcd write QPS vs. controller count
//   - Test mTLS handshake storms (gateway restart)
//   - Validate shard rebalancing (kill operator replicas)
//
// Two modes:
//
//	Self-contained (default): starts embedded NATS + gateway, generates
//	  ephemeral CA, and runs N fake mites against it. Good for quick profiling.
//
//	External: --gateway ADDR connects to a real gateway tier. Requires
//	  --ca-cert/--ca-key matching the gateway's CA.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	"github.com/varroaci/varroa-jenkins/internal/logging"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func main() {
	count := flag.Int("count", 50, "Number of fake mites to create")
	gatewayAddr := flag.String("gateway", "", "Gateway gRPC address (empty = self-contained mode)")
	rampRate := flag.Int("ramp", 10, "Mites per second to create during ramp-up")
	duration := flag.Duration("duration", 5*time.Minute, "How long to run")
	selfContained := flag.Bool("self-contained", false, "Run embedded NATS + gateway + mites")
	busUser := flag.String("bus-user", "fakemite", "NATS bus username (test-only identity)")
	busPassFile := flag.String("bus-pass-file", "", "Path to NATS bus password file (overrides BUS_PASSWORD env)")
	busCAFile := flag.String("bus-ca-file", "", "Path to NATS bus CA certificate file")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	logFormat := flag.String("log-format", "text", "Log format: text, json")
	flag.Parse()

	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: %v\n", *logLevel, err)
		os.Exit(1)
	}
	logger := logging.New(level, *logFormat, os.Stderr).With("binary", "fakemite")
	slog.SetDefault(logger)

	cluster, err := bus.ClusterFromEnv()
	if err != nil {
		logger.Error("invalid cluster name", "error", err)
		os.Exit(1)
	}
	logger.Info("cluster identity", "cluster", cluster)

	// Resolve bus credentials for self-contained mode.
	busPassword := os.Getenv("BUS_PASSWORD")
	if *busPassFile != "" {
		passBytes, err := os.ReadFile(*busPassFile)
		if err != nil {
			logger.Error("failed to read bus password file", "path", *busPassFile, "error", err)
			os.Exit(1)
		}
		busPassword = strings.TrimSpace(string(passBytes))
	}

	if *selfContained || *gatewayAddr == "" {
		runSelfContained(*count, *rampRate, *duration, cluster, *busUser, busPassword, *busCAFile)
		return
	}
	runExternal(*gatewayAddr, *count, *rampRate, *duration)
}

// --- Self-contained mode ---

func runSelfContained(N int, rampRate int, dur time.Duration, cluster, busUser, busPassword, busCAFile string) {
	slog.Default().Info("=== fakemite self-contained mode", "mites", N, "ramp_rate", rampRate, "duration", dur)

	// Start embedded NATS.
	natsSrv := startEmbeddedNATS()
	defer natsSrv.Shutdown()

	// Create bus connection with optional credentials.
	busConn, err := bus.Connect(natsSrv.ClientURL(), bus.Config{
		Username:    busUser,
		Password:    busPassword,
		CAFile:      busCAFile,
		InboxPrefix: "_INBOX_fakemite",
	})
	if err != nil {
		slog.Default().Error("bus", "error", err)
		os.Exit(1)
	}
	defer busConn.Close()

	// Create CA and token signer.
	certAuth, err := ca.NewCA()
	if err != nil {
		slog.Default().Error("ca", "error", err)
		os.Exit(1)
	}
	tokenSigner := mitesrv.NewTokenSigner([]byte(certAuth.PrivateKey()))

	// Start gateway.
	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 5*time.Minute)
	if err != nil {
		slog.Default().Error("kv", "error", err)
		os.Exit(1)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		slog.Default().Error("presence kv", "error", err)
		os.Exit(1)
	}
	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		slog.Default().Error("desired kv", "error", err)
		os.Exit(1)
	}
	srv := mitesrv.NewServer(certAuth, tokenSigner, "localhost")
	srv.ServerEndpoint = "127.0.0.1:0"
	srv.SetStreamHandler(mitesrv.NewBusHandler(cluster, busConn, snapshotKV, presenceKV, desiredKV, nil))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		slog.Default().Error("listen", "error", err)
		os.Exit(1)
	}
	go func() { _ = srv.GRPCServer().Serve(lis) }()
	defer srv.GRPCServer().GracefulStop()
	slog.Default().Info("gateway listening", "address", lis.Addr().String())

	// Run the mite generator.
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	runMiteGenerator(ctx, lis.Addr().String(), certAuth, N, rampRate)
}

func startEmbeddedNATS() *server.Server {
	opts := &server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  os.TempDir(),
	}
	s, err := server.NewServer(opts)
	if err != nil {
		slog.Default().Error("nats", "error", err)
		os.Exit(1)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		slog.Default().Error("nats not ready")
		os.Exit(1)
	}
	return s
}

// --- External mode ---

func runExternal(gatewayAddr string, N int, rampRate int, dur time.Duration) {
	slog.Default().Info("=== fakemite external mode", "gateway", gatewayAddr, "mites", N, "ramp_rate", rampRate)

	// Use ephemeral CA for client certs (must match gateway's CA).
	certAuth, err := ca.NewCA()
	if err != nil {
		slog.Default().Error("ca", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	runMiteGenerator(ctx, gatewayAddr, certAuth, N, rampRate)
}

// --- Mite generator ---

var (
	mitesCreated  atomic.Int64
	mitesActive   atomic.Int64
	messagesSent  atomic.Int64
	messagesRecv  atomic.Int64
	connectErrors atomic.Int64
)

func runMiteGenerator(ctx context.Context, addr string, certAuth *ca.CA, N int, rampRate int) {
	rampInterval := time.Second / time.Duration(rampRate)
	if rampInterval < time.Millisecond {
		rampInterval = time.Millisecond
	}

	var wg sync.WaitGroup
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Progress reporter.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				slog.Default().Info("metrics", "created", mitesCreated.Load(), "active", mitesActive.Load(), "sent", messagesSent.Load(), "recv", messagesRecv.Load(), "errors", connectErrors.Load())
			}
		}
	}()

	// Ramp mites.
loop:
	for i := 0; i < N; i++ {
		select {
		case <-ctx.Done():
			break loop
		case <-sigCh:
			break loop
		default:
		}

		name := fmt.Sprintf("ctrl-%d", i)
		ns := fmt.Sprintf("ns-%d", i%10) // distribute across 10 namespaces

		wg.Add(1)
		go func() {
			defer wg.Done()
			runFakeMite(ctx, addr, certAuth, name, ns)
		}()

		mitesCreated.Add(1)
		time.Sleep(rampInterval)
	}

	slog.Default().Info("ramp complete", "mites_created", N)

	// Wait for shutdown.
	select {
	case <-ctx.Done():
	case <-sigCh:
	}
	slog.Default().Info("shutting down mites...")
	wg.Wait()
	slog.Default().Info("final", "created", mitesCreated.Load(), "sent", messagesSent.Load(), "recv", messagesRecv.Load(), "errors", connectErrors.Load())
}

func runFakeMite(ctx context.Context, addr string, certAuth *ca.CA, name, ns string) {
	// Generate keypair and get client cert.
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		connectErrors.Add(1)
		return
	}

	cert, err := certAuth.IssueMiteCert(name, ns, pub)
	if err != nil {
		connectErrors.Add(1)
		return
	}

	// TLS config with client cert.
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

	gconn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		connectErrors.Add(1)
		return
	}
	defer func() { _ = gconn.Close() }()

	client := mitev1.NewMiteClient(gconn)

	// Open CommandStream.
	stream, err := client.CommandStream(ctx)
	if err != nil {
		connectErrors.Add(1)
		return
	}

	// Send hello.
	err = stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Hello{
			Hello: &mitev1.Hello{ControllerName: name, Namespace: ns, Version: "fakemite-0.1"},
		},
	})
	if err != nil {
		connectErrors.Add(1)
		return
	}

	mitesActive.Add(1)
	defer mitesActive.Add(-1)

	var sendMu sync.Mutex
	send := func(msg *mitev1.MiteMessage) {
		sendMu.Lock()
		defer sendMu.Unlock()
		if err := stream.Send(msg); err != nil {
			slog.Default().Warn("send failed", "name", name, "ns", ns, "error", err)
			return
		}
		messagesSent.Add(1)
	}

	// Heartbeat + snapshot ticker.
	heartbeatTick := time.NewTicker(15 * time.Second)
	defer heartbeatTick.Stop()

	// Recv goroutine for operator messages.
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			messagesRecv.Add(1)
			// ACK desired state commands.
			if ds, ok := msg.Message.(*mitev1.OperatorMessage_DesiredState); ok {
				send(&mitev1.MiteMessage{
					Message: &mitev1.MiteMessage_CommandResult{
						CommandResult: &mitev1.CommandResult{
							CommandId: ds.DesiredState.CommandId,
							Success:   true,
						},
					},
				})
			}
		}
	}()

	// Main loop: send periodic heartbeats + snapshots.
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTick.C:
			// Send heartbeat.
			send(&mitev1.MiteMessage{
				Message: &mitev1.MiteMessage_Heartbeat{
					Heartbeat: &mitev1.Heartbeat{
						Version:         "fakemite-0.1",
						ActualStateHash: randomHash(),
					},
				},
			})

			// Send snapshot.
			send(&mitev1.MiteMessage{
				Message: &mitev1.MiteMessage_StateSnapshot{
					StateSnapshot: &mitev1.StateSnapshot{
						JenkinsVersion:  "2.479",
						JenkinsHealth:   "healthy",
						ConfigHash:      randomHash(),
						PluginsHash:     randomHash(),
						RbacHash:        randomHash(),
						ActualStateHash: randomHash(),
					},
				},
			})
		}
	}
}

func randomHash() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
