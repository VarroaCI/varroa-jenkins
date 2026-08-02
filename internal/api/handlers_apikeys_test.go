package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/apikey"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// fakeKeyStore is an in-memory keyStore (structurally satisfies the unexported
// apikey.keyStore interface) for handler tests.
type fakeKeyStore struct {
	secrets map[string]map[string][]byte
	labels  map[string]map[string]string
	users   map[string]*v1alpha1.User
}

var fakeGR = schema.GroupResource{Resource: "secrets"}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{
		secrets: map[string]map[string][]byte{},
		labels:  map[string]map[string]string{},
		users:   map[string]*v1alpha1.User{},
	}
}

func (f *fakeKeyStore) GetSecret(_ context.Context, name, _ string) (map[string][]byte, error) {
	d, ok := f.secrets[name]
	if !ok {
		return nil, apierrors.NewNotFound(fakeGR, name)
	}
	return d, nil
}

func (f *fakeKeyStore) CreateSecretExclusive(_ context.Context, name, _ string, labels map[string]string, data map[string][]byte) error {
	if _, ok := f.secrets[name]; ok {
		return apierrors.NewAlreadyExists(fakeGR, name)
	}
	f.secrets[name] = data
	f.labels[name] = labels
	return nil
}

func (f *fakeKeyStore) PatchSecretData(_ context.Context, name, _ string, data map[string][]byte) error {
	d, ok := f.secrets[name]
	if !ok {
		return apierrors.NewNotFound(fakeGR, name)
	}
	for k, v := range data {
		d[k] = v
	}
	return nil
}

func (f *fakeKeyStore) DeleteSecret(_ context.Context, name, _ string) error {
	delete(f.secrets, name)
	delete(f.labels, name)
	return nil
}

func (f *fakeKeyStore) ListSecrets(_ context.Context, _, labelSelector string) ([]map[string][]byte, error) {
	wantKey, wantVal, has := strings.Cut(labelSelector, "=")
	var out []map[string][]byte
	for name, data := range f.secrets {
		if has && f.labels[name][wantKey] != wantVal {
			continue
		}
		c := map[string][]byte{"_name": []byte(name)}
		for k, v := range data {
			c[k] = v
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeKeyStore) ListGroupCRDs(_ context.Context) ([]*v1alpha1.Group, error) {
	return nil, nil
}

func (f *fakeKeyStore) GetUserCRD(_ context.Context, name, _ string) (*v1alpha1.User, error) {
	u, ok := f.users[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "users"}, name)
	}
	return u, nil
}

func testServer() (*Server, *apikey.Verifier) {
	return testServerWithMode("local")
}

func testServerWithMode(mode string) (*Server, *apikey.Verifier) {
	v := apikey.NewVerifier(newFakeKeyStore(), "ns")
	s := &Server{deps: &Dependencies{
		KeyVerifier:    v,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Authorizer:     NewAuthorizer(rbac.NewTestResolver(), false),
		IdentityConfig: IdentityConfig{Mode: mode},
	}}
	return s, v
}

func reqWithClaims(method, path, body string, claims *auth.Claims) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, http.NoBody)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	return r.WithContext(auth.ContextWithClaims(r.Context(), claims))
}

// F5: an empty body is a valid "no expiry" create, not a 400.
func TestCreateKeyEmptyBody(t *testing.T) {
	s, _ := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}
	w := httptest.NewRecorder()
	s.handleCreateKey(w, reqWithClaims(http.MethodPost, "/api/v1/me/apikeys", "", claims))

	if w.Code != http.StatusCreated {
		t.Fatalf("empty body create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp["token"], "vk_") {
		t.Errorf("expected vk_ token, got %q", resp["token"])
	}
}

func TestCreateKeyInvalidExpiresIn(t *testing.T) {
	s, _ := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}
	for _, body := range []string{`{"expiresIn":"-5m"}`, `{"expiresIn":"nonsense"}`} {
		w := httptest.NewRecorder()
		s.handleCreateKey(w, reqWithClaims(http.MethodPost, "/api/v1/me/apikeys", body, claims))
		if w.Code != http.StatusBadRequest {
			t.Errorf("expiresIn %s = %d, want 400", body, w.Code)
		}
	}
}

func TestListOwnKeysOmitsSecret(t *testing.T) {
	s, v := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}
	if _, _, _, err := v.Generate(context.Background(), claims, 0, "", ""); err != nil {
		t.Fatalf("generate: %v", err)
	}
	w := httptest.NewRecorder()
	s.handleListOwnKeys(w, reqWithClaims(http.MethodGet, "/api/v1/me/apikeys", "", claims))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "hash") || strings.Contains(body, "vk_") || strings.Contains(body, "secret") {
		t.Errorf("list leaked secret material: %s", body)
	}
	if !strings.Contains(body, "prefix") {
		t.Errorf("list missing prefix metadata: %s", body)
	}
}

