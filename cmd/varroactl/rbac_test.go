package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
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

// jrbItem returns a JenkinsRoleBinding document as the BFF serves it. The
// spec grants Group:payments on the Global scope, so every column is set.
func jrbItem(name string, labels map[string]string) map[string]any {
	meta := map[string]any{"name": name}
	if labels != nil {
		asAny := make(map[string]any, len(labels))
		for k, v := range labels {
			asAny[k] = v
		}
		meta["labels"] = asAny
	}
	return map[string]any{
		"metadata": meta,
		"spec": map[string]any{
			"roleRef":      "varroa-admin",
			"jenkinsScope": map[string]any{"type": "Global"},
			"subjects":     []any{map[string]any{"kind": "Group", "name": "payments"}},
		},
	}
}

// ownedLabels returns the three labels that the operator stamps on an
// rbacSpec binding of prod-a/jenkins-1.
func ownedLabels() map[string]string {
	return map[string]string{
		v1alpha1.LabelManagedBy:           v1alpha1.ManagedByOperator,
		v1alpha1.LabelControllerNamespace: "prod-a",
		v1alpha1.LabelControllerName:      "jenkins-1",
	}
}

func TestJenkinsRoleBindingColumns_Controller(t *testing.T) {
	owned := v1alpha1.RBACSpecBindingName("prod-a", "jenkins-1", "admin")
	cases := []struct {
		name string
		item map[string]any
		want string
	}{
		{"owned", jrbItem(owned, ownedLabels()), "prod-a/jenkins-1"},
		{"hand-made without labels", jrbItem("payments-admins", nil), "-"},
		{"renamed copy keeps labels", jrbItem("payments-copy", ownedLabels()), "-"},
		{"earlier dash-joined name with labels", jrbItem("ctrl-prod-a-jenkins-1-admin", ownedLabels()), "-"},
		{"hash name without managed-by", jrbItem(owned, map[string]string{
			v1alpha1.LabelControllerNamespace: "prod-a",
			v1alpha1.LabelControllerName:      "jenkins-1",
		}), "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cols := jenkinsRoleBindingColumns(tc.item)
			if got := cols[len(cols)-1]; got != tc.want {
				t.Errorf("CONTROLLER column = %q, want %q (row %v)", got, tc.want, cols)
			}
		})
	}
}

func TestRBAC_JenkinsRoleBindings_ControllerColumn(t *testing.T) {
	testSetup(t)
	owned := v1alpha1.RBACSpecBindingName("prod-a", "jenkins-1", "admin")
	var gotPath string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				jrbItem(owned, ownedLabels()),
				jrbItem("payments-admins", nil),
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "jenkinsrolebindings"})
	var runErr error
	out := captureStdout(t, func() { runErr = root.Execute() })
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.HasSuffix(gotPath, "/api/v1/clusters/core/jenkinsrolebindings") {
		t.Errorf("request path = %s, want .../api/v1/clusters/core/jenkinsrolebindings", gotPath)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header plus two rows, got %d lines:\n%s", len(lines), out)
	}
	if header := strings.Fields(lines[0]); strings.Join(header, " ") != "NAME ROLE SUBJECTS JENKINS-SCOPE CONTROLLER" {
		t.Errorf("header = %v, want NAME ROLE SUBJECTS JENKINS-SCOPE CONTROLLER", header)
	}
	if last := lastField(lines[1]); last != "prod-a/jenkins-1" {
		t.Errorf("owned row CONTROLLER = %q, want prod-a/jenkins-1\n%s", last, out)
	}
	if last := lastField(lines[2]); last != "-" {
		t.Errorf("hand-made row CONTROLLER = %q, want -\n%s", last, out)
	}
}

func TestRBAC_JenkinsRoleBinding_GetSingleShowsController(t *testing.T) {
	testSetup(t)
	owned := v1alpha1.RBACSpecBindingName("prod-a", "jenkins-1", "admin")
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jrbItem(owned, ownedLabels()))
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "jenkinsrolebinding", owned})
	var runErr error
	out := captureStdout(t, func() { runErr = root.Execute() })
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("want header and a row, got:\n%s", out)
	}
	if last := lastField(lines[len(lines)-1]); last != "prod-a/jenkins-1" {
		t.Errorf("row CONTROLLER = %q, want prod-a/jenkins-1\n%s", last, out)
	}
}

func TestRBAC_JenkinsRoleBindings_JSONIsAPIObject(t *testing.T) {
	testSetup(t)
	owned := v1alpha1.RBACSpecBindingName("prod-a", "jenkins-1", "admin")
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{jrbItem(owned, ownedLabels())},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "jenkinsrolebindings", "-o", "json"})
	var runErr error
	out := captureStdout(t, func() { runErr = root.Execute() })
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("output is not a JSON list: %v\n%s", err, out)
	}
	if len(items) != 1 {
		t.Fatalf("want one item, got %d", len(items))
	}
	if _, ok := items[0]["controller"]; ok {
		t.Errorf("JSON item carries a computed controller key: %v", items[0])
	}
	meta, _ := items[0]["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	if labels[v1alpha1.LabelControllerName] != "jenkins-1" {
		t.Errorf("JSON item lost its labels: %v", meta)
	}
}

// lastField returns the last whitespace-separated field of a table row.
func lastField(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
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
