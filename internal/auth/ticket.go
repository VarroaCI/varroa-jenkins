package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// SSEAudience is the audience claim carried by stream tickets. It is deliberately
// distinct from the dashboard OIDC client-id audience, so a session token cannot be
// used as a ticket and a ticket cannot be used as a session token.
const SSEAudience = "varroa:sse"

// ticketPrefix namespaces stream tickets away from vk_ API keys and bare bearer JWTs,
// letting the middleware cheaply pre-filter before an expensive verify.
const ticketPrefix = "vst_"

// ticketClaims is the payload of a stream ticket JWT.
type ticketClaims struct {
	Iss               string   `json:"iss"`
	Aud               string   `json:"aud"`
	Sub               string   `json:"sub"`
	Email             string   `json:"email,omitempty"`
	Name              string   `json:"name,omitempty"`
	PreferredUsername string   `json:"preferred_username,omitempty"`
	Groups            []string `json:"groups,omitempty"`
	Scope             string   `json:"scope"`
	Iat               int64    `json:"iat"`
	Exp               int64    `json:"exp"`
	JTI               string   `json:"jti"`
}

// TicketIssuer mints short-lived, single-purpose SSE stream tickets.
type TicketIssuer struct {
	signer *signing.Signer
	iss    string
	ttl    time.Duration
}

// NewTicketIssuer creates a TicketIssuer. ttl <= 0 defaults to 30s.
func NewTicketIssuer(signer *signing.Signer, iss string, ttl time.Duration) *TicketIssuer {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &TicketIssuer{signer: signer, iss: iss, ttl: ttl}
}

// Mint issues a ticket bound to the caller's identity and the requested stream scope.
// It returns the wire form (vst_<jws>) and the TTL in whole seconds.
func (i *TicketIssuer) Mint(c *Claims, scope string) (string, int, error) {
	if c == nil {
		return "", 0, fmt.Errorf("cannot mint ticket without claims")
	}
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", 0, fmt.Errorf("generate jti: %w", err)
	}
	now := time.Now()
	tc := ticketClaims{
		Iss:               i.iss,
		Aud:               SSEAudience,
		Sub:               c.Subject,
		Email:             c.Email,
		Name:              c.Name,
		PreferredUsername: c.PreferredUsername,
		Groups:            c.Groups,
		Scope:             scope,
		Iat:               now.Unix(),
		Exp:               now.Add(i.ttl).Unix(),
		JTI:               base64.RawURLEncoding.EncodeToString(jtiBytes),
	}
	jws, err := i.signer.SignJWT(tc)
	if err != nil {
		return "", 0, fmt.Errorf("sign ticket: %w", err)
	}
	return ticketPrefix + jws, int(i.ttl.Seconds()), nil
}

// TicketVerifier verifies stream tickets and enforces scope + one-time use.
type TicketVerifier struct {
	pub  *rsa.PublicKey
	iss  string
	seen *jtiCache
}

// NewTicketVerifier creates a TicketVerifier keyed on the issuer's public key.
func NewTicketVerifier(pub *rsa.PublicKey, iss string) *TicketVerifier {
	return &TicketVerifier{pub: pub, iss: iss, seen: newJTICache()}
}

// Verify validates a ticket's signature, audience, issuer, and expiry, checks that it
// was minted for requiredScope, and enforces best-effort single use. On success it
// returns the caller's claims carried inside the ticket.
func (v *TicketVerifier) Verify(_ context.Context, wire, requiredScope string) (*Claims, error) {
	if !strings.HasPrefix(wire, ticketPrefix) {
		return nil, fmt.Errorf("not a stream ticket")
	}
	jws := strings.TrimPrefix(wire, ticketPrefix)
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed ticket")
	}

	// Verify RS256 signature over "header.payload" (matches signing.Signer.SignJWT).
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(v.pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, fmt.Errorf("invalid ticket signature")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var tc ticketClaims
	if err := json.Unmarshal(payload, &tc); err != nil {
		return nil, fmt.Errorf("unmarshal ticket: %w", err)
	}

	if tc.Aud != SSEAudience {
		return nil, fmt.Errorf("wrong ticket audience")
	}
	if v.iss != "" && tc.Iss != v.iss {
		return nil, fmt.Errorf("wrong ticket issuer")
	}
	if time.Now().Unix() > tc.Exp {
		return nil, fmt.Errorf("ticket expired")
	}
	if tc.Scope != requiredScope {
		return nil, fmt.Errorf("ticket scope %q does not match stream %q", tc.Scope, requiredScope)
	}
	if !v.seen.consume(tc.JTI, time.Unix(tc.Exp, 0)) {
		return nil, fmt.Errorf("ticket already used")
	}

	return &Claims{
		Subject:           tc.Sub,
		Email:             tc.Email,
		Name:              tc.Name,
		PreferredUsername: tc.PreferredUsername,
		Groups:            tc.Groups,
	}, nil
}

// jtiCache is a small, mutex-guarded, per-replica replay guard for ticket jtis.
// It is ephemeral: a ticket replayed against a different replica or after restart
// within its brief TTL is not caught (documented tradeoff).
type jtiCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newJTICache() *jtiCache {
	c := &jtiCache{seen: make(map[string]time.Time)}
	go c.sweep()
	return c
}

// consume records jti under the lock, returning false if it is still present and
// unexpired (a replay). An expired entry is treated as absent and overwritten.
func (c *jtiCache) consume(jti string, exp time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if prev, ok := c.seen[jti]; ok && now.Before(prev) {
		return false
	}
	c.seen[jti] = exp
	return true
}

func (c *jtiCache) sweep() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for now := range ticker.C {
		c.mu.Lock()
		for jti, exp := range c.seen {
			if now.After(exp) {
				delete(c.seen, jti)
			}
		}
		c.mu.Unlock()
	}
}
