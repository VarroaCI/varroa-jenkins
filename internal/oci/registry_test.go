package oci

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestNewRegistryStore_ReferenceParsing(t *testing.T) {
	tests := []struct {
		ref     string
		wantErr bool
	}{
		{"registry.example.com/repo:latest", false},
		{"registry.example.com/repo@sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", false},
		{"", true},
		{":invalid", true},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			_, err := NewRegistryStore(tt.ref, RegistryOptions{})
			if (err != nil) != tt.wantErr {
				t.Errorf("NewRegistryStore(%q) error = %v, wantErr = %v", tt.ref, err, tt.wantErr)
			}
		})
	}
}

func TestRegistryStore_InsecureSetsPlainHTTP(t *testing.T) {
	store, err := NewRegistryStore("registry.example.com/repo:v1", RegistryOptions{Insecure: true})
	if err != nil {
		t.Fatalf("NewRegistryStore: %v", err)
	}
	repo := store.GetRepo()
	if repo == nil {
		t.Fatal("GetRepo returned nil")
	}
	if !repo.PlainHTTP {
		t.Error("PlainHTTP should be true when Insecure is set")
	}
}

func TestRegistryStore_InsecureFalseByDefault(t *testing.T) {
	store, err := NewRegistryStore("registry.example.com/repo:v1", RegistryOptions{})
	if err != nil {
		t.Fatalf("NewRegistryStore: %v", err)
	}
	repo := store.GetRepo()
	if repo.PlainHTTP {
		t.Error("PlainHTTP should be false by default")
	}
}

func TestRegistryStore_CredentialResolution(t *testing.T) {
	// Create a temporary Docker config.json fixture
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	username := "testuser"
	password := "testpass"
	authVal := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))

	// Write a minimal Docker config.json with auths
	configData := `{"auths":{"myregistry.example.com":{"auth":"` + authVal + `"}}}`
	if err := os.WriteFile(cfgPath, []byte(configData), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Test credential resolution for matching host
	credFunc, err := buildCredentialFuncForHost(cfgPath)
	if err != nil {
		t.Fatalf("buildCredentialFuncForHost: %v", err)
	}
	if credFunc == nil {
		t.Fatal("credFunc should not be nil for a matching host")
	}

	cred, err := credFunc(nil, "myregistry.example.com")
	if err != nil {
		t.Fatalf("credFunc: %v", err)
	}
	if cred.Username != username {
		t.Errorf("username = %q, want %q", cred.Username, username)
	}
	if cred.Password != password {
		t.Errorf("password = %q, want %q", cred.Password, password)
	}
}

func TestRegistryStore_CredentialResolution_NonMatchingHost(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// Write a minimal Docker config.json with auths for a different host
	authVal := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	configData := `{"auths":{"other.example.com":{"auth":"` + authVal + `"}}}`
	os.WriteFile(cfgPath, []byte(configData), 0644)

	// Non-matching host should return anonymous (nil credential func)
	credFunc, err := buildCredentialFuncForHost(cfgPath)
	if err != nil {
		t.Fatalf("buildCredentialFuncForHost: %v", err)
	}
	if credFunc != nil {
		// nil is fine for anonymous access
		// or it might return a credential func that returns EmptyCredential
		cred, err := credFunc(nil, "different.example.com")
		if err != nil {
			t.Fatalf("credFunc returned error: %v", err)
		}
		if cred != auth.EmptyCredential && (cred.Username != "" || cred.Password != "") {
			t.Errorf("expected empty credential for non-matching host, got %+v", cred)
		}
	}
}

func TestRegistryStore_CredentialResolution_NoConfigFile(t *testing.T) {
	// A nonexistent config path should result in anonymous access (nil credential func)
	credFunc, err := buildCredentialFuncForHost("/nonexistent/path/config.json")
	if err != nil {
		t.Fatalf("buildCredentialFuncForHost: %v", err)
	}
	if credFunc != nil {
		t.Log("credFunc is non-nil; anonymous is acceptable")
	}
}

func TestRegistryStore_FetchBlob(t *testing.T) {
	blob := []byte("the blob payload served with an explicit Content-Length")
	digest := godigest.FromBytes(blob)

	var mu sync.Mutex
	getHits := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobPath := "/v2/testrepo/blobs/" + digest.String()
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
		case blobPath:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
			switch r.Method {
			case http.MethodGet:
				mu.Lock()
				getHits++
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(blob)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store, err := NewRegistryStore(srv.Listener.Addr().String()+"/testrepo:v1", RegistryOptions{Insecure: true})
	if err != nil {
		t.Fatalf("NewRegistryStore: %v", err)
	}

	rc, err := store.FetchBlob(t.Context(), digest.String())
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Errorf("FetchBlob returned %q, want %q", got, blob)
	}

	mu.Lock()
	defer mu.Unlock()
	if getHits != 1 {
		t.Errorf("expected 1 GET request to fetch the blob, got %d", getHits)
	}
}
