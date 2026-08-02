package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// diffMap unit tests (design §8.1)
// ---------------------------------------------------------------------------

func TestDiffMap_NoChange(t *testing.T) {
	a := map[string]any{"name": "test", "phase": "Running"}
	b := map[string]any{"name": "test", "phase": "Running"}
	result := diffMap(a, b)
	if result != nil {
		t.Errorf("expected nil for no change, got %v", result)
	}
}

func TestDiffMap_DeletionToNull(t *testing.T) {
	a := map[string]any{"name": "test", "oldField": "value"}
	b := map[string]any{"name": "test"}
	result := diffMap(a, b)
	r, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if v, exists := r["oldField"]; !exists || v != nil {
		t.Errorf("expected oldField=null (nil), got %v", v)
	}
}

func TestDiffMap_ArrayReplace(t *testing.T) {
	a := map[string]any{"items": []any{"a", "b"}}
	b := map[string]any{"items": []any{"c", "d"}}
	result := diffMap(a, b)
	r, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	items, ok := r["items"].([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", r["items"])
	}
	if len(items) != 2 || items[0] != "c" {
		t.Errorf("expected [c d], got %v", items)
	}
}

// Exact nested-map case from the task: spec.ingressSpec.host changed,
// spec.ingressSpec.mode untouched → patch is {"spec":{"ingressSpec":{"host":"new"}}}
func TestDiffMap_NestedMerge(t *testing.T) {
	a := map[string]any{
		"spec": map[string]any{
			"ingressSpec": map[string]any{
				"host": "old.example.com",
				"mode": "subdomain",
			},
			"version": "1.0",
		},
	}
	b := map[string]any{
		"spec": map[string]any{
			"ingressSpec": map[string]any{
				"host": "new.example.com",
				"mode": "subdomain",
			},
			"version": "1.0",
		},
	}
	result := diffMap(a, b)
	r, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	spec, ok := r["spec"].(map[string]any)
	if !ok {
		t.Fatalf("expected spec map, got %T", r["spec"])
	}
	ingress, ok := spec["ingressSpec"].(map[string]any)
	if !ok {
		t.Fatalf("expected ingressSpec map, got %T", spec["ingressSpec"])
	}
	if host, ok := ingress["host"].(string); !ok || host != "new.example.com" {
		t.Errorf("expected host=new.example.com, got %v", ingress["host"])
	}
	// mode should NOT be in the patch since it didn't change
	if _, exists := ingress["mode"]; exists {
		t.Errorf("mode should not be in patch (unchanged)")
	}
	// version should NOT be in the patch
	if _, exists := spec["version"]; exists {
		t.Errorf("version should not be in patch (unchanged)")
	}
}

func TestDiffMap_EmptyPatch(t *testing.T) {
	a := map[string]any{"name": "test", "spec": map[string]any{"a": "b"}}
	b := map[string]any{"name": "test", "spec": map[string]any{"a": "b"}}
	result := diffMap(a, b)
	if result != nil {
		t.Errorf("expected nil for empty patch, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// stripHashComments tests
// ---------------------------------------------------------------------------

func TestStripHashComments(t *testing.T) {
	input := "# Please edit the object below.\n# Lines starting with '#' will be ignored.\n#\nname: test\nphase: Running\n"
	expected := "name: test\nphase: Running\n"
	result := stripHashComments(input)
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

// ---------------------------------------------------------------------------
// Helpers for httptest-based controller command tests
// ---------------------------------------------------------------------------

// testSetup creates a temp config dir and clears env vars.
func testSetup(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	t.Setenv("VARROACTL_CONFIG", configPath)
	t.Setenv("VARROACTL_CONTEXT", "")
	t.Setenv("VARROACTL_SERVER", "")
	t.Setenv("VARROACTL_API_KEY", "")
}

// testServer starts an httptest server and sets VARROACTL_SERVER + VARROACTL_API_KEY.
func testServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test_key")
	return srv
}

// ---------------------------------------------------------------------------
// get controllers (list)
// ---------------------------------------------------------------------------

func TestController_GetList_NFlag(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/api/v1/controllers") {
			t.Errorf("expected /api/v1/controllers, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("namespace") != "team-a" {
			t.Errorf("expected namespace=team-a, got %s", r.URL.Query().Get("namespace"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"namespace": "team-a", "name": "ctrl-1", "phase": "Running",
					"version": strPtr("2.0"), "miteConnected": true,
					"jenkinsHealth": strPtr("ok"), "endpoint": "https://ctrl-1.example.com",
					"routingMode": "subdomain",
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "controllers", "-n", "team-a"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestController_GetList_AFlag(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") != "" {
			t.Errorf("expected no namespace param for -A, got %s", r.URL.Query().Get("namespace"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "controllers", "-A"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// get controller (single)
// ---------------------------------------------------------------------------

func TestController_GetSingle(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v1/clusters/core/controllers/team-a/ctrl-1") {
			t.Errorf("expected path containing /api/v1/clusters/core/controllers/team-a/ctrl-1, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"namespace": "team-a", "name": "ctrl-1", "phase": "Running",
			"version": "2.0", "miteConnected": true, "endpoint": "https://ctrl-1.example.com",
			"routingMode": "subdomain",
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestController_GetSingleYaml(t *testing.T) {
	testSetup(t)
	rawCR := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: ctrl-1\n  namespace: team-a\nspec:\n  version: \"2.0\"\n"
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/yaml") {
			t.Errorf("expected path ending in /yaml, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Write([]byte(rawCR))
	})
	defer srv.Close()

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw

	root := newRootCmd()
	root.SetArgs([]string{"get", "controller", "team-a/ctrl-1", "-o", "yaml"})
	execErr := root.Execute()

	pw.Close()
	os.Stdout = old
	var buf strings.Builder
	b := make([]byte, 1024)
	for {
		n, _ := pr.Read(b)
		if n == 0 {
			break
		}
		buf.Write(b[:n])
	}

	if execErr != nil {
		t.Fatalf("unexpected error: %v", execErr)
	}
	if buf.String() != rawCR {
		t.Errorf("expected verbatim CR YAML:\n%q\n\ngot:\n%q", rawCR, buf.String())
	}
}

// ---------------------------------------------------------------------------
// create controller
// ---------------------------------------------------------------------------

func TestController_Create(t *testing.T) {
	testSetup(t)
	cr := map[string]any{
		"metadata": map[string]any{"name": "new-ctrl"},
		"spec": map[string]any{
			"version": "2.0",
			"probes": map[string]any{
				"startup": map[string]any{"failureThreshold": 60},
			},
		},
	}
	data, _ := yaml.Marshal(cr)
	f := filepath.Join(t.TempDir(), "cr.yaml")
	os.WriteFile(f, data, 0644)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/clusters/core/controllers/team-a") {
			t.Errorf("expected path with /clusters/core/controllers/team-a, got %s", r.URL.Path)
		}
		var got v1alpha1.Controller
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if got.Spec.Probes == nil || got.Spec.Probes.Startup == nil || got.Spec.Probes.Startup.FailureThreshold == nil || *got.Spec.Probes.Startup.FailureThreshold != 60 {
			t.Fatalf("request body lost probes: %#v", got.Spec.Probes)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"status": "created"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"create", "controller", "-f", f, "-n", "team-a"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ns conflict with metadata.namespace and -n → usageError + exit 2
func TestController_CreateNSConflict(t *testing.T) {
	testSetup(t)
	cr := map[string]any{
		"metadata": map[string]any{
			"name":      "new-ctrl",
			"namespace": "from-file",
		},
		"spec": map[string]any{"version": "2.0"},
	}
	data, _ := yaml.Marshal(cr)
	f := filepath.Join(t.TempDir(), "cr.yaml")
	os.WriteFile(f, data, 0644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}))
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"create", "controller", "-f", f, "-n", "from-cli"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usageError for namespace conflict")
	}
	if !strings.Contains(err.Error(), "namespace conflict") {
		t.Errorf("expected 'namespace conflict' in error, got %v", err)
	}
}

// preflight-400 → checks table + exit 1
func TestController_CreatePreflight400(t *testing.T) {
	testSetup(t)
	cr := map[string]any{
		"metadata": map[string]any{"name": "new-ctrl"},
		"spec":     map[string]any{"version": "2.0"},
	}
	data, _ := yaml.Marshal(cr)
	f := filepath.Join(t.TempDir(), "cr.yaml")
	os.WriteFile(f, data, 0644)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "preflight failed",
			"checks": []map[string]any{
				{"id": "ver-check", "status": "fail", "message": "version too low"},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"create", "controller", "-f", f, "-n", "test-ns"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for preflight failure")
	}
	if !strings.Contains(err.Error(), "preflight failed") {
		t.Errorf("expected 'preflight failed', got %v", err)
	}
}

// ---------------------------------------------------------------------------
// delete controller
// ---------------------------------------------------------------------------

func TestController_Delete(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"delete", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// patch controller
// ---------------------------------------------------------------------------

func TestController_Patch(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "patched"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"patch", "controller", "team-a/ctrl-1", "-p", `{"spec":{"version":"3.0"}}`})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// edit controller
// ---------------------------------------------------------------------------

func TestController_Edit_NoOp(t *testing.T) {
	testSetup(t)
	rawCR := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: ctrl-1\nspec:\n  version: \"2.0\"\n"
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/yaml") {
			w.Header().Set("Content-Type", "application/yaml")
			w.Write([]byte(rawCR))
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"status": "edited"})
	})
	defer srv.Close()

	oldEditor := runEditor
	runEditor = func(path string) error { return nil }
	t.Cleanup(func() { runEditor = oldEditor })

	root := newRootCmd()
	root.SetArgs([]string{"edit", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestController_Edit_SpecChange(t *testing.T) {
	testSetup(t)
	rawCR := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: ctrl-1\n  namespace: team-a\nspec:\n  version: \"2.0\"\n"
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/yaml") {
			w.Header().Set("Content-Type", "application/yaml")
			w.Write([]byte(rawCR))
			return
		}
		if r.Method == "PATCH" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"status": "edited"})
		}
	})
	defer srv.Close()

	oldEditor := runEditor
	runEditor = func(path string) error {
		m := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: ctrl-1\n  namespace: team-a\nspec:\n  version: \"3.0\"\n"
		return os.WriteFile(path, []byte(m), 0644)
	}
	t.Cleanup(func() { runEditor = oldEditor })

	root := newRootCmd()
	root.SetArgs([]string{"edit", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestController_Edit_400KeepsTemp(t *testing.T) {
	testSetup(t)
	rawCR := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: ctrl-1\nspec:\n  version: \"2.0\"\n"
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/yaml") {
			w.Header().Set("Content-Type", "application/yaml")
			w.Write([]byte(rawCR))
			return
		}
		if r.Method == "PATCH" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid spec"})
		}
	})
	defer srv.Close()

	oldEditor := runEditor
	runEditor = func(path string) error {
		m := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: ctrl-1\nspec:\n  version: \"3.0\"\n"
		return os.WriteFile(path, []byte(m), 0644)
	}
	t.Cleanup(func() { runEditor = oldEditor })

	root := newRootCmd()
	root.SetArgs([]string{"edit", "controller", "team-a/ctrl-1"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "patch rejected") {
		t.Errorf("expected 'patch rejected' in error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// describe controller
// ---------------------------------------------------------------------------

func TestController_Describe(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"namespace": "team-a", "name": "ctrl-1", "phase": "Running",
			"version": "2.0", "miteConnected": true, "endpoint": "https://ctrl-1.example.com",
			"routingMode": "subdomain",
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"describe", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// get controller --cluster dev-cluster (single)
// ---------------------------------------------------------------------------

func TestController_GetSingle_ClusterFlag(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v1/clusters/dev-cluster/controllers/team-a/ctrl-1") {
			t.Errorf("expected /clusters/dev-cluster/..., got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"namespace": "team-a", "name": "ctrl-1", "phase": "Running",
			"version": "2.0", "miteConnected": true, "endpoint": "https://ctrl-1.example.com",
			"routingMode": "subdomain",
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "controller", "team-a/ctrl-1", "--cluster", "dev-cluster"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// get controller with config defaultCluster
// ---------------------------------------------------------------------------

func TestController_GetSingle_DefaultClusterFromConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	t.Setenv("VARROACTL_CONFIG", configPath)
	t.Setenv("VARROACTL_SERVER", "")
	t.Setenv("VARROACTL_API_KEY", "")
	t.Setenv("VARROACTL_CONTEXT", "")

	// Seed config with a context that has defaultCluster: staging
	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:           "prod",
				Server:         "TO_BE_OVERRIDDEN",
				APIKey:         "vk_test",
				DefaultCluster: "staging",
			},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v1/clusters/staging/") {
			t.Errorf("expected /clusters/staging/..., got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"namespace": "team-a", "name": "ctrl-1", "phase": "Running",
			"version": "2.0", "miteConnected": true, "endpoint": "https://ctrl-1.example.com",
			"routingMode": "subdomain",
		})
	}))
	defer srv.Close()

	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")

	root := newRootCmd()
	root.SetArgs([]string{"get", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// get controllers --cluster dev-cluster (list filter)
// ---------------------------------------------------------------------------

func TestController_GetList_ClusterFilter(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cluster") != "dev-cluster" {
			t.Errorf("expected cluster=dev-cluster, got %s", r.URL.Query().Get("cluster"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "controllers", "--cluster", "dev-cluster"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// get controllers --all-clusters overrides config default
// ---------------------------------------------------------------------------

func TestController_GetList_AllClustersOverride(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	t.Setenv("VARROACTL_CONFIG", configPath)
	t.Setenv("VARROACTL_SERVER", "")
	t.Setenv("VARROACTL_API_KEY", "")
	t.Setenv("VARROACTL_CONTEXT", "")

	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:           "prod",
				Server:         "TO_BE_OVERRIDDEN",
				APIKey:         "vk_test",
				DefaultCluster: "staging",
			},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cluster") != "" {
			t.Errorf("expected no cluster param with --all-clusters, got %s", r.URL.Query().Get("cluster"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	}))
	defer srv.Close()

	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")

	root := newRootCmd()
	root.SetArgs([]string{"get", "controllers", "--all-clusters"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// get controllers --cluster x --all-clusters → exit 2, no request
// ---------------------------------------------------------------------------

func TestController_GetList_ClusterAndAllClustersExclusive(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}))
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "controllers", "--cluster", "dev-cluster", "--all-clusters"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usage error for mutually exclusive flags")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// get controllers no flag → no cluster query param
// ---------------------------------------------------------------------------

func TestController_GetList_NoClusterQuery(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cluster") != "" {
			t.Errorf("expected no cluster param, got %s", r.URL.Query().Get("cluster"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "controllers"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// create controller asserts clusters/core path
// ---------------------------------------------------------------------------

func TestController_Create_CorePath(t *testing.T) {
	testSetup(t)
	cr := map[string]any{
		"metadata": map[string]any{"name": "new-ctrl"},
		"spec":     map[string]any{"version": "2.0"},
	}
	data, _ := yaml.Marshal(cr)
	f := filepath.Join(t.TempDir(), "cr.yaml")
	os.WriteFile(f, data, 0644)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/api/v1/clusters/core/controllers/team-a") {
			t.Errorf("expected path with /clusters/core/controllers/team-a, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"status": "created"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"create", "controller", "-f", f, "-n", "team-a"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// edit controller --cluster dev-cluster asserts both YAML GET and PATCH hit dev-cluster
// ---------------------------------------------------------------------------

func TestController_Edit_ClusterDevCluster(t *testing.T) {
	testSetup(t)
	rawCR := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: ctrl-1\n  namespace: team-a\nspec:\n  version: \"2.0\"\n"
	var yamlRequested, patchRequested bool
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/yaml") {
			if !strings.Contains(r.URL.Path, "/clusters/dev-cluster/") {
				t.Errorf("expected YAML GET on /clusters/dev-cluster/, got %s", r.URL.Path)
			}
			yamlRequested = true
			w.Header().Set("Content-Type", "application/yaml")
			w.Write([]byte(rawCR))
			return
		}
		if r.Method == "PATCH" {
			if !strings.Contains(r.URL.Path, "/clusters/dev-cluster/") {
				t.Errorf("expected PATCH on /clusters/dev-cluster/, got %s", r.URL.Path)
			}
			patchRequested = true
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"status": "edited"})
		}
	})
	defer srv.Close()

	oldEditor := runEditor
	runEditor = func(path string) error {
		m := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: ctrl-1\n  namespace: team-a\nspec:\n  version: \"3.0\"\n"
		return os.WriteFile(path, []byte(m), 0644)
	}
	t.Cleanup(func() { runEditor = oldEditor })

	root := newRootCmd()
	root.SetArgs([]string{"edit", "controller", "team-a/ctrl-1", "--cluster", "dev-cluster"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !yamlRequested {
		t.Error("expected YAML GET request")
	}
	if !patchRequested {
		t.Error("expected PATCH request")
	}
}
