package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/ca"
)

// writeMiteIdentity mints a mite client certificate under caAuth and persists
// cert.pem/key.pem/ca.pem into miteDir, simulating an identity saved by a prior
// mite run. It returns the CA PEM that was written to ca.pem.
func writeMiteIdentity(t *testing.T, miteDir string, caAuth *ca.CA, controllerName, namespace string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate mite key: %v", err)
	}
	cert, err := caAuth.IssueMiteCert(controllerName, namespace, pub)
	if err != nil {
		t.Fatalf("issue mite cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})

	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal mite key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})

	if err := os.MkdirAll(miteDir, 0755); err != nil {
		t.Fatalf("mkdir miteDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(miteDir, "cert.pem"), certPEM, 0600); err != nil {
		t.Fatalf("write cert.pem: %v", err)
	}
	if err := os.WriteFile(filepath.Join(miteDir, "key.pem"), keyPEM, 0600); err != nil {
		t.Fatalf("write key.pem: %v", err)
	}
	if err := os.WriteFile(filepath.Join(miteDir, "ca.pem"), caAuth.CAPEM(), 0600); err != nil {
		t.Fatalf("write ca.pem: %v", err)
	}
}

// TestLoadOrCreateIdentity_StaleCADiscarded asserts that a saved identity
// minted under CA-A must be discarded and re-bootstrapped when the
// current CA (VARROA_CA_PEM) is a DIFFERENT CA-B — as happens after a
// control-plane reinstall regenerates the internal CA. Reuse based on expiry
// alone would loop forever on "certificate signed by unknown authority".
func TestLoadOrCreateIdentity_StaleCADiscarded(t *testing.T) {
	miteDir := t.TempDir()
	caA, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new CA-A: %v", err)
	}
	caB, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new CA-B: %v", err)
	}
	writeMiteIdentity(t, miteDir, caA, "test", "ns")

	// Bootstrap token must be readable so the fall-through bootstrap succeeds.
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("fresh-bootstrap-token"), 0600); err != nil {
		t.Fatal(err)
	}

	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		CAPEM:          string(caB.CAPEM()), // current CA differs from the saved identity's CA-A
		BootstrapFile:  tokenFile,
	})
	agent.Logger = slog.Default()
	agent.miteDir = miteDir

	id, err := agent.loadOrCreateIdentity(context.Background())
	if err != nil {
		t.Fatalf("loadOrCreateIdentity: %v", err)
	}

	if !id.isBootstrap {
		t.Error("expected bootstrap path when saved identity does not chain to the current CA")
	}
	if id.bootstrapToken != "fresh-bootstrap-token" {
		t.Errorf("expected bootstrap token to be read, got %q", id.bootstrapToken)
	}
	if agent.cert != nil {
		t.Error("expected in-memory cert to be cleared after discarding stale identity")
	}
	for _, f := range []string{"cert.pem", "key.pem", "ca.pem"} {
		if _, statErr := os.Stat(filepath.Join(miteDir, f)); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to be removed, stat err = %v", f, statErr)
		}
	}
}

// TestLoadOrCreateIdentity_CurrentCAReused verifies the happy path: a saved,
// unexpired identity that STILL chains to the current CA is reused (no
// bootstrap, files left intact).
func TestLoadOrCreateIdentity_CurrentCAReused(t *testing.T) {
	miteDir := t.TempDir()
	caA, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new CA-A: %v", err)
	}
	writeMiteIdentity(t, miteDir, caA, "test", "ns")

	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		CAPEM:          string(caA.CAPEM()), // current CA matches the saved identity
	})
	agent.Logger = slog.Default()
	agent.miteDir = miteDir

	id, err := agent.loadOrCreateIdentity(context.Background())
	if err != nil {
		t.Fatalf("loadOrCreateIdentity: %v", err)
	}

	if id.isBootstrap {
		t.Error("expected reuse (not bootstrap) when saved identity chains to the current CA")
	}
	if agent.cert == nil {
		t.Error("expected in-memory cert to be retained on reuse")
	}
	for _, f := range []string{"cert.pem", "key.pem", "ca.pem"} {
		if _, statErr := os.Stat(filepath.Join(miteDir, f)); statErr != nil {
			t.Errorf("expected %s to be retained on reuse, stat err = %v", f, statErr)
		}
	}
}

// TestLoadOrCreateIdentity_EmptyCAPEMExpiryOnly verifies the defensive/legacy
// fallback: with VARROA_CA_PEM unset, the mite keeps the historical
// expiry-only reuse behavior and does not discard a saved (unexpired) identity.
func TestLoadOrCreateIdentity_EmptyCAPEMExpiryOnly(t *testing.T) {
	miteDir := t.TempDir()
	caA, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new CA-A: %v", err)
	}
	writeMiteIdentity(t, miteDir, caA, "test", "ns")

	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		CAPEM:          "", // unset → expiry-only reuse
	})
	agent.Logger = slog.Default()
	agent.miteDir = miteDir

	id, err := agent.loadOrCreateIdentity(context.Background())
	if err != nil {
		t.Fatalf("loadOrCreateIdentity: %v", err)
	}

	if id.isBootstrap {
		t.Error("expected expiry-only reuse when CAPEM is empty, got bootstrap")
	}
	if agent.cert == nil {
		t.Error("expected in-memory cert to be retained under expiry-only reuse")
	}
}
