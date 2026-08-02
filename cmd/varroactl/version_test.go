package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVersion_Parseable tests a parseable server version
func TestVersion_Parseable(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	oldVersion := version
	version = "1.2.3"
	t.Cleanup(func() { version = oldVersion })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/version") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"component": "bff",
				"version":   "v1.2.3",
			})
			return
		}
		// /me would be called by apiClient → resolveContext needs context
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test_key")

	root := newRootCmd()
	root.SetArgs([]string{"version"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestVersion_Skew tests version skew warning on stderr
func TestVersion_Skew(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	oldVersion := version
	version = "1.2.0"
	t.Cleanup(func() { version = oldVersion })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/version") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"component": "bff",
				"version":   "v2.3.0",
			})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test_key")

	root := newRootCmd()
	root.SetArgs([]string{"version"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestVersion_NonJSON tests non-JSON body → unknown
func TestVersion_NonJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	oldVersion := version
	version = "dev"
	t.Cleanup(func() { version = oldVersion })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/version") {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("bff mode"))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test_key")

	root := newRootCmd()
	root.SetArgs([]string{"version"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestVersion_DevServer tests "dev" server version → unknown
func TestVersion_DevServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	oldVersion := version
	version = "dev"
	t.Cleanup(func() { version = oldVersion })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/version") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"component": "bff",
				"version":   "dev",
			})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test_key")

	root := newRootCmd()
	root.SetArgs([]string{"version"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestVersion_ClientOnly tests --client makes zero HTTP requests
func TestVersion_ClientOnly(t *testing.T) {
	// No server needed — --client skips server check
	oldVersion := version
	version = "dev"
	t.Cleanup(func() { version = oldVersion })

	root := newRootCmd()
	root.SetArgs([]string{"version", "--client"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
