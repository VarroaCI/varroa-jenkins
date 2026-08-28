package apikey

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

func TestVerifyHandler(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		header     string
		setup      func(*mockStore, *Verifier) (token string)
		wantStatus int
		wantBody   string // empty means expect empty body
	}{
		{
			name:       "405 method not POST",
			method:     http.MethodGet,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "400 missing auth header",
			method:     http.MethodPost,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 non-bearer auth",
			method:     http.MethodPost,
			header:     "Basic dGVzdDp0ZXN0",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "401 malformed token",
			method:     http.MethodPost,
			header:     "Bearer invalid-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "401 wrong secret",
			method: http.MethodPost,
			setup: func(store *mockStore, v *Verifier) string {
				claims := &auth.Claims{Subject: "user", PreferredUsername: "user"}
				_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
				if err != nil {
					t.Fatal(err)
				}
				// Forge a wrong-secret token with the same prefix.
				prefix, _, _ := Parse(token)
				return "vk_" + prefix + "." + strings.Repeat("x", 43)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "401 expired token",
			method: http.MethodPost,
			setup: func(store *mockStore, v *Verifier) string {
				claims := &auth.Claims{Subject: "user", PreferredUsername: "user"}
				_, _, token, err := v.Generate(context.Background(), claims, time.Hour, "", "")
				if err != nil {
					t.Fatal(err)
				}
				prefix, _, _ := Parse(token)
				data, _ := store.GetSecret(context.Background(), "apikey-"+prefix, "ns")
				data["expires"] = []byte(time.Now().Add(-time.Hour).Format(time.RFC3339))
				return token
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "200 valid token",
			method: http.MethodPost,
			setup: func(store *mockStore, v *Verifier) string {
				claims := &auth.Claims{
					Subject:           "user1",
					Email:             "user1@test.com",
					Name:              "User One",
					PreferredUsername: "user1",
				}
				_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
				if err != nil {
					t.Fatal(err)
				}
				return token
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"subject":"user1","preferredUsername":"user1","email":"user1@test.com","name":"User One","groups":[]}`,
		},
		{
			name:   "503 infrastructure error",
			method: http.MethodPost,
			setup: func(store *mockStore, v *Verifier) string {
				// First generate a valid token with the normal store.
				claims := &auth.Claims{Subject: "user", PreferredUsername: "user"}
				_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
				if err != nil {
					t.Fatal(err)
				}
				// Then replace the store with one that fails GetSecret.
				v.store = &errorGetSecretStore{mockStore: store}
				return token
			},
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMockStore()
			v := NewVerifier(store, "ns")
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))

			token := ""
			if tt.setup != nil {
				token = tt.setup(store, v)
			}

			handler := VerifyHandler(v, logger)
			req := httptest.NewRequest(tt.method, "/v1/verify-apikey", http.NoBody)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			} else if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body=%q", w.Code, tt.wantStatus, w.Body.String())
			}

			if tt.wantBody != "" {
				var want, got map[string]any
				if err := json.Unmarshal([]byte(tt.wantBody), &want); err != nil {
					t.Fatalf("unmarshal want: %v", err)
				}
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("unmarshal got: %v", err)
				}
				for _, key := range []string{"subject", "preferredUsername", "email", "name"} {
					if got[key] != want[key] {
						t.Errorf("field %q = %v, want %v", key, got[key], want[key])
					}
				}
				// groups are []interface{}, compare length.
				gotGroups, _ := got["groups"].([]any)
				wantGroups, _ := want["groups"].([]any)
				if len(gotGroups) != len(wantGroups) {
					t.Errorf("groups = %v, want %v", gotGroups, wantGroups)
				}
			} else {
				if w.Body.Len() > 0 {
					t.Errorf("expected empty body, got %q", w.Body.String())
				}
			}

			// 401 bodies must be byte-identical regardless of error type.
			if tt.wantStatus == http.StatusUnauthorized {
				// All 401 responses must have empty body.
				if w.Body.Len() != 0 {
					t.Errorf("401 body must be empty, got %q", w.Body.String())
				}
			}
		})
	}
}

// TestVerifyHandlerRevokedKey verifies that a revoked key returns 401 on the
// very next call (no caching in VerifyHandler since it uses VerifyFresh).
func TestVerifyHandlerRevokedKey(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "ns")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	claims := &auth.Claims{Subject: "user", PreferredUsername: "user"}
	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	prefix, _, _ := Parse(token)

	handler := VerifyHandler(v, logger)

	// First call succeeds.
	req := httptest.NewRequest(http.MethodPost, "/v1/verify-apikey", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first call = %d, want 200", w.Code)
	}

	// Revoke.
	v.Revoke(context.Background(), prefix)

	// Second call must fail immediately (no server-side cache).
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("after revoke = %d, want 401", w.Code)
	}
}

// errorGetSecretStore can also fail GetSecret to simulate infrastructure errors.
type errorGetSecretStore struct {
	*mockStore
}

func (e *errorGetSecretStore) GetSecret(_ context.Context, _, _ string) (map[string][]byte, error) {
	return nil, errors.New("simulated k8s API error")
}

func TestVerifyHandler_401BodiesIdentical(t *testing.T) {
	// Ensure that different 401 scenarios produce the same empty body.
	store := newMockStore()
	v := NewVerifier(store, "ns")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := VerifyHandler(v, logger)

	// Malformed token.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/verify-apikey", http.NoBody)
	req1.Header.Set("Authorization", "Bearer invalid")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	// Valid-looking but non-existent token.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/verify-apikey", http.NoBody)
	req2.Header.Set("Authorization", "Bearer vk_abc.xyz")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w1.Body.String() != w2.Body.String() {
		t.Error("401 bodies differ between malformed and non-existent token")
	}
	if w1.Body.Len() != 0 {
		t.Errorf("401 body should be empty, got %q", w1.Body.String())
	}
}
