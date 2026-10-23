package tenantagent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/accept"
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/token"
)

var (
	testKey  = []byte(strings.Repeat("k", 32))
	testNow  = time.Unix(1700000000, 0)
	testUID  = types.UID("uid-1")
	testSID  = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	testPrin = "agent:ns-a/team-a/agent-a"
)

// fakeObjects serves one TenantAgent and its Tenant.
type fakeObjects struct {
	ta     *v1alpha1.TenantAgent
	tenant *v1alpha1.Tenant
	rel    string
}

func (f fakeObjects) ByUID(uid string) (*v1alpha1.TenantAgent, bool) {
	if f.ta == nil || string(f.ta.UID) != uid {
		return nil, false
	}
	return f.ta, true
}

func (f fakeObjects) AcceptInput() accept.Input {
	in := accept.Input{ReleaseNamespace: f.rel, Tenants: map[string]*v1alpha1.Tenant{}}
	if f.ta != nil {
		in.TenantAgents = []v1alpha1.TenantAgent{*f.ta}
	}
	if f.tenant != nil {
		in.Tenants["team-a"] = f.tenant
	}
	return in
}

func baseObjects() fakeObjects {
	return fakeObjects{
		ta: &v1alpha1.TenantAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-a", Namespace: "ns-a", UID: testUID},
			Spec:       v1alpha1.TenantAgentSpec{TenantRef: v1alpha1.TenantAgentTenantRef{Name: "team-a"}},
		},
		tenant: &v1alpha1.Tenant{Spec: v1alpha1.TenantSpec{Namespaces: []string{"ns-a"}}},
		rel:    "varroa-system",
	}
}

func sessionsOK(principal string) SessionPrincipal {
	return func(context.Context, uuid.UUID) (string, error) { return principal, nil }
}

