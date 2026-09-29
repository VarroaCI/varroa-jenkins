package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// --- 2.3 TRIGGER_BUILD / CANCEL_BUILD handler tests ---

// TestTriggerAndCancelBuildCommands verifies that each of the two imperative
// build commands produces exactly one call to the plugin's run route and one
// CommandResult, matching the pattern the other imperative commands follow.
func TestTriggerAndCancelBuildCommands(t *testing.T) {
	tests := []struct {
		name  string
		route string
		build func() *mitev1.ImperativeCommand
	}{
		{
			name:  "trigger build",
			route: "/varroa-run/trigger",
			build: func() *mitev1.ImperativeCommand {
				return &mitev1.ImperativeCommand{
					CommandId: "trigger-test-001",
					Type:      mitev1.CommandTypeTriggerBuild,
					TriggerBuild: &mitev1.TriggerBuildPayload{
						RunId:      "run-1",
						IntentKey:  "manual:run-1",
						JobPath:    "team-a/job-1",
						Parameters: map[string]string{"BRANCH": "main"},
						Actor:      "alice",
					},
				}
			},
		},
		{
			name:  "cancel build",
			route: "/varroa-run/cancel",
			build: func() *mitev1.ImperativeCommand {
				return &mitev1.ImperativeCommand{
					CommandId: "cancel-test-001",
					Type:      mitev1.CommandTypeCancelBuild,
					CancelBuild: &mitev1.CancelBuildPayload{
						RunId:       "run-1",
						JobPath:     "team-a/job-1",
						BuildNumber: 0,
						Actor:       "alice",
					},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/login" {
					// The readiness check a trigger runs first. It is not
					// a run call.
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.URL.Path == "/crumbIssuer/api/json" {
					// CSRF protection off — the client fetches this once
					// before every state-changing request and does not
					// count it as a run call.
					w.WriteHeader(http.StatusNotFound)
					return
				}
				atomic.AddInt32(&calls, 1)
				if r.URL.Path != tt.route {
					t.Errorf("unexpected path: %s, want %s", r.URL.Path, tt.route)
				}
				if r.Method != http.MethodPost {
					t.Errorf("expected POST, got %s", r.Method)
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(srv.Close)

			stream, captured := testReplayStream(t)
			agent := NewAgent(Config{
				ControllerName: "test",
				Namespace:      "ns",
				JenkinsURL:     srv.URL,
			})
			agent.Logger = slog.Default()
			agent.stream = stream
			agent.jenkinsTokenMu.Lock()
			agent.jenkinsToken = "test-token"
			agent.jenkinsTokenMu.Unlock()
			agent.markItemsApplied()

			agent.runCommand(context.Background(), commandWork{
				imperativeCmd: tt.build(),
				ctx:           context.Background(),
			})

			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Errorf("expected exactly one Jenkins call, got %d", got)
			}

			result := captured.Load()
			if result == nil {
				t.Fatal("expected a CommandResult to be sent")
			}
			if !result.Success {
				t.Errorf("expected success, got error: %s", result.Error)
			}
			cmd := tt.build()
			if result.CommandType != cmd.Type || result.JobPath != "team-a/job-1" || result.RunId != "run-1" {
				t.Errorf("result names %v %q %q, want the command's type, job path, and run id",
					result.CommandType, result.JobPath, result.RunId)
			}
			if result.Actor != "alice" {
				t.Errorf("result actor = %q, want alice", result.Actor)
			}
			if result.HttpStatus != http.StatusAccepted {
				t.Errorf("result http_status = %d, want %d", result.HttpStatus, http.StatusAccepted)
			}
		})
	}
}

// TestImperativeCommandActor verifies the mite copies the sender of each
// run command onto its result, and leaves every other command empty.
func TestImperativeCommandActor(t *testing.T) {
	user := &mitev1.ActingUser{Name: "bob", Email: "bob@example.com"}
	cases := []struct {
		cmd  *mitev1.ImperativeCommand
		want string
	}{
		{&mitev1.ImperativeCommand{TriggerBuild: &mitev1.TriggerBuildPayload{Actor: "alice"}}, "alice"},
		{&mitev1.ImperativeCommand{CancelBuild: &mitev1.CancelBuildPayload{Actor: "varroa-recorder"}}, "varroa-recorder"},
		{&mitev1.ImperativeCommand{ProceedInput: &mitev1.ProceedInputPayload{ActingUser: user}}, "bob"},
		{&mitev1.ImperativeCommand{AbortInput: &mitev1.AbortInputPayload{ActingUser: user}}, "bob"},
		{&mitev1.ImperativeCommand{ReplayBuild: &mitev1.ReplayBuildPayload{ActingUser: user}}, "bob"},
		{&mitev1.ImperativeCommand{ReplayBuild: &mitev1.ReplayBuildPayload{}}, ""},
		{&mitev1.ImperativeCommand{Type: mitev1.CommandTypeSafeRestart}, ""},
	}
	for i, tc := range cases {
		if got := imperativeCommandActor(tc.cmd); got != tc.want {
			t.Errorf("case %d: actor = %q, want %q", i, got, tc.want)
		}
	}
}

// TestCancelBuildRefusedCarriesStatus verifies a refused cancel answers the
// plugin's HTTP status on its result.
func TestCancelBuildRefusedCarriesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Error(w, "no build carries run id run-1", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	stream, captured := testReplayStream(t)
	agent := NewAgent(Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: &mitev1.ImperativeCommand{
			CommandId: "cancel-build-run-1",
			Type:      mitev1.CommandTypeCancelBuild,
			CancelBuild: &mitev1.CancelBuildPayload{
				RunId: "run-1", JobPath: "team-a/job-1", Actor: "alice",
			},
		},
		ctx: context.Background(),
	})

	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	if result.Success || result.HttpStatus != http.StatusNotFound || result.Actor != "alice" {
		t.Fatalf("result = success %v status %d actor %q, want a refusal with 404 by alice",
			result.Success, result.HttpStatus, result.Actor)
	}
}

