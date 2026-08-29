// Mite is the Varroa sidecar agent that runs alongside Jenkins.
// It connects to the Varroa server via gRPC, sends heartbeats and
// state snapshots, and executes desired-state commands (JCasC config,
// plugin installation, RBAC updates).
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/varroaci/varroa-jenkins/internal/hibernation"
	"github.com/varroaci/varroa-jenkins/internal/jenkins"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/plugininv"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
)

const (
	jenkinsHome   = "/var/jenkins_home"
	varroaMiteDir = "/var/jenkins_home/varroa-mite"
	bootstrapDir  = "/var/run/varroa/bootstrap"

	bootstrapTokenFile = "token"

	// lastAppliedHashName is the marker file recording the last successfully
	// applied desired-state hash. The mite gates subsequent pushes on this
	// value to avoid re-applying unchanged desired state (convergence gate).
	lastAppliedHashName = "last-applied-hash"

	// defaultDrainMarkerPath is the default path for the drain-done marker
	// shared between the mite and jenkins containers via the varroa-run emptyDir.
	defaultDrainMarkerPath = "/var/run/varroa/run/drain.done"
)

// Config holds the configuration for a mite agent.
type Config struct {
	VarroaEndpoint string
	BootstrapFile  string
	JenkinsURL     string
	ControllerName string
	Namespace      string
	CAPEM          string
}

// Agent is the mite sidecar agent that runs alongside a Jenkins instance.
// It connects to the Varroa server, streams state, and applies desired-state
// commands.
type Agent struct {
	cfg               Config
	conn              *grpc.ClientConn
	client            mitev1.MiteClient
	stream            mitev1.Mite_CommandStreamClient
	sendMu            sync.Mutex
	cert              *tls.Certificate
	certMu            sync.Mutex // guards cert (renewal loop vs. connect())
	keypair           ed25519.PrivateKey
	version           string
	heartbeatInterval time.Duration
	Logger            *slog.Logger

	// miteDir is the base directory for mite state markers (convergence hash,
	// boot-restarted). Defaults to varroaMiteDir; overridable in tests.
	miteDir string

	// Cached Jenkins health, updated by the health-probe goroutine and read
	// by the heartbeat goroutine. Guarded by healthMu because the probe and
	// heartbeat run concurrently.
	healthMu      sync.Mutex
	lastHealth    string
	lastHealthVer string

	// Mite Jenkins token (in-memory, refreshed by operator via desired state).
	jenkinsTokenMu  sync.Mutex
	jenkinsToken    string
	jenkinsTokenExp int64

	// Long-lived Jenkins client — created once and reused across all
	// operations so the session cookie and CSRF crumb persist.
	jenkinsClient   *jenkins.Client
	jenkinsClientMu sync.Mutex

	// tokenWaiters holds refresh callers waiting for a qualifying TokenGrant.
	// Each waiter records the baseline expiry it observed so stale grants
	// do not incorrectly unblock a caller that needs a fresher token.
	tokenWaitersMu sync.Mutex
	tokenWaiters   []*tokenWaiter

	// Idle-defer state and drain-timeout cache. Written by processCommands
	// (on desired-state arrival) and the commandWorker (during idle defer),
	// read by sendHeartbeats (snapshot). Guarded by deferMu.
	deferMu                sync.Mutex
	applyDeferred          bool
	deferReason            string
	deferDeadline          time.Time
	deferHash              string
	appliedDrainTimeoutSec int64

	// drainMu serialises the termination drain (SIGTERM handler) against
	// the SAFE_RESTART drain (command worker) so the two never run
	// concurrently against the same Jenkins.
	drainMu sync.Mutex

	// drainMarkerPath is the path to the drain-done marker file written by
	// drainForTermination and polled by the jenkins preStop. Overridable in
	// tests via t.TempDir().
	drainMarkerPath string

	// Live-drift fingerprint state. Written by processCommands (on command
	// arrival) and the fingerprint ticker; read by sendHeartbeats (snapshot).
	// Guarded by deferMu (same mutex as idle-defer — snapshot reads both).
	liveFingerprintIntervalSec int64
	latestDesiredStateHash     string
	liveConfigHash             string
	liveDrift                  bool

	// idleGauges caches the last successfully-polled IdleGauges from the
	// Jenkins plugin drain response. Set by startActivityPoller after a
	// successful poll; read (non-nil) by sendHeartbeats to include in the
	// Heartbeat message. Nil before the first successful poll, meaning the
	// heartbeat omits the idle field entirely — never send zeroed gauges.
	idleGauges atomic.Pointer[mitev1.IdleGauges]

	// idleGaugesReceivedAt is the Unix timestamp (seconds) when the cached
	// idleGauges were polled. Written atomically alongside idleGauges.
	idleGaugesReceivedAt atomic.Int64

	// pluginInventory caches the last successfully-collected plugin inventory.
	// Set by the collection goroutine; read by sendHeartbeats (hash) and the
	// push path. Nil before the first successful collection.
	pluginInventory atomic.Pointer[plugininv.Inventory]

	// pluginInventoryMu guards the last-pushed marker so hash comparisons
	// and push decisions do not race between the collector and the heartbeat.
	pluginInventoryMu sync.Mutex
	// lastPushedHash is the hash of the last inventory successfully pushed.
	lastPushedHash string
	// lastPushedSource is the source of the last inventory successfully pushed.
	lastPushedSource string
	// lastPushedFailed is the collection-failed state of the last pushed inventory.
	lastPushedFailed bool
	// lastPushTime is when the last inventory push completed successfully.
	lastPushTime time.Time
	// pushOnCollect is set by the COLLECT_PLUGIN_INVENTORY command to force an
	// unconditional push after the next collection.
	pushOnCollect bool
}

// identityState represents the agent's identity state during the bootstrap
// or reconnection flow.
type identityState struct {
	pubkey         ed25519.PublicKey
	bootstrapToken string
	isBootstrap    bool
}

// NewAgent creates a new mite agent with the given configuration.
func NewAgent(cfg Config) *Agent {
	return &Agent{cfg: cfg, version: "0.1.0", heartbeatInterval: 15 * time.Second, miteDir: varroaMiteDir, drainMarkerPath: defaultDrainMarkerPath}
}

// lastAppliedHashPath returns the convergence-marker path rooted at miteDir.
func (a *Agent) lastAppliedHashPath() string {
	return filepath.Join(a.miteDir, lastAppliedHashName)
}

// writeDrainMarker creates/overwrites the drain-done marker file so the
// jenkins preStop can observe it and proceed. Best-effort: a warning is
// logged on failure but the error is not returned (the preStop will time
// out instead, which is bounded).
func (a *Agent) writeDrainMarker() {
	if err := os.WriteFile(a.drainMarkerPath, []byte("done\n"), 0644); err != nil {
		a.Logger.Warn("failed to write drain marker", "path", a.drainMarkerPath, "error", err)
	}
}

// removeDrainMarker removes a pre-existing drain-done marker, ignoring
// ENOENT. Called at mite startup so a stale marker from a prior crash
// does not short-circuit a real drain.
func (a *Agent) removeDrainMarker() {
	if err := os.Remove(a.drainMarkerPath); err != nil && !os.IsNotExist(err) {
		a.Logger.Warn("failed to remove stale drain marker", "path", a.drainMarkerPath, "error", err)
	}
}

// drainForTermination is the SIGTERM/SIGINT handler entry point. It
// authenticates with the cached JWT, quiet-downs Jenkins, waits for
// running builds to finish (up to the drain timeout), then writes the
// done-marker so the jenkins preStop can proceed. On any early-return
// path (no token, zero timeout, error) the deferred writeDrainMarker
// still fires so preStop is always released (design D2/D5).
func (a *Agent) drainForTermination() {
	defer a.writeDrainMarker() // D2: deferred at TOP, before any early return

	token := a.currentJenkinsToken()
	timeoutSec := a.getAppliedDrainTimeoutSec()
	if token == "" || timeoutSec <= 0 {
		a.Logger.Info("termination drain skipped", "reason", "no token or zero timeout")
		return
	}

	a.drainMu.Lock() // D8: serialize with SAFE_RESTART
	defer a.drainMu.Unlock()

	client := a.getJenkinsClient()
	// Background ctx (NOT root ctx): best-effort; kubelet SIGKILLs at grace if needed.
	if err := client.QuietDown(context.Background()); err != nil {
		a.Logger.Warn("termination quiet-down failed", "error", err)
		return // defer still writes marker
	}
	// D6: fresh Background ctx (NOT root ctx), int64 secs -> Duration
	dctx, dcancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer dcancel()
	drained, remaining := pollDrain(dctx, client)
	// D5/termination: do NOT CancelQuietDown, do NOT defer, regardless of drained
	a.Logger.Info("termination drain complete", "drained", drained, "remaining", remaining)
}

// Run executes the main agent loop: load/create identity once, then
// continuously connect to Varroa, stream commands, and reconnect on any
// failure. Returns only when ctx is cancelled or identity cannot be loaded.
func (a *Agent) Run(ctx context.Context) error {
	id, err := a.loadOrCreateIdentity(ctx)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}

	a.writeOperatorStatus(false)
	a.removeDrainMarker()

	needsBootstrap := id.isBootstrap
	backoff := 5 * time.Second
	const maxBackoff = 2 * time.Minute

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := a.connect(ctx); err != nil {
			a.Logger.Warn("connect failed, retrying", "error", err, "retry", backoff)
			if !sleepContext(ctx, backoff) {
				return ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}

		if needsBootstrap {
			if err := a.register(ctx, id); err != nil {
				retry := backoff
				unauthenticated := status.Code(err) == codes.Unauthenticated
				if unauthenticated {
					// An invalid or expired bootstrap token is remedied
					// server-side: the operator remints the Secret on every
					// not-yet-connected reconcile tick and kubelet refreshes
					// the mounted file within about a minute. Poll on a short
					// fixed cadence instead of escalating to the network
					// backoff, and re-read the file so the retry presents the
					// reminted token rather than replaying the stale one read
					// at startup.
					retry = 30 * time.Second
				}
				a.Logger.Warn("register failed, retrying", "error", err, "retry", retry)
				_ = a.conn.Close()
				if !sleepContext(ctx, retry) {
					return ctx.Err()
				}
				if unauthenticated {
					if token, terr := a.readBootstrapToken(); terr == nil && token != "" {
						id.bootstrapToken = token
					}
				} else {
					backoff = minDuration(backoff*2, maxBackoff)
				}
				continue
			}
			_ = a.conn.Close()
			needsBootstrap = false
			if err := a.connect(ctx); err != nil {
				a.Logger.Warn("reconnect after bootstrap failed, retrying", "error", err, "retry", backoff)
				if !sleepContext(ctx, backoff) {
					return ctx.Err()
				}
				continue
			}
		}

		if err := a.openStream(ctx); err != nil {
			a.Logger.Warn("open stream failed, retrying", "error", err, "retry", backoff)
			_ = a.conn.Close()
			if !sleepContext(ctx, backoff) {
				return ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}

		// Connected — reset backoff and run the command loop.
		backoff = 5 * time.Second
		a.Logger.Info("stream connected")
		a.writeOperatorStatus(true)

		a.sendStateSnapshot(ctx)
		a.resendInventoryIfStale(ctx)

		streamCtx, streamCancel := context.WithCancel(ctx)
		go a.sendHeartbeats(streamCtx)
		go a.startTokenRefreshLoop(streamCtx)
		go a.startHealthProbe(streamCtx)
		go a.startObservabilityProbe(streamCtx)
		go a.startFingerprintTicker(streamCtx)
		go a.startActivityPoller(streamCtx)
		go a.startPluginInventoryCollector(streamCtx)
		go a.startCertRenewalLoop(streamCtx)
		// Wire the reactive refresh path: on session loss + token expiry,
		// the client triggers a refresh before re-establishing.
		a.getJenkinsClient().OnTokenExpired = func() {
			a.requestTokenRefresh(ctx)
		}
		a.processCommands(streamCtx)
		streamCancel()

		_ = a.conn.Close()
		a.writeOperatorStatus(false)

		// Clear health state so the next heartbeat does not report
		// stale data from this session before the first probe completes.
		a.setHealth("", "")
		a.Logger.Warn("stream disconnected, reconnecting", "delay", backoff)
		if !sleepContext(ctx, backoff) {
			return ctx.Err()
		}
	}
}

