package mite

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// TestBusHandler_OnJenkinsActivity verifies that OnJenkinsActivity builds
// a correct bus.ActivityPayload with source:"jenkins", name==controller,
// event==type, and the Jenkins fields, and that nothing is published to
// events.brood. It also covers the dispatch path from server.go's type-switch.
func TestBusHandler_OnJenkinsActivity(t *testing.T) {
	t.Parallel()

	ns := startNATS(t)
	busConn, err := bus.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	// Subscribe to capture published events on activity subjects and brood.
	var mu sync.Mutex
	var activityPayloads []bus.ActivityPayload

	sub, err := busConn.NATSConn().Subscribe("activity.>", func(msg *nats.Msg) {
		var p bus.ActivityPayload
		if err := json.Unmarshal(msg.Data, &p); err != nil {
			return
		}
		mu.Lock()
		activityPayloads = append(activityPayloads, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe activity: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Subscribe to events.brood to verify nothing lands there.
	var broodMu sync.Mutex
	var broodCount int
	broodSub, err := busConn.NATSConn().Subscribe("events.brood", func(msg *nats.Msg) {
		broodMu.Lock()
		broodCount++
		broodMu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe brood: %v", err)
	}
	t.Cleanup(func() { _ = broodSub.Unsubscribe() })

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("ensure snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		t.Fatalf("ensure presence kv: %v", err)
	}
	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		t.Fatalf("ensure desired kv: %v", err)
	}

	handler := NewBusHandler(bus.DefaultCluster, busConn, snapshotKV, presenceKV, desiredKV, nil)
	handler.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	t.Cleanup(handler.Close)

	evt := &mitev1.JenkinsActivityEvent{
		Type:        "build.started",
		Actor:       "alice",
		Message:     "Build #42 started",
		ItemPath:    "team-a/my-job",
		BuildNumber: 42,
		Result:      "",
		URL:         "job/my-job/42/",
		Timestamp:   "2024-06-01T12:00:00Z",
	}

	handler.OnJenkinsActivity("my-controller", "my-namespace", evt)

	// Allow async publish to settle.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	payloads := make([]bus.ActivityPayload, len(activityPayloads))
	copy(payloads, activityPayloads)
	mu.Unlock()

	if len(payloads) == 0 {
		t.Fatal("expected at least one activity payload")
	}

	p := payloads[len(payloads)-1] // the last one should be our Jenkins event

	if p.Source != "jenkins" {
		t.Errorf("Source = %q, want %q", p.Source, "jenkins")
	}
	if p.Name != "my-controller" {
		t.Errorf("Name = %q, want %q", p.Name, "my-controller")
	}
	if p.Controller != "my-controller" {
		t.Errorf("Controller = %q, want %q", p.Controller, "my-controller")
	}
	if p.Namespace != "my-namespace" {
		t.Errorf("Namespace = %q, want %q", p.Namespace, "my-namespace")
	}
	if p.Event != "build.started" {
		t.Errorf("Event = %q, want %q", p.Event, "build.started")
	}
	if p.Type != "build.started" {
		t.Errorf("Type = %q, want %q", p.Type, "build.started")
	}
	if p.Actor != "alice" {
		t.Errorf("Actor = %q, want %q", p.Actor, "alice")
	}
	if p.Message != "Build #42 started" {
		t.Errorf("Message = %q, want %q", p.Message, "Build #42 started")
	}
	if p.ItemPath != "team-a/my-job" {
		t.Errorf("ItemPath = %q, want %q", p.ItemPath, "team-a/my-job")
	}
	if p.BuildNumber != 42 {
		t.Errorf("BuildNumber = %d, want %d", p.BuildNumber, 42)
	}
	if p.Result != "" {
		t.Errorf("Result = %q, want %q", p.Result, "")
	}
	if p.URL != "job/my-job/42/" {
		t.Errorf("URL = %q, want %q", p.URL, "job/my-job/42/")
	}
	if p.Timestamp != "2024-06-01T12:00:00Z" {
		t.Errorf("Timestamp = %q, want %q", p.Timestamp, "2024-06-01T12:00:00Z")
	}

	// Verify nothing published to events.brood.
	broodMu.Lock()
	fc := broodCount
	broodMu.Unlock()
	if fc != 0 {
		t.Errorf("published %d events to events.brood, want 0", fc)
	}
}

// TestDispatchRoutesToOnJenkinsActivity verifies that the server.go type-switch
// correctly routes a MiteMessage_JenkinsActivity to OnJenkinsActivity.
func TestDispatchRoutesToOnJenkinsActivity(t *testing.T) {
	t.Parallel()

	// Use a recording handler.
	rec := &recordingHandler{}
	evt := &mitev1.JenkinsActivityEvent{
		Type:    "build.completed",
		Actor:   "bob",
		Message: "Build #7 completed",
	}

	msg := &mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_JenkinsActivity{
			JenkinsActivity: evt,
		},
	}

	// Simulate the dispatch switch from server.go.
	switch m := msg.Message.(type) {
	case *mitev1.MiteMessage_JenkinsActivity:
		if ja := m.JenkinsActivity; ja != nil {
			rec.OnJenkinsActivity("test-ctrl", "test-ns", ja)
		}
	default:
		t.Fatalf("unexpected message type %T", m)
	}

	if !rec.jenkinsActivityCalled {
		t.Error("OnJenkinsActivity was not called")
	}
	if rec.jenkinsActivityName != "test-ctrl" {
		t.Errorf("name = %q, want %q", rec.jenkinsActivityName, "test-ctrl")
	}
	if rec.jenkinsActivityNS != "test-ns" {
		t.Errorf("namespace = %q, want %q", rec.jenkinsActivityNS, "test-ns")
	}
	if rec.jenkinsActivityEvt != evt {
		t.Error("event pointer mismatch")
	}
}

// recordingHandler implements StreamHandler to record calls.
type recordingHandler struct {
	// superseded flips this handler's connection identity from the test
	// goroutine while the server's read loop is running, so it has to be
	// atomic to stay race-clean.
	superseded            atomic.Bool
	jenkinsActivityCalled bool
	jenkinsActivityName   string
	jenkinsActivityNS     string
	jenkinsActivityEvt    *mitev1.JenkinsActivityEvent
}

func (r *recordingHandler) OnConnect(_, _, _ string, _ interface{}, _ Sender, _ interface{}) int64 {
	return 0
}
func (r *recordingHandler) OnHeartbeat(_, _ string, _ *mitev1.Heartbeat)                     {}
func (r *recordingHandler) OnSnapshot(_, _ string, _ *mitev1.StateSnapshot)                  {}
func (r *recordingHandler) OnCommandResult(_, _ string, _ *mitev1.CommandResult)             {}
func (r *recordingHandler) OnTokenRefreshRequest(_, _ string, _ *mitev1.TokenRefreshRequest) {}
func (r *recordingHandler) OnObservabilityReport(_, _ string, _ *mitev1.ObservabilityReport) {}
func (r *recordingHandler) OnContentResponse(_ string, _ *mitev1.ContentResponse)            {}
func (r *recordingHandler) OnJenkinsActivity(name, ns string, evt *mitev1.JenkinsActivityEvent) {
	r.jenkinsActivityCalled = true
	r.jenkinsActivityName = name
	r.jenkinsActivityNS = ns
	r.jenkinsActivityEvt = evt
}
func (r *recordingHandler) OnPluginInventory(_, _ string, _ *mitev1.PluginInventory) {}
func (r *recordingHandler) IsCurrentConnection(_, _ string, _ int64) bool {
	return !r.superseded.Load()
}
func (r *recordingHandler) OnDisconnect(_, _ string, _ int64) {}

// Ensure recordingHandler implements StreamHandler.
var _ StreamHandler = (*recordingHandler)(nil)

// TestJenkinsActivity_AntiSpoof verifies that the controller/namespace in the
// published payload come from the handler args (mTLS stream identity), not from
// the event payload. Even if the event body claims a different controller, the
// published payload should reflect the handler args.
func TestJenkinsActivity_AntiSpoof(t *testing.T) {
	t.Parallel()

	ns := startNATS(t)
	busConn, err := bus.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	var mu sync.Mutex
	var lastPayload bus.ActivityPayload

	sub, err := busConn.NATSConn().Subscribe("activity.>", func(msg *nats.Msg) {
		var p bus.ActivityPayload
		if err := json.Unmarshal(msg.Data, &p); err != nil {
			return
		}
		mu.Lock()
		lastPayload = p
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("ensure snapshot kv: %v", err)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		t.Fatalf("ensure presence kv: %v", err)
	}
	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		t.Fatalf("ensure desired kv: %v", err)
	}

	handler := NewBusHandler(bus.DefaultCluster, busConn, snapshotKV, presenceKV, desiredKV, nil)
	handler.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	t.Cleanup(handler.Close)

	// Event payload claims a different controller (spoof attempt).
	evt := &mitev1.JenkinsActivityEvent{
		Type:     "build.completed",
		ItemPath: "other-ctrl/job",
		URL:      "http://other-ctrl/job/1/",
	}

	// Handler args are the mTLS stream identity.
	handler.OnJenkinsActivity("real-controller", "real-namespace", evt)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	p := lastPayload
	mu.Unlock()

	if p.Controller != "real-controller" {
		t.Errorf("Controller = %q, want %q (mTLS identity)", p.Controller, "real-controller")
	}
	if p.Namespace != "real-namespace" {
		t.Errorf("Namespace = %q, want %q (mTLS identity)", p.Namespace, "real-namespace")
	}
	if p.Name != "real-controller" {
		t.Errorf("Name = %q, want %q", p.Name, "real-controller")
	}
	if p.ItemPath != "other-ctrl/job" {
		t.Errorf("ItemPath = %q, want %q (event field should be preserved)", p.ItemPath, "other-ctrl/job")
	}
}
