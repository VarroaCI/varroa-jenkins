package ca

import (
	"bytes"
	"testing"
)

func TestBootstrapHMACKey_DeterministicAndDistinct(t *testing.T) {
	c, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	k1 := c.BootstrapHMACKey()
	k2 := c.BootstrapHMACKey()
	if len(k1) != 32 {
		t.Fatalf("BootstrapHMACKey length = %d, want 32", len(k1))
	}
	if !bytes.Equal(k1, k2) {
		t.Error("BootstrapHMACKey must be deterministic for a given CA")
	}

	// Must not equal the raw signing key material (the whole point of deriving it).
	if bytes.Equal(k1, c.PrivateKey().Seed()) {
		t.Error("BootstrapHMACKey must differ from the ed25519 seed")
	}
	if bytes.Equal(k1, []byte(c.PrivateKey())) {
		t.Error("BootstrapHMACKey must differ from the private key bytes")
	}

	// A different CA must derive a different key.
	other, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, other.BootstrapHMACKey()) {
		t.Error("distinct CAs must derive distinct bootstrap HMAC keys")
	}
}