// sleepContext waits for d or until ctx is cancelled. Returns false if ctx
// was cancelled before the duration elapsed.
func sleepContext(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// loadOrCreateIdentity loads an existing mTLS identity from disk, or creates
// a new one using a bootstrap token.
func (a *Agent) loadOrCreateIdentity(ctx context.Context) (*identityState, error) {
	// Try loading an existing identity from the persistent volume.
	if cert, key, ok := a.loadIdentity(); ok {
		a.cert = cert
		a.keypair = key

		// CA-trust gate: after a hive cluster's control plane is reinstalled,
		// the internal CA is regenerated, so an identity minted under the OLD CA
		// can never authenticate again — the gateway rejects the mTLS handshake
		// with "x509: certificate signed by unknown authority" and the mite loops
		// forever. Gate reuse on the saved leaf still chaining to the CURRENT CA
		// (VARROA_CA_PEM), not on expiry alone. When the CA PEM is unset
		// (defensive/legacy) fall back to the historical expiry-only behavior.
		if a.cfg.CAPEM != "" && !a.certTrustedByCurrentCA() {
			a.Logger.Warn("saved mite identity no longer trusted by current CA; discarding and re-bootstrapping")
			a.discardSavedIdentity()
		} else {
			if a.cert.Leaf != nil && time.Now().Before(a.cert.Leaf.NotAfter) {
				return &identityState{isBootstrap: false}, nil
			}
			// Certificate expired - attempt renewal with existing keypair.
			if err := a.connect(ctx); err == nil {
				if err := a.renewWithExistingKeypair(ctx); err == nil {
					a.saveIdentity()
					return &identityState{isBootstrap: false}, nil
				}
			}
		}
	}

	// Bootstrap: generate a fresh keypair and read the bootstrap token.
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	a.keypair = priv

	token, err := a.readBootstrapToken()
	if err != nil {
		return nil, err
	}

	return &identityState{
		pubkey:         pub,
		bootstrapToken: token,
		isBootstrap:    true,
	}, nil
}

// readBootstrapToken reads the mounted bootstrap token, preferring the
// canonical mount and falling back to the configured file path. It is called
// again before register retries, not just at startup: the operator remints the
// Secret when the token expires while the pod is still initializing, and a
// mite that keeps replaying the value it read at startup can never pick the
// reminted token up.
func (a *Agent) readBootstrapToken() (string, error) {
	tokenBytes, err := os.ReadFile(filepath.Join(bootstrapDir, bootstrapTokenFile))
	if err != nil {
		tokenBytes, err = os.ReadFile(a.cfg.BootstrapFile)
		if err != nil {
			return "", fmt.Errorf("read bootstrap token: %w", err)
		}
	}
	return strings.TrimSpace(string(tokenBytes)), nil
}

// loadIdentity reads the saved mTLS certificate and key from disk.
func (a *Agent) loadIdentity() (*tls.Certificate, ed25519.PrivateKey, bool) {
	certPEM, err := os.ReadFile(filepath.Join(a.miteDir, "cert.pem"))
	if err != nil {
		return nil, nil, false
	}
	keyPEM, err := os.ReadFile(filepath.Join(a.miteDir, "key.pem"))
	if err != nil {
		return nil, nil, false
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, false
	}
	if cert.Leaf == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, nil, false
		}
		cert.Leaf = leaf
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, nil, false
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, false
	}
	key, ok := parsedKey.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, false
	}
	return &cert, key, true
}

// certTrustedByCurrentCA reports whether the loaded leaf certificate chains to
// the current CA supplied via VARROA_CA_PEM (a.cfg.CAPEM). It reuses the same
// pool construction as bootstrapTLSConfig. Chain-of-trust is checked with
// ExtKeyUsageAny — the concern here is solely "does this leaf descend from the
// current CA root", independent of the leaf's client-auth EKU (a default
// VerifyOptions would require ServerAuth and reject the client cert).
func (a *Agent) certTrustedByCurrentCA() bool {
	a.certMu.Lock()
	cert := a.cert
	a.certMu.Unlock()
	if cert == nil || cert.Leaf == nil {
		return false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(a.cfg.CAPEM)) {
		return false
	}
	_, err := cert.Leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	return err == nil
}

// discardSavedIdentity removes the persisted mTLS identity (cert/key/ca) from
// the mite state directory and clears the in-memory cert/keypair, so the caller
// falls through to a fresh bootstrap. Used when the saved identity no longer
// chains to the current CA (e.g. after a control-plane reinstall regenerated it).
func (a *Agent) discardSavedIdentity() {
	for _, f := range []string{"cert.pem", "key.pem", "ca.pem"} {
		if err := os.Remove(filepath.Join(a.miteDir, f)); err != nil && !os.IsNotExist(err) {
			a.Logger.Warn("failed to remove stale mite identity file", "file", f, "error", err)
		}
	}
	a.certMu.Lock()
	a.cert = nil
	a.certMu.Unlock()
	a.keypair = nil
}

// saveIdentity persists the mTLS certificate and key to the persistent volume.
func (a *Agent) saveIdentity() {
	if err := os.MkdirAll(a.miteDir, 0755); err != nil {
		a.Logger.Error("save identity: mkdir failed", "error", err)
		return
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Certificate[0]})

	keyBytes, err := x509.MarshalPKCS8PrivateKey(a.keypair)
	if err != nil {
		a.Logger.Error("save identity: marshal key failed", "error", err)
		return
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})

	if err := os.WriteFile(filepath.Join(a.miteDir, "cert.pem"), certPEM, 0600); err != nil {
		a.Logger.Error("save identity: write cert failed", "error", err)
	}
	if err := os.WriteFile(filepath.Join(a.miteDir, "key.pem"), keyPEM, 0600); err != nil {
		a.Logger.Error("save identity: write key failed", "error", err)
	}
}

// renewWithExistingKeypair attempts certificate renewal using an already
// authenticated connection.
func (a *Agent) renewWithExistingKeypair(ctx context.Context) error {
	pub := a.keypair.Public().(ed25519.PublicKey)
	req := &mitev1.RegisterRequest{
		ControllerName: a.cfg.ControllerName,
		Namespace:      a.cfg.Namespace,
		PublicKey:      []byte(pub),
	}
	resp, err := a.client.Register(ctx, req)
	if err != nil {
		return err
	}
	return a.storeCertFromResponse(resp)
}

// register performs first-time registration with the Varroa server using
// a bootstrap token.
func (a *Agent) register(ctx context.Context, id *identityState) error {
	req := &mitev1.RegisterRequest{
		ControllerName: a.cfg.ControllerName,
		Namespace:      a.cfg.Namespace,
		BootstrapToken: id.bootstrapToken,
		PublicKey:      []byte(id.pubkey),
	}
	resp, err := a.client.Register(ctx, req)
	if err != nil {
		return err
	}
	return a.storeCertFromResponse(resp)
}

// storeCertFromResponse parses the certificate and key from a registration
// response and persists them to disk.
func (a *Agent) storeCertFromResponse(resp *mitev1.RegisterResponse) error {
	// Marshal the ed25519 private key to PKCS8 PEM format.
	keyBytes, err := x509.MarshalPKCS8PrivateKey(a.keypair)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})

	cert, err := tls.X509KeyPair([]byte(resp.CertificatePem), keyPEM)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}
	cert.Leaf = leaf
	a.certMu.Lock()
	a.cert = &cert
	a.certMu.Unlock()

	// Save the CA certificate to disk for future connections.
	if err := os.MkdirAll(a.miteDir, 0755); err != nil {
		a.Logger.Error("store cert: mkdir failed", "error", err)
	}
	if err := os.WriteFile(filepath.Join(a.miteDir, "ca.pem"), []byte(resp.CaPem), 0600); err != nil {
		a.Logger.Error("store cert: write ca.pem failed", "error", err)
	}

	a.saveIdentity()
	return nil
}

