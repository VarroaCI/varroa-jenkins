package auth

import (
	"context"
	"testing"
	"time"
)

// TestTicket_CarriesExtraUserClaims checks that configured custom user claims
// survive the stream ticket round trip.
func TestTicket_CarriesExtraUserClaims(t *testing.T) {
	signer := newTestSigner(t)
	iss := NewTicketIssuer(signer, "https://bff.example", 30*time.Second)
	ver := NewTicketVerifier(signer.PublicKey(), "https://bff.example")

	caller := &Claims{Subject: "alice", Extra: map[string]string{"upn": "alice@corp"}}
	ticket, _, err := iss.Mint(caller, "controller:ns/ctl")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := ver.Verify(context.Background(), ticket, "controller:ns/ctl")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Extra["upn"] != "alice@corp" {
		t.Errorf("Extra = %v, want upn=alice@corp", got.Extra)
	}
	if vals := got.UserValues([]string{"upn"}); len(vals) != 1 || vals[0] != "alice@corp" {
		t.Errorf("UserValues(upn) = %v, want [alice@corp]", vals)
	}
}