// TestTriggerBuildWaitsForJenkins verifies a TRIGGER_BUILD command waits for
// the readiness check and triggers once Jenkins answers it.
func TestTriggerBuildWaitsForJenkins(t *testing.T) {
	var loginCalls, runCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			// The first probe finds a booting Jenkins.
			if atomic.AddInt32(&loginCalls, 1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/crumbIssuer/api/json":
			w.WriteHeader(http.StatusNotFound)
		default:
			atomic.AddInt32(&runCalls, 1)
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)

	stream, captured := testReplayStream(t)
	agent := NewAgent(Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	agent.markItemsApplied()

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: triggerCommand(),
		ctx:           context.Background(),
	})

	if got := atomic.LoadInt32(&runCalls); got != 1 {
		t.Fatalf("run calls = %d, want 1 after Jenkins became ready", got)
	}
	if got := atomic.LoadInt32(&loginCalls); got < 2 {
		t.Fatalf("login probes = %d, want at least 2", got)
	}
	if result := captured.Load(); result == nil || !result.Success {
		t.Fatalf("result = %+v, want success", result)
	}
}

// TestTriggerBuildReadinessWaitExpires verifies a trigger whose readiness
// wait expires makes no trigger call and reports the status 0, which the
// gateway maps onto the retryable reason code.
func TestTriggerBuildReadinessWaitExpires(t *testing.T) {
	var runCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		atomic.AddInt32(&runCalls, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	stream, captured := testReplayStream(t)
	agent := NewAgent(Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.triggerReadyWait = 50 * time.Millisecond
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: triggerCommand(),
		ctx:           context.Background(),
	})

	if got := atomic.LoadInt32(&runCalls); got != 0 {
		t.Fatalf("run calls = %d, want 0 while Jenkins is not ready", got)
	}
	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	if result.Success || result.HttpStatus != 0 {
		t.Fatalf("result = %+v, want a failure with the status 0", result)
	}
	if !strings.Contains(result.Error, "jenkins not ready") {
		t.Fatalf("error = %q, want it to name the readiness wait", result.Error)
	}
}

// triggerCommand builds one TRIGGER_BUILD command for the readiness tests.
func triggerCommand() *mitev1.ImperativeCommand {
	return &mitev1.ImperativeCommand{
		CommandId: "trigger-ready-001",
		Type:      mitev1.CommandTypeTriggerBuild,
		TriggerBuild: &mitev1.TriggerBuildPayload{
			RunId:     "run-2",
			IntentKey: "cron:team-a/job-1:2026-09-27T20:44:00Z",
			JobPath:   "team-a/job-1",
		},
	}
}

// TestTriggerBuildNilPayload verifies that a TRIGGER_BUILD command with no
// payload fails without making a Jenkins call, mirroring
// TestReplayWebhookNilPayload's coverage for REPLAY_WEBHOOK.
func TestTriggerBuildNilPayload(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	stream, captured := testReplayStream(t)
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     srv.URL,
	})
	agent.Logger = slog.Default()
	agent.stream = stream

	cmd := &mitev1.ImperativeCommand{
		CommandId: "trigger-nil-001",
		Type:      mitev1.CommandTypeTriggerBuild,
	}

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: cmd,
		ctx:           context.Background(),
	})

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("expected no Jenkins call for a nil payload, got %d", got)
	}
	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	if result.Success {
		t.Error("expected failure for a nil trigger_build payload")
	}
}
