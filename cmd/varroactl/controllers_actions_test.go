package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Action command tests
// ---------------------------------------------------------------------------

// TestAction_Restart tests `restart controller NS/NAME`
func TestAction_Restart(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/clusters/core/controllers/team-a/ctrl-1/restart") {
			t.Errorf("expected path with /clusters/core/..., got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "restarting"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"restart", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Reprovision tests `reprovision controller NS/NAME`
func TestAction_Reprovision(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/reprovision") {
			t.Errorf("expected path with /reprovision, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "reprovisioning"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"reprovision", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Reconcile tests `reconcile controller NS/NAME`
func TestAction_Reconcile(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/reconcile") {
			t.Errorf("expected path with /reconcile, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "triggered"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"reconcile", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Hibernate tests `hibernate controller NS/NAME`
func TestAction_Hibernate(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/clusters/core/controllers/team-a/ctrl-1/hibernate") {
			t.Errorf("expected path with /hibernate, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "hibernating"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"hibernate", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Wake tests `wake controller NS/NAME`
func TestAction_Wake(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/clusters/core/controllers/team-a/ctrl-1/wake") {
			t.Errorf("expected path with /wake, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "waking"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"wake", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Approve_Default tests `approve controller NS/NAME` (default action=approve)
func TestAction_Approve_Default(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/approve") {
			t.Errorf("expected path with /approve, got %s", r.URL.Path)
		}
		// Check body
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "approved", "action": "approve"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"approve", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Approve_CustomAction tests `approve controller NS/NAME --action reload`
func TestAction_Approve_CustomAction(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/approve") {
			t.Errorf("expected path with /approve, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "approved", "action": "reload"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"approve", "controller", "team-a/ctrl-1", "--action", "reload"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Approve_InvalidEnum tests invalid action → exit 2
func TestAction_Approve_InvalidEnum(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}))
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"approve", "controller", "team-a/ctrl-1", "--action", "bogus"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usageError for invalid action")
	}
	if !strings.Contains(err.Error(), "invalid action") {
		t.Errorf("expected 'invalid action' in error, got %v", err)
	}
}

// TestAction_Approve_Deletion tests `approve controller NS/NAME --deletion PATH`
func TestAction_Approve_Deletion(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/approve-deletion") {
			t.Errorf("expected path with /approve-deletion, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "approved", "path": "items/foo"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"approve", "controller", "team-a/ctrl-1", "--deletion", "items/foo"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Approve_ActionAndDeletionConflict tests --action + --deletion → usageError
func TestAction_Approve_ActionAndDeletionConflict(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}))
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"approve", "controller", "team-a/ctrl-1", "--action", "reload", "--deletion", "items/foo"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usageError")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got %v", err)
	}
}

// TestAction_Approve_409 tests 409 → exit 1 with server message
func TestAction_Approve_409(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": "no pending approval"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"approve", "controller", "team-a/ctrl-1"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for 409")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("expected '409' in error, got %v", err)
	}
}

// TestAction_Diff tests `diff controller NS/NAME`
func TestAction_Diff(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/diff") {
			t.Errorf("expected path with /diff, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"incoming": map[string]any{"items": "items content"},
			"applied":  map[string]any{"items": "applied content"},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"diff", "controller", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Logs tests top-level `logs NS/NAME`
func TestAction_Logs(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/logs") {
			t.Errorf("expected path with /logs, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"timestamp": "2024-01-15T10:00:00Z", "level": "info", "source": "mite", "message": "connected"},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"logs", "team-a/ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Preflight_Fail tests preflight with failing check → exit 1
func TestAction_Preflight_Fail(t *testing.T) {
	testSetup(t)
	cr := map[string]any{
		"metadata": map[string]any{"name": "test"},
		"spec":     map[string]any{"version": "2.0"},
	}
	data, _ := json.Marshal(cr)
	f := filepath.Join(t.TempDir(), "cr.yaml")
	os.WriteFile(f, data, 0644)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/preflight") {
			t.Errorf("expected path with /preflight, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"checks": []map[string]any{
				{"id": "ver-check", "status": "fail", "message": "version too low"},
				{"id": "ns-check", "status": "pass", "message": "ok"},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"preflight", "-n", "team-a", "-f", f})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for preflight fail")
	}
	if !strings.Contains(err.Error(), "preflight failed") {
		t.Errorf("expected 'preflight failed', got %v", err)
	}
}

// TestAction_Render tests render passes through raw YAML
func TestAction_Render(t *testing.T) {
	testSetup(t)
	rawYAML := "apiVersion: varroa.example.com/v1\nkind: Controller\nmetadata:\n  name: test\n"
	cr := map[string]any{
		"metadata": map[string]any{"name": "test"},
		"spec":     map[string]any{"version": "2.0"},
	}
	data, _ := json.Marshal(cr)
	f := filepath.Join(t.TempDir(), "cr.yaml")
	os.WriteFile(f, data, 0644)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/render") {
			t.Errorf("expected path with /render, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Write([]byte(rawYAML))
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"render", "-n", "team-a", "-f", f})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Preview tests preview with overlay file + baseline flag
func TestAction_Preview(t *testing.T) {
	testSetup(t)
	overlay := map[string]any{
		"podOverrides": map[string]any{"cpu": "2"},
		"probes": map[string]any{
			"startup": map[string]any{"failureThreshold": 60},
		},
	}
	data, _ := json.Marshal(overlay)
	f := filepath.Join(t.TempDir(), "overlay.yaml")
	os.WriteFile(f, data, 0644)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/preview") {
			t.Errorf("expected path with /preview, got %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode preview body: %v", err)
		}
		if _, ok := body["probes"].(map[string]any); !ok {
			t.Fatalf("expected probes in preview body, got %+v", body["probes"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"merged": map[string]string{"items": "merged content"},
			"diff":   map[string]string{"items": "diff content"},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"preview", "controller", "team-a/ctrl-1", "-f", f, "--baseline", "live"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAction_Restart_JSONOutput tests `restart controller NS/NAME -o json`
func TestAction_Restart_JSONOutput(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "restarting"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"restart", "controller", "team-a/ctrl-1", "-o", "json"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// restart controller --cluster dev-cluster
// ---------------------------------------------------------------------------

func TestAction_Restart_DevCluster(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/clusters/dev-cluster/controllers/team-a/ctrl-1/restart") {
			t.Errorf("expected /clusters/dev-cluster/..., got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "restarting"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"restart", "controller", "team-a/ctrl-1", "--cluster", "dev-cluster"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// approve controller --deletion --cluster dev-cluster
// ---------------------------------------------------------------------------

func TestAction_ApproveDeletion_DevCluster(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/clusters/dev-cluster/controllers/team-a/ctrl-1/approve-deletion") {
			t.Errorf("expected /clusters/dev-cluster/..., got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "approved", "path": "items/foo"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"approve", "controller", "team-a/ctrl-1", "--deletion", "items/foo", "--cluster", "dev-cluster"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// preflight -n NS -f FILE --cluster dev-cluster
// ---------------------------------------------------------------------------

func TestAction_Preflight_DevCluster(t *testing.T) {
	testSetup(t)
	cr := map[string]any{
		"metadata": map[string]any{"name": "test"},
		"spec":     map[string]any{"version": "2.0"},
	}
	data, _ := json.Marshal(cr)
	f := filepath.Join(t.TempDir(), "cr.yaml")
	os.WriteFile(f, data, 0644)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/clusters/dev-cluster/controllers/team-a/preflight") {
			t.Errorf("expected /clusters/dev-cluster/..., got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"checks": []map[string]any{
				{"id": "ver-check", "status": "pass", "message": "ok"},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"preflight", "-n", "team-a", "-f", f, "--cluster", "dev-cluster"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// power controller NS/NAME {running,stopped,hibernated}
// ---------------------------------------------------------------------------

func TestAction_Power_Hibernated(t *testing.T) {
	testSetup(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"power", "controller", "team-a/ctrl-1", "hibernated"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected a usage error for power controller ... hibernated")
	}
	if !strings.Contains(err.Error(), "hibernate controller") {
		t.Errorf("expected the error to point at hibernate controller, got %v", err)
	}
	if called {
		t.Error("server must not be called: hibernated is refused client-side, no spec.powerState patch is sent")
	}
}

func TestAction_Power_Running(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		spec, ok := body["spec"].(map[string]any)
		if !ok || spec["powerState"] != "Running" {
			t.Errorf("expected spec.powerState=Running, got %v", spec)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"status": "updated"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"power", "controller", "team-a/ctrl-1", "running"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAction_Power_Stopped(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		spec, ok := body["spec"].(map[string]any)
		if !ok || spec["powerState"] != "Stopped" {
			t.Errorf("expected spec.powerState=Stopped, got %v", spec)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"status": "updated"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"power", "controller", "team-a/ctrl-1", "stopped"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAction_Power_InvalidState(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called for invalid state")
	}))
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"power", "controller", "team-a/ctrl-1", "invalid"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usageError for invalid power state")
	}
	if !strings.Contains(err.Error(), "invalid power state") {
		t.Errorf("expected 'invalid power state' in error, got %v", err)
	}
	// Verify the error lists the allowed set
	if !strings.Contains(err.Error(), "running") || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("expected error to list allowed states, got %v", err)
	}
}
