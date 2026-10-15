package recordstore

import (
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestEnsureBuildsStreamCreatesD3Config starts against an empty
// JetStream and asserts EnsureBuildsStream creates varroa_builds with
// design D3's configuration.
func TestEnsureBuildsStreamCreatesD3Config(t *testing.T) {
	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure builds stream: %v", err)
	}

	info, err := conn.JetStream().StreamInfo(bus.BuildsStreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != bus.BuildsWildcard {
		t.Fatalf("subjects = %v, want [%s]", info.Config.Subjects, bus.BuildsWildcard)
	}
	if info.Config.Retention != nats.WorkQueuePolicy {
		t.Fatalf("retention = %v, want WorkQueuePolicy", info.Config.Retention)
	}
	if info.Config.Discard != nats.DiscardNew {
		t.Fatalf("discard = %v, want DiscardNew", info.Config.Discard)
	}
	if info.Config.MaxAge != 0 {
		t.Fatalf("max age = %v, want 0", info.Config.MaxAge)
	}
	if info.Config.MaxBytes != 1<<30 {
		t.Fatalf("max bytes = %d, want %d", info.Config.MaxBytes, int64(1<<30))
	}
	if info.Config.Storage != nats.FileStorage {
		t.Fatalf("storage = %v, want FileStorage", info.Config.Storage)
	}

	// EnsureBuildsStream is idempotent: calling it again must not error.
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure builds stream (idempotent call): %v", err)
	}
}