// build signs a token with the given header and payload maps.
func build(t *testing.T, key []byte, hdr, payload map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(hdr) + "." + enc(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// goodPayload returns the claims of a valid token at testNow.
func goodPayload() map[string]any {
	return map[string]any{
		"aud": accept.TokenAudience, "iss": accept.TokenIssuer, "sub": testPrin,
		"uid": string(testUID), "iat": testNow.Unix(), "exp": testNow.Add(15 * time.Minute).Unix(),
		"sid": testSID.String(), "jti": "00",
	}
}

func goodHeader() map[string]any {
	return map[string]any{"alg": "HS256", "typ": "JWT", "kid": accept.TokenKeyID}
}

func newTestVerifier(t *testing.T, objs Objects, sessions SessionPrincipal) *Verifier {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, testKey, 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(path, objs, sessions)
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return testNow }
	return v
}

func TestVerify(t *testing.T) {
	type tc struct {
		name      string
		token     func(t *testing.T) string
		path      string
		objs      Objects
		sessions  SessionPrincipal
		matched   bool
		wantError bool
	}
	withPayload := func(k string, val any) func(t *testing.T) string {
		return func(t *testing.T) string {
			p := goodPayload()
			p[k] = val
			return build(t, testKey, goodHeader(), p)
		}
	}
	cases := []tc{
		{name: "NotMine: two segments", token: func(*testing.T) string { return "a.b" }, matched: false},
		{name: "NotMine: other audience", token: withPayload("aud", "varroa-schedule"), matched: false},
		{name: "BadAlg", token: func(t *testing.T) string {
			h := goodHeader()
			h["alg"] = "none"
			return build(t, testKey, h, goodPayload())
		}, matched: true, wantError: true},
		{name: "BadMAC", token: func(t *testing.T) string {
			return build(t, []byte(strings.Repeat("x", 32)), goodHeader(), goodPayload())
		}, matched: true, wantError: true},
		{name: "Expired", token: withPayload("exp", testNow.Unix()), matched: true, wantError: true},
		{name: "FutureIat", token: withPayload("iat", testNow.Add(2*time.Minute).Unix()), matched: true, wantError: true},
		{name: "LongLife", token: withPayload("exp", testNow.Add(time.Hour).Unix()), matched: true, wantError: true},
		{name: "NotAgentSubject", token: withPayload("sub", "alice"), matched: true, wantError: true},
		{name: "UnknownUID", token: withPayload("uid", "other"), matched: true, wantError: true},
		{name: "PrincipalMismatch", token: withPayload("sub", "agent:ns-a/team-a/other"), matched: true, wantError: true},
		{name: "NotAccepted", objs: func() Objects {
			o := baseObjects()
			o.tenant = &v1alpha1.Tenant{Spec: v1alpha1.TenantSpec{Namespaces: []string{"elsewhere"}}}
			return o
		}(), token: func(t *testing.T) string { return build(t, testKey, goodHeader(), goodPayload()) }, matched: true, wantError: true},
		{name: "Disabled", objs: func() Objects {
			o := baseObjects()
			off := false
			o.ta.Spec.Enabled = &off
			return o
		}(), token: func(t *testing.T) string { return build(t, testKey, goodHeader(), goodPayload()) }, matched: true, wantError: true},
		{name: "WrongPath", path: "/api/v1/other", matched: true, wantError: true},
		{name: "ForeignSession", sessions: sessionsOK("agent:ns-a/team-a/other"), matched: true, wantError: true},
		{name: "OK", matched: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objs := c.objs
			if objs == nil {
				objs = baseObjects()
			}
			sessions := c.sessions
			if sessions == nil {
				sessions = sessionsOK(testPrin)
			}
			path := c.path
			if path == "" {
				path = "/api/v1/mcp"
			}
			tokenStr := build(t, testKey, goodHeader(), goodPayload())
			if c.token != nil {
				tokenStr = c.token(t)
			}
			v := newTestVerifier(t, objs, sessions)
			r := httptest.NewRequest("POST", path, nil)
			claims, sid, matched, err := v.Verify(r, tokenStr)
			if matched != c.matched {
				t.Fatalf("matched = %v, want %v (err %v)", matched, c.matched, err)
			}
			if c.wantError != (err != nil) {
				t.Fatalf("err = %v, want error %v", err, c.wantError)
			}
			if err == nil && matched {
				if claims.Subject != testPrin || claims.PreferredUsername != testPrin || claims.Name != testPrin {
					t.Errorf("claims = %+v", claims)
				}
				if len(claims.Groups) != 0 {
					t.Errorf("groups = %v, want none", claims.Groups)
				}
				if sid != testSID.String() {
					t.Errorf("sid = %q", sid)
				}
			}
		})
	}
}

// TestVerify_RealMintOK runs a token from token.Mint through the verifier.
func TestVerify_RealMintOK(t *testing.T) {
	v := newTestVerifier(t, baseObjects(), sessionsOK(testPrin))
	val, err := token.Mint(testKey, testPrin, testUID, testSID, testNow)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	val.SetHeader(h)
	raw := strings.TrimPrefix(h.Get("Authorization"), "Bearer ")
	if _, _, matched, err := v.Verify(httptest.NewRequest("GET", "/api/v1/mcp/tools", nil), raw); !matched || err != nil {
		t.Fatalf("minted token: matched=%v err=%v", matched, err)
	}
}

// TestVerify_RotatedKeyReread checks that a key file rewritten on disk takes
// effect on the next request, without a restart.
func TestVerify_RotatedKeyReread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, testKey, 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(path, baseObjects(), sessionsOK(testPrin))
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return testNow }
	rotated := []byte(strings.Repeat("r", 32))
	if err := os.WriteFile(path, rotated, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := build(t, rotated, goodHeader(), goodPayload())
	if _, _, matched, err := v.Verify(httptest.NewRequest("POST", "/api/v1/mcp", nil), raw); !matched || err != nil {
		t.Fatalf("rotated key: matched=%v err=%v", matched, err)
	}
}

func TestNewVerifierRejectsMissingKey(t *testing.T) {
	_, err := NewVerifier(filepath.Join(t.TempDir(), "absent"), baseObjects(), nil)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewVerifier on a missing key = %v", err)
	}
}
