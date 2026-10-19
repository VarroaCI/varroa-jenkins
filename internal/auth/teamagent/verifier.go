// Package teamagent verifies the session token of a Team agent. The verifier
// answers "not mine" for any token whose audience is not the Team agent
// audience. A token of that audience must pass every check below, or the
// request is refused.
package teamagent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/token"
)

// maxFutureIat is how far a token's iat may lie ahead of this clock.
const maxFutureIat = 60 * time.Second

// Objects gives the verifier the TeamAgent objects and the decision input.
// The BFF implements it from an informer cache, so a request costs no API
// call.
type Objects interface {
	// ByUID returns the TeamAgent object with the UID.
	ByUID(uid string) (*v1alpha1.TeamAgent, bool)
	// AcceptInput returns the state that accept.Decide reads.
	AcceptInput() accept.Input
}

// SessionPrincipal returns the principal that a session row names. It fails
// when the session does not exist.
type SessionPrincipal func(ctx context.Context, id uuid.UUID) (string, error)

// Verifier checks Team agent tokens against the mounted HMAC key.
type Verifier struct {
	keyPath  string
	objs     Objects
	sessions SessionPrincipal
	now      func() time.Time

	mu  sync.Mutex
	key []byte
}

// NewVerifier reads the key at start. A missing or short key fails here, so
// a broken mount stops the process before it serves a request.
func NewVerifier(keyPath string, objs Objects, sessions SessionPrincipal) (*Verifier, error) {
	key, err := token.ReadKey(keyPath)
	if err != nil {
		return nil, err
	}
	return &Verifier{keyPath: keyPath, objs: objs, sessions: sessions, now: time.Now, key: key}, nil
}

// jwtPayload holds the claims that route a token before any check.
type jwtPayload struct {
	Aud string `json:"aud"`
}

// jwtHeader holds the fields that the verifier checks.
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// Verify returns the claims of a Team agent token and its session id. The
// matched result is false when the token is not a Team agent token. An error
// with matched true means the token is refused.
func (v *Verifier) Verify(r *http.Request, raw string) (*auth.Claims, string, bool, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, "", false, nil
	}
	var pl jwtPayload
	if !decodedAudience(parts[1], &pl) {
		return nil, "", false, nil
	}
	claims, sid, err := v.check(r, parts)
	if err != nil {
		return nil, "", true, err
	}
	return claims, sid, true, nil
}

// check runs every rule of a Team agent token in order. The first failed
// rule is the error.
func (v *Verifier) check(r *http.Request, parts []string) (*auth.Claims, string, error) {
	var hdr jwtHeader
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return nil, "", fmt.Errorf("header: %w", err)
	}
	if hdr.Alg != "HS256" || hdr.Kid != accept.TokenKeyID {
		return nil, "", errors.New("unsupported header")
	}
	var c token.Claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return nil, "", fmt.Errorf("payload: %w", err)
	}
	if err := v.verifySignature(parts); err != nil {
		return nil, "", err
	}

	now := v.now()
	if !time.Unix(c.Exp, 0).After(now) {
		return nil, "", errors.New("token expired")
	}
	if time.Unix(c.Iat, 0).After(now.Add(maxFutureIat)) {
		return nil, "", errors.New("token issued in the future")
	}
	if c.Exp-c.Iat > int64(accept.TokenLifetime/time.Second) {
		return nil, "", errors.New("token lifetime too long")
	}
	if !accept.IsPrincipal(c.Sub) {
		return nil, "", errors.New("subject is not a team agent")
	}
	if err := v.checkObject(c); err != nil {
		return nil, "", err
	}
	if p := r.URL.Path; p != accept.MCPPath && !strings.HasPrefix(p, accept.MCPPath+"/") {
		return nil, "", errors.New("path not allowed for a team agent token")
	}
	sid, err := uuid.Parse(c.SID)
	if err != nil {
		return nil, "", fmt.Errorf("session id: %w", err)
	}
	if v.sessions == nil {
		return nil, "", errors.New("session lookup not configured")
	}
	principal, err := v.sessions(r.Context(), sid)
	if err != nil {
		return nil, "", fmt.Errorf("session: %w", err)
	}
	if principal != c.Sub {
		return nil, "", errors.New("session names another principal")
	}
	return &auth.Claims{Subject: c.Sub, PreferredUsername: c.Sub, Name: c.Sub}, c.SID, nil
}

// checkObject requires the TeamAgent named by uid to be the accepted, enabled
// agent whose principal equals the subject.
func (v *Verifier) checkObject(c token.Claims) error {
	ta, ok := v.objs.ByUID(c.UID)
	if !ok {
		return errors.New("unknown team agent")
	}
	if accept.Principal(ta) != c.Sub {
		return errors.New("subject does not match the team agent")
	}
	verdict := accept.Decide(v.objs.AcceptInput())[types.NamespacedName{Namespace: ta.Namespace, Name: ta.Name}]
	if !verdict.Accepted {
		return fmt.Errorf("team agent not accepted: %s", verdict.Reason)
	}
	if !accept.Enabled(ta) {
		return errors.New("team agent is disabled")
	}
	return nil
}

// verifySignature checks the HMAC over the signing input. On a mismatch it
// reads the key file once more, so a rotated key takes effect without a
// restart.
func (v *Verifier) verifySignature(parts []string) error {
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	signing := parts[0] + "." + parts[1]
	v.mu.Lock()
	defer v.mu.Unlock()
	if macMatches(v.key, signing, sig) {
		return nil
	}
	key, err := token.ReadKey(v.keyPath)
	if err != nil || !macMatches(key, signing, sig) {
		return errors.New("signature mismatch")
	}
	v.key = key
	return nil
}

// macMatches reports whether sig is the HMAC-SHA256 of signing under key.
func macMatches(key []byte, signing string, sig []byte) bool {
	if len(key) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signing))
	return hmac.Equal(mac.Sum(nil), sig)
}

// decodeSegment decodes one unpadded base64url JSON segment.
func decodeSegment(seg string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// decodedAudience reports whether the payload segment decodes and names the
// Team agent audience.
func decodedAudience(seg string, pl *jwtPayload) bool {
	return decodeSegment(seg, pl) == nil && pl.Aud == accept.TokenAudience
}