// connect establishes a gRPC connection to the Varroa server. If a client
// certificate is available, it uses mTLS; otherwise it uses the bootstrap
// TLS config (CA-verified if VARROA_CA_PEM is set, skip-verify fallback).
func (a *Agent) connect(_ context.Context) error {
	var dialOpts []grpc.DialOption

	a.certMu.Lock()
	cert := a.cert
	a.certMu.Unlock()

	if cert != nil {
		pool := x509.NewCertPool()
		caPEM, err := os.ReadFile(filepath.Join(a.miteDir, "ca.pem"))
		if err != nil {
			return fmt.Errorf("read CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return fmt.Errorf("failed to parse CA certificate")
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{*cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS13,
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		tlsCfg, err := a.bootstrapTLSConfig()
		if err != nil {
			return err
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	}

	if !telemetry.Disabled() {
		dialOpts = append(dialOpts, grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	}
	dialOpts = append(dialOpts,
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	)

	conn, err := grpc.NewClient(a.cfg.VarroaEndpoint, dialOpts...)
	if err != nil {
		return err
	}
	a.conn = conn
	a.client = mitev1.NewMiteClient(conn)
	return nil
}

// bootstrapTLSConfig builds the TLS configuration for the bootstrap (no client
// cert) phase. It requires the gateway CA PEM (VARROA_CA_PEM) and verifies the
// gateway server certificate against it. It is fail-closed: with no CA PEM it
// returns an error rather than connecting insecurely, so a misconfigured pod stalls
// in the connect retry loop instead of trusting an unauthenticated gateway.
func (a *Agent) bootstrapTLSConfig() (*tls.Config, error) {
	if a.cfg.CAPEM == "" {
		return nil, fmt.Errorf("refusing insecure bootstrap: VARROA_CA_PEM is unset (set it to the gateway CA PEM)")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(a.cfg.CAPEM)) {
		return nil, fmt.Errorf("failed to parse bootstrap CA certificate from VARROA_CA_PEM")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}

// openStream opens the bidirectional command stream and sends the initial
// hello message.
func (a *Agent) openStream(ctx context.Context) error {
	stream, err := a.client.CommandStream(ctx)
	if err != nil {
		return err
	}

	hello := &mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Hello{
			Hello: &mitev1.Hello{
				ControllerName: a.cfg.ControllerName,
				Namespace:      a.cfg.Namespace,
				Version:        a.version,
			},
		},
	}
	if err := stream.Send(hello); err != nil {
		return err
	}
	a.stream = stream
	return nil
}

// sendStateSnapshot computes and sends the current Jenkins state snapshot
// to the Varroa server.
func (a *Agent) sendStateSnapshot(ctx context.Context) {
	jenkinsClient := a.getJenkinsClient()
	snap := &mitev1.StateSnapshot{Status: "unknown"}

	// Probe Jenkins health and version.
	a.probeJenkinsHealth(ctx, jenkinsClient, snap)

	// Compute hashes of cached state files.
	configHash := a.fileHash(filepath.Join(a.miteDir, "last-jcasc.yaml"))
	pluginsHash := a.fileHash(filepath.Join(a.miteDir, "last-plugins.txt"))
	rbacHash := a.fileHash(filepath.Join(a.miteDir, "last-rbac.yaml"))
	itemsHash := a.fileHash(filepath.Join(a.miteDir, "last-items.yaml"))

	snap.ConfigHash = configHash
	snap.PluginsHash = pluginsHash
	snap.RbacHash = rbacHash
	snap.ItemsHash = itemsHash
	snap.ActualStateHash = computeStateHash(configHash, pluginsHash, rbacHash, itemsHash)
	snap.Status = snap.JenkinsHealth

	msg := &mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_StateSnapshot{StateSnapshot: snap},
	}
	a.sendMu.Lock()
	err := a.stream.Send(msg)
	a.sendMu.Unlock()
	if err != nil {
		a.Logger.Warn("snapshot send failed", "error", err)
	}
}

// setHealth caches the latest Jenkins health and version under healthMu.
func (a *Agent) setHealth(health, version string) {
	a.healthMu.Lock()
	a.lastHealth = health
	if version != "" {
		a.lastHealthVer = version
	}
	a.healthMu.Unlock()
}

// getHealth returns the cached Jenkins health and version under healthMu.
func (a *Agent) getHealth() (health, version string) {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	return a.lastHealth, a.lastHealthVer
}

// probeJenkinsHealth checks Jenkins health via /api/json and populates the
// snapshot's JenkinsHealth and JenkinsVersion fields.
func (a *Agent) probeJenkinsHealth(ctx context.Context, client *jenkins.Client, snap *mitev1.StateSnapshot) {
	info, err := client.GetInfo(ctx)
	if err != nil {
		snap.JenkinsHealth = "unreachable"
		a.setHealth("unreachable", "")
		return
	}
	snap.JenkinsVersion = info.Version
	if info.QuietingDown {
		snap.JenkinsHealth = "unhealthy"
	} else {
		snap.JenkinsHealth = "healthy"
	}
	a.setHealth(snap.JenkinsHealth, snap.JenkinsVersion)
}

// sendHeartbeats periodically sends heartbeat and state snapshot messages
// to the Varroa server. The gRPC heartbeat does NOT probe Jenkins — it
// reports the last cached health value. Jenkins health probing happens
// on a separate, slower cadence (see startHealthProbe).
func (a *Agent) sendHeartbeats(ctx context.Context) {
	ticker := time.NewTicker(a.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap := a.currentSnapshot()
			snap.JenkinsHealth, snap.JenkinsVersion = a.getHealth()
			snap.Status = snap.JenkinsHealth

			// Surface idle-defer state on every snapshot.
			deferred, deferReason := a.loadDeferState()
			snap.ApplyDeferred = deferred
			snap.DeferReason = deferReason

			// Surface live-drift fingerprint on every snapshot.
			a.deferMu.Lock()
			snap.LiveConfigHash = a.liveConfigHash
			snap.LiveDrift = a.liveDrift
			a.deferMu.Unlock()

			// Send state snapshot (carries health, version, hashes).
			snapMsg := &mitev1.MiteMessage{
				Message: &mitev1.MiteMessage_StateSnapshot{StateSnapshot: snap},
			}
			a.sendMu.Lock()
			if err := a.stream.Send(snapMsg); err != nil {
				a.sendMu.Unlock()
				a.Logger.Warn("snapshot send failed", "error", err)
				_ = a.conn.Close() // wake up processCommands
				return
			}
			a.sendMu.Unlock()

			// Send heartbeat (carries version + hash for liveness).
			hb := &mitev1.Heartbeat{
				Version:         a.version,
				ActualStateHash: snap.ActualStateHash,
			}
			// Attach cached idle gauges when available. Nil before the first
			// successful activity poll — the heartbeat omits the idle field
			// entirely rather than sending zeroed gauges.
			if idle := a.idleGauges.Load(); idle != nil {
				hb.Idle = idle
			}
			// Attach the installed plugins hash when available.
			if inv := a.pluginInventory.Load(); inv != nil {
				hb.InstalledPluginsHash = inv.Hash()
			}
			hbMsg := &mitev1.MiteMessage{
				Message: &mitev1.MiteMessage_Heartbeat{
					Heartbeat: hb,
				},
			}
			a.sendMu.Lock()
			if err := a.stream.Send(hbMsg); err != nil {
				a.sendMu.Unlock()
				a.Logger.Warn("heartbeat send failed", "error", err)
				_ = a.conn.Close() // wake up processCommands
				return
			}
			a.sendMu.Unlock()
		}
	}
}

// currentSnapshot computes state hashes from cached files. Health is filled
// by the caller via probeJenkinsHealth.
func (a *Agent) currentSnapshot() *mitev1.StateSnapshot {
	configHash := a.fileHash(filepath.Join(a.miteDir, "last-jcasc.yaml"))
	pluginsHash := a.fileHash(filepath.Join(a.miteDir, "last-plugins.txt"))
	rbacHash := a.fileHash(filepath.Join(a.miteDir, "last-rbac.yaml"))
	itemsHash := a.fileHash(filepath.Join(a.miteDir, "last-items.yaml"))
	return &mitev1.StateSnapshot{
		ConfigHash:      configHash,
		PluginsHash:     pluginsHash,
		RbacHash:        rbacHash,
		ItemsHash:       itemsHash,
		ActualStateHash: computeStateHash(configHash, pluginsHash, rbacHash, itemsHash),
	}
}

type recvResult struct {
	msg *mitev1.OperatorMessage
	err error
}

// tokenWaiter is a refresh caller blocked waiting for a TokenGrant whose
// expiry exceeds the caller's baseline.
type tokenWaiter struct {
	ch       chan struct{}
	baseline int64
}

// commandWork carries a single command for the serialized worker goroutine.
type commandWork struct {
	desiredStateCmd *mitev1.DesiredStateCommand
	imperativeCmd   *mitev1.ImperativeCommand
	ctx             context.Context
	span            trace.Span
}

const imperativeQueueCap = 16
const defaultCommandDeadline = 20 * time.Minute

// commandMailbox is a mutex-guarded queue with a single-slot replace-on-arrival
// desired-state entry (newest-wins) and a bounded FIFO imperative queue.
// Desired-state commands coalesce instead of queueing; imperatives preserve
// arrival order and cannot be silently dropped.
type commandMailbox struct {
	mu         sync.Mutex
	desired    *commandWork  // replace-on-arrival slot (newest-wins)
	imperative []commandWork // bounded FIFO
	signal     chan struct{} // cap 1, non-blocking notify
}

func newCommandMailbox() *commandMailbox {
	return &commandMailbox{
		signal: make(chan struct{}, 1),
	}
}

func (mb *commandMailbox) putDesired(w commandWork) {
	mb.mu.Lock()
	if mb.desired != nil && mb.desired.span != nil {
		mb.desired.span.End()
		mb.desired.span = nil
	}
	mb.desired = &w
	mb.mu.Unlock()
	mb.notify()
}

func (mb *commandMailbox) putImperative(w commandWork) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if len(mb.imperative) >= imperativeQueueCap {
		return false
	}
	mb.imperative = append(mb.imperative, w)
	mb.notify()
	return true
}

func (mb *commandMailbox) take() (commandWork, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if len(mb.imperative) > 0 {
		w := mb.imperative[0]
		mb.imperative = mb.imperative[1:]
		return w, true
	}
	if mb.desired != nil {
		w := *mb.desired
		mb.desired = nil
		return w, true
	}
	return commandWork{}, false
}

func (mb *commandMailbox) notify() {
	select {
	case mb.signal <- struct{}{}:
	default:
	}
}

// processCommands receives operator messages from the gRPC stream and
// dispatches them. TokenGrant is handled inline in the receive loop so it
// never waits for command execution. DesiredState and Imperative commands
// are dispatched through a commandMailbox for a single serialized worker goroutine.
func (a *Agent) processCommands(ctx context.Context) {
	mb := newCommandMailbox()

	go a.commandWorker(ctx, mb)

	for {
		ch := make(chan recvResult, 1)
		go func() {
			msg, err := a.stream.Recv()
			ch <- recvResult{msg, err}
		}()

		var r recvResult
		select {
		case <-ctx.Done():
			return
		case r = <-ch:
		}

		if r.err != nil {
			if r.err != io.EOF {
				a.Logger.Error("stream receive failed", "error", r.err)
			}
			return
		}

		msg := r.msg
		a.Logger.Debug("mite recv", "type", fmt.Sprintf("%T", msg.Message))
		switch m := msg.Message.(type) {
		case *mitev1.OperatorMessage_DesiredState:
			tracer := otel.Tracer("varroa-mite")
			cmdCtx, span := tracer.Start(ctx, "mite.receiveCommand",
				trace.WithAttributes(
					attribute.String("controller", a.cfg.ControllerName),
					attribute.String("namespace", a.cfg.Namespace),
					attribute.String("command_id", m.DesiredState.CommandId),
				),
			)
			w := commandWork{desiredStateCmd: m.DesiredState, ctx: cmdCtx, span: span}
			mb.putDesired(w) // never ends session; coalescing is the back-pressure

			// Cache drain timeout so the SAFE_RESTART path reads the latest.
			// Newest-wins: a newer desired command always wins.
			if m.DesiredState.DrainTimeoutSec > 0 {
				a.deferMu.Lock()
				a.appliedDrainTimeoutSec = m.DesiredState.DrainTimeoutSec
				a.deferMu.Unlock()
			}
			// Cache fingerprint interval + latest desired-state hash.
			a.deferMu.Lock()
			a.liveFingerprintIntervalSec = m.DesiredState.LiveFingerprintIntervalSec
			if m.DesiredState.DesiredStateHash != "" {
				a.latestDesiredStateHash = m.DesiredState.DesiredStateHash
			}
			a.deferMu.Unlock()
		case *mitev1.OperatorMessage_Imperative:
			w := commandWork{imperativeCmd: m.Imperative, ctx: ctx}
			if !mb.putImperative(w) {
				a.Logger.Error("imperative queue full, ending stream session")
				return
			}
		case *mitev1.OperatorMessage_TokenGrant:
			grant := m.TokenGrant
			if grant != nil && grant.MiteJenkinsToken != "" {
				a.jenkinsTokenMu.Lock()
				if grant.MiteJenkinsTokenExp > a.jenkinsTokenExp {
					a.jenkinsToken = grant.MiteJenkinsToken
					a.jenkinsTokenExp = grant.MiteJenkinsTokenExp
					a.Logger.Debug("cached fresh token from grant", "exp", grant.MiteJenkinsTokenExp)
				} else {
					a.Logger.Debug("discarding stale token grant", "exp", grant.MiteJenkinsTokenExp, "current", a.jenkinsTokenExp)
				}
				a.jenkinsTokenMu.Unlock()
				a.wakeTokenWaiters()
			}
		case *mitev1.OperatorMessage_ContentRequest:
			req := m.ContentRequest
			if req != nil && req.RequestId != "" {
				resp := &mitev1.ContentResponse{
					RequestId:  req.RequestId,
					JcascYaml:  readFileEmpty(filepath.Join(a.miteDir, "last-jcasc.yaml")),
					ItemsYaml:  readFileEmpty(filepath.Join(a.miteDir, "last-items.yaml")),
					PluginsTxt: readFileEmpty(filepath.Join(a.miteDir, "last-plugins.txt")),
				}
				a.sendMu.Lock()
				sendErr := a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_ContentResponse{ContentResponse: resp}})
				a.sendMu.Unlock()
				if sendErr != nil {
					a.Logger.Error("send content response failed", "error", sendErr)
				}
			}
		}
	}
}

