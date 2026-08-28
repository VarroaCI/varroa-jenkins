// Package schedule provides authentication for BroodSchedule-triggered JWT tokens.
//
// The schedule.Verifier disambiguates schedule tokens from other JWTs (OIDC, local
// auth, mite Jenkins tokens) on the unverified aud claim before performing any
// cryptographic verification. This avoids false-positive routing of local-mode
// session JWTs (which share the same mite signing key and kid) into the schedule
// token path.
package schedule

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// Audience is the reserved JWT audience for BroodSchedule-triggered tokens.
// It must be used as the aud claim when minting schedule tokens (task 3.2) and
// checked by Verifier.Verify. Import this constant rather than hardcoding the
// string to avoid silent drift.
const Audience = "varroa-schedule"

// payload is the subset of JWT claims decoded before signature verification
// to determine whether the token is a schedule token.
type payload struct {
	Aud string `json:"aud"`
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
}

// jwtHeader is the standard JWT header decoded for algorithm and KID checks.
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// Verifier validates BroodSchedule-triggered JWT tokens by checking the aud
// claim against the reserved Audience constant, then performing RS256 signature
// verification against the shared mite signing key.
type Verifier struct {
	signer *signing.Signer
}

// NewVerifier creates a Verifier that validates schedule tokens using the given
// shared mite signing key.
func NewVerifier(signer *signing.Signer) *Verifier {
	return &Verifier{signer: signer}
}

// Verify checks whether the given raw JWT is a schedule token.
//
// It returns three states:
//   - (nil, false, nil) — the token's aud does not match Audience; not a schedule
//     token, caller should fall through to the next verifier.
//   - (nil, true, err) — aud matches Audience but signature or expiration check
//     failed; caller should return a hard 401 with no fallback.
//   - (claims, true, nil) — aud matches, signature is valid, and exp is not
//     exceeded; caller should accept the request with the returned claims.
func (v *Verifier) Verify(ctx context.Context, token string) (*auth.Claims, bool, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false, nil
	}

	// Decode payload (unverified) to check aud.
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false, nil //nolint:nilerr // undecodable payload = not a schedule token; fall through
	}
	var p payload
	if err := json.Unmarshal(payloadJSON, &p); err != nil {
		return nil, false, nil //nolint:nilerr // non-JSON payload = not a schedule token; fall through
	}

	// aud mismatch → not a schedule token, fall through.
	if p.Aud != Audience {
		return nil, false, nil
	}

	// aud matches → this positively identifies as a schedule token.
	// Any failure below is a hard 401.

	// Decode header.
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, true, fmt.Errorf("decode header: %w", err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return nil, true, fmt.Errorf("unmarshal header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return nil, true, fmt.Errorf("unsupported algorithm: %s", hdr.Alg)
	}
	if hdr.Kid != v.signer.KID() {
		return nil, true, fmt.Errorf("unknown kid: %s", hdr.Kid)
	}

	// Verify signature.
	signingInput := parts[0] + "." + parts[1]
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, true, fmt.Errorf("decode signature: %w", err)
	}
	pub := v.signer.PublicKey()
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig); err != nil {
		return nil, true, fmt.Errorf("signature verification failed: %w", err)
	}

	// Check expiration.
	if time.Now().Unix() > p.Exp {
		return nil, true, fmt.Errorf("token expired")
	}

	// Success: populate all four Claims fields identically from the sub claim.
	// This ensures that Claims.UserValues resolves correctly regardless of which
	// field(s) the deployment's userClaimNames configuration selects.
	return &auth.Claims{
		Subject:           p.Sub,
		Email:             p.Sub,
		Name:              p.Sub,
		PreferredUsername: p.Sub,
	}, true, nil
}
