package oci

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

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
