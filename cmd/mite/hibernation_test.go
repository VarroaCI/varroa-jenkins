package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// --- 3.1 Gauge cache → heartbeat tests ---

// TestHeartbeatOmitsIdleBeforeFirstPoll verifies that sendHeartbeats sends
// a Heartbeat with a nil Idle field when no activity poll has ever
// succeeded (the atomic pointer is nil).
func TestHeartbeatOmitsIdleBeforeFirstPoll(t *testing.T) {
	var captured *mitev1.Heartbeat
	stream := &concurrentSendStream{}
	stream.sendFn = func(m *mitev1.MiteMessage) error {
		if hb := m.GetHeartbeat(); hb != nil {
			captured = hb
		}
		return nil
	}
	stream.recvFn = func() (*mitev1.OperatorMessage, error) {
		<-time.After(200 * time.Millisecond)
		return nil, context.DeadlineExceeded
	}

	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.heartbeatInterval = 30 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	// idleGauges is nil by default — no poll has run yet.
	agent.sendHeartbeats(ctx)

	if captured == nil {
		t.Fatal("expected at least one heartbeat to be sent")
	}
	if captured.Idle != nil {
		t.Errorf("expected nil Idle before first poll, got %+v", captured.Idle)
	}
}

// TestGaugesMatchFixture verifies that after a successful activity poll
// with a fixture JSON containing the idle object, the cached gauges match
// the expected values.
func TestGaugesMatchFixture(t *testing.T) {
	fixture := map[string]interface{}{
		"events":  []interface{}{},
		"dropped": 0,
		"idle": map[string]interface{}{
			"last_http_activity_unix": float64(1719000000),
			"last_event_unix":         float64(1719000100),
			"running_builds":          float64(2),
			"queue_length":            float64(5),
			"timer_trigger_jobs":      float64(1),
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/varroa-activity/events" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(fixture); err != nil {
			t.Errorf("encode fixture: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     srv.URL,
	})
	agent.Logger = slog.Default()
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()

	resp, err := agent.pollActivityEvents(context.Background(), 256)
	if err != nil {
		t.Fatalf("pollActivityEvents failed: %v", err)
	}
	if resp.Idle == nil {
		t.Fatal("expected Idle gauges in poll response, got nil")
	}

	// Store as the real poller would.
	agent.idleGauges.Store(resp.Idle)
	agent.idleGaugesReceivedAt.Store(time.Now().Unix())

	got := agent.idleGauges.Load()
	if got == nil {
		t.Fatal("idleGauges atomic pointer is nil after store")
	}
	if got.LastHttpActivityUnix != 1719000000 {
		t.Errorf("LastHttpActivityUnix = %d, want 1719000000", got.LastHttpActivityUnix)
	}
	if got.LastEventUnix != 1719000100 {
		t.Errorf("LastEventUnix = %d, want 1719000100", got.LastEventUnix)
	}
	if got.RunningBuilds != 2 {
		t.Errorf("RunningBuilds = %d, want 2", got.RunningBuilds)
	}
	if got.QueueLength != 5 {
		t.Errorf("QueueLength = %d, want 5", got.QueueLength)
	}
	if got.TimerTriggerJobs != 1 {
		t.Errorf("TimerTriggerJobs = %d, want 1", got.TimerTriggerJobs)
	}
}

// TestHeartbeatIncludesIdleAfterPoll verifies that after a successful poll
// populates the cache, subsequent heartbeats include the idle gauges.
func TestHeartbeatIncludesIdleAfterPoll(t *testing.T) {
	var captured *mitev1.Heartbeat
	stream := &concurrentSendStream{}
	stream.sendFn = func(m *mitev1.MiteMessage) error {
		if hb := m.GetHeartbeat(); hb != nil {
			captured = hb
		}
		return nil
	}
	stream.recvFn = func() (*mitev1.OperatorMessage, error) {
		<-time.After(200 * time.Millisecond)
		return nil, context.DeadlineExceeded
	}

	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.heartbeatInterval = 30 * time.Millisecond

	gauges := &mitev1.IdleGauges{
		LastHttpActivityUnix: 1719000000,
		LastEventUnix:        1719000100,
		RunningBuilds:        2,
		QueueLength:          5,
		TimerTriggerJobs:     1,
	}
	agent.idleGauges.Store(gauges)
	agent.idleGaugesReceivedAt.Store(time.Now().Unix())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	agent.sendHeartbeats(ctx)

	if captured == nil {
		t.Fatal("expected at least one heartbeat to be sent")
	}
	if captured.Idle == nil {
		t.Fatal("expected non-nil Idle in heartbeat after poll")
	}
	if captured.Idle.LastHttpActivityUnix != 1719000000 {
		t.Errorf("Idle.LastHttpActivityUnix = %d, want 1719000000", captured.Idle.LastHttpActivityUnix)
	}
}

// --- 3.2 REPLAY_WEBHOOK handler tests ---

// testReplayStream creates a mock stream that captures a single CommandResult.
func testReplayStream(t *testing.T) (*concurrentSendStream, *atomic.Pointer[mitev1.CommandResult]) {
	t.Helper()
	var captured atomic.Pointer[mitev1.CommandResult]
	stream := &concurrentSendStream{}
	stream.sendFn = func(m *mitev1.MiteMessage) error {
		if cr := m.GetCommandResult(); cr != nil {
			captured.Store(cr)
		}
		return nil
	}
	stream.recvFn = func() (*mitev1.OperatorMessage, error) {
		<-time.After(200 * time.Millisecond)
		return nil, context.DeadlineExceeded
	}
	return stream, &captured
}

const replayCmdID = "replay-test-001"

// TestReplayWebhook200 verifies that replaying to a listening upstream
// returns a success result carrying the HTTP status code.
func TestReplayWebhook200(t *testing.T) {
	// Start a test server on localhost:8080 if possible. In CI there is
	// no real Jenkins listening there, so this is safe. If the port is
	// already in use we skip the test gracefully.
	l, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Skip("skipping: localhost:8080 not available:", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify the request structure.
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if r.URL.Path != "/github-webhook/" {
				t.Errorf("expected /github-webhook/, got %s", r.URL.Path)
			}
			if r.URL.RawQuery != "test=1" {
				t.Errorf("expected query test=1, got %s", r.URL.RawQuery)
			}
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("Authorization = %q, want Bearer test-token", r.Header.Get("Authorization"))
			}
			if r.Header.Get("X-Custom") != "val" {
				t.Errorf("X-Custom = %q, want val", r.Header.Get("X-Custom"))
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	// Give the server a moment to start.
	time.Sleep(10 * time.Millisecond)

	stream, captured := testReplayStream(t)
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()

	cmd := &mitev1.ImperativeCommand{
		CommandId: replayCmdID,
		Type:      mitev1.CommandTypeReplayWebhook,
		ReplayWebhook: &mitev1.ReplayWebhookPayload{
			Path:       "github-webhook/",
			Query:      "test=1",
			Headers:    map[string]string{"X-Custom": "val"},
			Body:       []byte("payload-body"),
			DeliveryId: "delivery-200",
		},
	}

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: cmd,
		ctx:           context.Background(),
	})

	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	if !result.Success {
		t.Errorf("expected success for upstream 200, got error: %s", result.Error)
	}
	if result.HttpStatus != 200 {
		t.Errorf("expected HttpStatus=200, got %d", result.HttpStatus)
	}
	if result.Error != "" {
		t.Errorf("expected empty Error on success, got: %q", result.Error)
	}
}

