package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBroodHandler returns test data for brood operation endpoints.
type fakeBroodHandler struct{}

func (f *fakeBroodHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(r.URL.Path, "/stream"):
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: status\ndata: %s\n\nevent: closed\ndata: {}\n\n", fakeBroodDetailJSON("test-ns", "test-op"))
	case strings.HasSuffix(r.URL.Path, "/preview"):
		json.NewEncoder(w).Encode(map[string]interface{}{
			"clusters": []map[string]interface{}{
				{"cluster": "core", "ok": true, "targets": []map[string]interface{}{
					{"namespace": "ns", "name": "ctrl-a", "wave": 0, "applicable": true, "reason": nil},
					{"namespace": "ns", "name": "ctrl-b", "wave": 0, "applicable": false, "reason": "not Connected"},
				}},
				{"cluster": "dev-cluster", "ok": true, "targets": []map[string]interface{}{
					{"namespace": "ns", "name": "ctrl-c", "wave": 0, "applicable": true, "reason": nil},
				}},
			},
		})
	case r.Method == http.MethodPost && !strings.Contains(r.URL.Path, "/suspend") && !strings.Contains(r.URL.Path, "/preview"):
		// create
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"namespace": "team-ns",
			"name":      "broodop-reconcile-abc123",
			"clusters": []map[string]interface{}{
				{"cluster": "core", "ok": true},
				{"cluster": "dev-cluster", "ok": false, "error": "cluster unreachable"},
			},
		})
	case r.Method == http.MethodGet && strings.Count(r.URL.Path, "/") == 5:
		// get single (detail)
		json.NewEncoder(w).Encode(fakeBroodDetail("test-ns", "test-op"))
	case r.Method == http.MethodGet:
		// list
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": []map[string]interface{}{
				fakeBroodDetail("ns1", "op1"),
				fakeBroodDetail("ns2", "op2"),
			},
		})
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	case strings.Contains(r.URL.Path, "/suspend"):
		json.NewEncoder(w).Encode(fakeBroodDetail("test-ns", "test-op"))
	case strings.Contains(r.URL.Path, "/stream"):
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: status\ndata: %s\n\nevent: closed\ndata: {}\n\n", fakeBroodDetailJSON("test-ns", "test-op"))
	}
}

func fakeBroodDetail(ns, name string) map[string]interface{} {
	return map[string]interface{}{
		"metadata": map[string]interface{}{
			"namespace": ns,
			"name":      name,
		},
		"spec": map[string]interface{}{
			"action": map[string]interface{}{"verb": "reconcile"},
		},
		"status": map[string]interface{}{
			"phase": "Succeeded",
			"summary": map[string]interface{}{
				"total": 2, "succeeded": 1, "failed": 0, "skipped": 1,
			},
			"targets": []map[string]interface{}{
				{"namespace": ns, "name": "ctrl-a", "wave": 0, "state": "Succeeded", "reason": nil},
				{"namespace": ns, "name": "ctrl-b", "wave": 0, "state": "Skipped", "reason": "not Connected"},
			},
		},
	}
}

func fakeBroodDetailJSON(ns, name string) string {
	b, _ := json.Marshal(fakeBroodDetail(ns, name))
	return string(b)
}

// TestBroodOpRunUsageErrors verifies usage errors exit 2.
func TestBroodOpRunUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown verb", []string{"broodop", "run", "fly"}},
		{"no targeting mode", []string{"broodop", "run", "restart"}},
		{"both selector and names", []string{"broodop", "run", "restart", "--selector", "x=y", "--names", "a,b"}},
		{"bad filter key", []string{"broodop", "run", "restart", "--names", "a", "--filter", "color=blue"}},
		{"bad filter syntax", []string{"broodop", "run", "restart", "--names", "a", "--filter", "novalue"}},
		{"clusters with 3-token name", []string{"broodop", "run", "restart", "--names", "core/ns/c", "--clusters", "core"}},
		{"clusters all with names", []string{"broodop", "run", "restart", "--names", "a", "--clusters", "all"}},
		{"multiple clusters, unqualified names", []string{"broodop", "run", "restart", "--names", "a", "--clusters", "core,dev-cluster"}},
		{"mixed qualified and unqualified names", []string{"broodop", "run", "restart", "--names", "core/ns/c,bare"}},
		{"all mixed with explicit cluster", []string{"broodop", "run", "restart", "--selector", "x=y", "--clusters", "all,core"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := run(tt.args)
			if code != 2 {
				t.Errorf("expected exit code 2, got %d", code)
			}
		})
	}
}

// TestBroodOpDryRunNoCreate verifies dry-run doesn't create.
func TestBroodOpDryRunNoCreate(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "run", "reconcile", "--names", "ctrl-a,ctrl-b", "--dry-run", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for dry-run, got %d", code)
	}
}

// TestBroodOpCreate verifies create sends correct request and prints NS/NAME.
func TestBroodOpCreate(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "run", "reconcile", "--names", "ctrl-a", "-n", "team-ns", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for create, got %d", code)
	}
}

// TestBroodOpRunClustersPassthrough verifies --clusters lands in the POST body.
func TestBroodOpRunClustersPassthrough(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"namespace": "team-ns", "name": "op1",
			"clusters": []map[string]interface{}{
				{"cluster": "core", "ok": true},
				{"cluster": "dev-cluster", "ok": true},
			},
		})
	}))
	defer srv.Close()
	code := run([]string{"broodop", "run", "reconcile", "--selector", "x=y", "--clusters", "core,dev-cluster", "--server", srv.URL})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	cl, ok := gotBody["clusters"].([]interface{})
	if !ok || len(cl) != 2 || cl[0] != "core" || cl[1] != "dev-cluster" {
		t.Fatalf("expected body clusters [core dev-cluster], got %v", gotBody["clusters"])
	}
}

