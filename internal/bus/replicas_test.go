package bus

import (
	"testing"
	"time"
)

// TestClampReplicas verifies the shared floor-of-1 clamp used everywhere a
// JetStream replica count is set.
func TestClampReplicas(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{-5, 1},
		{-1, 1},
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 3},
		{7, 7}, // clamp is a floor only; the 1..3 cap lives in the chart
	}
	for _, tc := range cases {
		if got := clampReplicas(tc.in); got != tc.want {
			t.Errorf("clampReplicas(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestConnReplicas verifies the Conn replica field: zero value defaults to 1,
// SetReplicas clamps values < 1 to 1.
func TestConnReplicas(t *testing.T) {
	var c Conn
	if got := c.Replicas(); got != 1 {
		t.Fatalf("zero-value Conn.Replicas() = %d, want 1", got)
	}
	c.SetReplicas(3)
	if got := c.Replicas(); got != 3 {
		t.Fatalf("after SetReplicas(3), Replicas() = %d, want 3", got)
	}
	c.SetReplicas(0)
	if got := c.Replicas(); got != 1 {
		t.Fatalf("after SetReplicas(0), Replicas() = %d, want 1 (clamped)", got)
	}
	c.SetReplicas(-4)
	if got := c.Replicas(); got != 1 {
		t.Fatalf("after SetReplicas(-4), Replicas() = %d, want 1 (clamped)", got)
	}
}

// TestStreamConfigReplicas verifies StreamConfig honors the variadic replica
// count: omitted defaults to 1, a value is used verbatim, and < 1 clamps to 1.
func TestStreamConfigReplicas(t *testing.T) {
	if r := StreamConfig("varroa").Replicas; r != 1 {
		t.Errorf("StreamConfig(name) default Replicas = %d, want 1", r)
	}
	if r := StreamConfig("varroa", 3).Replicas; r != 3 {
		t.Errorf("StreamConfig(name, 3) Replicas = %d, want 3", r)
	}
	if r := StreamConfig("varroa", 0).Replicas; r != 1 {
		t.Errorf("StreamConfig(name, 0) Replicas = %d, want 1 (clamped)", r)
	}
	if r := StreamConfig("varroa", -2).Replicas; r != 1 {
		t.Errorf("StreamConfig(name, -2) Replicas = %d, want 1 (clamped)", r)
	}
}

// TestActivityStreamConfigReplicas verifies ActivityStreamConfig honors the
// variadic replica count the same way as StreamConfig.
func TestActivityStreamConfigReplicas(t *testing.T) {
	const name = "varroa_activity"
	if r := ActivityStreamConfig(name, time.Hour, 1, 1).Replicas; r != 1 {
		t.Errorf("ActivityStreamConfig(...) default Replicas = %d, want 1", r)
	}
	if r := ActivityStreamConfig(name, time.Hour, 1, 1, 3).Replicas; r != 3 {
		t.Errorf("ActivityStreamConfig(..., 3) Replicas = %d, want 3", r)
	}
	if r := ActivityStreamConfig(name, time.Hour, 1, 1, 0).Replicas; r != 1 {
		t.Errorf("ActivityStreamConfig(..., 0) Replicas = %d, want 1 (clamped)", r)
	}
}

// TestWebhookStreamConfigReplicas verifies WebhookStreamConfig sets and clamps
// the (non-variadic) replica count.
func TestWebhookStreamConfigReplicas(t *testing.T) {
	if r := WebhookStreamConfig("varroa_webhooks", 3).Replicas; r != 3 {
		t.Errorf("WebhookStreamConfig(name, 3) Replicas = %d, want 3", r)
	}
	if r := WebhookStreamConfig("varroa_webhooks", 0).Replicas; r != 1 {
		t.Errorf("WebhookStreamConfig(name, 0) Replicas = %d, want 1 (clamped)", r)
	}
}

// TestKeyValueConfigReplicas verifies the KV config builder wires and clamps
// the replica count (and keeps bucket/ttl/storage intact). This lets us assert
// EnsureKV's replica wiring without a live NATS server.
func TestKeyValueConfigReplicas(t *testing.T) {
	cfg := keyValueConfig("mite_snapshots", 5*time.Minute, 3)
	if cfg.Bucket != "mite_snapshots" {
		t.Errorf("keyValueConfig Bucket = %q, want mite_snapshots", cfg.Bucket)
	}
	if cfg.TTL != 5*time.Minute {
		t.Errorf("keyValueConfig TTL = %v, want 5m", cfg.TTL)
	}
	if cfg.Replicas != 3 {
		t.Errorf("keyValueConfig(_, _, 3) Replicas = %d, want 3", cfg.Replicas)
	}
	if r := keyValueConfig("b", 0, 0).Replicas; r != 1 {
		t.Errorf("keyValueConfig(_, _, 0) Replicas = %d, want 1 (clamped)", r)
	}
	if r := keyValueConfig("b", 0, -1).Replicas; r != 1 {
		t.Errorf("keyValueConfig(_, _, -1) Replicas = %d, want 1 (clamped)", r)
	}
}
