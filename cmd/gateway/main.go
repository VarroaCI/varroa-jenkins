// Command gateway is the Varroa Connection Gateway. It terminates mTLS/gRPC
// connections from mite sidecars and bridges each CommandStream to the NATS
// bus. It does not reconcile controllers — that is the operator's job.
//
// The gateway loads the shared CA from the varroa-ca Secret (created by the
// operator), connects to NATS, and starts the mite gRPC server. Inbound mite
// messages are published to mite.<ns>.<name>.in; operator messages published
// to mite.<ns>.<name>.out are forwarded to the live stream.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/apikey"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/logging"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	busURL := flag.String("bus-url", "", "NATS bus URL (defaults to nats://localhost:4222)")
	busUser := flag.String("bus-user", "gateway", "NATS bus username")
	busPassFile := flag.String("bus-pass-file", "", "Path to NATS bus password file (overrides BUS_PASSWORD env)")
	busCAFile := flag.String("bus-ca-file", "", "Path to NATS bus CA certificate file")
	grpcPort := flag.Int("grpc-port", 9090, "gRPC listen port for mite connections")
	verifyPort := flag.Int("verify-port", 9092, "HTTP listen port for apikey verify endpoint")
	gatewayEndpoint := flag.String("gateway-endpoint", "", "Gateway endpoint returned to mites on Register (e.g. gateway.varroa.svc.cluster.local:9090)")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	logFormat := flag.String("log-format", "text", "Log format: text, json")
	flag.Parse()

	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: %v\n", *logLevel, err)
		os.Exit(1)
	}
	logger := logging.New(level, *logFormat, os.Stderr).With("binary", "gateway")
	slog.SetDefault(logger)

	cluster, err := bus.ClusterFromEnv()
	if err != nil {
		logger.Error("invalid cluster name", "error", err)
		os.Exit(1)
	}
	logger.Info("cluster identity", "cluster", cluster)

	_ = os.Setenv("VARROA_SERVICE_NAME", "varroa-gateway")
	telemetryShutdown := telemetry.InitTelemetry(context.Background())
	if !telemetry.Disabled() {
		logger = telemetry.LogHandler(logger)
		slog.SetDefault(logger)
	}
	defer func() {
		if err := telemetryShutdown(context.Background()); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}()

	// --- Bus connection ---
	if *busURL == "" {
		if u := os.Getenv("VARROA_BUS_URL"); u != "" {
			*busURL = u
		}
	}

	// Resolve bus credentials.
	busPassword := os.Getenv("BUS_PASSWORD")
	if *busPassFile != "" {
		passBytes, err := os.ReadFile(*busPassFile)
		if err != nil {
			logger.Error("failed to read bus password file", "path", *busPassFile, "error", err)
			os.Exit(1)
		}
		busPassword = strings.TrimSpace(string(passBytes))
	}

	busConn, err := bus.Connect(*busURL, bus.Config{
		Username:    *busUser,
		Password:    busPassword,
		CAFile:      *busCAFile,
		InboxPrefix: "_INBOX_gateway",
	})
	if err != nil {
		logger.Error("failed to connect to bus", "error", err)
		os.Exit(1)
	}
	defer busConn.Close()
	// JetStream replica count for KV buckets and the imperative command stream
	// (from the NATS cluster size, clamped 1..3 by the chart). Hive-mode
	// gateways read the core's replica count via this env. Values < 1 are clamped
	// to 1 by SetReplicas.
	busConn.SetReplicas(jetStreamReplicasFromEnv())
	logger.Info("connected to NATS bus", "url", busConn.NATSConn().ConnectedUrl(), "jetStreamReplicas", busConn.Replicas())

	// --- CA: load from varroa-ca Secret (created by the operator). ---
	// The gateway verifies mite client certs and issues new mite certs, so it
	// needs the SAME CA as the operator. The operator creates the varroa-ca
	// Secret at startup; on a fresh install the gateway can race ahead of it, so
	// we WAIT for the Secret rather than mint an ephemeral CA. An ephemeral CA
	// would silently split-brain the control plane — every mite's mTLS then
	// fails against the operator-signed identity until a manual gateway restart,
	// which is exactly what blocks hive-cluster recovery after a reinstall.
	operatorNamespace := controller.ResolveOperatorNamespace(logger)

	var certAuth *ca.CA
	if k8sClient := newK8sClientset(); k8sClient != nil {
		var err error
		certAuth, err = waitForCASecret(context.Background(), k8sClient, operatorNamespace,
			caSecretWaitTimeout, caSecretPollInterval, logger)
		if err != nil {
			logger.Error("failed to load CA from Secret", "error", err)
			os.Exit(1)
		}
		if certAuth != nil {
			logger.Info("loaded existing CA from Secret", "namespace", operatorNamespace)
		}
	}
	if certAuth == nil {
		// No Secret after waiting. Refuse to run with an ephemeral CA by default
		// (it would break every mite's mTLS); allow an explicit opt-in for local
		// dev / tests where the gateway runs without an operator.
		if os.Getenv("VARROA_ALLOW_EPHEMERAL_CA") != "true" {
			logger.Error("varroa-ca Secret not found after waiting; refusing to start with an "+
				"ephemeral CA (would split-brain the CA and break mite mTLS). Set "+
				"VARROA_ALLOW_EPHEMERAL_CA=true to override for local/dev without an operator.",
				"namespace", operatorNamespace, "waited", caSecretWaitTimeout.String())
			os.Exit(1)
		}
		var err error
		certAuth, err = ca.NewCA()
		if err != nil {
			logger.Error("failed to create CA", "error", err)
			os.Exit(1)
		}
		logger.Warn("using ephemeral CA (VARROA_ALLOW_EPHEMERAL_CA=true; no varroa-ca Secret found)")
	}

	// --- Token signer (shared with operator via CA private key) ---
	tokenSigner := mitesrv.NewTokenSigner(certAuth.BootstrapHMACKey())

	// --- gRPC server ---
	endpoint := *gatewayEndpoint
	if endpoint == "" {
		if ep := os.Getenv("VARROA_GATEWAY_ENDPOINT"); ep != "" {
			endpoint = ep
		}
	}

	// Include the gateway endpoint hostname as a SAN so that mite TLS clients
	// can verify the server certificate against the advertised DNS name.
	var extraSANs []string
	if endpoint != "" {
		host, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			logger.Warn("gateway SplitHostPort failed, using full endpoint as SAN", "error", err, "endpoint", endpoint)
			host = endpoint
		}
		if host != "" {
			extraSANs = append(extraSANs, host)
		}
	}

	srv := mitesrv.NewServer(certAuth, tokenSigner, extraSANs...)
	srv.ServerEndpoint = endpoint

	// --- Bus handler ---
	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 5*time.Minute)
	if err != nil {
		logger.Error("failed to create snapshot KV", "error", err)
		os.Exit(1)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		logger.Error("failed to create presence KV", "error", err)
		os.Exit(1)
	}
	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		logger.Error("failed to create desired KV", "error", err)
		os.Exit(1)
	}
	// Cross-replica bootstrap-token single-use. Bucket TTL must be >= the 15m
	// token TTL (minted in controller_controller.go) so consumed entries outlive
	// their token before evicting. EnsureKV does not retro-update the TTL of a
	// bucket created by an earlier deployment (same caveat as the buckets above).
	// Unlike the buckets above this one is non-fatal: without it the server
	// falls back to its per-replica in-memory store.
	consumedKV, err := busConn.EnsureKV(bus.KVConsumedTokensBucket, 16*time.Minute)
	if err != nil {
		logger.Warn("failed to create consumed-token KV; cross-replica bootstrap single-use disabled, per-replica in-memory store in effect", "error", err)
	} else {
		srv.SetConsumedTokenStore(mitesrv.NewKVConsumedStore(consumedKV, logger))
	}
	// --- Activity publisher (mode-agnostic, publishes to activity.* subjects) ---
	actPub := activity.NewPublisher(cluster, busConn)

	handler := mitesrv.NewBusHandler(cluster, busConn, snapshotKV, presenceKV, desiredKV, actPub)
	srv.SetStreamHandler(handler)
	defer handler.Close()
	logger.Info("gateway using bus transport for mite streams", "endpoint", endpoint)

	// --- Start gateway ---
	port := flagOrEnv("VARROA_GATEWAY_PORT", fmt.Sprintf("%d", *grpcPort))
	listener, err := net.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		logger.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	// Separate metrics listener on the next port (gRPC port + 1)
	if !telemetry.Disabled() {
		metricsPort := *grpcPort + 1
		metricsListener, err := net.Listen("tcp", fmt.Sprintf(":%d", metricsPort))
		if err != nil {
			logger.Warn("failed to start metrics listener", "port", metricsPort, "error", err)
		} else {
			metricsMux := http.NewServeMux()
			metricsMux.Handle("/metrics", telemetry.MetricsAuthMiddleware(promhttp.Handler()))
			metricsMux.Handle("/healthz", telemetry.HealthzHandler())
			go func() {
				_ = http.Serve(metricsListener, metricsMux)
			}()
		}
	}

	// --- Verify endpoint ---
	var verifyServer *http.Server
	verifyPortStr := flagOrEnv("VARROA_VERIFY_PORT", fmt.Sprintf("%d", *verifyPort))
	kclient, err := controller.NewClientsetClient()
	if err != nil {
		logger.Warn("verify endpoint disabled: failed to create clientset", "error", err)
	} else {
		verifier := apikey.NewVerifier(gatewayKeyStore{kclient, kclient}, operatorNamespace)
		verifyMux := http.NewServeMux()
		verifyMux.Handle("/v1/verify-apikey", apikey.VerifyHandler(verifier, logger))

		var verifySANs []string
		if endpoint != "" {
			host, _, err := net.SplitHostPort(endpoint)
			if err != nil {
				logger.Warn("SplitHostPort failed for verify SAN", "error", err, "endpoint", endpoint)
				host = endpoint
			}
			if host != "" {
				verifySANs = append(verifySANs, host)
			}
		}
		verifySANs = append(verifySANs, "localhost")
		verifyCert, err := certAuth.IssueServerCert(verifySANs...)
		if err != nil {
			logger.Warn("verify endpoint disabled: failed to issue cert", "error", err)
		} else {
			verifyListener, err := net.Listen("tcp", fmt.Sprintf(":%s", verifyPortStr))
			if err != nil {
				logger.Warn("verify endpoint disabled: failed to listen", "port", verifyPortStr, "error", err)
			} else {
				tlsConfig := &tls.Config{
					Certificates: []tls.Certificate{verifyCert},
					MinVersion:   tls.VersionTLS13,
				}
				verifyServer = &http.Server{
					Handler:   verifyMux,
					TLSConfig: tlsConfig,
				}
				tlsListener := tls.NewListener(verifyListener, tlsConfig)
				go func() {
					logger.Info("verify endpoint listening", "port", verifyPortStr)
					if err := verifyServer.Serve(tlsListener); err != nil {
						logger.Warn("verify endpoint serve error", "error", err)
					}
				}()
			}
		}
	}

	go func() {
		logger.Info("gateway gRPC server listening", "addr", listener.Addr())
		if err := srv.GRPCServer().Serve(listener); err != nil {
			logger.Error("gRPC server error", "error", err)
			os.Exit(1)
		}
	}()
	_ = telemetryShutdown // ensure no unused warning

	// --- Wait for shutdown ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("gateway shutting down...")
	if verifyServer != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := verifyServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("verify endpoint shutdown error", "error", err)
		}
		cancel()
	}
	srv.GRPCServer().GracefulStop()
}

