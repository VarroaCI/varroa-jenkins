package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Exhaustive test for one RBAC family (roles)
// ---------------------------------------------------------------------------

func TestRBAC_Roles_ListEnvelope(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/api/v1/roles") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"metadata": map[string]any{"name": "admin"}, "spec": map[string]any{"jenkinsRoleRef": "admin", "apiRules": []any{"rule1", "rule2"}}},
				{"metadata": map[string]any{"name": "viewer"}, "spec": map[string]any{"jenkinsRoleRef": "view", "apiRules": []any{"rule1"}}},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "roles"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRBAC_Roles_GetSingle(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/roles/admin") {
			t.Errorf("expected path /api/v1/roles/admin, got %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"name": "admin"},
			"spec":     map[string]any{"jenkinsRoleRef": "admin", "apiRules": []any{}},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "role", "admin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRBAC_Roles_Create(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/roles") {
			t.Errorf("expected path /api/v1/roles, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"name": "myrole"})
	})
	defer srv.Close()

	// Create temp YAML file
	tmp := tempFile(t, "role.yaml", "metadata:\n  name: myrole\nspec:\n  jenkinsRoleRef: admin\n")
	defer os.Remove(tmp)

	root := newRootCmd()
	root.SetArgs([]string{"create", "role", "-f", tmp})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRBAC_Roles_Edit(t *testing.T) {
	testSetup(t)
	callCount := 0
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// GET - return current doc
			if r.Method != "GET" {
				t.Errorf("call 1: expected GET, got %s", r.Method)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{
					"name":            "myrole",
					"resourceVersion": "123",
					"uid":             "abc",
				},
				"spec": map[string]any{
					"jenkinsRoleRef": "admin",
					"apiRules":       []any{},
				},
				"status": map[string]any{"phase": "active"},
			})
		} else {
			// PUT - should receive doc without status/metadata noise
			if r.Method != "PUT" {
				t.Errorf("call 2: expected PUT, got %s", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		}
	})
	defer srv.Close()

	origEditor := runEditorC3
	runEditorC3 = func(path string) error {
		// Write edited file: change jenkinsRoleRef
		content := "metadata:\n  name: myrole\nspec:\n  jenkinsRoleRef: superadmin\n  apiRules: []\n"
		return os.WriteFile(path, []byte(content), 0644)
	}
	defer func() { runEditorC3 = origEditor }()

	root := newRootCmd()
	root.SetArgs([]string{"edit", "role", "myrole"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount < 2 {
		t.Errorf("expected 2 calls (GET + PUT), got %d", callCount)
	}
}

func TestRBAC_Roles_Delete(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/roles/myrole") {
			t.Errorf("expected /api/v1/roles/myrole, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"delete", "role", "myrole"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRBAC_Roles_NamespaceRejected(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"get", "roles", "-n", "default"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for cluster-scoped -n")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected 'not supported' error, got: %v", err)
	}
}

func TestRBAC_Roles_NameOutput(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"metadata": map[string]any{"name": "admin"}},
				{"metadata": map[string]any{"name": "viewer"}},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "roles", "-o", "name"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Smoke tests for the other three families
// ---------------------------------------------------------------------------

func TestRBAC_RoleBindings_Smoke(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"metadata": map[string]any{"name": "binding1"},
					"spec": map[string]any{
						"roleRef": "admin",
						"subjects": []any{
							map[string]any{"kind": "User", "name": "alice"},
						},
					},
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "rolebindings"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRBAC_JenkinsRoles_Smoke(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"metadata": map[string]any{"name": "jrole1"},
					"spec":     map[string]any{"roleType": "Global", "permissions": []any{"perm1"}},
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "jenkinsroles"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRBAC_JenkinsRoleBindings_Smoke(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"metadata": map[string]any{"name": "jrb1"},
					"spec": map[string]any{
						"roleRef":      "jrole1",
						"jenkinsScope": map[string]any{"type": "Global"},
						"subjects":     []any{},
					},
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "jenkinsrolebindings"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func tempFile(t *testing.T, name, content string) string {
	t.Helper()
	tmp := t.TempDir() + "/" + name
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return tmp
}
