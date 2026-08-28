package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestApikey_List tests the apikey list command with envelope unwrap.
func TestApikey_List(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/me/apikeys") {
			t.Errorf("expected /api/v1/me/apikeys, got %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"prefix":   "vk_abc",
					"name":     "my-key",
					"created":  "2024-01-01T00:00:00Z",
					"expires":  "",
					"lastUsed": "",
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"apikey", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApikey_ListAdmin tests the admin path for listing another user's keys.
func TestApikey_ListAdmin(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v1/users/alice/apikeys") {
			t.Errorf("expected /api/v1/users/alice/apikeys, got %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"apikey", "list", "--user", "alice"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApikey_Create tests the create command body and stdout token.
func TestApikey_Create(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["name"]; ok {
			t.Error("create without args should omit name")
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"token":   "vk_new_token",
			"warning": "save this key",
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"apikey", "create"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApikey_CreateWithNameAndExpiry tests creating with name and --expires-in.
func TestApikey_CreateWithNameAndExpiry(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if n, ok := body["name"]; !ok || n != "mykey" {
			t.Errorf("expected name 'mykey', got %v", n)
		}
		if e, ok := body["expiresIn"]; !ok || e != "720h" {
			t.Errorf("expected expiresIn '720h', got %v", e)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"token":   "vk_new_token",
			"warning": "save this key",
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"apikey", "create", "mykey", "--expires-in", "720h"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApikey_CreateInvalidExpiry tests that invalid --expires-in causes usageError (exit 2).
func TestApikey_CreateInvalidExpiry(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"apikey", "create", "--expires-in", "30d"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

// TestApikey_CreateZeroExpiry tests that --expires-in "0s" is rejected.
func TestApikey_CreateZeroExpiry(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"apikey", "create", "--expires-in", "0s"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for zero duration")
	}
}

// TestApikey_Revoke tests revoke 204 success.
func TestApikey_Revoke(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/me/apikeys/vk_abc") {
			t.Errorf("expected /api/v1/me/apikeys/vk_abc, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"apikey", "revoke", "vk_abc"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApikey_RevokeNotFound tests revoke 404.
func TestApikey_RevokeNotFound(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"apikey", "revoke", "vk_abc"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

// TestApikey_Rotate tests successful rotation.
func TestApikey_Rotate(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/me/apikeys/vk_abc/rotate") {
			t.Errorf("expected /api/v1/me/apikeys/vk_abc/rotate, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"token":   "vk_new_token",
			"warning": "old key will be revoked",
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"apikey", "rotate", "vk_abc"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApikey_RotatePartialFailure tests the 500 partial-failure path.
func TestApikey_RotatePartialFailure(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error":    "old key revocation failed",
			"newToken": "vk_partial_new_token",
		})
	})
	defer srv.Close()

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	root := newRootCmd()
	root.SetArgs([]string{"apikey", "rotate", "vk_abc"})
	err := root.Execute()

	w.Close()
	os.Stdout = oldStdout
	var stdoutBuf strings.Builder
	_ = copyBuffer(&stdoutBuf, r)

	// Should exit with error (partial failure)
	if err == nil {
		t.Fatal("expected error for partial failure")
	}

	// But stdout should contain the new token
	if !strings.Contains(stdoutBuf.String(), "vk_partial_new_token") {
		t.Errorf("expected new token in stdout, got: %s", stdoutBuf.String())
	}
}
