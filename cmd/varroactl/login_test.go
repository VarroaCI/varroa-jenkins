package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// login tests
// ---------------------------------------------------------------------------

// TestLogin_BrowserHappyPath tests the browser loopback flow with a real callback
func TestLogin_BrowserHappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")
	t.Setenv("VARROACTL_SERVER", "")
	t.Setenv("VARROACTL_API_KEY", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" || strings.HasSuffix(r.URL.Path, "/me") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"email": "user@example.com", "subject": "user-1",
				"authMode": "local", "name": "user",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test_key_for_me_stub")

	urlCh := make(chan string, 1)
	oldBrowser := openBrowser
	openBrowser = func(u string) error {
		urlCh <- u
		return nil
	}
	t.Cleanup(func() { openBrowser = oldBrowser })

	root := newRootCmd()
	root.SetArgs([]string{"login", "--timeout", "5s"})
	errCh := make(chan error, 1)
	go func() {
		errCh <- root.Execute()
	}()

	// Wait for the browser URL
	var openedURL string
	select {
	case openedURL = <-urlCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for browser URL")
	}
	u, _ := url.Parse(openedURL)
	port := u.Query().Get("port")
	state := u.Query().Get("state")
	name := u.Query().Get("name")

	if port == "" {
		t.Fatal("port not found in URL")
	}
	if state == "" {
		t.Fatal("state not found in URL")
	}
	if !strings.Contains(name, "varroactl@") {
		t.Errorf("expected name containing varroactl@, got %s", name)
	}

	// Send callback from a separate HTTP client
	cbURL := fmt.Sprintf("http://127.0.0.1:%s/callback?state=%s&token=vk_testtoken.abc123", port, url.QueryEscape(state))
	resp, err := http.Get(cbURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for login to complete")
	}

	// Verify the context was stored
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext == "" {
		t.Fatal("expected current context to be set")
	}
	// Check the context has the API key
	for _, ctx := range cfg.Contexts {
		if ctx.Name == cfg.CurrentContext {
			if ctx.APIKey != "vk_testtoken.abc123" {
				t.Errorf("expected API key to be stored, got %s", ctx.APIKey)
			}
			break
		}
	}
}

// TestLogin_BrowserDenied tests the denied path
func TestLogin_BrowserDenied(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")
	t.Setenv("VARROACTL_SERVER", "")
	t.Setenv("VARROACTL_API_KEY", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	urlCh := make(chan string, 1)
	oldBrowser := openBrowser
	openBrowser = func(u string) error {
		urlCh <- u
		return nil
	}
	t.Cleanup(func() { openBrowser = oldBrowser })

	root := newRootCmd()
	root.SetArgs([]string{"login", "--timeout", "5s"})
	errCh := make(chan error, 1)
	go func() {
		errCh <- root.Execute()
	}()

	var openedURL string
	select {
	case openedURL = <-urlCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for browser URL")
	}

	u, _ := url.Parse(openedURL)
	port := u.Query().Get("port")
	state := u.Query().Get("state")

	cbURL := fmt.Sprintf("http://127.0.0.1:%s/callback?state=%s&error=denied", port, url.QueryEscape(state))
	resp, err := http.Get(cbURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error for denied login")
		}
		if !strings.Contains(err.Error(), "denied") {
			t.Errorf("expected 'denied' in error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}
}

// TestLogin_BrowserTimeout tests timeout
func TestLogin_BrowserTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	oldBrowser := openBrowser
	openBrowser = func(u string) error { return nil }
	t.Cleanup(func() { openBrowser = oldBrowser })

	root := newRootCmd()
	// Very short timeout
	root.SetArgs([]string{"login", "--timeout", "50ms"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected 'timed out' in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "--api-key") {
		t.Errorf("expected headless hint in error, got %v", err)
	}
}

// TestLogin_APIKey tests --api-key flow
func TestLogin_APIKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/me") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"email": "user@example.com", "subject": "u1", "authMode": "local", "name": "user",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test_key")

	root := newRootCmd()
	root.SetArgs([]string{"login", "--api-key", "vk_mytestkey.abc"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify stored
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext == "" {
		t.Fatal("expected current context")
	}
}

// TestLogin_APIKey401 tests --api-key with invalid key
func TestLogin_APIKey401(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"login", "--api-key", "vk_badkey"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("expected 'invalid API key', got %v", err)
	}
}

// TestLogin_APIKeyStdin tests --api-key - (read from stdin)
func TestLogin_APIKeyStdin(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/me") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"email": "user@example.com", "subject": "u1", "authMode": "local", "name": "user",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	oldStdin := os.Stdin
	pr, pw, _ := os.Pipe()
	pw.WriteString("vk_stdin_key.abc\n")
	pw.Close()
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	root := newRootCmd()
	root.SetArgs([]string{"login", "--api-key", "-"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLogin_Username_OIDCRefusal tests username flow when server uses OIDC
func TestLogin_Username_OIDCRefusal(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth-config") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"mode": "oidc"})
			return
		}
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"login", "--username", "testuser"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for OIDC refusal")
	}
	if !strings.Contains(err.Error(), "OIDC") {
		t.Errorf("expected OIDC error, got %v", err)
	}
}

// TestLogin_Username_401 tests username flow with invalid credentials
func TestLogin_Username_401(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth-config") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"mode": "local"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/login") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"login", "--username", "testuser", "--password-stdin"})
	oldStdin := os.Stdin
	pr, pw, _ := os.Pipe()
	pw.WriteString("wrongpass\n")
	pw.Close()
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for bad password")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("expected 'invalid credentials', got %v", err)
	}
}