// TestReplayWebhookAllowlist verifies that the handler checks the path
// allowlist: allowlisted paths proceed, non-allowlisted paths are
// rejected without any HTTP call.
func TestReplayWebhookAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantOK  bool
		wantErr string // non-empty: error must contain this
	}{
		{name: "github-webhook slash", path: "github-webhook/", wantOK: true},
		{name: "generic-webhook-trigger invoke", path: "generic-webhook-trigger/invoke", wantOK: true},
		{name: "github-webhook with suffix", path: "github-webhook/something", wantOK: true},
		{name: "arbitrary path", path: "anything-else", wantOK: false, wantErr: "not allowlisted"},
		{name: "path traversal", path: "../etc/passwd", wantOK: false, wantErr: "not allowlisted"},
		{name: "empty path", path: "", wantOK: false, wantErr: "not allowlisted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream, captured := testReplayStream(t)
			agent := NewAgent(Config{
				ControllerName: "test",
				Namespace:      "ns",
				JenkinsURL:     "http://localhost:1",
			})
			agent.Logger = slog.Default()
			agent.stream = stream
			// Token is required for allowlisted paths to avoid
			// connection-refused masking the allowlist check.
			agent.jenkinsTokenMu.Lock()
			agent.jenkinsToken = "test-token"
			agent.jenkinsTokenMu.Unlock()

			cmd := &mitev1.ImperativeCommand{
				CommandId: replayCmdID,
				Type:      mitev1.CommandTypeReplayWebhook,
				ReplayWebhook: &mitev1.ReplayWebhookPayload{
					Path:       tt.path,
					Query:      "",
					Headers:    map[string]string{},
					Body:       []byte("test-body"),
					DeliveryId: "delivery-allowlist",
				},
			}

			agent.runCommand(context.Background(), commandWork{
				imperativeCmd: cmd,
				ctx:           context.Background(),
			})

			result := captured.Load()
			if result == nil {
				t.Fatal("expected a CommandResult to be sent")
			}
			if result.CommandId != replayCmdID {
				t.Errorf("CommandId = %q, want %q", result.CommandId, replayCmdID)
			}
			if tt.wantOK {
				if !result.Success {
					// Allowlisted path may still fail with connection refused,
					// but that's a different kind of failure from allowlist rejection.
					// Just verify it's not the "not allowlisted" error.
					if strings.Contains(result.Error, "not allowlisted") {
						t.Errorf("allowlisted path %q was rejected: %s", tt.path, result.Error)
					}
				}
			} else {
				if result.Success {
					t.Errorf("expected failure for non-allowlisted path %q", tt.path)
				}
				if !strings.Contains(result.Error, tt.wantErr) {
					t.Errorf("error = %q, want containing %q", result.Error, tt.wantErr)
				}
			}
		})
	}
}

