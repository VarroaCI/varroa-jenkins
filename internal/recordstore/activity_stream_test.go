package recordstore

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestEnsureActivityStreamCreatesWhenAbsent asserts D2's step 1: an
// absent stream is created with the fixed limits.
func TestEnsureActivityStreamCreatesWhenAbsent(t *testing.T) {
	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}

	info, err := conn.StreamInfo(bus.ActivityStreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Config.MaxAge != 72*time.Hour {
		t.Fatalf("max age = %v, want 72h", info.Config.MaxAge)
	}
	if info.Config.MaxMsgs != 1000000 {
		t.Fatalf("max msgs = %d, want 1000000", info.Config.MaxMsgs)
	}
	if info.Config.MaxBytes != 1<<30 {
		t.Fatalf("max bytes = %d, want %d", info.Config.MaxBytes, int64(1<<30))
	}

	// Idempotent: calling it again against the now-existing, already
	// target-configured stream must not error.
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream (2nd): %v", err)
	}
}

// TestEnsureActivityStreamPreservesLongerRetentionUntilDrained asserts
// D2's steps 2-4: a stored MaxAge longer than the fixed target survives
// while the activity-recorder consumer holds pending or unacknowledged
// work, and the target applies only once both counters reach zero.
func TestEnsureActivityStreamPreservesLongerRetentionUntilDrained(t *testing.T) {
	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	// Simulate a stream an earlier release created with a longer
	// retention (the old activity.retention="90d" dial).
	longRetention := &nats.StreamConfig{
		Name:      bus.ActivityStreamName,
		Subjects:  []string{bus.ActivityWildcard},
		Retention: nats.LimitsPolicy,
		MaxAge:    2160 * time.Hour,
		MaxMsgs:   100000,
		MaxBytes:  1 << 30,
		Discard:   nats.DiscardOld,
		Storage:   nats.FileStorage,
	}
	if err := conn.EnsureStream(longRetention); err != nil {
		t.Fatalf("create long-retention stream: %v", err)
	}

	// A publisher's fire-and-forget core publish, matching the
	// activity-persistence requirement.
	if err := conn.Publish("activity.core.team-a.foo", []byte(`{"type":"test"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Give the server a moment to store the message before StreamInfo is read.
	waitForRow(t, func() bool {
		info, err := conn.StreamInfo(bus.ActivityStreamName)
		return err == nil && info.State.Msgs == 1
	})

	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream (pending work): %v", err)
	}

	info, err := conn.StreamInfo(bus.ActivityStreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Config.MaxAge != 2160*time.Hour {
		t.Fatalf("max age = %v, want the stored 2160h to survive while the consumer holds work", info.Config.MaxAge)
	}

	// Drain the one pending message so both NumPending and NumAckPending
	// reach zero.
	sub, err := conn.PullSubscribe(bus.ActivityWildcard, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	msgs, err := conn.DrainConsumer(sub, 10)
	if err != nil {
		t.Fatalf("drain consumer: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 pending message, got %d", len(msgs))
	}
	if err := msgs[0].Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	_ = sub.Unsubscribe()

	waitForRow(t, func() bool {
		cinfo, err := conn.ConsumerInfo(bus.ActivityStreamName, ActivityRecorderConsumerName)
		return err == nil && cinfo.NumPending == 0 && cinfo.NumAckPending == 0
	})

	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream (drained): %v", err)
	}

	info, err = conn.StreamInfo(bus.ActivityStreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Config.MaxAge != 72*time.Hour {
		t.Fatalf("max age = %v, want the fixed 72h to apply once the consumer drains", info.Config.MaxAge)
	}
}
