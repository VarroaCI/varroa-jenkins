package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/logging"
)

var (
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
	kid     string
	issuer  string
	codes   sync.Map // code (string) -> claims (map)
)

// Test users: username -> {password, claims}
type testUser struct {
	Password string
	Claims   map[string]interface{}
}

var users = map[string]testUser{
	"admin": {
		Password: "password",
		Claims: map[string]interface{}{
			"sub":    "admin",
			"email":  "admin@varroa.local",
			"name":   "Admin User",
			"groups": []string{"org:admins"},
		},
	},
	"dev": {
		Password: "password",
		Claims: map[string]interface{}{
			"sub":    "dev",
			"email":  "dev@varroa.local",
			"name":   "Dev User",
			"groups": []string{"org:developers"},
		},
	},
}

func main() {
	logger := logging.NewFromEnv().With("binary", "mock-oidc")

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		logger.Error("keygen failed", "error", err)
		os.Exit(1)
	}
	pubKey = pub
	privKey = priv
	h := sha256.Sum256(pub)
	kid = fmt.Sprintf("%x", h[:8])

	issuer = os.Getenv("MOCK_OIDC_ISSUER")
	if issuer == "" {
		issuer = "http://mock-oidc.varroa-ci.svc:5557"
	}

	port := os.Getenv("MOCK_OIDC_PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", handleDiscovery)
	mux.HandleFunc("/jwks", handleJWKS)
	mux.HandleFunc("/authorize", handleAuthorize)
	mux.HandleFunc("/token", handleToken)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	logger.Info("Mock OIDC server starting", "port", port, "issuer", issuer)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

func handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"jwks_uri":                              issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"EdDSA"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{"openid", "profile", "email", "groups"},
		"claims_supported":                      []string{"sub", "iss", "aud", "exp", "iat", "email", "name", "groups"},
	})
}

func handleJWKS(w http.ResponseWriter, _ *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"keys": []map[string]interface{}{
			{
				"kty": "OKP",
				"crv": "Ed25519",
				"x":   base64url(pubKey),
				"kid": kid,
				"use": "sig",
				"alg": "EdDSA",
			},
		},
	})
}

func handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")

	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}

	// In CI mode, auto-approve with a hardcoded admin identity.
	// Dex doesn't pass a login_hint by default, so default to admin.
	username := q.Get("login_hint")
	if username == "" {
		username = "admin"
	}
	if _, ok := users[username]; !ok {
		username = "admin"
	}

	code := randomHex(32)
	codes.Store(code, users[username].Claims)

	sep := "?"
	if strings.Contains(redirectURI, "?") {
		sep = "&"
	}
	http.Redirect(w, r, redirectURI+sep+"code="+code+"&state="+state, http.StatusFound)
}

func handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	grantType := r.FormValue("grant_type")

	switch grantType {
	case "authorization_code":
		handleAuthCodeGrant(w, r)
	case "password":
		handlePasswordGrant(w, r)
	default:
		http.Error(w, "unsupported grant_type: "+grantType, http.StatusBadRequest)
	}
}

func handleAuthCodeGrant(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	claimsRaw, ok := codes.LoadAndDelete(code)
	if !ok {
		http.Error(w, "invalid code", http.StatusBadRequest)
		return
	}
	claims := claimsRaw.(map[string]interface{})

	idToken := signJWT(claims)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": randomHex(32),
		"token_type":   "Bearer",
		"id_token":     idToken,
		"expires_in":   3600,
	})
}

func handlePasswordGrant(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	user, ok := users[username]
	if !ok || user.Password != password {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	// Make a copy of claims with standard OIDC fields
	claims := make(map[string]interface{})
	for k, v := range user.Claims {
		claims[k] = v
	}

	idToken := signJWT(claims)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": randomHex(32),
		"token_type":   "Bearer",
		"id_token":     idToken,
		"expires_in":   3600,
	})
}

func signJWT(claims map[string]interface{}) string {
	now := time.Now()
	claims["iss"] = issuer
	claims["aud"] = []string{"varroa"}
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(1 * time.Hour).Unix()

	header := map[string]interface{}{
		"alg": "EdDSA",
		"kid": kid,
		"typ": "JWT",
	}

	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(claims)

	headerB64 := base64url(headerJSON)
	payloadB64 := base64url(payloadJSON)

	signingInput := headerB64 + "." + payloadB64
	sig := ed25519.Sign(privKey, []byte(signingInput))
	sigB64 := base64url(sig)

	return signingInput + "." + sigB64
}

func base64url(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