// F-self-service: a user cannot revoke another user's key (404, not 204).
func TestRevokeOwnKeyNotOwned(t *testing.T) {
	s, v := testServer()
	bob := &auth.Claims{Subject: "bob", PreferredUsername: "bob"}
	bobPrefix, _, _, err := v.Generate(context.Background(), bob, 0, "", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	alice := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}
	w := httptest.NewRecorder()
	s.handleRevokeOwnKey(w, reqWithClaims(http.MethodDelete, "/", "", alice), bobPrefix)
	if w.Code != http.StatusNotFound {
		t.Fatalf("alice revoking bob's key = %d, want 404", w.Code)
	}

	// Owner can revoke.
	w = httptest.NewRecorder()
	s.handleRevokeOwnKey(w, reqWithClaims(http.MethodDelete, "/", "", bob), bobPrefix)
	if w.Code != http.StatusNoContent {
		t.Fatalf("bob revoking own key = %d, want 204", w.Code)
	}
}

// F6: admin revoke must honor the {name} in the path.
func TestAdminRevokeWrongUser(t *testing.T) {
	s, v := testServer()
	bob := &auth.Claims{Subject: "bob", PreferredUsername: "bob"}
	bobPrefix, _, _, err := v.Generate(context.Background(), bob, 0, "", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// URL names alice but targets bob's prefix → 404, key untouched.
	w := httptest.NewRecorder()
	s.handleAdminRevokeKey(w, httptest.NewRequest(http.MethodDelete, "/", http.NoBody), "alice", bobPrefix)
	if w.Code != http.StatusNotFound {
		t.Fatalf("admin revoke alice/<bob-prefix> = %d, want 404", w.Code)
	}

	// Correct user → 204.
	w = httptest.NewRecorder()
	s.handleAdminRevokeKey(w, httptest.NewRequest(http.MethodDelete, "/", http.NoBody), "bob", bobPrefix)
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin revoke bob/<bob-prefix> = %d, want 204", w.Code)
	}
}

// Rotate accepts an empty body (no expiry) but rejects malformed JSON.
func TestRotateBodyHandling(t *testing.T) {
	s, v := testServer()
	alice := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}
	prefix, _, _, err := v.Generate(context.Background(), alice, 0, "", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Empty body → 200 with a fresh token.
	w := httptest.NewRecorder()
	s.handleRotateOwnKey(w, reqWithClaims(http.MethodPost, "/", "", alice), prefix)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate empty body = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	newPrefix, _, _ := apikey.Parse(resp["token"])

	// Malformed JSON → 400.
	w = httptest.NewRecorder()
	s.handleRotateOwnKey(w, reqWithClaims(http.MethodPost, "/", "{not json", alice), newPrefix)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("rotate malformed body = %d, want 400", w.Code)
	}
}

// Admin endpoints are denied to non-admins (empty resolver grants no admin cap).
func TestAdminEndpointForbiddenForNonAdmin(t *testing.T) {
	s, _ := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}
	w := httptest.NewRecorder()
	s.handleUsersApiKeysDispatch(w, reqWithClaims(http.MethodGet, "/api/v1/users/bob/apikeys", "", claims))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin admin list = %d, want 403", w.Code)
	}
}

// TestCreateKeyStoresUserRef verifies that the created Secret contains the
// correct userRef for both local-mode and oidc-mode configurations.
func TestCreateKeyStoresUserRef(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		claims *auth.Claims
	}{
		{
			name:   "local mode",
			mode:   "local",
			claims: &auth.Claims{Subject: "jdoe", PreferredUsername: "jdoe"},
		},
		{
			name:   "oidc mode",
			mode:   "oidc",
			claims: &auth.Claims{Subject: "google-oauth2|12345", PreferredUsername: "jdoe@example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeKeyStore()
			v := apikey.NewVerifier(store, "ns")
			s := &Server{deps: &Dependencies{
				KeyVerifier:    v,
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				Authorizer:     NewAuthorizer(rbac.NewTestResolver(), false),
				IdentityConfig: IdentityConfig{Mode: tt.mode},
			}}

			w := httptest.NewRecorder()
			s.handleCreateKey(w, reqWithClaims(http.MethodPost, "/api/v1/me/apikeys", "", tt.claims))
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
			}

			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			token := resp["token"]

			prefix, _, _ := apikey.Parse(token)

			// Check that the stored secret has the userRef field.
			secretData, err := store.GetSecret(context.Background(), "apikey-"+prefix, "ns")
			if err != nil {
				t.Fatalf("GetSecret() error: %v", err)
			}
			gotUserRef := string(secretData["userRef"])
			if gotUserRef == "" {
				t.Error("userRef is empty in stored secret")
			}
		})
	}
}

