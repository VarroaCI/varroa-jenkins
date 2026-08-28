package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestPasswd_SelfBody tests the body of the self-password PUT request.
func TestPasswd_SelfBody(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/me/password") {
			t.Errorf("expected /api/v1/me/password, got %s", r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["oldPassword"]; !ok {
			t.Error("body should contain oldPassword")
		}
		if _, ok := body["newPassword"]; !ok {
			t.Error("body should contain newPassword")
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	defer srv.Close()

	// Replace password prompts with canned responses
	readOldPasswordFn = func() ([]byte, error) { return []byte("oldpass"), nil }
	readNewPasswordFn = func() ([]byte, error) { return []byte("newpass"), nil }
	readConfirmPasswordFn = func() ([]byte, error) { return []byte("newpass"), nil }

	root := newRootCmd()
	root.SetArgs([]string{"passwd"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPasswd_AdminBody tests the admin password PUT request.
func TestPasswd_AdminBody(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/users/alice/password") {
			t.Errorf("expected /api/v1/users/alice/password, got %s", r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["newPassword"]; !ok {
			t.Error("body should contain newPassword")
		}
		if _, ok := body["oldPassword"]; ok {
			t.Error("admin body should NOT contain oldPassword")
		}
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	readNewPasswordFn = func() ([]byte, error) { return []byte("newpass"), nil }
	readConfirmPasswordFn = func() ([]byte, error) { return []byte("newpass"), nil }

	root := newRootCmd()
	root.SetArgs([]string{"passwd", "alice"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPasswd_Mismatch tests that password mismatch returns a usage error.
func TestPasswd_Mismatch(t *testing.T) {
	testSetup(t)
	readNewPasswordFn = func() ([]byte, error) { return []byte("pass1"), nil }
	readConfirmPasswordFn = func() ([]byte, error) { return []byte("pass2"), nil }

	root := newRootCmd()
	root.SetArgs([]string{"passwd", "alice"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for password mismatch")
	}
	if !strings.Contains(err.Error(), "passwords do not match") {
		t.Errorf("expected 'passwords do not match', got: %v", err)
	}
}

// TestPasswd_405Error tests that a 405 surfaces as an error.
func TestPasswd_405Error(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "password changes not available in current auth mode"})
	})
	defer srv.Close()

	readOldPasswordFn = func() ([]byte, error) { return []byte("oldpass"), nil }
	readNewPasswordFn = func() ([]byte, error) { return []byte("newpass"), nil }
	readConfirmPasswordFn = func() ([]byte, error) { return []byte("newpass"), nil }

	root := newRootCmd()
	root.SetArgs([]string{"passwd"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for 405")
	}
	if !strings.Contains(err.Error(), "405") {
		t.Errorf("expected 405 in error, got: %v", err)
	}
}
