package mite

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
)

// Server is the gRPC server that implements the Mite service. It accepts
// connections from mite sidecars, verifies their TLS certificates, and
// provides command-and-control over a bidirectional stream.
type Server struct {
	ca            *ca.CA
	reg           *Registry
	tokens        *TokenSigner
	consumed      ConsumedTokenStore // single-use enforcement for bootstrap tokens
	grpcServer    *grpc.Server
	broadcaster   *sse.Broadcaster
	streamHandler StreamHandler // pluggable stream backend

	// TokenGrantFunc is called when a mite sends a TokenRefreshRequest.
	// It returns a fresh Jenkins token and its expiry. If nil, token
	// refresh requests are forwarded to the StreamHandler only.
	TokenGrantFunc func(controllerName, namespace string) (token string, exp int64, err error)

	// ServerEndpoint is the endpoint returned in RegisterResponse so mites
	// know where to connect their CommandStream. Defaults to the legacy
	// hardcoded value if not set.
	ServerEndpoint string

	Logger *slog.Logger
}

// NewServer creates a new Mite gRPC server with TLS credentials issued by
// the given CA. Additional DNS SANs (e.g. the operator service hostname) are
// included in the server certificate so clients can verify the hostname.
// It registers the Mite service and installs the mTLS auth interceptor.
func NewServer(certAuth *ca.CA, tokenSigner *TokenSigner, extraSANs ...string) *Server {
	s := &Server{
		ca:       certAuth,
		reg:      NewRegistry(),
		tokens:   tokenSigner,
		consumed: newInMemoryConsumedStore(),
	}

	serverCert, err := certAuth.IssueServerCert(extraSANs...)
	if err != nil {
		slog.Error("failed to issue server certificate", "error", err)
		os.Exit(1)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    certAuth.CertPool(),
		MinVersion:   tls.VersionTLS13,
	}

	opts := []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(AuthInterceptor(certAuth)),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	}
	if !telemetry.Disabled() {
		opts = append(opts, grpc.StatsHandler(otelgrpc.NewServerHandler()))
	}

	s.grpcServer = grpc.NewServer(opts...)

	mitev1.RegisterMiteServer(s.grpcServer, s)

	return s
}

// GRPCServer returns the underlying gRPC server.
func (s *Server) GRPCServer() *grpc.Server {
	return s.grpcServer
}

// Registry returns the mite connection registry.
func (s *Server) Registry() *Registry {
	return s.reg
}

// TokenSigner returns the bootstrap token signer.
func (s *Server) TokenSigner() *TokenSigner {
	return s.tokens
}

// SetBroadcaster sets the SSE broadcaster for fan-out of mite events.
func (s *Server) SetBroadcaster(b *sse.Broadcaster) {
	s.broadcaster = b
}

// SetStreamHandler sets the stream handler for CommandStream. If nil
// (the default), the server uses the default RegistryHandler backed by
// the internal Registry.
func (s *Server) SetStreamHandler(h StreamHandler) {
	s.streamHandler = h
}

// SetConsumedTokenStore substitutes the bootstrap-token single-use backend (e.g. a
// shared store for cross-replica enforcement). It must be called before serving; it
// stops the default in-memory sweeper it replaces.
func (s *Server) SetConsumedTokenStore(store ConsumedTokenStore) {
	if c, ok := s.consumed.(interface{ stop() }); ok {
		c.stop()
	}
	s.consumed = store
}

