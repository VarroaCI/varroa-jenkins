package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Task 10.4: the printed "inbound" command, byte for byte against design
// D12:
//
//	java -jar agent.jar -url https://<door host>/runners/<token>/ \
//	  -name <tenant>-<runner> -secret <token> -webSocket -workDir <remoteFS>
// ---------------------------------------------------------------------------

func TestInboundInstruction_GoldenString(t *testing.T) {
	got := inboundInstruction(
		"https://door.example.com/runners/abc123/",
		"payments-win-1",
		"abc123",
		"/var/j",
	)
	want := "java -jar agent.jar -url https://door.example.com/runners/abc123/ \\\n" +
		"  -name payments-win-1 -secret abc123 -webSocket -workDir /var/j"
	if got != want {
		t.Errorf("printed inbound command does not match design D12:\ngot:  %q\nwant: %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Task 10.3: register creates, polls, and prints.
// ---------------------------------------------------------------------------

func TestRunnerRegister_MissingName_UsageError(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	defer srv.Close()

	code := run([]string{"runner", "register", "--tenant", "payments", "--remote-fs", "/var/j", "--server", srv.URL})
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
}

func TestRunnerRegister_MalformedName_UsageError(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	defer srv.Close()

	code := run([]string{"runner", "register", "Win_1", "--tenant", "payments", "--remote-fs", "/var/j", "--server", srv.URL})
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
}

func TestRunnerRegister_MissingTenant_UsageError(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	defer srv.Close()

	code := run([]string{"runner", "register", "win-1", "--remote-fs", "/var/j", "--server", srv.URL})
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
}

func TestRunnerRegister_InboundPrint(t *testing.T) {
	testSetup(t)
	var createReq map[string]any
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runners":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &createReq)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "win-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runners/win-1":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "win-1",
				"spec": map[string]any{
					"tenant": "payments", "launcher": "inbound", "remoteFS": "/var/j",
				},
				"status": map[string]any{
					"nodeName":  "payments-win-1",
					"token":     "abc123def456",
					"connected": false,
				},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	stdout := captureStdout(t, func() {
		code := run([]string{
			"runner", "register", "win-1",
			"--tenant", "payments", "--remote-fs", "/var/j",
			"--server", srv.URL,
		})
		if code != 0 {
			t.Fatalf("expected exit code 0, got %d", code)
		}
	})

	if createReq["name"] != "win-1" {
		t.Errorf("expected create request name win-1, got %v", createReq["name"])
	}

	// Design D12: "https://<door host>/runners/<token>/". The test server's
	// own URL stands in for the door host here.
	wantDoorURL := srv.URL + "/runners/abc123def456/"
	want := "java -jar agent.jar -url " + wantDoorURL + " \\\n" +
		"  -name payments-win-1 -secret abc123def456 -webSocket -workDir /var/j"
	if strings.TrimRight(stdout, "\n") != want {
		t.Errorf("printed inbound command mismatch:\ngot:  %q\nwant: %q", stdout, want)
	}
}

func TestRunnerRegister_SSHPrint(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runners":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "unix-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runners/unix-1":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "unix-1",
				"spec": map[string]any{
					"tenant": "payments", "launcher": "ssh", "remoteFS": "/home/jenkins",
					"host": "10.0.0.5", "port": 22, "credentialRef": "unix-1-ssh-key",
				},
				"status": map[string]any{
					"nodeName":  "payments-unix-1",
					"connected": false,
				},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	stdout := captureStdout(t, func() {
		code := run([]string{
			"runner", "register", "unix-1",
			"--tenant", "payments", "--remote-fs", "/home/jenkins",
			"--launcher", "ssh", "--host", "10.0.0.5", "--port", "22",
			"--credential", "unix-1-ssh-key",
			"--server", srv.URL,
		})
		if code != 0 {
			t.Fatalf("expected exit code 0, got %d", code)
		}
	})

	if !strings.Contains(stdout, "10.0.0.5:22") {
		t.Errorf("expected host:port in output, got %q", stdout)
	}
	if !strings.Contains(stdout, "unix-1-ssh-key") {
		t.Errorf("expected Secret name in output, got %q", stdout)
	}
	if !strings.Contains(stdout, "varroa.dev/runner-credential=true") {
		t.Errorf("expected Secret label in output, got %q", stdout)
	}
	if !strings.Contains(stdout, "username") || !strings.Contains(stdout, "privateKey") || !strings.Contains(stdout, "passphrase") {
		t.Errorf("expected the three Secret data keys in output, got %q", stdout)
	}
	if strings.Contains(stdout, "-secret") {
		t.Errorf("an ssh runner must print no token, got %q", stdout)
	}
}

func TestRunnerRegister_Timeout(t *testing.T) {
	testSetup(t)
	origInterval, origTimeout := runnerRegisterPollInterval, runnerRegisterTimeout
	runnerRegisterPollInterval = 2 * time.Millisecond
	runnerRegisterTimeout = 20 * time.Millisecond
	defer func() { runnerRegisterPollInterval, runnerRegisterTimeout = origInterval, origTimeout }()

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runners":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "win-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runners/win-1":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "win-1",
				"spec": map[string]any{
					"tenant": "payments", "launcher": "inbound", "remoteFS": "/var/j",
				},
				"status": map[string]any{
					"connected": false,
					"conditions": []map[string]any{
						{"type": "Ready", "status": "False", "reason": "AwaitingPlacement"},
					},
				},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	code := run([]string{
		"runner", "register", "win-1",
		"--tenant", "payments", "--remote-fs", "/var/j",
		"--server", srv.URL,
	})
	if code != 1 {
		t.Fatalf("expected exit code 1 on timeout, got %d", code)
	}
}