// TestCreateKeyWithName verifies that a named create key round-trips through
// the list endpoint with the name intact.
func TestCreateKeyWithName(t *testing.T) {
	s, v := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}

	// Create a key with a name.
	w := httptest.NewRecorder()
	body := `{"name":"my-ci-key"}`
	s.handleCreateKey(w, reqWithClaims(http.MethodPost, "/api/v1/me/apikeys", body, claims))
	if w.Code != http.StatusCreated {
		t.Fatalf("named create = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	// List keys and verify the name appears.
	w = httptest.NewRecorder()
	s.handleListOwnKeys(w, reqWithClaims(http.MethodGet, "/api/v1/me/apikeys", "", claims))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	var listResp struct {
		Items []apikey.KeyMeta `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("got %d keys, want 1", len(listResp.Items))
	}
	if listResp.Items[0].Name != "my-ci-key" {
		t.Errorf("Name = %q, want %q", listResp.Items[0].Name, "my-ci-key")
	}
	_ = v
}

// TestCreateKeyNameTooLong verifies that a name longer than 128 chars is
// rejected with 400.
func TestCreateKeyNameTooLong(t *testing.T) {
	s, _ := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}

	longName := strings.Repeat("a", 129)
	body := `{"name":"` + longName + `"}`
	w := httptest.NewRecorder()
	s.handleCreateKey(w, reqWithClaims(http.MethodPost, "/api/v1/me/apikeys", body, claims))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("long name create = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "name too long (max 128)" {
		t.Errorf("error = %q, want %q", resp["error"], "name too long (max 128)")
	}
}

// TestRotateWithoutNamePreservesName verifies that rotating a named key without
// sending a name in the request carries the old name forward.
func TestRotateWithoutNamePreservesName(t *testing.T) {
	s, v := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}

	// Create a named key.
	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "my-ci-key")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	prefix, _, _ := apikey.Parse(token)

	// Rotate without sending a name (empty body).
	w := httptest.NewRecorder()
	s.handleRotateOwnKey(w, reqWithClaims(http.MethodPost, "/", "", claims), prefix)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// The new key should have the same name. List to verify.
	w = httptest.NewRecorder()
	s.handleListOwnKeys(w, reqWithClaims(http.MethodGet, "/api/v1/me/apikeys", "", claims))
	if w.Code != http.StatusOK {
		t.Fatalf("list after rotate = %d, want 200", w.Code)
	}
	var listResp struct {
		Items []apikey.KeyMeta `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("got %d keys, want 1", len(listResp.Items))
	}
	if listResp.Items[0].Name != "my-ci-key" {
		t.Errorf("Name after rotate = %q, want %q", listResp.Items[0].Name, "my-ci-key")
	}
}

// TestRotateWithNameOverrides verifies that rotating a named key while
// providing a new name overrides the old name.
func TestRotateWithNameOverrides(t *testing.T) {
	s, v := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}

	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "old-name")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	prefix, _, _ := apikey.Parse(token)

	w := httptest.NewRecorder()
	body := `{"name":"new-name"}`
	s.handleRotateOwnKey(w, reqWithClaims(http.MethodPost, "/", body, claims), prefix)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate with name = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// List and verify new name.
	w = httptest.NewRecorder()
	s.handleListOwnKeys(w, reqWithClaims(http.MethodGet, "/api/v1/me/apikeys", "", claims))
	if w.Code != http.StatusOK {
		t.Fatalf("list after rotate = %d, want 200", w.Code)
	}
	var listResp struct {
		Items []apikey.KeyMeta `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("got %d keys, want 1", len(listResp.Items))
	}
	if listResp.Items[0].Name != "new-name" {
		t.Errorf("Name after rotate = %q, want %q", listResp.Items[0].Name, "new-name")
	}
}

// TestCreateKeyEmptyNameIsUnnamed verifies that an empty or absent name
// produces a key with no name (not an error).
func TestCreateKeyEmptyNameIsUnnamed(t *testing.T) {
	s, _ := testServer()
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice"}

	// Create without name.
	w := httptest.NewRecorder()
	s.handleCreateKey(w, reqWithClaims(http.MethodPost, "/api/v1/me/apikeys", "", claims))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	// List and verify name is empty.
	w = httptest.NewRecorder()
	s.handleListOwnKeys(w, reqWithClaims(http.MethodGet, "/api/v1/me/apikeys", "", claims))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	var listResp struct {
		Items []apikey.KeyMeta `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("got %d keys, want 1", len(listResp.Items))
	}
	if listResp.Items[0].Name != "" {
		t.Errorf("Name = %q, want empty string", listResp.Items[0].Name)
	}
}
