package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"testing"
	"time"
)

// generateTestCAPEM creates a self-signed CA certificate and returns its PEM
// encoding. The key material is ephemeral and insecure — for testing only.
func generateTestCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(1 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

func TestBootstrapTLSConfig_CAPEMSet(t *testing.T) {
	caPEM := generateTestCAPEM(t)
	agent := &Agent{
		cfg:    Config{CAPEM: string(caPEM)},
		Logger: slog.Default(),
	}

	tlsCfg, err := agent.bootstrapTLSConfig()
	if err != nil {
		t.Fatalf("bootstrapTLSConfig with valid CAPEM returned error: %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("bootstrapTLSConfig returned nil config")
	}
	if tlsCfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be false when CAPEM is set")
	}
	if tlsCfg.RootCAs == nil {
		t.Error("RootCAs should be non-nil when CAPEM is set")
	}
	if tlsCfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %d, want %d (TLS 1.3)", tlsCfg.MinVersion, tls.VersionTLS13)
	}
	// ServerName must NOT be set — gRPC derives it from the dial target,
	// and the gateway cert already has the endpoint host as a SAN.
	if tlsCfg.ServerName != "" {
		t.Errorf("ServerName should not be set (gRPC derives it), got %q", tlsCfg.ServerName)
	}
}

func TestBootstrapTLSConfig_MalformedCAPEM(t *testing.T) {
	// PEM wrapping garbage bytes that won't parse as a cert.
	badPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-a-valid-der")})
	agent := &Agent{
		cfg:    Config{CAPEM: string(badPEM)},
		Logger: slog.Default(),
	}

	_, err := agent.bootstrapTLSConfig()
	if err == nil {
		t.Fatal("bootstrapTLSConfig with malformed CAPEM should return error")
	}
}

func TestBootstrapTLSConfig_CAPEMEmpty(t *testing.T) {
	// Fail-closed: with no CA PEM, bootstrap must refuse rather than fall back to an
	// insecure (unverified) connection — the #232 hole must stay closed.
	agent := &Agent{
		cfg:    Config{CAPEM: ""},
		Logger: slog.Default(),
	}

	tlsCfg, err := agent.bootstrapTLSConfig()
	if err == nil {
		t.Fatal("bootstrapTLSConfig with empty CAPEM must return an error, got nil")
	}
	if tlsCfg != nil {
		t.Errorf("bootstrapTLSConfig must return a nil config on error, got %+v", tlsCfg)
	}
}

// TestConnectWithCertNotBrokenByRefactor is a smoke check that the mTLS
// (post-registration) branch in connect() still compiles and is reachable.
// A full unit test of that branch would require writing ca.pem to the
// package-level constant path varroaMiteDir, which is not hermetic. The
// mTLS path is unchanged by this task and is exercised by integration tests.
func TestConnectWithCertNotBrokenByRefactor(t *testing.T) {
	// Construct an agent with a non-nil cert (dummy, not a real keypair).
	// connect() will hit the a.cert != nil branch, try to read ca.pem
	// from varroaMiteDir, and fail with an fs error — not a crash.
	agent := &Agent{
		cfg:     Config{CAPEM: ""},
		cert:    &tls.Certificate{},
		Logger:  slog.Default(),
		miteDir: t.TempDir(), // hermetic: never read the real shared path
	}
	// This should NOT panic. It will return an error because ca.pem
	// doesn't exist at the constant path, but it won't crash.
	err := agent.connect(context.TODO())
	if err == nil {
		t.Error("expected error (no ca.pem on disk), got nil")
	}
}
