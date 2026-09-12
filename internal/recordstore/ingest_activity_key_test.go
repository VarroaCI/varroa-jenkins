package recordstore

import (
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// TestEventKey covers D1's edge cases: an absent cluster, a zero build
// number, an empty message, and a non-UTC timestamp. It also asserts two
// equal events produce the same key, which is the property a redelivery
// relies on.
func TestEventKey(t *testing.T) {
	pdt := time.FixedZone("PDT", -7*60*60)

	tests := []struct {
		name string
		e    activity.Event
	}{
		{
			name: "absent cluster",
			e: activity.Event{
				Cluster:   "",
				Timestamp: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
				Source:    "operator",
				Type:      "controller.created",
				Message:   "created",
			},
		},
		{
			name: "zero build number",
			e: activity.Event{
				Cluster:     "core",
				Timestamp:   time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
				Source:      "jenkins",
				Type:        "run.completed",
				BuildNumber: 0,
				Message:     "done",
			},
		},
		{
			name: "empty message",
			e: activity.Event{
				Cluster:   "core",
				Timestamp: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
				Source:    "mite",
				Type:      "heartbeat",
				Message:   "",
			},
		},
		{
			name: "non-UTC timestamp",
			e: activity.Event{
				Cluster:   "core",
				Timestamp: time.Date(2026, 9, 10, 5, 0, 0, 0, pdt),
				Source:    "mite",
				Type:      "heartbeat",
				Message:   "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := EventKey(tt.e)
			if len(key) != 64 {
				t.Fatalf("EventKey(%+v) = %q, want 64 hex characters, got length %d", tt.e, key, len(key))
			}
		})
	}

	// A non-UTC timestamp must normalize to the same key as its UTC
	// equivalent, because EventKey formats the timestamp in UTC before
	// hashing it.
	utcEvent := activity.Event{
		Cluster:   "core",
		Timestamp: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Source:    "mite",
		Type:      "heartbeat",
		Message:   "",
	}
	pdtEvent := activity.Event{
		Cluster:   "core",
		Timestamp: time.Date(2026, 9, 10, 5, 0, 0, 0, pdt),
		Source:    "mite",
		Type:      "heartbeat",
		Message:   "",
	}
	if EventKey(utcEvent) != EventKey(pdtEvent) {
		t.Fatalf("EventKey should be equal across time zones for the same instant: %q != %q",
			EventKey(utcEvent), EventKey(pdtEvent))
	}

	// Two structurally-equal events produce the same key, which is the
	// property a redelivery relies on to avoid a duplicate row.
	a := activity.Event{
		Cluster: "core", Timestamp: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Source: "operator", Type: "controller.created", Namespace: "jenkins", Controller: "foo",
		BuildNumber: 5, Message: "created",
	}
	b := a
	if EventKey(a) != EventKey(b) {
		t.Fatalf("EventKey should be equal for equal events: %q != %q", EventKey(a), EventKey(b))
	}
}