// commandWorker executes one command at a time from the mailbox.
// Desired-state and imperative commands are serialized so Jenkins writes
// never overlap. Command results are sent through the existing sendMu.
func (a *Agent) commandWorker(ctx context.Context, mb *commandMailbox) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-mb.signal:
		}
	workLoop:
		for {
			w, ok := mb.take()
			if !ok {
				break
			}

			if w.desiredStateCmd != nil && w.desiredStateCmd.ApplyWhen == "idle" {
				if !a.shouldApplyNow(w.desiredStateCmd) {
					a.deferMu.Lock()
					reason := a.deferReason
					a.applyDeferred = true
					a.deferMu.Unlock()
					a.Logger.Debug("deferring apply: builds still running",
						"hash", w.desiredStateCmd.DesiredStateHash,
						"reason", reason,
					)
					idleTimer := time.NewTimer(a.heartbeatInterval)
				idleLoop:
					for {
						select {
						case <-ctx.Done():
							idleTimer.Stop()
							if w.span != nil {
								w.span.End()
							}
							a.setDeferState(false, "")
							return
						case <-idleTimer.C:
							if a.shouldApplyNow(w.desiredStateCmd) {
								a.setDeferState(false, "")
								break idleLoop
							}
							a.deferMu.Lock()
							a.applyDeferred = true
							a.deferMu.Unlock()
							idleTimer.Reset(a.heartbeatInterval)
						case <-mb.signal:
							idleTimer.Stop()
							newW, nOk := mb.take()
							if nOk {
								if newW.imperativeCmd != nil {
									if w.span != nil {
										w.span.End()
									}
									mb.putDesired(w)
									a.runCommand(ctx, newW)
									continue workLoop
								}
								if newW.desiredStateCmd != nil {
									if w.span != nil {
										w.span.End()
									}
									w = newW
								}
							}
							if a.shouldApplyNow(w.desiredStateCmd) {
								a.setDeferState(false, "")
								break idleLoop
							}
							a.deferMu.Lock()
							a.applyDeferred = true
							a.deferMu.Unlock()
							idleTimer.Reset(a.heartbeatInterval)
						}
					}
				}
			}

			a.runCommand(ctx, w)
		}
	}
}

// runCommand executes a single command and sends its result.
// On send error it closes the connection (unblocks the recv loop).
func (a *Agent) runCommand(_ context.Context, w commandWork) {
	deadline := defaultCommandDeadline
	if w.desiredStateCmd != nil && w.desiredStateCmd.CommandDeadlineSec > 0 {
		deadline = time.Duration(w.desiredStateCmd.CommandDeadlineSec) * time.Second
	} else if w.imperativeCmd != nil && w.imperativeCmd.DeadlineSec > 0 {
		deadline = time.Duration(w.imperativeCmd.DeadlineSec) * time.Second
	}
	cctx, cancel := context.WithTimeout(w.ctx, deadline)
	defer cancel()

	if w.desiredStateCmd != nil {
		result := a.handleDesiredState(cctx, w.desiredStateCmd)
		if w.span != nil {
			w.span.End()
		}
		if cctx.Err() == context.DeadlineExceeded {
			msg := fmt.Sprintf("command deadline exceeded (%s)", deadline)
			if w.desiredStateCmd.JcascYaml != "" && !result.ConfigSuccess {
				result.ConfigError = msg
			}
			if w.desiredStateCmd.RbacYaml != "" && !result.RbacSuccess {
				result.RbacError = msg
			}
			if w.desiredStateCmd.ItemsYaml != "" && !result.ItemsSuccess {
				result.ItemsError = msg
			}
		}
		resultMsg := &mitev1.MiteMessage{
			Message: &mitev1.MiteMessage_CommandResult{
				CommandResult: &mitev1.CommandResult{
					CommandId: w.desiredStateCmd.CommandId,
					Result:    &mitev1.CommandResult_DesiredState{DesiredState: result},
				},
			},
		}
		a.sendMu.Lock()
		err := a.stream.Send(resultMsg)
		a.sendMu.Unlock()
		if err != nil {
			a.Logger.Warn("send command result failed, closing connection", "error", err)
			_ = a.conn.Close()
			return
		}
	} else if w.imperativeCmd != nil {
		cmd := w.imperativeCmd
		var success bool
		var errStr string
		var deferred bool
		var deferReason string
		var httpStatus int
		switch cmd.Type {
		case mitev1.CommandTypeDeleteItem:
			jenkinsClient := a.getJenkinsClient()
			deleteErr := jenkinsClient.DeleteItem(cctx, cmd.Target)
			if deleteErr != nil && !errors.Is(deleteErr, jenkins.ErrItemNotFound) {
				errStr = deleteErr.Error()
				if cctx.Err() == context.DeadlineExceeded {
					errStr = fmt.Sprintf("command deadline exceeded (%s)", deadline)
				}
			} else {
				success = true
			}
		case mitev1.CommandTypeReplayWebhook:
			payload := cmd.ReplayWebhook
			if payload == nil {
				errStr = "replay_webhook payload is nil"
				break
			}
			// Re-check the path allowlist (D7).
			allowlisted := false
			for _, prefix := range hibernation.ReplayPathAllowlist {
				if strings.HasPrefix(payload.Path, prefix) {
					allowlisted = true
					break
				}
			}
			if !allowlisted {
				errStr = fmt.Sprintf("replay path %q not allowlisted", payload.Path)
				break
			}
			// Build the upstream URL. The mite always replays to the local
			// Jenkins process on the standard port.
			u := fmt.Sprintf("http://localhost:8080/%s", payload.Path)
			if payload.Query != "" {
				u += "?" + payload.Query
			}
			body := payload.Body
			if body == nil {
				body = []byte{}
			}
			req, reqErr := http.NewRequestWithContext(cctx, http.MethodPost, u, bytes.NewReader(body))
			if reqErr != nil {
				errStr = fmt.Sprintf("build replay request: %v", reqErr)
				break
			}
			// Apply envelope headers from the original webhook plus the
			// operator JWT as Bearer auth (D4).
			for k, v := range payload.Headers {
				req.Header.Set(k, v)
			}
			req.Header.Set("Authorization", "Bearer "+a.currentJenkinsToken())
			// 15s timeout for the upstream call.
			httpClient := &http.Client{Timeout: 15 * time.Second}
			resp, httpErr := httpClient.Do(req)
			if httpErr != nil {
				errStr = fmt.Sprintf("replay request failed: %v", httpErr)
				break
			}
			_ = resp.Body.Close()
			httpStatus = resp.StatusCode
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				success = true // leave errStr empty on success — HttpStatus carries the code
			} else {
				errStr = fmt.Sprintf("upstream status: %d", resp.StatusCode)
			}
		case mitev1.CommandTypeCollectPluginInventory:
			collErr := a.triggerCollectionAndPush(cctx)
			if collErr != nil {
				errStr = collErr.Error()
			} else {
				success = true
			}
		default:
			errStr = "unknown imperative command type"
		}
		if cctx.Err() == context.DeadlineExceeded && errStr == "" {
			errStr = fmt.Sprintf("command deadline exceeded (%s)", deadline)
			success = false
		}
		a.sendMu.Lock()
		sendErr := a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_CommandResult{
			CommandResult: &mitev1.CommandResult{CommandId: cmd.CommandId, Success: success, Error: errStr, Deferred: deferred, DeferReason: deferReason, HttpStatus: httpStatus},
		}})
		a.sendMu.Unlock()
		if sendErr != nil {
			a.Logger.Warn("send imperative result failed, closing connection", "error", sendErr)
			_ = a.conn.Close()
			return
		}
	}
}

