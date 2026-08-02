package auth

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/signing"
)

func newTestSigner(t *testing.T) *signing.Signer {
	t.Helper()
	s, err := signing.New()
	if err != nil {
		t.Fatalf("signing.New: %v", err)
	}
	return s
}

func TestTicket_MintVerifyRoundTrip(t *testing.T) {
	signer := newTestSigner(t)
	iss := NewTicketIssuer(signer, "https://bff.example", 30*time.Second)
	ver := NewTicketVerifier(signer.PublicKey(), "https://bff.example")

	caller := &Claims{Subject: "alice", Email: "a@x", Name: "Alice", Groups: []string{"devs"}}
	ticket, ttl, err := iss.Mint(caller, "controller:ns/ctl")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if ttl != 30 {
		t.Errorf("ttl = %d, want 30", ttl)
	}

	got, err := ver.Verify(context.Background(), ticket, "controller:ns/ctl")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "alice" || got.Name != "Alice" || len(got.Groups) != 1 {
		t.Errorf("claims not carried through: %+v", got)
	}
}

func TestTicket_ScopeMismatchRejected(t *testing.T) {
	signer := newTestSigner(t)
	iss := NewTicketIssuer(signer, "iss", 30*time.Second)
	ver := NewTicketVerifier(signer.PublicKey(), "iss")

	ticket, _, _ := iss.Mint(&Claims{Subject: "a"}, "controller:ns/a")
	if _, err := ver.Verify(context.Background(), ticket, "controller:ns/b"); err == nil {
		t.Error("expected scope-mismatch rejection")
	}
	if _, err := ver.Verify(context.Background(), ticket, "brood"); err == nil {
		t.Error("ticket for a controller must not open the brood stream")
	}
}

func TestTicket_ReplayRejected(t *testing.T) {
	signer := newTestSigner(t)
	iss := NewTicketIssuer(signer, "iss", 30*time.Second)
	ver := NewTicketVerifier(signer.PublicKey(), "iss")

	ticket, _, _ := iss.Mint(&Claims{Subject: "a"}, "brood")
	if _, err := ver.Verify(context.Background(), ticket, "brood"); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if _, err := ver.Verify(context.Background(), ticket, "brood"); err == nil {
		t.Error("replayed ticket must be rejected (single use)")
	}
}

func TestTicket_ExpiredRejected(t *testing.T) {
	signer := newTestSigner(t)
	iss := NewTicketIssuer(signer, "iss", time.Millisecond) // very short TTL
	ver := NewTicketVerifier(signer.PublicKey(), "iss")

	ticket, _, _ := iss.Mint(&Claims{Subject: "a"}, "brood")
	time.Sleep(1100 * time.Millisecond) // exp is unix-second granularity
	if _, err := ver.Verify(context.Background(), ticket, "brood"); err == nil {
		t.Error("expired ticket must be rejected")
	}
}

func TestTicket_WrongIssuerRejected(t *testing.T) {
	signer := newTestSigner(t)
	iss := NewTicketIssuer(signer, "issuer-A", 30*time.Second)
	ver := NewTicketVerifier(signer.PublicKey(), "issuer-B")

	ticket, _, _ := iss.Mint(&Claims{Subject: "a"}, "brood")
	if _, err := ver.Verify(context.Background(), ticket, "brood"); err == nil {
		t.Error("ticket from a different issuer must be rejected")
	}
}

func TestTicket_NonTicketRejected(t *testing.T) {
	signer := newTestSigner(t)
	ver := NewTicketVerifier(signer.PublicKey(), "iss")
	// A bare JWT (no vst_ prefix) — e.g. a session token presented as a ticket.
	if _, err := ver.Verify(context.Background(), "eyJhbGc.payload.sig", "brood"); err == nil {
		t.Error("a non-ticket token must be rejected")
	}
}

func TestSSEScope(t *testing.T) {
	cases := map[string]string{
		"/api/v1/activity/stream":                  "activity",
		"/api/v1/stream/brood":                     "brood",
		"/api/v1/controllers/ns/ctl/events":        "controller:ns/ctl",
		"/api/v1/controllers/ns/ctl/logs":          "controller:ns/ctl",
		"/api/v1/controllers/ns/ctl":               "",
		"/api/v1/controllers":                      "",
		"/api/v1/settings":                         "",
		"/api/v1/brood-operations/a/b/stream":      "broodop:a/b",
		"/api/v1/brood-operations/ns/name/stream":  "broodop:ns/name",
		"/api/v1/brood-operations":                 "",
		"/api/v1/brood-operations/preview":         "",
		"/api/v1/brood-operations/ns/name":         "",
		"/api/v1/brood-operations/ns/name/suspend": "",
	}
	for path, want := range cases {
		if got := sseScope(path); got != want {
			t.Errorf("sseScope(%q) = %q, want %q", path, got, want)
		}
	}
}