// TestLogin_Username_Success tests the full username flow
func TestLogin_Username_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/auth-config") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"mode": "local"})
			return
		}
		if strings.HasSuffix(path, "/login") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id_token": "jwt_token_here", "expires_in": 3600,
			})
			return
		}
		if strings.HasSuffix(path, "/me/apikeys") || strings.HasSuffix(path, "/apikeys") {
			// Check that Bearer token is the JWT, not vk_
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer jwt") {
				t.Errorf("expected Bearer JWT, got %s", auth)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"token": "vk_created.abc123", "warning": "save it",
			})
			return
		}
		if strings.HasSuffix(path, "/me") {
			// This is called by validateAndStore with the vk_ token
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer vk_") {
				t.Errorf("expected Bearer vk_ token, got %s", auth)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"email": "user@example.com", "subject": "u1", "authMode": "local", "name": "user",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"login", "--username", "testuser", "--password-stdin"})
	oldStdin := os.Stdin
	pr, pw, _ := os.Pipe()
	pw.WriteString("correctpass\n")
	pw.Close()
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLogin_ContextNaming tests default context name (host) vs --context
func TestLogin_ContextNaming(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/me") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"email": "user@example.com", "subject": "u1", "authMode": "local", "name": "user",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	// Test with --context
	root := newRootCmd()
	root.SetArgs([]string{"login", "--api-key", "vk_mykey.abc", "--context", "my-context"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "my-context" {
		t.Errorf("expected context name 'my-context', got %q", cfg.CurrentContext)
	}
}

// TestLogin_APIKeyAndUsernameConflict tests --api-key and --username together
func TestLogin_APIKeyAndUsernameConflict(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	root := newRootCmd()
	root.SetArgs([]string{"login", "--api-key", "vk_key", "--username", "user"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usageError")
	}
}

// TestLogin_BrowserStateMismatch: a callback with the wrong state gets a 400
// and the flow keeps waiting; a subsequent good callback completes the login.
func TestLogin_BrowserStateMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/me") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"email": "user@example.com", "subject": "u1", "authMode": "local", "name": "user",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	urlCh := make(chan string, 1)
	oldBrowser := openBrowser
	openBrowser = func(u string) error {
		urlCh <- u
		return nil
	}
	t.Cleanup(func() { openBrowser = oldBrowser })

	root := newRootCmd()
	root.SetArgs([]string{"login", "--timeout", "5s"})
	errCh := make(chan error, 1)
	go func() { errCh <- root.Execute() }()

	var openedURL string
	select {
	case openedURL = <-urlCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for browser URL")
	}
	u, _ := url.Parse(openedURL)
	port := u.Query().Get("port")
	state := u.Query().Get("state")

	// Wrong state: expect 400, flow keeps waiting.
	badURL := fmt.Sprintf("http://127.0.0.1:%s/callback?state=WRONG&token=vk_evil.x", port)
	resp, err := http.Get(badURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for state mismatch, got %d", resp.StatusCode)
	}
	select {
	case err := <-errCh:
		t.Fatalf("login finished early after bad callback: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Good callback completes the flow.
	goodURL := fmt.Sprintf("http://127.0.0.1:%s/callback?state=%s&token=vk_good.abc", port, url.QueryEscape(state))
	resp2, err := http.Get(goodURL)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for login to complete")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfg.Contexts {
		if c.Name == cfg.CurrentContext && c.APIKey != "vk_good.abc" {
			t.Errorf("expected the good token stored, got %s", c.APIKey)
		}
	}
}

// TestLogin_Username_429 surfaces the rate-limit message.
func TestLogin_Username_429(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth-config") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"mode": "local"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/login") {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{"login", "--username", "testuser", "--password-stdin"})
	oldStdin := os.Stdin
	pr, pw, _ := os.Pipe()
	pw.WriteString("pass\n")
	pw.Close()
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	err := root.Execute()
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("expected 'rate limited', got %v", err)
	}
}

// TestLogin_TimeoutFlagWiring proves --timeout actually moves the deadline:
// with --timeout 1s a callback delivered after ~300ms still succeeds (the
// 50ms case in TestLogin_BrowserTimeout proves the short side).
func TestLogin_TimeoutFlagWiring(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/me") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"email": "user@example.com", "subject": "u1", "authMode": "local", "name": "user",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)

	urlCh := make(chan string, 1)
	oldBrowser := openBrowser
	openBrowser = func(u string) error {
		urlCh <- u
		return nil
	}
	t.Cleanup(func() { openBrowser = oldBrowser })

	root := newRootCmd()
	root.SetArgs([]string{"login", "--timeout", "1s"})
	errCh := make(chan error, 1)
	go func() { errCh <- root.Execute() }()

	var openedURL string
	select {
	case openedURL = <-urlCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for browser URL")
	}
	u, _ := url.Parse(openedURL)
	port := u.Query().Get("port")
	state := u.Query().Get("state")

	time.Sleep(300 * time.Millisecond)
	cbURL := fmt.Sprintf("http://127.0.0.1:%s/callback?state=%s&token=vk_late.abc", port, url.QueryEscape(state))
	resp, err := http.Get(cbURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("callback within the 1s deadline should succeed, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for login to complete")
	}
}