// flagOrEnv returns the env var value if set, otherwise the default.
func flagOrEnv(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// jetStreamReplicasFromEnv reads VARROA_JETSTREAM_REPLICAS, defaulting to 1
// when unset or unparseable. Values < 1 are treated as 1 (SetReplicas clamps).
func jetStreamReplicasFromEnv() int {
	v := os.Getenv("VARROA_JETSTREAM_REPLICAS")
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 1
	}
	return n
}

// newK8sClientset creates a Kubernetes clientset from in-cluster config or
// kubeconfig. Returns nil if no cluster is reachable (development mode).
const (
	caSecretName         = "varroa-ca"
	caSecretWaitTimeout  = 3 * time.Minute
	caSecretPollInterval = 3 * time.Second
)

// waitForCASecret polls for the operator-created varroa-ca Secret and loads the
// shared internal CA from it. On a fresh install the operator creates this
// Secret at startup and the gateway may start first, so we wait for it instead
// of minting an ephemeral CA (which would split-brain the CA and break every
// mite's mTLS until a manual gateway restart). Returns (nil, nil) if the Secret
// never becomes available within timeout, leaving the fallback decision to the
// caller.
func waitForCASecret(ctx context.Context, k8s kubernetes.Interface, namespace string, timeout, poll time.Duration, logger *slog.Logger) (*ca.CA, error) {
	deadline := time.Now().Add(timeout)
	for {
		secret, err := k8s.CoreV1().Secrets(namespace).Get(ctx, caSecretName, metav1.GetOptions{})
		if err == nil && len(secret.Data["tls.crt"]) > 0 && len(secret.Data["tls.key"]) > 0 {
			return ca.LoadCA(secret.Data["tls.crt"], secret.Data["tls.key"])
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		logger.Info("waiting for varroa-ca Secret (created by operator)",
			"namespace", namespace, "secret", caSecretName)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func newK8sClientset() *kubernetes.Clientset {
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", "")
		if err != nil {
			return nil
		}
	}
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil
	}
	return cs
}

// gatewayKeyStore adapts *ClientsetClient + crdstore.Backend to apikey.keyStore.
type gatewayKeyStore struct {
	client *controller.ClientsetClient
	store  crdstore.Backend
}

func (s gatewayKeyStore) GetUserCRD(ctx context.Context, name, namespace string) (*v1alpha1.User, error) {
	return crdstore.Get[v1alpha1.User](ctx, s.store, name, namespace)
}
func (s gatewayKeyStore) GetSecret(ctx context.Context, name, namespace string) (map[string][]byte, error) {
	return s.client.GetSecret(ctx, name, namespace)
}
func (s gatewayKeyStore) CreateSecretExclusive(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error {
	return s.client.CreateSecretExclusive(ctx, name, namespace, labels, data)
}
func (s gatewayKeyStore) PatchSecretData(ctx context.Context, name, namespace string, data map[string][]byte) error {
	return s.client.PatchSecretData(ctx, name, namespace, data)
}
func (s gatewayKeyStore) DeleteSecret(ctx context.Context, name, namespace string) error {
	return s.client.DeleteSecret(ctx, name, namespace)
}
func (s gatewayKeyStore) ListSecrets(ctx context.Context, namespace, labelSelector string) ([]map[string][]byte, error) {
	return s.client.ListSecrets(ctx, namespace, labelSelector)
}
func (s gatewayKeyStore) ListGroupCRDs(ctx context.Context) ([]*v1alpha1.Group, error) {
	return crdstore.List[v1alpha1.Group](ctx, s.store, "", "")
}