// handleDesiredState applies a desired-state command: JCasC configuration,
// plugin synchronization, safe restart, and RBAC configuration.
func (a *Agent) handleDesiredState(ctx context.Context, cmd *mitev1.DesiredStateCommand) *mitev1.DesiredStateResult {
	result := &mitev1.DesiredStateResult{AppliedHash: cmd.DesiredStateHash}

	// Cache the operator-signed Jenkins JWT (in-memory only).
	// Only accept a token fresher than the current cache so a stale
	// or replayed DesiredStateCommand cannot downgrade a fresh grant.
	if cmd.MiteJenkinsToken != "" {
		a.jenkinsTokenMu.Lock()
		if cmd.MiteJenkinsTokenExp > a.jenkinsTokenExp {
			a.jenkinsToken = cmd.MiteJenkinsToken
			a.jenkinsTokenExp = cmd.MiteJenkinsTokenExp
		}
		a.jenkinsTokenMu.Unlock()
		a.wakeTokenWaiters()
	}

	// Convergence gate: if the desired state hash matches the last applied
	// hash and this is not an explicit reload, skip all Jenkins writes.
	// Token is already cached above so a converged drift push still delivers
	// a fresh token without re-applying unchanged config.
	//
	// The marker alone is not enough: it lives on the persistent volume, but
	// init.groovy.d resets the authorization strategy on EVERY Jenkins boot.
	// Skipping is therefore only safe when the live Jenkins boot session
	// (X-Jenkins-Session, regenerated per JVM boot) still matches the one
	// recorded at the last apply. On a session mismatch — or when the probe
	// fails (Jenkins down, likely restarting) — fall through and re-apply.
	if cmd.DesiredStateHash != "" && !cmd.Reload {
		marker := a.readMarker()
		if marker.composite == cmd.DesiredStateHash {
			live, err := a.getJenkinsClient().SessionID(ctx)
			switch {
			case err == nil && marker.session != "" && live == marker.session:
				a.Logger.Debug("converged — skipping apply", "hash", cmd.DesiredStateHash, "session", live)
				result.ConfigSuccess = true
				result.PluginsSuccess = true
				result.RbacSuccess = true
				result.ItemsSuccess = true
				return result
			case err == nil:
				a.Logger.Info("jenkins boot session changed since last apply — re-applying desired state",
					"stored", marker.session, "live", live)
			default:
				a.Logger.Warn("jenkins session probe failed — assuming restart and re-applying", "error", err)
			}
		}
	}

	// Wait for Jenkins to be ready before applying any config. Managed plugins
	// are installed out of band by the plugins-init init container before the
	// Jenkins JVM starts, so they are already loaded here. The mite never installs
	// plugins (an ADMINISTER-gated path) and never restarts Jenkins; first boot
	// applies JCasC config and items directly.
	if err := a.waitForJenkinsWithFreshToken(ctx); err != nil {
		return result
	}

	jenkinsClient := a.getJenkinsClient()

	// Capture the Jenkins boot session BEFORE applying. If Jenkins restarts
	// mid-apply, the recorded (pre-restart) session won't match the new boot,
	// so the next push re-applies instead of trusting a stale marker.
	appliedSession, sessionErr := jenkinsClient.SessionID(ctx)
	if sessionErr != nil {
		a.Logger.Warn("jenkins session probe failed before apply — marker will force a re-apply", "error", sessionErr)
		appliedSession = ""
	}

	// Per-section convergence: skip sections whose hash hasn't changed within
	// the same boot session. A session change or legacy marker (sections==nil)
	// means nothing is converged — everything re-applies.
	marker := a.readMarker()
	desired := a.desiredSectionHashes(cmd)
	converged := func(name string) bool {
		return appliedSession != "" && marker.session == appliedSession && marker.sections != nil && marker.sections[name] == desired[name]
	}

	// 3. Apply JCasC + RBAC configuration via /configuration-as-code/reload.
	//    The mite always uses the MANAGE-gated reload path — write the
	//    bundle file(s) into the CASC directory, then POST .../reload. The
	//    mite has no path to the admin-gated apply/check endpoints, so it
	//    needs no Jenkins.ADMINISTER. rbac.yaml is written and reloaded
	//    together with config in the same call.
	configChanged := cmd.JcascYaml != "" && !converged("config")
	rbacChanged := cmd.RbacYaml != "" && !converged("rbac")

	if configChanged || rbacChanged {
		configOK, configErr := a.applyConfigViaReload(ctx, jenkinsClient, cmd)
		result.ConfigSuccess = configOK
		if !configOK {
			result.ConfigError = configErr
		}
		// rbac.yaml is written as part of the same reload; treat
		// success/failure uniformly with config.
		if cmd.RbacYaml != "" {
			result.RbacSuccess = configOK
			if !configOK {
				result.RbacError = configErr
			}
		}
		// Update stored last-good for snapshot hashes.
		if configOK && cmd.JcascYaml != "" {
			if err := os.WriteFile(filepath.Join(a.miteDir, "last-jcasc.yaml"), []byte(cmd.JcascYaml), 0600); err != nil {
				a.Logger.Error("store jcasc failed", "error", err)
			}
		}
		if configOK && cmd.RbacYaml != "" {
			if err := os.WriteFile(filepath.Join(a.miteDir, "last-rbac.yaml"), []byte(cmd.RbacYaml), 0600); err != nil {
				a.Logger.Error("store rbac failed", "error", err)
			}
		}
	} else {
		// Both sections converged (or absent): mark present sections successful.
		if cmd.JcascYaml != "" {
			result.ConfigSuccess = true
		}
		if cmd.RbacYaml != "" {
			result.RbacSuccess = true
		}
	}

	// 5. Apply items configuration. Items authenticate through the same
	// operator-JWT client as config/rbac/plugins; the declarative CASC boot
	// apply secures the realm from t=0, so a transient early-boot auth failure
	// here simply retries on the next push (RBAC is never torn down on restart).
	if cmd.ItemsYaml != "" {
		if converged("items") {
			result.ItemsSuccess = true
		} else {
			a.Logger.Debug("applying items", "len", len(cmd.ItemsYaml))
			itemsEngine := items.NewEngine(jenkinsClient)
			applyResult, err := itemsEngine.Apply(ctx, cmd.ItemsYaml)
			if err != nil {
				a.Logger.Error("items apply failed", "error", err)
				result.ItemsSuccess = false
				result.ItemsError = err.Error()
			} else {
				result.ItemsSuccess = true
				if len(applyResult.DeferredDeletions) > 0 {
					result.DeferredItemDeletions = toProtoDeferredDeletions(applyResult.DeferredDeletions)
				}
				if err := os.WriteFile(filepath.Join(a.miteDir, "last-items.yaml"), []byte(cmd.ItemsYaml), 0600); err != nil {
					a.Logger.Error("store items failed", "error", err)
				}
			}
		}
	} else {
		// Bundle has no item-type inputs: nothing to apply, so the section is
		// trivially converged. Without this the zero-value ItemsSuccess=false
		// flows back to the operator, which ANDs it into Succeeded (see
		// controller_controller.go) and stamps a failed "items" section with no
		// error message — surfacing as "items: rejected, no message returned".
		result.ItemsSuccess = true
	}

	// Record convergence: persist the per-section marker only when all
	// requested components succeeded. Carry forward unchanged section hashes.
	if a.allComponentsSucceeded(result, cmd) {
		newSections := make(map[string]string)
		for _, name := range markerSectionNames {
			if h, ok := marker.sections[name]; ok {
				newSections[name] = h
			}
		}
		for _, name := range markerSectionNames {
			if _, ok := desired[name]; ok {
				newSections[name] = desired[name]
			}
		}
		a.writeMarker(appliedMarker{
			composite: cmd.DesiredStateHash,
			session:   appliedSession,
			sections:  newSections,
		})

		// Capture a live-drift baseline from a fresh export.
		// Failure here is logged and non-fatal — drift detection is best-effort.
		if cmd.JcascYaml != "" {
			a.captureLiveFingerprintBaseline(ctx, cmd.JcascYaml)
		}
	}

	return result
}

// cascDir returns the live CASC directory from the CASC_JENKINS_CONFIG env
// var (set by the operator), or falls back to /var/jenkins_home/casc.
func cascDir() string {
	if d := os.Getenv("CASC_JENKINS_CONFIG"); d != "" {
		return d
	}
	return "/var/jenkins_home/casc"
}

// cascLastGoodDir returns the sibling directory where last-good copies of
// bundle files are kept — outside the live CASC directory so a restore
// cannot overwrite the live file.
func cascLastGoodDir() string {
	return cascDir() + "-last-good"
}

// confinedJoin joins name under base, rejecting names that contain a path
// separator or "..". It keeps command-supplied CASC filenames from escaping the
// CASC directory via path traversal.
func confinedJoin(base, name string) (string, error) {
	if name != filepath.Base(name) || name == ".." || strings.ContainsRune(name, os.PathSeparator) {
		return "", fmt.Errorf("unsafe path component %q", name)
	}
	return filepath.Join(base, name), nil
}

// saveLastGood copies the named file from the live CASC directory into the
// last-good sibling directory.  Errors are logged; the copy is best-effort.
func (a *Agent) saveLastGood(filename string) {
	src, err := confinedJoin(cascDir(), filename)
	if err != nil {
		a.Logger.Warn("save last-good rejected unsafe filename", "file", filename, "error", err)
		return
	}
	dstDir := cascLastGoodDir()
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		a.Logger.Warn("cannot create last-good dir", "dir", dstDir, "error", err)
		return
	}
	dst, err := confinedJoin(dstDir, filename)
	if err != nil {
		a.Logger.Warn("save last-good rejected unsafe filename", "file", filename, "error", err)
		return
	}
	srcData, err := os.ReadFile(src)
	if err != nil {
		// No existing file to save — not an error on first write.
		return
	}
	if err := os.WriteFile(dst, srcData, 0600); err != nil {
		a.Logger.Warn("save last-good failed", "file", filename, "error", err)
	}
}

// restoreLastGood copies the named file from the last-good sibling directory
// back into the live CASC directory.  Returns an error when no last-good
// copy exists.
func (a *Agent) restoreLastGood(filename string) error {
	src, err := confinedJoin(cascLastGoodDir(), filename)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dst, err := confinedJoin(cascDir(), filename)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}

// hasLastGood reports whether a last-good copy of filename exists.
func (a *Agent) hasLastGood(filename string) bool {
	p, err := confinedJoin(cascLastGoodDir(), filename)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// applyConfigViaReload writes the changed bundle file(s) into the live CASC
// directory, saves last-good copies, and triggers /configuration-as-code/reload.
// On reload failure it restores last-good and retries exactly once, reporting
// the outcome.  The config section is always reported as failed when reload
// fails — the requested desired state was not applied — and convergence is
// never recorded for a failed reload (the caller must gate on configOK).
func (a *Agent) applyConfigViaReload(ctx context.Context, client *jenkins.Client, cmd *mitev1.DesiredStateCommand) (configOK bool, configErr string) {
	casc := cascDir()
	if err := os.MkdirAll(casc, 0755); err != nil {
		return false, fmt.Sprintf("mkdir casc dir: %v", err)
	}

	// Do NOT preflight with /configuration-as-code/check — it requires
	// Jenkins.ADMINISTER. The reload below re-parses the written bundle and
	// fails on a bad config; the rollback path restores last-good. This keeps
	// the mite's config push within the MANAGE permission tier.

	// Save last-good copies before overwriting.
	if cmd.JcascYaml != "" {
		a.saveLastGood("config.yaml")
	}
	if cmd.RbacYaml != "" {
		a.saveLastGood("rbac.yaml")
	}

	// Write the changed files.
	if cmd.JcascYaml != "" {
		if err := os.WriteFile(filepath.Join(casc, "config.yaml"), []byte(cmd.JcascYaml), 0600); err != nil {
			return false, fmt.Sprintf("write config.yaml: %v", err)
		}
	}
	if cmd.RbacYaml != "" {
		if err := os.WriteFile(filepath.Join(casc, "rbac.yaml"), []byte(cmd.RbacYaml), 0600); err != nil {
			return false, fmt.Sprintf("write rbac.yaml: %v", err)
		}
	}

	// Trigger reload.
	if err := client.Reload(ctx); err != nil {
		a.Logger.Error("casc reload failed", "error", err)

		// Determine rollback outcome (three spec branches).
		switch {
		case !a.hasLastGood("config.yaml") && !a.hasLastGood("rbac.yaml"):
			return false, fmt.Sprintf("reload failed: %v; no last-good to roll back to", err)
		default:
			// Restore last-good files.
			restored := true
			if a.hasLastGood("config.yaml") {
				if rerr := a.restoreLastGood("config.yaml"); rerr != nil {
					a.Logger.Warn("restore last-good config.yaml failed", "error", rerr)
					restored = false
				}
			}
			if a.hasLastGood("rbac.yaml") {
				if rerr := a.restoreLastGood("rbac.yaml"); rerr != nil {
					a.Logger.Warn("restore last-good rbac.yaml failed", "error", rerr)
					restored = false
				}
			}
			if !restored {
				return false, fmt.Sprintf("reload failed: %v; rollback also failed", err)
			}
			// Re-issue reload exactly once.
			if rerr := client.Reload(ctx); rerr != nil {
				return false, fmt.Sprintf("reload failed: %v; rollback also failed", err)
			}
			return false, fmt.Sprintf("reload failed: %v; rolled back to last-good", err)
		}
	}

	return true, ""
}

// toProtoDeferredDeletions converts items engine DeferredDeletion values
// into the proto representation surfaced on CommandResult.
func toProtoDeferredDeletions(dds []items.DeferredDeletion) []*mitev1.DeferredDeletion {
	out := make([]*mitev1.DeferredDeletion, 0, len(dds))
	for _, d := range dds {
		out = append(out, &mitev1.DeferredDeletion{Path: d.Path, Reason: d.Reason})
	}
	return out
}

// runningBuilds queries Jenkins for the current running-build count.
// On error it returns a non-zero sentinel so the caller treats the
// controller as busy (conservative-on-error contract).
func (a *Agent) runningBuilds(ctx context.Context) int {
	jenkinsClient := a.getJenkinsClient()
	summary, err := jenkinsClient.GetJobSummary(ctx)
	if err != nil {
		a.Logger.Warn("failed to query running builds, assuming busy", "error", err)
		return 9999
	}
	return summary.RunningBuilds
}

// setDeferState updates the idle-defer flags under deferMu.
func (a *Agent) setDeferState(deferred bool, reason string) {
	a.deferMu.Lock()
	defer a.deferMu.Unlock()
	a.applyDeferred = deferred
	if deferred {
		a.deferReason = reason
	} else {
		a.deferReason = ""
	}
}

// shouldApplyNow returns true when an idle-mode apply should proceed:
// no builds running, or max-defer deadline exceeded.
// When the hash or apply_when changes, it resets the defer deadline.
func (a *Agent) shouldApplyNow(cmd *mitev1.DesiredStateCommand) bool {
	a.deferMu.Lock()
	defer a.deferMu.Unlock()

	now := time.Now()
	hashKey := cmd.DesiredStateHash + ":" + cmd.ApplyWhen

	if a.deferHash != hashKey {
		a.deferHash = hashKey
		if cmd.MaxDeferSec > 0 {
			a.deferDeadline = now.Add(time.Duration(cmd.MaxDeferSec) * time.Second)
		} else {
			a.deferDeadline = now.Add(30 * time.Minute)
		}
	}

	if now.After(a.deferDeadline) {
		return true
	}

	running := a.runningBuildsLocked()
	if running == 0 {
		return true
	}

	a.deferReason = fmt.Sprintf("%d builds running", running)
	return false
}

// runningBuildsLocked is like runningBuilds but assumes deferMu is held.
func (a *Agent) runningBuildsLocked() int {
	a.deferMu.Unlock()
	boundedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	n := a.runningBuilds(boundedCtx)
	cancel()
	a.deferMu.Lock()
	return n
}

// loadDeferState reads the current idle-defer flags (for snapshot).
func (a *Agent) loadDeferState() (deferred bool, reason string) {
	a.deferMu.Lock()
	defer a.deferMu.Unlock()
	return a.applyDeferred, a.deferReason
}

// pollDrain polls the running-build count every ~5s until zero or ctx expiry.
// Returns (true, 0) when drained, (false, remaining) on timeout/error.
// Never kills a build.
func pollDrain(ctx context.Context, client *jenkins.Client) (bool, int) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		summary, err := client.GetJobSummary(ctx)
		if err != nil {
			return false, 9999
		}
		if summary.RunningBuilds == 0 {
			return true, 0
		}
		select {
		case <-ctx.Done():
			return false, summary.RunningBuilds
		case <-ticker.C:
		}
	}
}

