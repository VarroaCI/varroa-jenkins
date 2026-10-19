package token

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

func mustMint(t *testing.T) (Value, string) {
	t.Helper()
	key := []byte(strings.Repeat("k", 32))
	v, err := Mint(key, "agent:ns/team/agent", types.UID("uid-1"), uuid.New(), time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	v.SetHeader(h)
	return v, strings.TrimPrefix(h.Get("Authorization"), "Bearer ")
}

// TestValueNeverPrints checks that no formatting or encoding path writes any
// part of the token.
func TestValueNeverPrints(t *testing.T) {
	v, raw := mustMint(t)
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("token", "value", v)
	jsonOut, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	textOut, err := v.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{
		"%v":     fmt.Sprintf("%v", v),
		"%s":     fmt.Sprintf("%s", v),
		"%+v":    fmt.Sprintf("%+v", v),
		"%#v":    fmt.Sprintf("%#v", v),
		"Sprint": fmt.Sprint(v),
		"json":   string(jsonOut),
		"text":   string(textOut),
		"slog":   logBuf.String(),
	}
	for name, out := range outputs {
		for _, seg := range parts {
			if strings.Contains(out, seg) {
				t.Errorf("%s leaks part of the token", name)
			}
		}
		if !strings.Contains(out, Redacted) {
			t.Errorf("%s does not carry the placeholder: %q", name, out)
		}
	}
}

// TestMintClaims decodes the minted token and checks each claim and the
// HMAC signature.
func TestMintClaims(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	now := time.Unix(1700000000, 0)
	sid := uuid.New()
	v, err := Mint(key, "agent:ns/team/agent", types.UID("uid-1"), sid, now)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	v.SetHeader(h)
	parts := strings.Split(strings.TrimPrefix(h.Get("Authorization"), "Bearer "), ".")

	var hdr header
	decodeSegment(t, parts[0], &hdr)
	if hdr.Alg != "HS256" || hdr.Typ != "JWT" || hdr.Kid != accept.TokenKeyID {
		t.Errorf("header = %+v", hdr)
	}
	var c Claims
	decodeSegment(t, parts[1], &c)
	checks := map[string][2]any{
		"aud": {c.Aud, accept.TokenAudience},
		"iss": {c.Iss, accept.TokenIssuer},
		"sub": {c.Sub, "agent:ns/team/agent"},
		"uid": {c.UID, "uid-1"},
		"iat": {c.Iat, now.Unix()},
		"exp": {c.Exp, now.Add(15 * time.Minute).Unix()},
		"sid": {c.SID, sid.String()},
	}
	for name, pair := range checks {
		if pair[0] != pair[1] {
			t.Errorf("claim %s = %v, want %v", name, pair[0], pair[1])
		}
	}
	if len(c.JTI) != 32 {
		t.Errorf("jti = %q, want 16 bytes in hex", c.JTI)
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if parts[2] != want {
		t.Error("signature does not verify with the key")
	}
	if !v.ExpiresAt().Equal(now.Add(15 * time.Minute)) {
		t.Errorf("ExpiresAt = %v", v.ExpiresAt())
	}
}

func decodeSegment(t *testing.T, seg string, into any) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

func TestMintRejectsShortKey(t *testing.T) {
	if _, err := Mint([]byte("short"), "agent:a/b/c", "u", uuid.New(), time.Now()); err == nil {
		t.Fatal("a short key must be refused")
	}
}

func TestReadKeyLength(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKey(short); err == nil || strings.Contains(err.Error(), "tiny") {
		t.Fatalf("short key: err=%v, want refusal that omits the bytes", err)
	}
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte(strings.Repeat("x", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadKey(ok); err != nil || len(b) != 32 {
		t.Fatalf("ReadKey = %d bytes, %v", len(b), err)
	}
}
