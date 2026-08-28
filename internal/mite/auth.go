package mite

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/varroaci/varroa-jenkins/internal/ca"
)

// contextKey is used to prevent collisions with keys defined in other packages.
type contextKey string

const (
	miteControllerKey contextKey = "mite-controller"
	miteNamespaceKey  contextKey = "mite-namespace"
)

// ControllerFromContext extracts the controller name from the context.
// Returns an empty string if not present.
func ControllerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(miteControllerKey).(string)
	return v
}

// NamespaceFromContext extracts the namespace from the context.
// Returns an empty string if not present.
func NamespaceFromContext(ctx context.Context) string {
	v, _ := ctx.Value(miteNamespaceKey).(string)
	return v
}

// AuthInterceptor returns a gRPC unary interceptor that verifies client
// certificates against the given CA.
func AuthInterceptor(certAuth *ca.CA) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// Skip the Register RPC; it performs its own auth via bootstrap tokens
		// or client certificate verification.
		if info.FullMethod == "/mitev1.Mite/Register" {
			return handler(ctx, req)
		}

		p, ok := peer.FromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "no peer info")
		}

		tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "no TLS info")
		}

		if len(tlsInfo.State.PeerCertificates) == 0 {
			return nil, status.Error(codes.Unauthenticated, "no client certificate")
		}

		// Convert certificates to raw DER bytes for verification.
		rawCerts := make([][]byte, len(tlsInfo.State.PeerCertificates))
		for i, cert := range tlsInfo.State.PeerCertificates {
			rawCerts[i] = cert.Raw
		}

		leaf, err := certAuth.VerifyClientCert(rawCerts)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "verify client cert: %v", err)
		}

		// The certificate CN uses the format "controllerName.namespace".
		cn := leaf.Subject.CommonName
		controllerName, namespace, err := parseCN(cn)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "invalid certificate CN: %v", err)
		}

		ctx = context.WithValue(ctx, miteControllerKey, controllerName)
		ctx = context.WithValue(ctx, miteNamespaceKey, namespace)

		return handler(ctx, req)
	}
}

// parseCN splits a CommonName in "controllerName.namespace" format on the
// last dot, so that controller names containing dots are handled correctly.
func parseCN(cn string) (controllerName, namespace string, err error) {
	for i := len(cn) - 1; i >= 0; i-- {
		if cn[i] == '.' {
			return cn[:i], cn[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("CN %q does not contain a dot separator", cn)
}