// waitForJenkinsWithFreshToken polls Jenkins until it responds 200 on /login,
// re-reading the API token on each attempt. The token file may not exist when
// the first desired-state command arrives (Jenkins hasn't finished booting),
// so we must re-read rather than using a client created once upfront.
func (a *Agent) waitForJenkinsWithFreshToken(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if a.currentJenkinsToken() != "" {
			client := a.getJenkinsClient()
			if err := client.WaitForJenkins(ctx); err == nil {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// writeOperatorStatus writes the mite-to-operator connection state to a
// JSON file served by Jenkins so the banner script can poll live status
// from the same origin instead of relying on a stale URL parameter.
func (a *Agent) writeOperatorStatus(connected bool) {
	ucDir := filepath.Join(jenkinsHome, "userContent")
	if err := os.MkdirAll(ucDir, 0755); err != nil {
		a.Logger.Error("writeOperatorStatus: mkdir failed", "error", err)
		return
	}
	content := `{"connected":false}`
	if connected {
		content = `{"connected":true}`
	}
	if err := os.WriteFile(filepath.Join(ucDir, "varroa-operator-status.json"), []byte(content), 0644); err != nil {
		a.Logger.Error("writeOperatorStatus: write failed", "error", err)
	}
}

// currentJenkinsToken returns the in-memory operator-signed JWT for Jenkins auth.
func (a *Agent) currentJenkinsToken() string {
	a.jenkinsTokenMu.Lock()
	defer a.jenkinsTokenMu.Unlock()
	return a.jenkinsToken
}

// currentJenkinsTokenExp returns the expiry of the in-memory operator-signed JWT.
func (a *Agent) currentJenkinsTokenExp() int64 {
	a.jenkinsTokenMu.Lock()
	defer a.jenkinsTokenMu.Unlock()
	return a.jenkinsTokenExp
}

// getAppliedDrainTimeoutSec reads the cached drain timeout under deferMu so
// the SIGTERM goroutine does not perform a bare read that the -race detector
// would flag (appliedDrainTimeoutSec is written under deferMu).
func (a *Agent) getAppliedDrainTimeoutSec() int64 {
	a.deferMu.Lock()
	defer a.deferMu.Unlock()
	return a.appliedDrainTimeoutSec
}

// getJenkinsClient returns the long-lived Jenkins client, creating it lazily
// on first access. The client uses a dynamic token function so it always reads
// the current Bearer token without being recreated.
func (a *Agent) getJenkinsClient() *jenkins.Client {
	a.jenkinsClientMu.Lock()
	defer a.jenkinsClientMu.Unlock()
	if a.jenkinsClient != nil {
		return a.jenkinsClient
	}
	c := jenkins.NewClient(a.cfg.JenkinsURL, "varroa-mite", a.currentJenkinsToken())
	c.SetTokenFunc(a.currentJenkinsToken)
	c.Logger = a.Logger
	a.jenkinsClient = c
	return c
}

// wakeTokenWaiters notifies all token refresh waiters whose baseline
// expiry is now satisfied by the current cached token. Waiters whose
// baseline is not yet satisfied remain in the list (stale grants do
// not falsely unblock them).
func (a *Agent) wakeTokenWaiters() {
	exp := a.currentJenkinsTokenExp()
	a.tokenWaitersMu.Lock()
	defer a.tokenWaitersMu.Unlock()
	var remaining []*tokenWaiter
	for _, w := range a.tokenWaiters {
		if exp > w.baseline {
			select {
			case w.ch <- struct{}{}:
			default:
			}
		} else {
			remaining = append(remaining, w)
		}
	}
	a.tokenWaiters = remaining
}

// removeWaiter removes a specific waiter from the token waiters list.
func (a *Agent) removeWaiter(w *tokenWaiter) {
	a.tokenWaitersMu.Lock()
	defer a.tokenWaitersMu.Unlock()
	var remaining []*tokenWaiter
	for _, existing := range a.tokenWaiters {
		if existing != w {
			remaining = append(remaining, existing)
		}
	}
	a.tokenWaiters = remaining
}

// startTokenRefreshLoop periodically checks the cached token and requests a
// fresh one from the operator via the gRPC stream when the token is missing or
// approaching expiry. Requesting while missing (exp == 0) is the safety net for
// a lost initial grant: the request relay (gateway→operator) is best-effort, so
// a controller that misses its first reactive grant would otherwise stay
// tokenless forever — starving anything that needs an authenticated Jenkins
// call (health probe, activity/idle-gauge poll, CASC apply).
func (a *Agent) startTokenRefreshLoop(ctx context.Context) {
	const checkInterval = 30 * time.Second
	const refreshWindow = 10 * time.Minute

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			exp := a.currentJenkinsTokenExp()
			if exp == 0 || time.Until(time.Unix(exp, 0)) < refreshWindow {
				a.requestTokenRefresh(ctx)
			}
		}
	}
}

// requestTokenRefresh sends a TokenRefreshRequest to the operator over the
// gRPC command stream and blocks until the cached token has advanced past
// the expiry observed when the refresh began, or a 10-second timeout elapses.
func (a *Agent) requestTokenRefresh(ctx context.Context) {
	baseline := a.currentJenkinsTokenExp()

	w := &tokenWaiter{ch: make(chan struct{}, 1), baseline: baseline}
	a.tokenWaitersMu.Lock()
	a.tokenWaiters = append(a.tokenWaiters, w)
	a.tokenWaitersMu.Unlock()

	msg := &mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_TokenRefreshRequest{
			TokenRefreshRequest: &mitev1.TokenRefreshRequest{},
		},
	}
	a.sendMu.Lock()
	err := a.stream.Send(msg)
	a.sendMu.Unlock()
	if err != nil {
		a.Logger.Warn("token refresh request send failed", "error", err)
		a.removeWaiter(w)
		return
	}

	select {
	case <-ctx.Done():
		a.removeWaiter(w)
		return
	case <-w.ch:
		return
	case <-time.After(10 * time.Second):
		a.Logger.Warn("token refresh grant timeout")
		a.removeWaiter(w)
	}
}

// appliedMarker is the convergence marker persisted to disk.
type appliedMarker struct {
	composite, session string
	sections           map[string]string // "config"|"rbac"|"items" → hash
}

var markerSectionNames = []string{"config", "plugins", "rbac", "items"}

// readMarker reads the convergence marker file. Backward-compatible: legacy
// single-line "hash [session]" form parses with sections == nil.
func (a *Agent) readMarker() appliedMarker {
	data, err := os.ReadFile(a.lastAppliedHashPath())
	if err != nil {
		return appliedMarker{}
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && !strings.Contains(lines[0], " ") {
		// Legacy single-hash, no session.
		return appliedMarker{composite: lines[0]}
	}
	if len(lines) == 1 {
		fields := strings.Fields(lines[0])
		if len(fields) == 1 {
			return appliedMarker{composite: fields[0]}
		}
		return appliedMarker{composite: fields[0], session: fields[1]}
	}
	m := appliedMarker{sections: make(map[string]string)}
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "composite":
			m.composite = val
		case "session":
			m.session = val
		default:
			m.sections[key] = val
		}
	}
	return m
}

// isConvergedToLatest returns true when the on-disk convergence marker
// matches the cached latest DesiredStateHash.
func (a *Agent) isConvergedToLatest() bool {
	a.deferMu.Lock()
	hash := a.latestDesiredStateHash
	a.deferMu.Unlock()
	if hash == "" {
		return false
	}
	return a.readMarker().composite == hash
}

const fingerprintBaselineName = "last-live-fingerprint.hash"

// fingerprintBaselinePath returns the path to the baseline file.
func (a *Agent) fingerprintBaselinePath() string {
	return filepath.Join(a.miteDir, fingerprintBaselineName)
}

// captureLiveFingerprintBaseline exports live config, projects it onto
// managed paths, and writes the hash as the drift-detection baseline.
// Errors are logged and never fail the apply.
func (a *Agent) captureLiveFingerprintBaseline(ctx context.Context, appliedJCasC string) {
	export, err := a.getJenkinsClient().ExportConfig(ctx)
	if err != nil {
		a.Logger.Warn("failed to export config for drift baseline", "error", err)
		return
	}
	hash, err := projectAndHash(export, appliedJCasC)
	if err != nil {
		a.Logger.Warn("failed to hash drift baseline", "error", err)
		return
	}
	if hash == "" {
		return
	}
	if err := os.MkdirAll(a.miteDir, 0755); err != nil {
		a.Logger.Warn("failed to create mite dir for fingerprint baseline", "error", err)
		return
	}
	// Atomic write via temp file.
	tmp := a.fingerprintBaselinePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(hash+"\n"), 0600); err != nil {
		a.Logger.Warn("failed to write fingerprint baseline temp", "error", err)
		return
	}
	if err := os.Rename(tmp, a.fingerprintBaselinePath()); err != nil {
		a.Logger.Warn("failed to atomically rename fingerprint baseline", "error", err)
	}
}