// TestReplayWebhookConnectionRefused verifies that when the upstream Jenkins
// is not listening, the handler returns a failure result without panicking.
func TestReplayWebhookConnectionRefused(t *testing.T) {
	stream, captured := testReplayStream(t)
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()

	cmd := &mitev1.ImperativeCommand{
		CommandId: replayCmdID,
		Type:      mitev1.CommandTypeReplayWebhook,
		ReplayWebhook: &mitev1.ReplayWebhookPayload{
			Path:       "github-webhook/",
			Query:      "test=1",
			Headers:    map[string]string{"X-Test": "value"},
			Body:       []byte("payload"),
			DeliveryId: "delivery-refused",
		},
	}

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: cmd,
		ctx:           context.Background(),
	})

	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	if result.Success {
		t.Errorf("expected failure for connection refused, got success")
	}
	if result.Error == "" {
		t.Error("expected non-empty error for connection refused")
	}
}

// TestReplayWebhookNilPayload verifies that a REPLAY_WEBHOOK command with
// a nil ReplayWebhook payload returns a failure result (not a crash).
func TestReplayWebhookNilPayload(t *testing.T) {
	stream, captured := testReplayStream(t)
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()
	agent.stream = stream

	cmd := &mitev1.ImperativeCommand{
		CommandId:     replayCmdID,
		Type:          mitev1.CommandTypeReplayWebhook,
		ReplayWebhook: nil,
	}

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: cmd,
		ctx:           context.Background(),
	})

	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	if result.Success {
		t.Errorf("expected failure for nil payload")
	}
	if !strings.Contains(result.Error, "nil") {
		t.Errorf("error should mention nil payload, got: %q", result.Error)
	}
}

// TestReplayWebhookNonAllowlistedRefusedWithoutHTTP ensures that a
// non-allowlisted path is rejected without any HTTP call (the error
// message about allowlist appears, not a connection error).
func TestReplayWebhookNonAllowlistedRefusedWithoutHTTP(t *testing.T) {
	stream, captured := testReplayStream(t)
	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		JenkinsURL:     "http://localhost:1",
	})
	agent.Logger = slog.Default()
	agent.stream = stream
	// No token set — if the handler tried an HTTP call it would get
	// connection refused. But the allowlist check should reject first.

	cmd := &mitev1.ImperativeCommand{
		CommandId: replayCmdID,
		Type:      mitev1.CommandTypeReplayWebhook,
		ReplayWebhook: &mitev1.ReplayWebhookPayload{
			Path:       "not-allowlisted-path",
			Query:      "",
			Headers:    map[string]string{},
			Body:       []byte("body"),
			DeliveryId: "delivery-blocked",
		},
	}

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: cmd,
		ctx:           context.Background(),
	})

	result := captured.Load()
	if result == nil {
		t.Fatal("expected a CommandResult to be sent")
	}
	// Must be rejected with "not allowlisted", not a connection error.
	if strings.Contains(result.Error, "connection") || strings.Contains(result.Error, "refused") {
		t.Errorf("non-allowlisted path should be rejected before HTTP call, got: %q", result.Error)
	}
	if !strings.Contains(result.Error, "not allowlisted") {
		t.Errorf("error should say 'not allowlisted', got: %q", result.Error)
	}
}