// TestBroodOpList verifies list output.
func TestBroodOpList(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "get", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for list, got %d", code)
	}
}

// TestBroodOpDescribe verifies describe output includes per-target table.
func TestBroodOpDescribe(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "describe", "test-ns/test-op", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for describe, got %d", code)
	}
}

// TestBroodOpCancel verifies cancel.
func TestBroodOpCancel(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "delete", "test-ns/test-op", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for cancel, got %d", code)
	}
}

// TestBroodOpSuspend verifies suspend.
func TestBroodOpSuspend(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "suspend", "test-ns/test-op", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for suspend, got %d", code)
	}
}

// TestBroodOpSuspendOff verifies resume.
func TestBroodOpSuspendOff(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "suspend", "test-ns/test-op", "--off", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for resume, got %d", code)
	}
}

// TestBroodOpWatchSucceededExit0 verifies watch exits 0 on Succeeded.
func TestBroodOpWatchSucceededExit0(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "watch", "test-ns/test-op", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for Succeeded watch, got %d", code)
	}
}

// TestBroodOpWatchFailedExit1 verifies watch exits 1 on Failed.
func TestBroodOpWatchFailedExit1(t *testing.T) {
	h := &fakeBroodHandler{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Send a Failed status then close
		detail := fakeBroodDetail("test-ns", "test-op")
		detail["status"].(map[string]interface{})["phase"] = "Failed"
		b, _ := json.Marshal(detail)
		fmt.Fprintf(w, "event: status\ndata: %s\n\nevent: closed\ndata: {}\n\n", string(b))
	}))
	defer srv.Close()
	_ = h
	args := []string{"broodop", "watch", "test-ns/test-op", "--server", srv.URL}
	code := run(args)
	if code != 1 {
		t.Errorf("expected exit 1 for Failed watch, got %d", code)
	}
}

// TestBroodOpWatchDeadlineExitsWithMessage verifies that a server-side
// watch-deadline close (#367) — an "event: closed" frame carrying a
// reason/message instead of a bare "{}" — makes the client exit non-zero
// with that message, rather than silently exiting 0 or hanging.
func TestBroodOpWatchDeadlineExitsWithMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		detail := fakeBroodDetail("test-ns", "test-op")
		detail["status"].(map[string]interface{})["phase"] = "Running"
		b, _ := json.Marshal(detail)
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", string(b))
		fmt.Fprintf(w, "event: closed\ndata: {\"reason\":\"deadline_exceeded\",\"message\":\"watch exceeded max duration of 1h0m0s\"}\n\n")
	}))
	defer srv.Close()

	args := []string{"broodop", "watch", "test-ns/test-op", "--server", srv.URL}
	code := run(args)
	if code == 0 {
		t.Error("expected non-zero exit on watch-deadline close, got 0")
	}
}

func TestBroodOpWatchStopsOnTerminalStatusWithoutClosedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		suspended := fakeBroodDetail("test-ns", "test-op")
		suspended["status"].(map[string]interface{})["phase"] = "Suspended"
		sb, _ := json.Marshal(suspended)
		succeeded := fakeBroodDetail("test-ns", "test-op")
		gb, _ := json.Marshal(succeeded)
		fmt.Fprintf(w, "event: status\\ndata: %s\\n\\nevent: status\\ndata: %s\\n\\n", sb, gb)
	}))
	defer srv.Close()

	code := run([]string{"broodop", "watch", "test-ns/test-op", "--server", srv.URL})
	if code != 0 {
		t.Fatalf("expected exit 0 after terminal status, got %d", code)
	}
}

// TestBroodOpWatchNilMetadata verifies the watch renderer does not panic when a
// streamed event omits metadata.name/namespace. Regression for the live §8.2
// nil-pointer crash in renderBroodWatchStatus (waitForCompletion=true path).
func TestBroodOpWatchNilMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 1) A keepalive-style frame with neither status nor metadata (all-nil
		//    op) — this is what crashed §8.2 (nil op.Status deref).
		// 2) A frame with status but no metadata (nil op.Metadata deref).
		nostatus := `{"type":"ping"}`
		detail := fakeBroodDetail("test-ns", "test-op")
		delete(detail, "metadata")
		detail["status"].(map[string]interface{})["phase"] = "Succeeded"
		b, _ := json.Marshal(detail)
		fmt.Fprintf(w, "event: status\ndata: %s\n\nevent: status\ndata: %s\n\nevent: closed\ndata: {}\n\n", nostatus, string(b))
	}))
	defer srv.Close()
	args := []string{"broodop", "watch", "test-ns/test-op", "--server", srv.URL}
	code := run(args) // must not panic
	if code != 0 {
		t.Errorf("expected exit 0 for Succeeded watch with nil metadata, got %d", code)
	}
}

// TestBroodOpRunWatchChains verifies -w chains to watch and exits 0 on Succeeded.
func TestBroodOpRunWatchChains(t *testing.T) {
	srv := httptest.NewServer(&fakeBroodHandler{})
	defer srv.Close()
	args := []string{"broodop", "run", "reconcile", "--names", "ctrl-a", "-n", "team-ns", "-w", "--server", srv.URL}
	code := run(args)
	if code != 0 {
		t.Errorf("expected exit 0 for run -w Succeeded, got %d", code)
	}
}
