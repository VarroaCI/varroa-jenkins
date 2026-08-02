package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// fakeBroodScheduleHandler returns test data for brood schedule endpoints.
type fakeBroodScheduleHandler struct{}

func (f *fakeBroodScheduleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/suspend"):
		var body struct {
			Suspend bool `json:"suspend"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"suspend": body.Suspend,
		})

	case r.Method == http.MethodPost:
		// create
		w.WriteHeader(http.StatusCreated)
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		ns := "team-ns"
		if n, ok := body["namespace"].(string); ok && n != "" {
			ns = n
		}
		name, _ := body["name"].(string)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"namespace": ns,
			"name":      name,
			"spec":      body["spec"],
		})

	case r.Method == http.MethodGet && strings.Count(strings.TrimRight(r.URL.Path, "/"), "/") == 5:
		// get single: /brood-schedules/{ns}/{name}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		ns := parts[3]
		name := parts[4]
		json.NewEncoder(w).Encode(map[string]interface{}{
			"namespace": ns,
			"name":      name,
			"spec": map[string]interface{}{
				"schedule": "*/5 * * * *",
			},
		})

	case r.Method == http.MethodGet:
		// list
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": []map[string]interface{}{
				{"namespace": "ns1", "name": "sched1", "spec": map[string]interface{}{"schedule": "*/5 * * * *"}},
				{"namespace": "ns2", "name": "sched2", "spec": map[string]interface{}{"schedule": "0 */2 * * *"}},
			},
		})

	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	}
}

func TestBroodScheduleCreate(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodScheduleHandler{})
	defer srv.Close()

	t.Run("basic create with names", func(t *testing.T) {
		code := run([]string{"broodschedule", "create", "test-sched",
			"--verb", "reconcile",
			"--cron", "*/5 * * * *",
			"--names", "ctrl-1",
			"--server", srv.URL,
		})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})

	t.Run("create with selector-json", func(t *testing.T) {
		code := run([]string{"broodschedule", "create", "sel-sched",
			"--verb", "restart",
			"--cron", "0 * * * *",
			"--selector-json", `{"matchLabels":{"team":"payments","tier":"prod"}}`,
			"--server", srv.URL,
		})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})

	t.Run("missing verb", func(t *testing.T) {
		code := run([]string{"broodschedule", "create", "test-sched",
			"--cron", "*/5 * * * *",
			"--names", "ctrl-1",
			"--server", srv.URL,
		})
		if code != 2 {
			t.Errorf("expected exit 2 for missing verb, got %d", code)
		}
	})

	t.Run("missing targeting", func(t *testing.T) {
		code := run([]string{"broodschedule", "create", "test-sched",
			"--verb", "reconcile",
			"--cron", "*/5 * * * *",
			"--server", srv.URL,
		})
		if code != 2 {
			t.Errorf("expected exit 2 for missing targeting, got %d", code)
		}
	})

	t.Run("both selector and names", func(t *testing.T) {
		code := run([]string{"broodschedule", "create", "test-sched",
			"--verb", "reconcile",
			"--cron", "*/5 * * * *",
			"--selector", "team=payments",
			"--names", "ctrl-1",
			"--server", srv.URL,
		})
		if code != 2 {
			t.Errorf("expected exit 2 for both selector and names, got %d", code)
		}
	})
}

func TestBroodScheduleGet(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodScheduleHandler{})
	defer srv.Close()

	t.Run("get all", func(t *testing.T) {
		code := run([]string{"broodschedule", "get", "--server", srv.URL})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})

	t.Run("get single", func(t *testing.T) {
		code := run([]string{"broodschedule", "get", "team-ns/test-sched", "--server", srv.URL})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})
}

func TestBroodScheduleDescribe(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodScheduleHandler{})
	defer srv.Close()

	code := run([]string{"broodschedule", "describe", "team-ns/test-sched", "--server", srv.URL})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
}

func TestBroodScheduleDelete(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodScheduleHandler{})
	defer srv.Close()

	code := run([]string{"broodschedule", "delete", "team-ns/test-sched", "--server", srv.URL})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
}

func TestBroodScheduleSuspend(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodScheduleHandler{})
	defer srv.Close()

	t.Run("suspend", func(t *testing.T) {
		code := run([]string{"broodschedule", "suspend", "team-ns/test-sched", "--server", srv.URL})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})

	t.Run("resume", func(t *testing.T) {
		code := run([]string{"broodschedule", "suspend", "team-ns/test-sched", "--resume", "--server", srv.URL})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})
}

func TestBroodScheduleCreateSpecConstruction(t *testing.T) {
	// Test that buildSpecMap produces the expected spec when marshaled.
	cmd := &cobra.Command{}
	cmd.Flags().Int("max-parallel", 0, "")
	cmd.Flags().String("order", "", "")
	cmd.Flags().String("failure-policy", "", "")
	cmd.Flags().Int("ttl", 0, "")
	cmd.Flags().String("cluster", "", "")
	cmd.Flags().StringSlice("namespaces", nil, "")
	cmd.Flags().StringSlice("filter", nil, "")
	_ = cmd.Flags().Set("max-parallel", "3")
	_ = cmd.Flags().Set("order", "name")
	_ = cmd.Flags().Set("ttl", "300")

	spec := buildSpecMap("reconcile", "", `{"matchLabels":{"team":"payments"}}`, "", nil, cmd)

	// Verify the structure.
	if spec["action"].(map[string]interface{})["verb"] != "reconcile" {
		t.Errorf("verb = %v, want reconcile", spec["action"])
	}
	if spec["ttlSecondsAfterFinished"] != 300 {
		t.Errorf("ttl = %v, want 300", spec["ttlSecondsAfterFinished"])
	}
	exec := spec["execution"].(map[string]interface{})
	if exec["maxParallel"] != 3 {
		t.Errorf("maxParallel = %v, want 3", exec["maxParallel"])
	}
	if exec["order"] != "name" {
		t.Errorf("order = %v, want name", exec["order"])
	}
	tgts := spec["targets"].(map[string]interface{})
	if _, ok := tgts["selector"]; !ok {
		t.Errorf("expected selector in targets")
	}
}

func TestBroodScheduleCreateJSONRoundTrip(t *testing.T) {
	// Verify the JSON round-trip works.
	specMap := map[string]interface{}{
		"schedule": "*/5 * * * *",
		"template": map[string]interface{}{
			"action": map[string]interface{}{"verb": "reconcile"},
			"targets": map[string]interface{}{
				"names": []string{"ctrl-1"},
			},
		},
		"waitForCompletion": true,
	}
	specJSON, _ := json.Marshal(specMap)
	var result map[string]interface{}
	if err := json.Unmarshal(specJSON, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["schedule"] != "*/5 * * * *" {
		t.Errorf("schedule = %v, want */5 * * * *", result["schedule"])
	}
	if tmpl, ok := result["template"].(map[string]interface{}); ok {
		if action, ok := tmpl["action"].(map[string]interface{}); ok {
			if action["verb"] != "reconcile" {
				t.Errorf("verb = %v, want reconcile", action["verb"])
			}
		}
	}
}
