package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Groups tests
// ---------------------------------------------------------------------------

func TestGroup_List(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name":        "team-a",
					"displayName": "Team A",
					"memberCount": float64(3),
					"source":      "local",
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "groups"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGroup_Create(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"name": "newgroup"})
	})
	defer srv.Close()

	tmp := tempFile(t, "group.yaml", "name: newgroup\ndisplayName: New Group\n")
	defer os.Remove(tmp)

	root := newRootCmd()
	root.SetArgs([]string{"create", "group", "-f", tmp})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGroup_Delete(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"delete", "group", "mygroup"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGroup_DetailAndEditRejected(t *testing.T) {
	// There is no `get group NAME` or `edit group` registered.
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"get", "group", "somegroup"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for single group get (no detail endpoint)")
	}
}

// ---------------------------------------------------------------------------
// Users tests
// ---------------------------------------------------------------------------

func TestUser_List(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name":      "alice",
					"email":     "alice@example.com",
					"groups":    []any{"team-a"},
					"managedBy": "local",
					"lastLogin": "",
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "users"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUser_CreateWithPasswordStdin(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		// Verify password is in body
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["password"]; !ok {
			t.Error("password should be present in body")
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"name": "newuser"})
	})
	defer srv.Close()

	tmp := tempFile(t, "user.yaml", "username: newuser\nemail: new@example.com\n")
	defer os.Remove(tmp)

	// Write password to stdin
	r, w, _ := os.Pipe()
	w.Write([]byte("secret123\n"))
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	root := newRootCmd()
	root.SetArgs([]string{"create", "user", "-f", tmp, "--password-stdin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUser_EditSendsBothKeys(t *testing.T) {
	testSetup(t)
	callCount := 0
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// GET /users list to find the entry
			if r.Method != "GET" || !strings.Contains(r.URL.Path, "/api/v1/users") {
				t.Errorf("call 1: expected GET /api/v1/users, got %s %s", r.Method, r.URL.Path)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name":        "alice",
						"email":       "alice@old.com",
						"displayName": "Alice",
					},
				},
			})
		} else {
			// PUT /users/{name}
			if r.Method != "PUT" {
				t.Errorf("call 2: expected PUT, got %s", r.Method)
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			// Must always send both keys
			if _, ok := body["email"]; !ok {
				t.Error("PUT body must include email")
			}
			if _, ok := body["displayName"]; !ok {
				t.Error("PUT body must include displayName")
			}
			w.WriteHeader(http.StatusOK)
		}
	})
	defer srv.Close()

	origEditor := runEditorC3
	runEditorC3 = func(path string) error {
		content := "email: alice@new.com\ndisplayName: Alice New\n"
		return os.WriteFile(path, []byte(content), 0644)
	}
	defer func() { runEditorC3 = origEditor }()

	root := newRootCmd()
	root.SetArgs([]string{"edit", "user", "alice"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUser_Delete(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"delete", "user", "alice"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
