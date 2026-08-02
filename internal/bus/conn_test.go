package bus

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// generateTestCA returns a PEM-encoded self-signed CA certificate for testing.
func generateTestCA(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

// TestConnectConfigNoAuth tests that an empty Config preserves existing
// behaviour (no TLS, no auth, default inbox) via a real connection.
func TestConnectConfigNoAuth(t *testing.T) {
	s := startTestServer(t)
	conn, err := Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	nc := conn.nc
	if nc.Opts.TLSConfig != nil {
		t.Error("expected nil TLSConfig")
	}
	if nc.Opts.User != "" {
		t.Errorf("expected empty User, got %q", nc.Opts.User)
	}
	if nc.Opts.InboxPrefix != "" {
		t.Errorf("expected empty InboxPrefix, got %q", nc.Opts.InboxPrefix)
	}
}

// TestConnectConfigNoAuthExplicit tests that an explicit zero Config also preserves behaviour.
func TestConnectConfigNoAuthExplicit(t *testing.T) {
	s := startTestServer(t)
	conn, err := Connect(s.ClientURL(), Config{})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	nc := conn.nc
	if nc.Opts.TLSConfig != nil {
		t.Error("expected nil TLSConfig")
	}
	if nc.Opts.User != "" {
		t.Errorf("expected empty User, got %q", nc.Opts.User)
	}
	if nc.Opts.InboxPrefix != "" {
		t.Errorf("expected empty InboxPrefix, got %q", nc.Opts.InboxPrefix)
	}
}

// TestConnectConfigCredentials tests that username+password are applied.
func TestConnectConfigCredentials(t *testing.T) {
	s := startTestServer(t)
	conn, err := Connect(s.ClientURL(), Config{Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	nc := conn.nc
	if nc.Opts.User != "alice" {
		t.Errorf("expected User 'alice', got %q", nc.Opts.User)
	}
	if nc.Opts.Password != "secret" {
		t.Errorf("expected Password 'secret', got %q", nc.Opts.Password)
	}
}

// TestConnectConfigInboxPrefix tests that a custom inbox prefix is applied.
func TestConnectConfigInboxPrefix(t *testing.T) {
	s := startTestServer(t)
	conn, err := Connect(s.ClientURL(), Config{InboxPrefix: "_INBOX_test"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	nc := conn.nc
	if nc.Opts.InboxPrefix != "_INBOX_test" {
		t.Errorf("expected InboxPrefix '_INBOX_test', got %q", nc.Opts.InboxPrefix)
	}
}

// TestConnectConfigTLSCABytes validates that CABytes option composition works
// by applying the option function directly to a default Options struct.
func TestConnectConfigTLSCABytes(t *testing.T) {
	caPEM := generateTestCA(t)
	// Build the options the same way Connect does.
	opts := nats.GetDefaultOptions()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to parse CA PEM")
	}
	opt := nats.Secure(&tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	})
	if err := opt(&opts); err != nil {
		t.Fatalf("option: %v", err)
	}
	if opts.TLSConfig == nil {
		t.Fatal("expected non-nil TLSConfig")
	}
	if opts.TLSConfig.RootCAs == nil {
		t.Fatal("expected non-nil RootCAs in TLSConfig")
	}
	if !opts.Secure {
		t.Error("expected Secure=true")
	}
}

// TestConnectConfigTLSCAFile validates that CAFile option composition works.
func TestConnectConfigTLSCAFile(t *testing.T) {
	caPEM := generateTestCA(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := nats.GetDefaultOptions()
	opt := nats.RootCAs(caPath)
	if err := opt(&opts); err != nil {
		t.Fatalf("option: %v", err)
	}
	if !opts.Secure {
		t.Error("expected Secure=true")
	}
	if opts.RootCAsCB == nil {
		t.Error("expected non-nil RootCAsCB")
	}
}

// TestConnectConfigAllCombined verifies all options compose correctly together.
func TestConnectConfigAllCombined(t *testing.T) {
	s := startTestServer(t)
	conn, err := Connect(s.ClientURL(), Config{
		Username:    "combined",
		Password:    "secret",
		InboxPrefix: "_INBOX_combined",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	nc := conn.nc
	if nc.Opts.User != "combined" {
		t.Errorf("expected User 'combined', got %q", nc.Opts.User)
	}
	if nc.Opts.Password != "secret" {
		t.Errorf("expected Password 'secret', got %q", nc.Opts.Password)
	}
	if nc.Opts.InboxPrefix != "_INBOX_combined" {
		t.Errorf("expected InboxPrefix '_INBOX_combined', got %q", nc.Opts.InboxPrefix)
	}
}
