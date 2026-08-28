package activity

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestJetStreamBackfillIntegration tests the JetStream-backed backfill with
// an embedded NATS server. It skips if NATS is not available (CI).
func TestJetStreamBackfillIntegration(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping JetStream integration test; set NATS_TEST=1 to run")
	}

	// Start embedded NATS server.
	opts := &server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	// Create the activity stream.
	maxAge := 168 * time.Hour
	cfg := bus.ActivityStreamConfig(bus.ActivityStreamName, maxAge, 100000, 1<<30)
	if err := conn.EnsureStream(cfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	// Publish some events to various subjects.
	publish := func(subj string, e Event) {
		data, _ := json.Marshal(e)
		if err := conn.Publish(subj, data); err != nil {
			t.Fatalf("publish to %s: %v", subj, err)
		}
	}

	// Wait for JetStream to capture.
	time.Sleep(200 * time.Millisecond)

	publish(bus.ActivitySubject(bus.DefaultCluster, "team-a", "foo"), Event{
		Timestamp:  time.Now().Add(-3 * time.Minute),
		Type:       "connected",
		Source:     "mite",
		Controller: "foo",
		Namespace:  "team-a",
		Message:    "mite v1 connected",
	})
	publish(bus.ActivitySubject(bus.DefaultCluster, "team-a", "foo"), Event{
		Timestamp:  time.Now().Add(-2 * time.Minute),
		Type:       "heartbeat",
		Source:     "mite",
		Controller: "foo",
		Namespace:  "team-a",
		Message:    "mite heartbeat",
	})
	publish(bus.ActivitySubject(bus.DefaultCluster, "team-b", "bar"), Event{
		Timestamp:  time.Now().Add(-1 * time.Minute),
		Type:       "connected",
		Source:     "mite",
		Controller: "bar",
		Namespace:  "team-b",
		Message:    "mite v1 connected",
	})
	publish(bus.ActivityGlobal(bus.DefaultCluster), Event{
		Timestamp: time.Now(),
		Type:      "operator.event",
		Source:    "operator",
		Message:   "operator started",
	})

	// Allow stream to index.
	time.Sleep(200 * time.Millisecond)

	// Test global feed.
	bf := NewJetStreamBackfill(bus.DefaultCluster, conn, bus.ActivityStreamName, 500)
	events, err := bf.Recent(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("global backfill: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 events in global feed, got %d", len(events))
	}
	// Newest first.
	if events[0].Type != "operator.event" {
		t.Errorf("expected newest event type 'operator.event', got %q", events[0].Type)
	}

	// Test per-controller feed.
	events, err = bf.Recent(context.Background(), "team-a/foo", 10)
	if err != nil {
		t.Fatalf("per-controller backfill: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events for team-a/foo, got %d", len(events))
	}
	for _, e := range events {
		if e.Controller != "foo" {
			t.Errorf("expected Controller 'foo', got %q", e.Controller)
		}
	}

	// Test limit.
	events, err = bf.Recent(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("limited backfill: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

// TestJetStreamBackfillIntegration_LegacySubjects tests that the JetStream
// backfill tolerates legacy-subject messages retained from before the
// multicluster subject cutover — per-controller reads exclude them, the
// global read returns them unattributed, and undecodable messages are
// skipped without aborting.
func TestJetStreamBackfillIntegration_LegacySubjects(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping JetStream integration test; set NATS_TEST=1 to run")
	}

	// Start embedded NATS server.
	opts := &server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	// Create the activity stream.
	maxAge := 168 * time.Hour
	cfg := bus.ActivityStreamConfig(bus.ActivityStreamName, maxAge, 100000, 1<<30)
	if err := conn.EnsureStream(cfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	// Publish helper.
	publish := func(subj string, e Event) {
		data, _ := json.Marshal(e)
		if err := conn.Publish(subj, data); err != nil {
			t.Fatalf("publish to %s: %v", subj, err)
		}
	}

	// Wait for JetStream to capture.
	time.Sleep(200 * time.Millisecond)

	// 1. Current 4-token (cluster-qualified) subject.
	publish(bus.ActivitySubject(bus.DefaultCluster, "team-a", "foo"), Event{
		Type:       "connected",
		Controller: "foo",
		Namespace:  "team-a",
		Source:     "mite",
	})

	// 2. Legacy 3-token subject (pre-cutover: activity.<ns>.<ctrl>).
	data, _ := json.Marshal(Event{
		Type:       "legacy-controller",
		Controller: "foo",
		Namespace:  "team-a",
		Source:     "mite",
	})
	if err := conn.Publish("activity.team-a.foo", data); err != nil {
		t.Fatalf("publish legacy 3-token: %v", err)
	}

	// 3. Legacy 2-token global subject (activity._global).
	data, _ = json.Marshal(Event{
		Type:   "legacy-global",
		Source: "operator",
	})
	if err := conn.Publish("activity._global", data); err != nil {
		t.Fatalf("publish legacy 2-token: %v", err)
	}

	// 4. Undecodable payload (proving skip-not-abort).
	if err := conn.Publish("activity.team-b.junk", []byte("not-json{{{")); err != nil {
		t.Fatalf("publish undecodable: %v", err)
	}

	// Allow stream to index.
	time.Sleep(200 * time.Millisecond)

	bf := NewJetStreamBackfill(bus.DefaultCluster, conn, bus.ActivityStreamName, 500)

	// Per-controller exclusion: legacy 3-token message is not returned.
	events, err := bf.Recent(context.Background(), "team-a/foo", 10)
	if err != nil {
		t.Fatalf("per-controller backfill: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event for team-a/foo, got %d", len(events))
	}
	if events[0].Type != "connected" {
		t.Errorf("expected event type 'connected', got %q", events[0].Type)
	}

	// Global tolerance: legacy messages returned unattributed, undecodable
	// payload skipped without abort.
	events, err = bf.Recent(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("global backfill: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events in global feed (undecodable skipped), got %d", len(events))
	}
	types := make(map[string]bool)
	for _, e := range events {
		types[e.Type] = true
	}
	for _, want := range []string{"connected", "legacy-controller", "legacy-global"} {
		if !types[want] {
			t.Errorf("expected type %q in global results", want)
		}
	}
	// Legacy events are never re-attributed — Cluster is empty.
	for _, e := range events {
		if e.Type == "legacy-controller" || e.Type == "legacy-global" {
			if e.Cluster != "" {
				t.Errorf("expected legacy event %q to have empty Cluster, got %q", e.Type, e.Cluster)
			}
		}
	}
}
