package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// varroa-cli delta: "runners" is an ordinary CRD noun over the generated
// client, alias "rnr" only, cluster-scoped with no namespace segment.
// ---------------------------------------------------------------------------

func TestRunnersNoun_GetList(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runners" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name": "win-1",
					"spec": map[string]any{
						"tenant": "payments", "launcher": "inbound", "remoteFS": "/var/j",
					},
					"status": map[string]any{
						"cell": "cell-1", "connected": true, "lastSeen": "2026-09-11T00:00:00Z",
					},
				},
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "runners"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnersNoun_GetSingle(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runners/win-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "win-1",
			"spec": map[string]any{
				"tenant": "payments", "launcher": "inbound", "remoteFS": "/var/j",
			},
			"status": map[string]any{"connected": false},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "runners", "win-1", "-o", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnersNoun_GetSingle_NameOutput(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "win-1",
			"spec": map[string]any{
				"tenant": "payments", "launcher": "inbound", "remoteFS": "/var/j",
			},
			"status": map[string]any{"connected": false},
		})
	})
	defer srv.Close()

	stdout := captureStdout(t, func() {
		root := newRootCmd()
		root.SetArgs([]string{"get", "runners", "win-1", "-o", "name"})
		if err := root.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if strings.TrimSpace(stdout) != "win-1" {
		t.Errorf("expected bare name \"win-1\" (runners is cluster-scoped), got %q", stdout)
	}
}

func TestRunnersNoun_Describe(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runners/win-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "win-1",
			"spec": map[string]any{
				"tenant": "payments", "launcher": "inbound", "remoteFS": "/var/j",
			},
			"status": map[string]any{"connected": false},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"describe", "runners", "win-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnersNoun_Create(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runners" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("invalid request body: %v", err)
		}
		if req["name"] != "win-1" {
			t.Errorf("expected name win-1, got %v", req["name"])
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "win-1"})
	})
	defer srv.Close()

	tmp := tempFile(t, "runner.yaml",
		"metadata:\n  name: win-1\nspec:\n  tenant: payments\n  launcher: inbound\n  remoteFS: /var/j\n")

	root := newRootCmd()
	root.SetArgs([]string{"create", "runners", "-f", tmp})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnersNoun_Delete(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/runners/win-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"delete", "runners", "win-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRunnersNoun_Patch proves task 10.0b/10.1: "varroactl patch runners
// NAME -p JSON" reaches the generated client as a merge patch.
func TestRunnersNoun_Patch(t *testing.T) {
	testSetup(t)
	var gotContentType string
	var gotBody []byte
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/runners/win-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "win-1"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"patch", "runners", "win-1", "-p", `{"spec":{"numExecutors":2}}`})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentType != "application/merge-patch+json" {
		t.Errorf("expected content-type application/merge-patch+json, got %q", gotContentType)
	}
	var patch map[string]any
	if err := json.Unmarshal(gotBody, &patch); err != nil {
		t.Fatalf("invalid patch body: %v", err)
	}
	spec, ok := patch["spec"].(map[string]any)
	if !ok {
		t.Fatalf("expected spec in patch body, got %v", patch)
	}
	if spec["numExecutors"] != float64(2) {
		t.Errorf("expected numExecutors=2, got %v", spec["numExecutors"])
	}
}

func TestRunnersNoun_PatchHelpListsRunners(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	patchCmd, _, err := root.Find([]string{"patch"})
	if err != nil {
		t.Fatalf("patch parent not found: %v", err)
	}
	found := false
	for _, sub := range patchCmd.Commands() {
		if sub.Name() == "runners" {
			found = true
		}
	}
	if !found {
		t.Error("expected \"patch\" to list a \"runners\" subcommand")
	}
}

func TestRunnersNoun_NamespaceFlagRefused(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	defer srv.Close()

	code := run([]string{"get", "runners", "-n", "team-a", "--server", srv.URL})
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
}

// TestRunnersNoun_AliasRnrOnly proves the varroa-cli delta: the noun takes
// the alias "rnr" and never "runner" (the top-level group reserves it).
func TestRunnersNoun_AliasRnrOnly(t *testing.T) {
	root := newRootCmd()
	getCmd, _, err := root.Find([]string{"get"})
	if err != nil {
		t.Fatalf("get parent not found: %v", err)
	}
	found := false
	for _, sub := range getCmd.Commands() {
		if sub.Name() != "runners" {
			continue
		}
		found = true
		if contains(sub.Aliases, "runner") {
			t.Error("the runners noun must not carry the alias \"runner\"")
		}
		if !contains(sub.Aliases, "rnr") {
			t.Errorf("expected alias \"rnr\", got %v", sub.Aliases)
		}
	}
	if !found {
		t.Fatal("expected a \"runners\" subcommand under \"get\"")
	}
}

func contains(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Top-level "runner" group (task 10.2): register, list, delete.
// ---------------------------------------------------------------------------

func TestRunnerGroup_Registered(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{
		{"runner"},
		{"runner", "register"},
		{"runner", "list"},
		{"runner", "delete"},
	} {
		found, _, err := root.Find(path)
		if err != nil || found == nil || found.Name() != path[len(path)-1] {
			t.Errorf("expected %v to be registered, got %v, err %v", path, found, err)
		}
	}
}

func TestRunnerList_TenantFilters(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runners" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("tenant"); got != "payments" {
			t.Errorf("expected tenant=payments, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"runner", "list", "--tenant", "payments"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnerGroupDelete_DelegatesToNoun(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.HasSuffix(r.URL.Path, "/api/v1/runners/win-1") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"runner", "delete", "win-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