// Register handles the mite registration RPC. It validates the bootstrap
// token (first-time registration) or the existing client certificate
// (renewal) and issues a new client certificate.
func (s *Server) Register(ctx context.Context, req *mitev1.RegisterRequest) (*mitev1.RegisterResponse, error) {
	if req.ControllerName == "" || req.Namespace == "" {
		return nil, status.Error(codes.InvalidArgument, "controller_name and namespace are required")
	}

	if req.BootstrapToken != "" {
		// First-time registration with a bootstrap token.
		tokenCtrl, tokenNS, err := ParseTokenIdentity(req.BootstrapToken)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "malformed bootstrap token")
		}
		if tokenCtrl != req.ControllerName || tokenNS != req.Namespace {
			return nil, status.Error(codes.PermissionDenied, "bootstrap token does not match requested identity")
		}
		claims, err := s.tokens.ValidateToken(req.BootstrapToken, req.ControllerName, req.Namespace)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid or expired bootstrap token")
		}
		// Single-use: reject replay of an already-consumed jti.
		if !s.consumed.Consume(claims.JTI, time.Unix(claims.Expiry, 0)) {
			return nil, status.Error(codes.PermissionDenied, "bootstrap token already used")
		}
	} else {
		// Certificate renewal: verify the existing client certificate.
		p, ok := peer.FromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "no peer info")
		}
		tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
			return nil, status.Error(codes.Unauthenticated, "client certificate required for renewal")
		}
		cn := tlsInfo.State.PeerCertificates[0].Subject.CommonName
		expectedCN := fmt.Sprintf("%s.%s", req.ControllerName, req.Namespace)
		if cn != expectedCN {
			return nil, status.Error(codes.PermissionDenied, "certificate CN does not match requested identity")
		}
	}

	pubkey := ed25519.PublicKey(req.PublicKey)
	cert, err := s.ca.IssueMiteCert(req.ControllerName, req.Namespace, pubkey)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("issue mite cert failed", "controller", req.ControllerName, "namespace", req.Namespace, "error", err)
		}
		return nil, status.Errorf(codes.Internal, "issue certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})

	endpoint := s.ServerEndpoint
	if endpoint == "" {
		endpoint = "varroa-varroa-gateway.varroa-system.svc.cluster.local:9090"
	}

	return &mitev1.RegisterResponse{
		CertificatePem: string(certPEM),
		CaPem:          string(s.ca.CAPEM()),
		ServerEndpoint: endpoint,
	}, nil
}

