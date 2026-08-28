package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLogout_NoKey tests logout when no context exists
func TestLogout_NoKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	root := newRootCmd()
	root.SetArgs([]string{"logout"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for not logged in")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("expected 'not logged in', got %v", err)
	}
}

// TestLogout_Default tests logout without --revoke (just clears key)
func TestLogout_Default(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := tmpDir + "/config.yaml"
	t.Setenv("VARROACTL_CONFIG", configPath)

	// First set up a context with a key
	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:   "prod",
				Server: "https://example.com",
				APIKey: "vk_abc123.def456",
			},
		},
	}
	saveConfig(cfg)

	root := newRootCmd()
	root.SetArgs([]string{"logout"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the key was cleared but context entry remains
	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CurrentContext != "prod" {
		t.Errorf("expected current context to remain, got %q", loaded.CurrentContext)
	}
	if len(loaded.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(loaded.Contexts))
	}
	if loaded.Contexts[0].APIKey != "" {
		t.Errorf("expected API key to be cleared, got %q", loaded.Contexts[0].APIKey)
	}
}

// TestLogout_Revoke tests logout --revoke
func TestLogout_Revoke(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := tmpDir + "/config.yaml"
	t.Setenv("VARROACTL_CONFIG", configPath)

	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:   "prod",
				Server: "https://example.com",
				APIKey: "vk_abc123.def456",
			},
		},
	}
	saveConfig(cfg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && strings.Contains(r.URL.Path, "/me/apikeys/abc123") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// Update config to point to the test server
	cfg2, _ := loadConfig()
	cfg2.Contexts[0].Server = srv.URL
	saveConfig(cfg2)

	root := newRootCmd()
	root.SetArgs([]string{"logout", "--revoke"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(loaded.Contexts))
	}
	if loaded.Contexts[0].APIKey != "" {
		t.Errorf("expected API key cleared after revoke, got %q", loaded.Contexts[0].APIKey)
	}
}

// TestLogout_Revoke404 tests 404 on revoke (key already gone)
func TestLogout_Revoke404(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := tmpDir + "/config.yaml"
	t.Setenv("VARROACTL_CONFIG", configPath)

	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:   "prod",
				Server: "https://example.com",
				APIKey: "vk_abc123.def456",
			},
		},
	}
	saveConfig(cfg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg2, _ := loadConfig()
	cfg2.Contexts[0].Server = srv.URL
	saveConfig(cfg2)

	root := newRootCmd()
	root.SetArgs([]string{"logout", "--revoke"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLogout_RevokeFailure tests revoke with a non-204/non-404 status
func TestLogout_RevokeFailure(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := tmpDir + "/config.yaml"
	t.Setenv("VARROACTL_CONFIG", configPath)

	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:   "prod",
				Server: "https://example.com",
				APIKey: "vk_abc123.def456",
			},
		},
	}
	saveConfig(cfg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"error": "server error"})
	}))
	defer srv.Close()

	cfg2, _ := loadConfig()
	cfg2.Contexts[0].Server = srv.URL
	saveConfig(cfg2)

	root := newRootCmd()
	root.SetArgs([]string{"logout", "--revoke"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for revoke failure")
	}
	if !strings.Contains(err.Error(), "failed to revoke") {
		t.Errorf("expected 'failed to revoke', got %v", err)
	}
}
