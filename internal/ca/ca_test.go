package ca

import (
	"crypto/ed25519"
	"testing"
)

func TestIssueMiteCert_CellInstanceRoundTrips(t *testing.T) {
	c, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cert, err := c.IssueMiteCert("ctrl1", "team-a", pub, "pod-uid-123")
	if err != nil {
		t.Fatalf("IssueMiteCert: %v", err)
	}

	if len(cert.URIs) != 1 {
		t.Fatalf("cert.URIs = %v, want exactly one URI SAN", cert.URIs)
	}
	if got, want := cert.URIs[0].String(), "urn:varroa:cell-instance:pod-uid-123"; got != want {
		t.Fatalf("URI SAN = %q, want %q", got, want)
	}

	if got := CellInstanceFromCert(cert); got != "pod-uid-123" {
		t.Fatalf("CellInstanceFromCert = %q, want %q", got, "pod-uid-123")
	}
}

func TestIssueMiteCert_EmptyCellInstanceAddsNoURISAN(t *testing.T) {
	c, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cert, err := c.IssueMiteCert("ctrl1", "team-a", pub, "")
	if err != nil {
		t.Fatalf("IssueMiteCert: %v", err)
	}

	if len(cert.URIs) != 0 {
		t.Fatalf("cert.URIs = %v, want none for an empty cell instance", cert.URIs)
	}
	if got := CellInstanceFromCert(cert); got != "" {
		t.Fatalf("CellInstanceFromCert = %q, want empty", got)
	}
}
