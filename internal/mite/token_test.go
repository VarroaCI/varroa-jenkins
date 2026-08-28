package mite

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestGenerateValidateToken_RoundTrip(t *testing.T) {
	ts := NewTokenSigner([]byte("test-key-0123456789"))
	tok, err := ts.GenerateToken("ctrl", "ns", time.Minute)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	claims, err := ts.ValidateToken(tok, "ctrl", "ns")
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.Version != "v2" || claims.Controller != "ctrl" || claims.Namespace != "ns" {
		t.Errorf("unexpected claims: %+v", claims)
	}
	if claims.JTI == "" {
		t.Error("claims.JTI must be non-empty")
	}
}

func TestGenerateToken_UniqueJTI(t *testing.T) {
	ts := NewTokenSigner([]byte("k"))
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := ts.GenerateToken("c", "n", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		c, err := ts.ValidateToken(tok, "c", "n")
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.JTI] {
			t.Fatalf("duplicate jti %q", c.JTI)
		}
		seen[c.JTI] = true
	}
}

func TestValidateToken_Rejections(t *testing.T) {
	ts := NewTokenSigner([]byte("key"))
	good, _ := ts.GenerateToken("ctrl", "ns", time.Minute)

	t.Run("wrong controller", func(t *testing.T) {
		if _, err := ts.ValidateToken(good, "other", "ns"); err == nil {
			t.Error("expected identity-mismatch error")
		}
	})
	t.Run("wrong namespace", func(t *testing.T) {
		if _, err := ts.ValidateToken(good, "ctrl", "other"); err == nil {
			t.Error("expected identity-mismatch error")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		other := NewTokenSigner([]byte("different"))
		if _, err := other.ValidateToken(good, "ctrl", "ns"); err == nil {
			t.Error("expected signature error")
		}
	})
	t.Run("tampered payload", func(t *testing.T) {
		raw, _ := base64.RawURLEncoding.DecodeString(good)
		raw[0] ^= 0xff
		tampered := base64.RawURLEncoding.EncodeToString(raw)
		if _, err := ts.ValidateToken(tampered, "ctrl", "ns"); err == nil {
			t.Error("expected signature error on tamper")
		}
	})
	t.Run("expired", func(t *testing.T) {
		expired, _ := ts.GenerateToken("ctrl", "ns", -time.Second)
		if _, err := ts.ValidateToken(expired, "ctrl", "ns"); err == nil {
			t.Error("expected expiry error")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, err := ts.ValidateToken("not-base64-!!!", "ctrl", "ns"); err == nil {
			t.Error("expected decode error")
		}
	})
}

func TestIsCurrentTokenFormat(t *testing.T) {
	ts := NewTokenSigner([]byte("key"))
	good, _ := ts.GenerateToken("ctrl", "ns", time.Minute)
	if !IsCurrentTokenFormat(good) {
		t.Error("valid v2 token should be current format")
	}
	expired, _ := ts.GenerateToken("ctrl", "ns", -time.Second)
	if IsCurrentTokenFormat(expired) {
		t.Error("expired token must not be current format")
	}
	if IsCurrentTokenFormat("garbage") {
		t.Error("garbage must not be current format")
	}
}

func TestParseTokenIdentity(t *testing.T) {
	ts := NewTokenSigner([]byte("key"))
	tok, _ := ts.GenerateToken("my-ctrl", "my-ns", time.Minute)
	c, n, err := ParseTokenIdentity(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c != "my-ctrl" || n != "my-ns" {
		t.Errorf("got (%q,%q), want (my-ctrl,my-ns)", c, n)
	}
}

func TestInMemoryConsumedStore_ReplayRejected(t *testing.T) {
	store := newInMemoryConsumedStore()
	defer store.stop()
	exp := time.Now().Add(time.Minute)
	if !store.Consume("jti-1", exp) {
		t.Fatal("first Consume must succeed")
	}
	if store.Consume("jti-1", exp) {
		t.Fatal("replay of same jti must be rejected")
	}
	if !store.Consume("jti-2", exp) {
		t.Fatal("distinct jti must succeed")
	}
}