// CommandStream handles the bidirectional streaming RPC for mite
// command-and-control. Mites send heartbeats, state snapshots, and command
// results; the server sends desired-state commands.
func (s *Server) CommandStream(stream mitev1.Mite_CommandStreamServer) error {
	// The first message from the mite must be a hello.
	firstMsg, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.Internal, "recv hello: %v", err)
	}

	hello := firstMsg.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be hello")
	}

	// Extract client certificate from TLS peer info.
	p, ok := peer.FromContext(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "no peer info")
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return status.Error(codes.Unauthenticated, "no client certificate")
	}

	// Defense-in-depth: explicitly verify the presented chain against our CA rather
	// than trusting PeerCertificates[0] (ClientAuth is VerifyClientCertIfGiven), and
	// read the CN from the VERIFIED leaf.
	rawCerts := make([][]byte, len(tlsInfo.State.PeerCertificates))
	for i, cert := range tlsInfo.State.PeerCertificates {
		rawCerts[i] = cert.Raw
	}
	leaf, err := s.ca.VerifyClientCert(rawCerts)
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "verify client certificate: %v", err)
	}
	cn := leaf.Subject.CommonName

	// Parse "controllerName.namespace" from the certificate CN.
	controllerName, namespace, err := parseCN(cn)
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "invalid certificate CN: %v", err)
	}

	if s.Logger != nil {
		s.Logger.Info("mite stream connected", "controller", controllerName, "namespace", namespace, "version", hello.Version)
	}

	// Determine the handler. If none is set, default to RegistryHandler.
	handler := s.streamHandler
	if handler == nil {
		rh := NewRegistryHandler(s.reg, s.broadcaster)
		rh.Logger = s.Logger
		handler = rh
	}

	// Create a sender function bound to this stream, protected by a mutex
	// so background goroutines can send safely.
	var sendMu sync.Mutex
	send := func(msg *mitev1.OperatorMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		if ds := msg.GetDesiredState(); ds != nil {
			span := trace.SpanFromContext(stream.Context())
			span.AddEvent("desiredState.push", trace.WithAttributes(attribute.String("command_id", ds.CommandId)))
		}
		return stream.Send(msg)
	}

	connToken := handler.OnConnect(controllerName, namespace, hello.Version, leaf.NotAfter, send, stream)
	defer handler.OnDisconnect(controllerName, namespace, connToken)

	// Read loop: process incoming messages from the mite.
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return status.Errorf(codes.Internal, "recv: %v", err)
		}

		// The mite may have reconnected while this stream was still open. Its
		// watch goroutines were already cancelled by the replacement's
		// OnConnect, but the read loop keeps dispatching, so a late heartbeat,
		// snapshot, plugin inventory or command result from here writes stale
		// data into state the replacement now owns — a Connected controller
		// reporting a dead connection's version or health, with nothing marking
		// it as stale. Gate once, centrally: end the stream instead of
		// dispatching. A superseded stream that then goes silent still holds
		// its transport until the peer closes it or the transport fails —
		// keepalive only reaps a peer that stops answering pings, so a
		// duplicate client that keeps the connection healthy but says nothing
		// lingers. It writes nothing, which is the part that mattered. The
		// deferred OnDisconnect is
		// token-guarded and correctly no-ops for a superseded stream.
		//
		// This is a check, not a reservation: a supersede landing between the
		// check and the handler's write still lets this one message through.
		// Closing that window means holding a per-controller lock across the
		// handler's NATS publish and KV Put, which AGENTS.md rules out for the
		// presence path — one slow JetStream write would stall every
		// controller's heartbeat. Nor is it the real bound: a message already
		// in flight when the replacement connects is equally stale and no gate
		// can see it. What this removes is the unbounded case, a superseded
		// stream dispatching for the life of the gateway pod; what remains is
		// at most one message and self-heals on the live mite's next heartbeat
		// (15s).
		if !handler.IsCurrentConnection(controllerName, namespace, connToken) {
			if s.Logger != nil {
				s.Logger.Info("closing superseded mite stream",
					"controller", controllerName, "namespace", namespace, "token", connToken)
			}
			return status.Error(codes.Aborted, "stream superseded by a newer connection")
		}

		switch m := msg.Message.(type) {
		case *mitev1.MiteMessage_Heartbeat:
			handler.OnHeartbeat(controllerName, namespace, m.Heartbeat)

		case *mitev1.MiteMessage_StateSnapshot:
			handler.OnSnapshot(controllerName, namespace, m.StateSnapshot)

		case *mitev1.MiteMessage_CommandResult:
			handler.OnCommandResult(controllerName, namespace, m.CommandResult)

		case *mitev1.MiteMessage_TokenRefreshRequest:
			// First notify the handler so it can forward to the bus if needed.
			handler.OnTokenRefreshRequest(controllerName, namespace, m.TokenRefreshRequest)
			// If a grant function is configured, mint and send a TokenGrant directly.
			if s.TokenGrantFunc != nil {
				token, exp, err := s.TokenGrantFunc(controllerName, namespace)
				if err != nil {
					if s.Logger != nil {
						s.Logger.Error("token grant failed", "controller", controllerName, "namespace", namespace, "error", err)
					}
				} else {
					grant := &mitev1.OperatorMessage{
						Message: &mitev1.OperatorMessage_TokenGrant{
							TokenGrant: &mitev1.TokenGrant{
								MiteJenkinsToken:    token,
								MiteJenkinsTokenExp: exp,
							},
						},
					}
					if sndErr := send(grant); sndErr != nil {
						if s.Logger != nil {
							s.Logger.Error("token grant send failed", "controller", controllerName, "namespace", namespace, "error", sndErr)
						}
					}
				}
			}

		case *mitev1.MiteMessage_ObservabilityReport:
			handler.OnObservabilityReport(controllerName, namespace, m.ObservabilityReport)

		case *mitev1.MiteMessage_ContentResponse:
			if cr := m.ContentResponse; cr != nil {
				handler.OnContentResponse(cr.RequestId, cr)
			}

		case *mitev1.MiteMessage_JenkinsActivity:
			if ja := m.JenkinsActivity; ja != nil {
				handler.OnJenkinsActivity(controllerName, namespace, ja)
			}

		case *mitev1.MiteMessage_PluginInventory:
			if pi := m.PluginInventory; pi != nil {
				handler.OnPluginInventory(controllerName, namespace, pi)
			}
		}
	}
}

// Ensure Server implements the MiteServer interface.
var _ mitev1.MiteServer = (*Server)(nil)