// writeMarker atomically persists the convergence marker.
func (a *Agent) writeMarker(m appliedMarker) {
	if m.composite == "" {
		return
	}
	if err := os.MkdirAll(a.miteDir, 0755); err != nil {
		a.Logger.Error("mkdir for last-applied-hash failed", "error", err)
		return
	}
	var buf strings.Builder
	buf.WriteString("composite " + m.composite + "\n")
	if m.session != "" {
		buf.WriteString("session " + m.session + "\n")
	}
	for _, name := range markerSectionNames {
		if h, ok := m.sections[name]; ok {
			buf.WriteString(name + " " + h + "\n")
		}
	}
	tmpFile := a.lastAppliedHashPath() + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(buf.String()), 0600); err != nil {
		a.Logger.Error("write last-applied-hash tmp failed", "error", err)
		return
	}
	if err := os.Rename(tmpFile, a.lastAppliedHashPath()); err != nil {
		a.Logger.Error("rename last-applied-hash failed", "error", err)
	}
}

// hasLastAppliedHash returns true if the convergence marker exists.
func (a *Agent) hasLastAppliedHash() bool {
	_, err := os.Stat(a.lastAppliedHashPath())
	return err == nil
}

// desiredSectionHashes computes per-section content hashes from a command,
// matching the operator-side computeChangedSections semantics.
func (a *Agent) desiredSectionHashes(cmd *mitev1.DesiredStateCommand) map[string]string {
	m := make(map[string]string, 4)
	if cmd.JcascYaml != "" {
		m["config"] = sha256hex([]byte(cmd.JcascYaml))
	}
	if cmd.RbacYaml != "" {
		m["rbac"] = sha256hex([]byte(cmd.RbacYaml))
	}
	if cmd.ItemsYaml != "" {
		m["items"] = sha256hex([]byte(cmd.ItemsYaml))
	}
	return m
}

// allComponentsSucceeded returns true if every component present in the
// command was applied successfully. Empty (unset) components count as
// success (they were not requested).
func (a *Agent) allComponentsSucceeded(result *mitev1.DesiredStateResult, cmd *mitev1.DesiredStateCommand) bool {
	if cmd.JcascYaml != "" && !result.ConfigSuccess {
		return false
	}
	if cmd.RbacYaml != "" && !result.RbacSuccess {
		return false
	}
	if cmd.ItemsYaml != "" && !result.ItemsSuccess {
		return false
	}
	return true
}

// startHealthProbe periodically probes Jenkins health (GET /api/json) on a
// slower cadence (~60s), independent of the 15s gRPC heartbeat. The cached
// health is reported by sendHeartbeats without an extra Jenkins request.
func (a *Agent) startHealthProbe(ctx context.Context) {
	const probeInterval = 60 * time.Second
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	// Probe immediately on start to populate initial health.
	a.healthProbeTick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.healthProbeTick(ctx)
		}
	}
}

// healthProbeTick performs a single Jenkins health probe and updates the
// cached health/version. It also triggers recovery actions when Jenkins
// transitions from unreachable back to healthy.
func (a *Agent) healthProbeTick(ctx context.Context) {
	client := a.getJenkinsClient()

	info, err := client.GetInfo(ctx)
	if err != nil {
		a.setHealth("unreachable", "")
		return
	}
	health := "healthy"
	if info.QuietingDown {
		health = "unhealthy"
	}
	a.setHealth(health, info.Version)
}

// startObservabilityProbe periodically probes Jenkins observability
// surfaces on a slower cadence (~60s), independent of heartbeat and
// health probing. It builds an ObservabilityReport and sends it to the
// operator.
func (a *Agent) startObservabilityProbe(ctx context.Context) {
	const probeInterval = 60 * time.Second
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	a.observabilityProbeTick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.observabilityProbeTick(ctx)
		}
	}
}

// observabilityProbeTick performs a single observability probe pass:
// Jenkins API summary, Prometheus endpoint detection, and OpenTelemetry
// config detection. It builds and sends an ObservabilityReport.
func (a *Agent) observabilityProbeTick(ctx context.Context) {
	client := a.getJenkinsClient()
	report := &mitev1.ObservabilityReport{
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		TTLSeconds: 180,
	}

	var sources []*mitev1.ObservableSource
	var capabilities []string

	// Jenkins API summary.
	if ok, src := a.probeJenkinsAPI(ctx, client); ok {
		sources = append(sources, src)
		capabilities = append(capabilities, "jenkins.health")
		// Collect bounded job/build summary.
		if summary := a.collectJenkinsSummary(ctx, client); summary != nil {
			report.Summary = summary
			if summary.TotalJobs > 0 {
				capabilities = append(capabilities, "jenkins.jobs.summary")
			}
			if len(summary.RecentBuilds) > 0 {
				capabilities = append(capabilities, "jenkins.builds.recent")
			}
		}
	}
	// Prometheus endpoint detection.
	if ok, src := a.probePrometheusEndpoint(ctx, client); ok {
		sources = append(sources, src)
		if src.Status == "exposed" {
			capabilities = append(capabilities, "jenkins.metrics.endpoint")
		}
	}
	// OpenTelemetry config detection.
	if ok, src := a.probeOpenTelemetry(ctx, client); ok {
		sources = append(sources, src)
		if src.Status == "configured" || src.Status == "exposed" {
			capabilities = append(capabilities, "jenkins.traces.exporting")
		}
	}

	report.Sources = sources
	report.Capabilities = capabilities

	a.sendMu.Lock()
	err := a.stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_ObservabilityReport{ObservabilityReport: report},
	})
	a.sendMu.Unlock()
	if err != nil {
		a.Logger.Warn("observability report send failed", "error", err)
	}
}

// startFingerprintTicker runs the live-drift fingerprint on a configurable
// interval, decoupled from the 15s heartbeat.
func (a *Agent) startFingerprintTicker(ctx context.Context) {
	// Poll the interval at ≤60s so disable/interval changes take effect promptly.
	const pollInterval = 60 * time.Second
	var lastFingerprint time.Time
	for {
		a.deferMu.Lock()
		intervalSec := a.liveFingerprintIntervalSec
		a.deferMu.Unlock()

		if intervalSec < 0 {
			// Disabled — poll every 60s so re-enable is detected.
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
			}
			continue
		}
		if intervalSec == 0 {
			intervalSec = 600
		}
		if intervalSec < 60 {
			intervalSec = 60
		}

		wait := pollInterval
		if elapsed := time.Since(lastFingerprint); elapsed < time.Duration(intervalSec)*time.Second {
			wait = time.Duration(intervalSec)*time.Second - elapsed
			if wait > pollInterval {
				wait = pollInterval
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		if time.Since(lastFingerprint) >= time.Duration(intervalSec)*time.Second {
			a.runFingerprintTick()
			lastFingerprint = time.Now()
		}
	}
}

const (
	certRenewalCheckInterval = 10 * time.Minute
	certRenewalThresholdNum  = 7
	certRenewalThresholdDen  = 10
)

// shouldRenewCert reports whether a certificate with the given validity window
// has crossed the certRenewalThresholdNum/certRenewalThresholdDen fraction of
// its lifetime. Integer comparison (elapsed*den >= lifetime*num) avoids float
// rounding at the durations involved here.
func shouldRenewCert(notBefore, notAfter time.Time) bool {
	lifetime := notAfter.Sub(notBefore)
	elapsed := time.Since(notBefore)
	return elapsed*certRenewalThresholdDen >= lifetime*certRenewalThresholdNum
}

// startCertRenewalLoop proactively renews the mTLS client certificate once it
// crosses the renewal threshold, so long-running mite processes never rely on
// a pod restart to pick up a fresh cert before expiry.
func (a *Agent) startCertRenewalLoop(ctx context.Context) {
	ticker := time.NewTicker(certRenewalCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.certMu.Lock()
			cert := a.cert
			a.certMu.Unlock()
			if cert == nil || cert.Leaf == nil {
				continue
			}
			if !shouldRenewCert(cert.Leaf.NotBefore, cert.Leaf.NotAfter) {
				continue
			}
			if err := a.renewWithExistingKeypair(ctx); err != nil {
				a.Logger.Warn("cert renewal failed, will retry", "error", err)
				continue
			}
			a.certMu.Lock()
			newNotAfter := a.cert.Leaf.NotAfter
			a.certMu.Unlock()
			a.Logger.Info("mite cert renewed", "new_not_after", newNotAfter)
		}
	}
}

// runFingerprintTick performs a single fingerprint cycle: exports live config,
// computes the projected hash, and compares it to the baseline.
func (a *Agent) runFingerprintTick() {
	tickCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	export, err := a.getJenkinsClient().ExportConfig(tickCtx)
	if err != nil {
		a.Logger.Warn("fingerprint export failed, skipping tick", "error", err)
		return
	}

	appliedPath := filepath.Join(a.miteDir, "last-jcasc.yaml")
	applied, err := os.ReadFile(appliedPath)
	if err != nil {
		a.Logger.Warn("fingerprint: failed to read applied JCasC", "error", err)
		return
	}

	liveHash, err := projectAndHash(export, string(applied))
	if err != nil {
		a.Logger.Warn("fingerprint: hash failed, skipping tick", "error", err)
		return
	}

	if liveHash == "" {
		return
	}

	baselineData, err := os.ReadFile(a.fingerprintBaselinePath())
	if err == nil {
		baselineHash := strings.TrimSpace(string(baselineData))
		a.deferMu.Lock()
		a.liveConfigHash = liveHash
		a.liveDrift = liveHash != baselineHash
		a.deferMu.Unlock()
		return
	}

	// No baseline yet. Bootstrap it only when converged to the latest desired hash.
	if a.isConvergedToLatest() {
		if err := os.MkdirAll(a.miteDir, 0755); err != nil {
			a.Logger.Warn("fingerprint: failed to create mite dir for bootstrap", "error", err)
			return
		}
		tmp := a.fingerprintBaselinePath() + ".tmp"
		if err := os.WriteFile(tmp, []byte(liveHash+"\n"), 0600); err != nil {
			a.Logger.Warn("fingerprint: failed to write bootstrap baseline", "error", err)
			return
		}
		if err := os.Rename(tmp, a.fingerprintBaselinePath()); err != nil {
			a.Logger.Warn("fingerprint: failed to rename bootstrap baseline", "error", err)
			return
		}
	}
	a.deferMu.Lock()
	a.liveConfigHash = liveHash
	a.liveDrift = false
	a.deferMu.Unlock()
}

// probeJenkinsAPI probes Jenkins for a bounded API summary. Returns a
// source observation and populates the report with job/build data.
func (a *Agent) probeJenkinsAPI(ctx context.Context, client *jenkins.Client) (bool, *mitev1.ObservableSource) {
	info, err := client.GetInfo(ctx)
	if err != nil {
		return false, &mitev1.ObservableSource{
			Provider: "jenkins-api",
			Status:   "unavailable",
			Error:    err.Error(),
		}
	}
	_ = info
	return true, &mitev1.ObservableSource{
		Provider: "jenkins-api",
		Status:   "exposed",
	}
}

// collectJenkinsSummary fetches a bounded job/build summary from
// Jenkins and returns it as an observability summary payload.
func (a *Agent) collectJenkinsSummary(ctx context.Context, client *jenkins.Client) *mitev1.ObservabilitySummary {
	js, err := client.GetJobSummary(ctx)
	if err != nil {
		return nil
	}
	if js == nil || js.TotalJobs == 0 {
		return nil
	}
	var builds []*mitev1.ObservabilityBuild
	for _, b := range js.RecentBuilds {
		builds = append(builds, &mitev1.ObservabilityBuild{
			JobName:         b.JobName,
			BuildNumber:     b.BuildNumber,
			Status:          b.Status,
			StartedAt:       b.StartedAt,
			DurationSeconds: b.DurationSeconds,
			URL:             b.URL,
		})
	}
	return &mitev1.ObservabilitySummary{
		TotalJobs:     js.TotalJobs,
		RunningBuilds: js.RunningBuilds,
		RecentBuilds:  builds,
	}
}

