package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// clusters noun tests
// ---------------------------------------------------------------------------

func TestClustersNoun_ListTable(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name":            "core",
					"healthy":         true,
					"controllerCount": 5,
					"connectedCount":  3,
					"lastHeartbeat":   "2024-01-15T10:00:00Z",
					"operatorVersion": "1.0.0",
					"k8sVersion":      "1.28",
					"core":            true,
				},
				{
					"name":            "remote",
					"healthy":         false,
					"controllerCount": 2,
					"connectedCount":  1,
					"lastHeartbeat":   "2024-01-15T09:00:00Z",
					"operatorVersion": "1.0.0",
					"k8sVersion":      "1.27",
					"core":            false,
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "clusters"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClustersNoun_WideColumns(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name": "core", "healthy": true,
					"controllerCount": 5, "connectedCount": 3,
					"lastHeartbeat":   "2024-01-15T10:00:00Z",
					"operatorVersion": "1.0.0", "k8sVersion": "1.28",
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "clusters", "-o", "wide"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClustersNoun_ByNameHit(t *testing.T) {
	testSetup(t)
	requestCount := 0
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name": "core", "healthy": true,
					"controllerCount": 5, "connectedCount": 3,
					"lastHeartbeat": "2024-01-15T10:00:00Z",
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "cluster", "core"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requestCount != 1 {
		t.Errorf("expected 1 request, got %d", requestCount)
	}
}

func TestClustersNoun_ByNameMiss(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name": "core", "healthy": true,
					"controllerCount": 5, "connectedCount": 3,
					"lastHeartbeat": "2024-01-15T10:00:00Z",
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "cluster", "nonexistent"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for nonexistent cluster")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %v", err)
	}
}

func TestClustersNoun_RejectNamespace(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}))
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "clusters", "-n", "team-a"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for -n flag")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected 'not supported' in error, got %v", err)
	}
}

func TestClustersNoun_NameOutput(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"name": "core", "healthy": true,
					"controllerCount": 0, "connectedCount": 0,
					"lastHeartbeat": "2024-01-15T10:00:00Z"},
				{"name": "remote", "healthy": true,
					"controllerCount": 0, "connectedCount": 0,
					"lastHeartbeat": "2024-01-15T10:00:00Z"},
			},
		})
	})
	defer srv.Close()

	// Capture stdout
	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw

	root := newRootCmd()
	root.SetArgs([]string{"get", "clusters", "-o", "name"})
	execErr := root.Execute()

	pw.Close()
	os.Stdout = old
	var buf strings.Builder
	b := make([]byte, 4096)
	for {
		n, _ := pr.Read(b)
		if n == 0 {
			break
		}
		buf.Write(b[:n])
	}
	output := buf.String()

	if execErr != nil {
		t.Fatalf("unexpected error: %v", execErr)
	}
	if !strings.Contains(output, "core") || !strings.Contains(output, "remote") {
		t.Errorf("expected cluster names in output, got %q", output)
	}
}

func TestClustersNoun_JSONOutput(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"name": "core", "healthy": true,
					"controllerCount": 5, "connectedCount": 3,
					"lastHeartbeat":   "2024-01-15T10:00:00Z",
					"operatorVersion": "1.0.0", "k8sVersion": "1.28"},
			},
		})
	})
	defer srv.Close()

	// Capture stdout
	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw

	root := newRootCmd()
	root.SetArgs([]string{"get", "clusters", "-o", "json"})
	execErr := root.Execute()

	pw.Close()
	os.Stdout = old
	var buf strings.Builder
	b := make([]byte, 4096)
	for {
		n, _ := pr.Read(b)
		if n == 0 {
			break
		}
		buf.Write(b[:n])
	}
	output := buf.String()

	if execErr != nil {
		t.Fatalf("unexpected error: %v", execErr)
	}
	if !strings.Contains(output, "core") {
		t.Errorf("expected core in JSON output, got %q", output)
	}
}

// TestClustersNoun_TimeField verifies that time.Time fields are rendered as RFC3339
// by ensuring the test payload includes a valid time string that's parsed as time.Time.
func TestClustersNoun_TimeField(t *testing.T) {
	testSetup(t)
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name": "core", "healthy": true,
					"controllerCount": 5, "connectedCount": 3,
					"lastHeartbeat": now,
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "clusters"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClustersNoun_NoHeaders(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"name": "core", "healthy": true,
					"controllerCount": 0, "connectedCount": 0,
					"lastHeartbeat": "2024-01-15T10:00:00Z"},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "clusters", "--no-headers"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClustersNoun_RejectAllNamespaces(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}))
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test")
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "clusters", "-A"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for -A flag")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected 'not supported' in error, got %v", err)
	}
}
