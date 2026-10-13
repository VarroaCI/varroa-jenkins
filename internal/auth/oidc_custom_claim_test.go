package auth

import (
	"context"
	"errors"
	"testing"
)

// TestValidatorVerify_RefusesLocalPrefixInCustomUserClaim checks that a
// configured custom user claim cannot carry the local account prefix.
func TestValidatorVerify_RefusesLocalPrefixInCustomUserClaim(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames([]string{"upn", "sub"}, nil)
	if _, err := v.Verify(context.Background(), sign(map[string]any{"upn": "local:alice"})); !errors.Is(err, ErrLocalPrefixFromIdP) {
		t.Errorf("local: custom user claim: err = %v, want ErrLocalPrefixFromIdP", err)
	}
	if _, err := v.Verify(context.Background(), sign(map[string]any{"upn": "alice@corp"})); err != nil {
		t.Errorf("plain custom user claim: err = %v, want nil", err)
	}
}

// TestValidatorVerify_RefusesLocalPrefixInEmailUserClaim checks that a
// local: value in the email claim is refused when email is a user claim.
func TestValidatorVerify_RefusesLocalPrefixInEmailUserClaim(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames([]string{"email", "sub"}, nil)
	if _, err := v.Verify(context.Background(), sign(map[string]any{"email": "local:alice"})); !errors.Is(err, ErrLocalPrefixFromIdP) {
		t.Errorf("local: email: err = %v, want ErrLocalPrefixFromIdP", err)
	}
	if _, err := v.Verify(context.Background(), sign(map[string]any{"email": "alice@corp.example"})); err != nil {
		t.Errorf("normal email: err = %v, want nil", err)
	}
}

// TestValidatorVerify_RefusesLocalPrefixNormalized checks that surrounding
// space and letter case do not hide the local: prefix.
func TestValidatorVerify_RefusesLocalPrefixNormalized(t *testing.T) {
	v, sign := newTestIssuer(t)
	v.SetClaimNames([]string{"upn", "sub"}, nil)
	if _, err := v.Verify(context.Background(), sign(map[string]any{"upn": " Local:alice"})); !errors.Is(err, ErrLocalPrefixFromIdP) {
		t.Errorf("padded mixed-case upn: err = %v, want ErrLocalPrefixFromIdP", err)
	}
}