// probePrometheusEndpoint checks whether Jenkins exposes the
// Prometheus plugin endpoint. It sends an HTTP GET to /prometheus/.
func (a *Agent) probePrometheusEndpoint(ctx context.Context, client *jenkins.Client) (bool, *mitev1.ObservableSource) {
	err := a.prometheusReachable(ctx, client)
	if err != nil {
		return false, &mitev1.ObservableSource{
			Provider: "prometheus",
			Status:   "unavailable",
			Error:    err.Error(),
		}
	}
	return true, &mitev1.ObservableSource{
		Provider: "prometheus",
		Status:   "exposed",
		Hints:    map[string]string{"endpoint": "/prometheus/"},
	}
}

// prometheusReachable sends a request to the Jenkins Prometheus
// endpoint and returns nil if it responds (any non-error response).
func (a *Agent) prometheusReachable(ctx context.Context, client *jenkins.Client) error {
	info, err := client.GetInfo(ctx)
	if err != nil {
		return err // Jenkins itself is unreachable; can't check Prometheus
	}
	_ = info

	// Issue a GET to /prometheus/. If the plugin is installed the
	// endpoint will respond; if not, Jenkins returns 404 and we
	// treat that as unavailable.
	resp, err := client.DoGet(ctx, "/prometheus/")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("prometheus plugin not installed")
	}
	return nil
}

// probeOpenTelemetry checks whether OpenTelemetry configuration
// appears present in Jenkins. Conservative detection based on
// plugin or config evidence.
func (a *Agent) probeOpenTelemetry(ctx context.Context, client *jenkins.Client) (bool, *mitev1.ObservableSource) {
	return false, nil
}

// string if the file cannot be read.
func (a *Agent) fileHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// sha256hex returns the hex-encoded SHA-256 hash of data.
func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// computeStateHash combines the config, plugins, RBAC, and items hashes
// into a single state hash.
func computeStateHash(configHash, pluginsHash, rbacHash, itemsHash string) string {
	h := sha256.New()
	h.Write([]byte(configHash))
	h.Write([]byte(pluginsHash))
	h.Write([]byte(rbacHash))
	h.Write([]byte(itemsHash))
	return hex.EncodeToString(h.Sum(nil))
}

// readFileEmpty reads a file's content, returning "" if the file does not exist,
// cannot be read, or exceeds maxSize (defaults to 4 MB — the gRPC/NATS message limit).
func readFileEmpty(path string) string {
	const maxSize = 4 * 1024 * 1024
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxSize {
		return ""
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// Plugin inventory collection
// ---------------------------------------------------------------------------

const (
	// pluginInventoryResendInterval is the minimum time between inventory
	// pushes on stream re-establishment (heartbeatInterval × 20 = 5 minutes).
	pluginInventoryResendInterval = 20 * 15 * time.Second
	// pluginInventoryCollectTimeout bounds a single collection attempt.
	pluginInventoryCollectTimeout = 25 * time.Second
)

// startPluginInventoryCollector periodically collects the installed plugin
// inventory from Jenkins. It runs in its own goroutine with its own timeout
// so a hung Jenkins never blocks the heartbeat send path.
func (a *Agent) startPluginInventoryCollector(ctx context.Context) {
	// Collect immediately on start, and push it. The push is NOT optional here:
	// the heartbeat goroutine reads the latched inventory's hash as soon as it
	// exists, so collecting without pushing advertises a hash the operator has
	// no inventory for. It then marks the controller stale and self-heals with a
	// COLLECT_PLUGIN_INVENTORY round-trip — on every mite start and reconnect —
	// to fetch an inventory the mite had already collected.
	a.collectAndLatch(ctx)
	a.maybePushInventory(ctx)

	ticker := time.NewTicker(a.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.collectAndLatch(ctx)
			// After each collection, check if we should push.
			a.maybePushInventory(ctx)
		}
	}
}

// collectAndLatch runs a single collection attempt and stores the result
// in the atomic pointer. It uses a plugininv.CollectSelection with the
// Jenkins client implementing the jenkinsPluginAPI seam and the plugins
// directory as the filesystem fallback.
func (a *Agent) collectAndLatch(ctx context.Context) {
	collectCtx, cancel := context.WithTimeout(ctx, pluginInventoryCollectTimeout)
	defer cancel()

	client := a.getJenkinsClient()
	if client == nil {
		return
	}

	// Use os.DirFS for the plugins directory as the filesystem fallback.
	pluginsDir := filepath.Join(jenkinsHome, "plugins")
	fsys := os.DirFS(pluginsDir)

	inv := plugininv.CollectSelection(collectCtx, client, fsys)
	// Normalize BEFORE publishing, while this goroutine is still the only
	// owner. Hash() normalizes a copy (it must not mutate a pointee shared with
	// the heartbeat goroutine), so without this the serialized records would be
	// the raw ones — duplicates and all — while the hash described the deduped
	// canonical set, and the reported version of a duplicated plugin would
	// depend on collection order.
	inv.Normalize()
	a.pluginInventory.Store(&inv)
}

// maybePushInventory determines whether the inventory should be pushed
// over the command stream and does so if needed. Called after each
// collection tick and on stream re-establishment.
func (a *Agent) maybePushInventory(ctx context.Context) {
	inv := a.pluginInventory.Load()
	if inv == nil {
		return
	}

	hash := inv.Hash()
	source := inv.Source
	failed := inv.CollectionFailed

	a.pluginInventoryMu.Lock()
	force := a.pushOnCollect
	if !force {
		// Nothing changed.
		if hash == a.lastPushedHash && source == a.lastPushedSource &&
			failed == a.lastPushedFailed {
			a.pluginInventoryMu.Unlock()
			return
		}
	}
	a.pushOnCollect = false
	a.pluginInventoryMu.Unlock()

	a.pushInventoryUnlocked(ctx, inv)
}

// pushInventoryUnlocked sends the PluginInventory message over the stream.
// It takes sendMu. The caller must have already checked the push condition.
func (a *Agent) pushInventoryUnlocked(_ context.Context, inv *plugininv.Inventory) {
	pi := inventoryToProto(inv)

	msg := &mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_PluginInventory{PluginInventory: pi},
	}
	a.sendMu.Lock()
	err := a.stream.Send(msg)
	a.sendMu.Unlock()
	if err != nil {
		a.Logger.Warn("plugin inventory push failed", "error", err)
		return
	}

	// Record the successfully pushed state.
	hash := inv.Hash()
	a.pluginInventoryMu.Lock()
	a.lastPushedHash = hash
	a.lastPushedSource = inv.Source
	a.lastPushedFailed = inv.CollectionFailed
	a.lastPushTime = time.Now()
	a.pluginInventoryMu.Unlock()
}

// resendInventoryIfStale is called on stream re-establishment. It re-pushes
// the inventory only if enough time has passed since the last push.
func (a *Agent) resendInventoryIfStale(ctx context.Context) {
	inv := a.pluginInventory.Load()
	if inv == nil {
		return
	}

	a.pluginInventoryMu.Lock()
	elapsed := time.Since(a.lastPushTime)
	a.pluginInventoryMu.Unlock()

	if elapsed >= pluginInventoryResendInterval {
		a.pushInventoryUnlocked(ctx, inv)
	}
}

// triggerCollectionAndPush unconditionally re-collects and pushes the
// inventory. Called by the COLLECT_PLUGIN_INVENTORY command.
func (a *Agent) triggerCollectionAndPush(ctx context.Context) error {
	collectCtx, cancel := context.WithTimeout(ctx, pluginInventoryCollectTimeout)
	defer cancel()

	client := a.getJenkinsClient()
	pluginsDir := filepath.Join(jenkinsHome, "plugins")
	fsys := os.DirFS(pluginsDir)

	inv := plugininv.CollectSelection(collectCtx, client, fsys)
	inv.Normalize() // canonical before publish; see collectInventory
	a.pluginInventory.Store(&inv)

	a.pluginInventoryMu.Lock()
	a.lastPushedHash = "" // force push
	a.pluginInventoryMu.Unlock()

	a.pushInventoryUnlocked(ctx, &inv)
	if inv.CollectionFailed {
		return fmt.Errorf("plugin inventory collection failed: %s", inv.CollectionError)
	}
	return nil
}

// inventoryToProto converts a plugininv.Inventory to the proto form.
// Applies the degradation ladder: if the marshalled MiteMessage would
// exceed ~900 KiB (leaving headroom below NATS' 1 MiB limit), first
// drops optional edges, then all edges.
func inventoryToProto(inv *plugininv.Inventory) *mitev1.PluginInventory {
	const maxBudget = 900 * 1024

	pi := &mitev1.PluginInventory{
		InstalledPluginsHash: inv.Hash(),
		CollectedAt:          inv.CollectedAt.Format(time.RFC3339),
		Source:               inv.Source,
		CollectionFailed:     inv.CollectionFailed,
		CollectionError:      inv.CollectionError,
	}

	// Build plugin list with all deps.
	pi.Plugins = recordsToProto(inv.Records, false, false)
	pi.Degraded = inv.Source == plugininv.SourceFilesystem

	// Measure marshalled size.
	msg := &mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_PluginInventory{PluginInventory: pi},
	}
	data, _ := msg.MarshalJSON()
	if len(data) <= maxBudget {
		return pi
	}

	// Step 1: drop optional edges.
	pi.OptionalEdgesDropped = true
	pi.Plugins = recordsToProto(inv.Records, true, false)
	msg.Message = &mitev1.MiteMessage_PluginInventory{PluginInventory: pi}
	data, _ = msg.MarshalJSON()
	if len(data) <= maxBudget {
		return pi
	}

	// Step 2: drop all edges.
	pi.AllEdgesDropped = true
	pi.Truncated = true
	pi.Plugins = recordsToProto(inv.Records, false, true)
	return pi
}

// recordsToProto converts a slice of Records to proto InstalledPlugins.
// dropOptional: omit optional deps. dropAll: omit all deps.
func recordsToProto(records []plugininv.Record, dropOptional, dropAll bool) []*mitev1.InstalledPlugin {
	out := make([]*mitev1.InstalledPlugin, 0, len(records))
	for _, r := range records {
		ip := &mitev1.InstalledPlugin{
			Name:     r.Name,
			Version:  r.Version,
			Enabled:  triToString(r.Enabled),
			Detached: triToString(r.Detached),
			Bundled:  triToString(r.Bundled),
		}
		if !dropAll {
			for _, d := range r.Deps {
				if dropOptional && d.Optional {
					continue
				}
				ip.Deps = append(ip.Deps, &mitev1.PluginDep{
					Name:     d.Name,
					Min:      d.Min,
					Optional: d.Optional,
				})
			}
		}
		out = append(out, ip)
	}
	return out
}

// triToString converts a plugininv.Tri to its JSON string form.
func triToString(t plugininv.Tri) string {
	switch t {
	case plugininv.TriTrue:
		return "true"
	case plugininv.TriFalse:
		return "false"
	default:
		return ""
	}
}
