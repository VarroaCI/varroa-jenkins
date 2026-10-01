package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLocksRelease_PostsTheBody asserts the command posts name, runId, and
// reason to /api/v1/locks/release and prints the released row.
func TestLocksRelease_PostsTheBody(t *testing.T) {
	testSetup(t)
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/locks/release" {
			t.Errorf("request = %s %s, want POST /api/v1/locks/release", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"acme/deploy-prod","runId":"run-1","cluster":"core","tenant":"acme",` +
			`"jobPath":"acme/release","state":"released"}`))
	}))
	defer srv.Close()

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"locks", "release", "acme/deploy-prod", "--run", "run-1",
			"--reason", "the old pod is gone", "--server", srv.URL})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	want := map[string]string{"name": "acme/deploy-prod", "runId": "run-1", "reason": "the old pod is gone"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("body %s = %q, want %q", k, got[k], v)
		}
	}
	if !strings.Contains(out, "released fleet lock acme/deploy-prod") {
		t.Errorf("output = %q, want the released row", out)
	}
}

// TestLocksRelease_RequiresEveryInput asserts a missing name, run, or
// reason is a usage error raised before any request.
func TestLocksRelease_RequiresEveryInput(t *testing.T) {
	cases := map[string][]string{
		"no name":      {"locks", "release", "--run", "run-1", "--reason", "x"},
		"no run":       {"locks", "release", "acme/deploy-prod", "--reason", "x"},
		"no reason":    {"locks", "release", "acme/deploy-prod", "--run", "run-1"},
		"blank reason": {"locks", "release", "acme/deploy-prod", "--run", "run-1", "--reason", " "},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			testSetup(t)
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("the server must not be called")
			}))
			defer srv.Close()
			if code := run(append(args, "--server", srv.URL)); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
		})
	}
}

// TestLocksRelease_ReportsARefusal asserts a 409 reply exits non-zero.
func TestLocksRelease_ReportsARefusal(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"the lock is held; only a releasing lock can be force-released"}`))
	}))
	defer srv.Close()
	code := run([]string{"locks", "release", "acme/deploy-prod", "--run", "run-1", "--reason", "x", "--server", srv.URL})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
