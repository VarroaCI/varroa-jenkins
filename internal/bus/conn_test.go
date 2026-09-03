package bus

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestConnectConfigPasswordFile verifies the option plumbing when a password
// file is configured: the credential is supplied through a handler read per
// connect attempt, the static password stays unset, and auth errors no longer
// abort reconnects.
func TestConnectConfigPasswordFile(t *testing.T) {
	s := startTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := Connect(s.ClientURL(), Config{Username: "alice", Password: "ignored", PasswordFile: path})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	nc := conn.nc
	if nc.Opts.UserInfo == nil {
		t.Fatal("expected a UserInfo handler when PasswordFile is set")
	}
	user, pass := nc.Opts.UserInfo()
	if user != "alice" || pass != "from-file" {
		t.Fatalf("UserInfo() = (%q,%q), want (alice, from-file)", user, pass)
	}
	if nc.Opts.Password != "" {
		t.Fatalf("static Password must not be set when PasswordFile is used, got %q", nc.Opts.Password)
	}
	if !nc.Opts.IgnoreAuthErrorAbort {
		t.Fatal("expected IgnoreAuthErrorAbort")
	}
}

// TestConnectConfigPasswordFileMissing verifies Connect fails fast when the
// configured password file cannot be read at startup.
func TestConnectConfigPasswordFileMissing(t *testing.T) {
	s := startTestServer(t)
	_, err := Connect(s.ClientURL(), Config{Username: "alice", PasswordFile: filepath.Join(t.TempDir(), "absent")})
	if err == nil {
		t.Fatal("expected Connect to fail when the password file cannot be read at startup")
	}
}

// TestConnConnected verifies Connected() tracks the underlying connection.
func TestConnConnected(t *testing.T) {
	s := startTestServer(t)
	conn, err := Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !conn.Connected() {
		t.Fatal("expected Connected() true after Connect")
	}
	conn.Close()
	if conn.Connected() {
		t.Fatal("expected Connected() false after Close")
	}
}

// syncBuffer is an io.Writer safe for the NATS callback goroutines that write
// log records concurrently with the test reading them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConnectConfigLogger proves Config.Logger reaches the connection handlers.
// The logger must arrive through Connect rather than being assigned afterwards:
// the handlers run on NATS goroutines, so a later assignment races them.
func TestConnectConfigLogger(t *testing.T) {
	s := startTestServer(t)
	var out syncBuffer
	logger := slog.New(slog.NewTextHandler(&out, nil))

	conn, err := Connect(s.ClientURL(), Config{Logger: logger})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	// Dropping the server fires DisconnectErrHandler on a library goroutine.
	s.Shutdown()
	waitFor(t, 10*time.Second, func() bool {
		return strings.Contains(out.String(), "nats disconnected")
	})
	// A lost server is a reconnect, not a permanent close: MaxReconnects is
	// unlimited, so the connection keeps retrying and the permanent-close
	// diagnostic must stay silent.
	if strings.Contains(out.String(), "closed permanently") {
		t.Fatalf("a server outage must not log a permanent close, got %q", out.String())
	}
}

// TestCloseDoesNotLogPermanentClose pins that a graceful shutdown stays quiet.
// nats.go fires the closed callback for a client-initiated Close unless it is
// told not to, which would make every pod shutdown log the error that
// troubleshooting docs treat as an unrecoverable connection.
func TestCloseDoesNotLogPermanentClose(t *testing.T) {
	s := startTestServer(t)
	var out syncBuffer
	conn, err := Connect(s.ClientURL(), Config{Logger: slog.New(slog.NewTextHandler(&out, nil))})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	conn.Close()

	// The closed callback runs on a library goroutine, so Close returns before
	// it would fire. An absence cannot be polled for, so wait a fixed window
	// well beyond the observed dispatch latency before asserting.
	time.Sleep(time.Second)

	// Control: the logger is installed and does emit on Close, so the absence
	// asserted below is a suppressed callback and not a dead logger.
	if !strings.Contains(out.String(), "connection closed") {
		t.Fatalf("expected Close to log through the Config logger, got %q", out.String())
	}
	if strings.Contains(out.String(), "closed permanently") {
		t.Fatalf("a graceful Close must not log a permanent close, got %q", out.String())
	}
}

// TestConnectConfigLoggerOnly verifies a Config carrying nothing but a Logger
// is still honoured, even though it reports IsZero.
func TestConnectConfigLoggerOnly(t *testing.T) {
	if !(Config{Logger: slog.Default()}).IsZero() {
		t.Fatal("a Config with only a Logger should report IsZero: it sets no connection option")
	}
	s := startTestServer(t)
	var out syncBuffer
	conn, err := Connect(s.ClientURL(), Config{Logger: slog.New(slog.NewTextHandler(&out, nil))})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	conn.Close()
	if !strings.Contains(out.String(), "connection closed") {
		t.Fatalf("expected the Config logger to be installed, got %q", out.String())
	}
}

// TestConnectConfigPasswordFileWithoutUsername verifies Connect refuses a
// password file with no username. The file here is readable, so the rejection
// has to come from the missing username rather than the unreadable-file check.
func TestConnectConfigPasswordFileWithoutUsername(t *testing.T) {
	s := startTestServer(t)
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Connect(s.ClientURL(), Config{PasswordFile: path})
	if err == nil {
		t.Fatal("expected Connect to fail when PasswordFile is set without Username")
	}
	if !strings.Contains(err.Error(), "requires Username") {
		t.Fatalf("error should name the missing Username, got %v", err)
	}
}
