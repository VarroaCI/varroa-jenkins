package apikey

import (
	"encoding/base64"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	secret := "test-secret-value"
	hash := Hash(secret)
	if hash == "" {
		t.Fatal("Hash returned empty")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(hash)
	if err != nil {
		t.Fatalf("Hash not valid base64url: %v", err)
	}
	if len(decoded) != 32 {
		t.Errorf("Hash length = %d bytes, want 32", len(decoded))
	}
	if !Verify(secret, hash) {
		t.Error("Verify(secret, hash) returned false")
	}
	if Verify("wrong-secret", hash) {
		t.Error("Verify(wrong, hash) returned true")
	}
	if Verify(secret, "badhash") {
		t.Error("Verify(secret, badhash) returned true")
	}
}
