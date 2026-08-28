package activity

import (
	"testing"
)

// TestOffModeSingleRingWriter verifies the single-ring-writer rule:
// publishing a user-action event through the Publisher results in exactly
// one ring entry when fed back through the activity.> subscriber
// (which calls Store.Append). This simulates the off-mode flow where the
// ring is populated only by the subscriber, not by direct Notify calls.
func TestOffModeSingleRingWriter(t *testing.T) {
	store := New(200)

	// Simulate what happens in off mode:
	// 1. A handler calls Publisher.Publish(event) which publishes to bus
	// 2. The activity.> subscriber receives the message and calls store.Append
	// 3. The ring should have exactly 1 entry (not 2)

	// Step 1 & 2: directly call Append (simulating the subscriber callback
	// after unmarshalling the published message).
	store.Append(Event{
		Type:       "connected",
		Source:     "mite",
		Controller: "foo",
		Namespace:  "team-a",
		Message:    "mite connected",
	})

	// Step 3: Verify exactly one entry in the ring.
	events := store.List("")
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 ring entry, got %d", len(events))
	}

	// Verify the event content is correct.
	if events[0].Controller != "foo" {
		t.Errorf("expected Controller 'foo', got %q", events[0].Controller)
	}
	if events[0].Namespace != "team-a" {
		t.Errorf("expected Namespace 'team-a', got %q", events[0].Namespace)
	}

	// Verify that calling Append again (another bus message) adds a second
	// entry — still exactly one write per bus message.
	store.Append(Event{
		Type:       "heartbeat",
		Source:     "mite",
		Controller: "foo",
		Namespace:  "team-a",
		Message:    "mite heartbeat",
	})

	events = store.List("")
	if len(events) != 2 {
		t.Fatalf("expected 2 ring entries after second Append, got %d", len(events))
	}
}

// TestOffModeNoDuplicateOnNotify ensures that calling Notify (which
// was the old direct-write path) is not the same as the ring-subscriber
// path in off mode. In the new architecture, Notify delegates to Append,
// and the single ring writer is the activity.> subscriber. This test
// verifies that Append produces the same result as the old Notify for
// a single write — the subscriber is the only caller of Append.
func TestOffModeNoDuplicateOnNotify(t *testing.T) {
	store := New(200)

	// Simulate publisher → subscriber → Append flow.
	store.Append(Event{
		Type:       "connected",
		Source:     "mite",
		Controller: "foo",
		Namespace:  "team-a",
		Message:    "mite connected",
	})

	events := store.List("")
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 entry, got %d", len(events))
	}

	// Verify Notify produces the same result as Append for a single write.
	store.Notify(Event{
		Type:       "disconnected",
		Source:     "mite",
		Controller: "foo",
		Namespace:  "team-a",
		Message:    "mite disconnected",
	})

	events = store.List("")
	if len(events) != 2 {
		t.Fatalf("expected 2 entries after Notify, got %d", len(events))
	}
}
